// Package budget runs the background reclamation of expired reservations.
package budget

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/DiegohNY/costlane/internal/store"
)

// Reaper releases the budget held by reservations whose TTL has passed.
//
// It exists because a process that dies between reserve and settle leaves a
// reservation pending forever, and the key's budget permanently understated.
// Nothing else notices: the reserve is durable by design, and the settle that
// would have released it never arrives.
type Reaper struct {
	db       *store.DB
	interval time.Duration
	batch    int
	logger   *slog.Logger
	metrics  Metrics
}

// Metrics receives what a sweep did. It is an interface so the budget package
// does not depend on a metrics backend.
type Metrics interface {
	ReservationsReaped(n int)
	ReapDuration(d time.Duration)
	DeadlockRetries(n int)
}

// Options configures a reaper.
type Options struct {
	DB       *store.DB
	Interval time.Duration
	Batch    int
	Logger   *slog.Logger
	Metrics  Metrics
}

// NewReaper builds a reaper.
func NewReaper(opts Options) *Reaper {
	if opts.Interval <= 0 {
		opts.Interval = 30 * time.Second
	}
	if opts.Batch <= 0 {
		opts.Batch = 500
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Metrics == nil {
		opts.Metrics = noopMetrics{}
	}
	return &Reaper{
		db:       opts.DB,
		interval: opts.Interval,
		batch:    opts.Batch,
		logger:   opts.Logger,
		metrics:  opts.Metrics,
	}
}

// Run sweeps until the context is cancelled.
//
// On cancellation it returns after the batch in flight has finished, so a
// shutdown never abandons a sweep midway and leaves reservations claimed but
// unreleased.
//
// Each interval carries jitter, so several replicas started by the same
// deployment do not converge on sweeping at the same instant.
func (r *Reaper) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.jittered()):
		}

		// The sweep uses a context detached from the caller's, so a
		// SIGTERM arriving mid-batch lets it finish rather than aborting
		// a transaction that has already claimed rows.
		sweepCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.interval)
		r.sweep(sweepCtx)
		cancel()

		if ctx.Err() != nil {
			return
		}
	}
}

// Sweep runs one batch. It is exported so a test can drive it directly rather
// than waiting for a tick.
func (r *Reaper) Sweep(ctx context.Context) {
	r.sweep(ctx)
}

func (r *Reaper) sweep(ctx context.Context) {
	started := time.Now()
	out, err := r.db.ReapExpired(ctx, r.batch)
	r.metrics.ReapDuration(time.Since(started))

	if err != nil {
		// A failed sweep is not fatal: the reservations stay pending and
		// the next sweep tries again. Logging it is what turns a silent
		// budget leak into something someone can see.
		r.logger.Error("reaping expired reservations failed", "error", err)
		return
	}
	if out.DeadlockRetries > 0 {
		r.metrics.DeadlockRetries(out.DeadlockRetries)
	}
	if out.Reaped > 0 {
		r.metrics.ReservationsReaped(out.Reaped)
		r.logger.Info("released expired reservations", "count", out.Reaped)
	}
}

// jittered spreads sweeps across replicas by up to a quarter of the interval.
//
// math/rand is deliberate: this decides when to sweep, not anything an
// adversary gains from predicting. Drawing scheduling jitter from
// crypto/rand would cost entropy for no security benefit.
func (r *Reaper) jittered() time.Duration {
	//nolint:gosec // scheduling jitter, not a security decision
	jitter := time.Duration(rand.Int64N(int64(r.interval / 4)))
	return r.interval + jitter
}

type noopMetrics struct{}

func (noopMetrics) ReservationsReaped(int)     {}
func (noopMetrics) ReapDuration(time.Duration) {}
func (noopMetrics) DeadlockRetries(int)        {}
