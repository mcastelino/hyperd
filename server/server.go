package server

import (
	"crypto/tls"
	"net"
	"net/http"
	"strings"

	ddaemon "github.com/docker/docker/daemon"
	"github.com/docker/docker/pkg/authorization"
	"github.com/docker/docker/utils"
	"github.com/docker/go-connections/sockets"
	"github.com/golang/glog"
	"github.com/hyperhq/hyper/daemon"
	"github.com/hyperhq/hyper/server/httputils"
	"github.com/hyperhq/hyper/server/router"
	"github.com/hyperhq/hyper/server/router/build"
	"github.com/hyperhq/hyper/server/router/container"
	"github.com/hyperhq/hyper/server/router/local"
	"github.com/hyperhq/hyper/server/router/pod"
	"github.com/hyperhq/hyper/server/router/service"
	"github.com/hyperhq/hyper/server/router/system"

	"github.com/gorilla/mux"
	"golang.org/x/net/context"
)

// versionMatcher defines a variable matcher to be parsed by the router
// when a request is about to be served.
const versionMatcher = "/v{version:[0-9.]+}"

// Config provides the configuration for the API server
type Config struct {
	Logging                  bool
	EnableCors               bool
	CorsHeaders              string
	AuthorizationPluginNames []string
	Version                  string
	SocketGroup              string
	TLSConfig                *tls.Config
	Addrs                    []Addr
}

// Server contains instance details for the server
type Server struct {
	cfg           *Config
	servers       []*HTTPServer
	routers       []router.Router
	authZPlugins  []authorization.Plugin
	routerSwapper *routerSwapper
}

// Addr contains string representation of address and its protocol (tcp, unix...).
type Addr struct {
	Proto string
	Addr  string
}

// New returns a new instance of the server based on the specified configuration.
// It allocates resources which will be needed for ServeAPI(ports, unix-sockets).
func New(cfg *Config) (*Server, error) {
	s := &Server{
		cfg: cfg,
	}
	for _, addr := range cfg.Addrs {
		srv, err := s.newServer(addr.Proto, addr.Addr)
		if err != nil {
			return nil, err
		}
		glog.V(3).Infof("Server created for HTTP on %s (%s)", addr.Proto, addr.Addr)
		s.servers = append(s.servers, srv...)
	}
	return s, nil
}

// Close closes servers and thus stop receiving requests
func (s *Server) Close() {
	for _, srv := range s.servers {
		if err := srv.Close(); err != nil {
			glog.Error(err)
		}
	}
}

// serveAPI loops through all initialized servers and spawns goroutine
// with Server method for each. It sets createMux() as Handler also.
func (s *Server) serveAPI() error {
	s.initRouterSwapper()

	var chErrors = make(chan error, len(s.servers))
	for _, srv := range s.servers {
		srv.srv.Handler = s.routerSwapper
		go func(srv *HTTPServer) {
			var err error
			glog.V(3).Infof("API listen on %s", srv.l.Addr())
			if err = srv.Serve(); err != nil && strings.Contains(err.Error(), "use of closed network connection") {
				err = nil
			}
			chErrors <- err
		}(srv)
	}

	for i := 0; i < len(s.servers); i++ {
		err := <-chErrors
		if err != nil {
			return err
		}
	}

	return nil
}

// HTTPServer contains an instance of http server and the listener.
// srv *http.Server, contains configuration to create a http server and a mux router with all api end points.
// l   net.Listener, is a TCP or Socket listener that dispatches incoming request to the router.
type HTTPServer struct {
	srv *http.Server
	l   net.Listener
}

// Serve starts listening for inbound requests.
func (s *HTTPServer) Serve() error {
	return s.srv.Serve(s.l)
}

// Close closes the HTTPServer from listening for the inbound requests.
func (s *HTTPServer) Close() error {
	return s.l.Close()
}

