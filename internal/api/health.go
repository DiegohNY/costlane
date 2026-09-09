package api

import (
	"context"
	"net/http"
	"sync/atomic"
)

// Health reports whether the process should receive traffic.
//
// Liveness and readiness answer different questions on purpose. Liveness asks
// whether the process is running; readiness asks whether it can serve. Putting
// the database in liveness would restart every pod when the database went
// away, turning a recoverable outage into a crash loop.
type Health struct {
	db       Pinger
	pricesOK func() bool
	draining atomic.Bool
}

// Pinger is the little of a pool that a health check needs.
type Pinger interface {
	PingRead(ctx context.Context) error
}

// NewHealth builds the checks.
func NewHealth(db Pinger, pricesOK func() bool) *Health {
	return &Health{db: db, pricesOK: pricesOK}
}

// BeginDraining marks the process as shutting down.
//
// It is called first at SIGTERM, before anything stops accepting, so a load
// balancer sees 503 and stops sending traffic while requests can still be
// served. Reversing that order means refusing requests that were routed here
// because we still claimed to be ready.
func (h *Health) BeginDraining() { h.draining.Store(true) }

// Draining reports whether shutdown has begun.
func (h *Health) Draining() bool { return h.draining.Load() }

// Live answers whether the process is up. Nothing else: a dependency failing
// is not a reason to be restarted.
func (h *Health) Live(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// Ready answers whether this process can serve a request now.
func (h *Health) Ready(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	if h.draining.Load() {
		writePlain(w, http.StatusServiceUnavailable, "draining\n")
		return
	}
	if h.pricesOK != nil && !h.pricesOK() {
		// A gateway that cannot price a request has no business accepting
		// one: it would meter nothing.
		writePlain(w, http.StatusServiceUnavailable, "prices not loaded\n")
		return
	}
	if h.db != nil {
		if err := h.db.PingRead(r.Context()); err != nil {
			writePlain(w, http.StatusServiceUnavailable, "database unreachable\n")
			return
		}
	}
	writePlain(w, http.StatusOK, "ready\n")
}

func writePlain(w http.ResponseWriter, status int, body string) {
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
