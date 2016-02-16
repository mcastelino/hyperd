package system

import (
	"github.com/hyperhq/hyper/server/router"
	"github.com/hyperhq/hyper/server/router/local"
)

// systemRouter is a Router that provides information about
// the Docker system overall. It gathers information about
// host, daemon and container events.
type systemRouter struct {
	backend Backend
	routes  []router.Route
}

// NewRouter initializes a new systemRouter
func NewRouter(b Backend) router.Router {
	r := &systemRouter{
		backend: b,
	}

	r.routes = []router.Route{
		local.NewGetRoute("/_ping", pingHandler),
		local.NewGetRoute("/info", r.getInfo),
		local.NewGetRoute("/version", r.getVersion),
		local.NewPostRoute("/auth", r.postAuth),
	}

	return r
}

// Routes return all the API routes dedicated to the docker system.
func (s *systemRouter) Routes() []router.Route {
	return s.routes
}