func writeCorsHeaders(w http.ResponseWriter, r *http.Request, corsHeaders string) {
	glog.V(3).Infof("CORS header is enabled and set to: %s", corsHeaders)
	w.Header().Add("Access-Control-Allow-Origin", corsHeaders)
	w.Header().Add("Access-Control-Allow-Headers", "Origin, X-Requested-With, Content-Type, Accept, X-Registry-Auth")
	w.Header().Add("Access-Control-Allow-Methods", "HEAD, GET, POST, DELETE, PUT, OPTIONS")
}

func (s *Server) initTCPSocket(addr string) (l net.Listener, err error) {
	if s.cfg.TLSConfig == nil || s.cfg.TLSConfig.ClientAuth != tls.RequireAndVerifyClientCert {
		glog.Warning("/!\\ DON'T BIND ON ANY IP ADDRESS WITHOUT setting -tlsverify IF YOU DON'T KNOW WHAT YOU'RE DOING /!\\")
	}
	if l, err = sockets.NewTCPSocket(addr, s.cfg.TLSConfig); err != nil {
		return nil, err
	}

	return l, nil
}

func (s *Server) makeHTTPHandler(handler httputils.APIFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// log the handler call
		glog.V(3).Infof("Calling %s %s", r.Method, r.URL.Path)

		// Define the context that we'll pass around to share info
		// like the docker-request-id.
		//
		// The 'context' will be used for global data that should
		// apply to all requests. Data that is specific to the
		// immediate function being called should still be passed
		// as 'args' on the function call.
		ctx := context.Background()
		handlerFunc := s.handleWithGlobalMiddlewares(handler)

		vars := mux.Vars(r)
		if vars == nil {
			vars = make(map[string]string)
		}

		if err := handlerFunc(ctx, w, r, vars); err != nil {
			glog.Errorf("Handler for %s %s returned error: %s", r.Method, r.URL.Path, utils.GetErrorMessage(err))
			httputils.WriteError(w, err)
		}
	}
}

// InitRouters initializes a list of routers for the server.
func (s *Server) InitRouters(d *daemon.Daemon) {
	s.addRouter(container.NewRouter(d))
	s.addRouter(pod.NewRouter(d))
	s.addRouter(service.NewRouter(d))
	s.addRouter(local.NewRouter(d))
	s.addRouter(system.NewRouter(d))
	s.addRouter(build.NewRouter(d))
}

// addRouter adds a new router to the server.
func (s *Server) addRouter(r router.Router) {
	s.routers = append(s.routers, r)
}

// createMux initializes the main router the server uses.
// we keep enableCors just for legacy usage, need to be removed in the future
func (s *Server) createMux() *mux.Router {
	m := mux.NewRouter()
	if utils.IsDebugEnabled() {
		profilerSetup(m, "/debug/")
	}

	glog.V(3).Infof("Registering routers")
	for _, apiRouter := range s.routers {
		for _, r := range apiRouter.Routes() {
			f := s.makeHTTPHandler(r.Handler())

			glog.V(3).Infof("Registering %s, %s", r.Method(), r.Path())
			m.Path(versionMatcher + r.Path()).Methods(r.Method()).Handler(f)
			m.Path(r.Path()).Methods(r.Method()).Handler(f)
		}
	}

	return m
}

// Wait blocks the server goroutine until it exits.
// It sends an error message if there is any error during
// the API execution.
func (s *Server) Wait(waitChan chan error) {
	if err := s.serveAPI(); err != nil {
		glog.Errorf("ServeAPI error: %v", err)
		waitChan <- err
		return
	}
	waitChan <- nil
}

func (s *Server) initRouterSwapper() {
	s.routerSwapper = &routerSwapper{
		router: s.createMux(),
	}
}

// Reload reads configuration changes and modifies the
// server according to those changes.
// Currently, only the --debug configuration is taken into account.
func (s *Server) Reload(config *ddaemon.Config) {
	debugEnabled := utils.IsDebugEnabled()
	switch {
	case debugEnabled && !config.Debug: // disable debug
		utils.DisableDebug()
		s.routerSwapper.Swap(s.createMux())
	case config.Debug && !debugEnabled: // enable debug
		utils.EnableDebug()
		s.routerSwapper.Swap(s.createMux())
	}
}
