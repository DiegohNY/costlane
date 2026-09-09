package api

import (
	"context"
	"net/http"
	"time"

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

	// MaxQueryWindow bounds a reporting range, so no query can walk the
	// whole table.
	MaxQueryWindow time.Duration

	// Health serves the liveness and readiness probes.
	Health *Health

	// ReloadPricing re-reads the price files. It returns an error rather
	// than swapping a broken table in, so a bad file leaves the running
	// one serving.
	ReloadPricing func(context.Context) error

	// MaxDrainMS bounds a per-key drain timeout, mirroring the boot-time
	// validation so an operator cannot set through the API what the
	// configuration would have refused.
	MaxDrainMS int
}

// Server holds the HTTP routes.
type Server struct {
	db             *store.DB
	masterKey      obs.Secret
	maxDrainMS     int
	maxQueryWindow time.Duration
	proxy          http.Handler
	models         http.Handler
	health         *Health
	reloadPricing  func(context.Context) error
}

// New builds a server.
func New(opts Options) *Server {
	return &Server{
		db:             opts.DB,
		masterKey:      opts.MasterKey,
		maxDrainMS:     opts.MaxDrainMS,
		proxy:          opts.Proxy,
		models:         opts.Models,
		maxQueryWindow: opts.MaxQueryWindow,
		health:         opts.Health,
		reloadPricing:  opts.ReloadPricing,
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

	// A virtual key reads its own usage; a master credential reads every
	// key's. The distinction is enforced in the repository, not here.
	mux.HandleFunc("GET /v1/usage", s.handleUsage)
	mux.HandleFunc("GET /v1/usage/requests", s.handleRequests)
	mux.HandleFunc("GET /v1/budget", s.handleBudget)

	mux.HandleFunc("GET /admin/reconcile", s.requireMaster(s.handleReconcile))
	mux.HandleFunc("POST /admin/pricing/reload", s.requireMaster(s.handlePricingReload))

	if s.health != nil {
		// Health probes carry no credential: a load balancer has none, and
		// neither endpoint reveals anything.
		mux.HandleFunc("GET /healthz", s.health.Live)
		mux.HandleFunc("GET /readyz", s.health.Ready)
	}

	// Credentials are rejected from the query string before any route runs,
	// so no handler can be reached by a URL that leaks one.
	return RejectQueryCredentials(mux)
}
