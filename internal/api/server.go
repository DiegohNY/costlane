package api

import (
	"net/http"

	"github.com/DiegohNY/costlane/internal/obs"
	"github.com/DiegohNY/costlane/internal/store"
)

// Options configures the server.
type Options struct {
	DB        *store.DB
	MasterKey obs.Secret

	// Proxy and Models are mounted when set, so the admin surface can be
	// served on its own during earlier phases.
	Proxy  http.Handler
	Models http.Handler

	// MaxDrainMS bounds a per-key drain timeout, mirroring the boot-time
	// validation so an operator cannot set through the API what the
	// configuration would have refused.
	MaxDrainMS int
}

// Server holds the HTTP routes.
type Server struct {
	db         *store.DB
	masterKey  obs.Secret
	maxDrainMS int
	proxy      http.Handler
	models     http.Handler
}

// New builds a server.
func New(opts Options) *Server {
	return &Server{
		db:         opts.DB,
		masterKey:  opts.MasterKey,
		maxDrainMS: opts.MaxDrainMS,
		proxy:      opts.Proxy,
		models:     opts.Models,
	}
}

// Handler returns the routed handler with its middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /admin/keys", s.requireMaster(s.handleCreateKey))
	mux.HandleFunc("GET /admin/keys", s.requireMaster(s.handleListKeys))
	mux.HandleFunc("PATCH /admin/keys/{id}", s.requireMaster(s.handleUpdateKey))
	mux.HandleFunc("DELETE /admin/keys/{id}", s.requireMaster(s.handleRevokeKey))

	if s.proxy != nil {
		mux.Handle("POST /v1/chat/completions", s.proxy)
	}
	if s.models != nil {
		mux.Handle("GET /v1/models", s.models)
	}

	// Credentials are rejected from the query string before any route runs,
	// so no handler can be reached by a URL that leaks one.
	return RejectQueryCredentials(mux)
}
