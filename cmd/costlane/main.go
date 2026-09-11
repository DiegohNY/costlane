// Command costlane runs the gateway.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/DiegohNY/costlane/internal/api"
	"github.com/DiegohNY/costlane/internal/budget"
	"github.com/DiegohNY/costlane/internal/config"
	"github.com/DiegohNY/costlane/internal/obs"
	"github.com/DiegohNY/costlane/internal/pricing"
	"github.com/DiegohNY/costlane/internal/provider"
	"github.com/DiegohNY/costlane/internal/proxy"
	"github.com/DiegohNY/costlane/internal/store"
	"github.com/DiegohNY/costlane/internal/usage"
)

// version is stamped at link time:
//
//	go build -ldflags="-X main.version=v0.1.0" ./cmd/costlane
//
// An unstamped build reports "dev", which is what a local one is.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "costlane: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger.Info("configuration loaded", "version", version, "config", cfg.String())

	db, err := store.Open(ctx, store.Options{
		DSN:                  cfg.DatabaseURL.Expose(),
		ReadStatementTimeout: cfg.ReadTimeout,
	})
	if err != nil {
		return err
	}
	defer db.Close()

	if cfg.MigrateOnBoot {
		if err := store.Migrate(ctx, db.Pool()); err != nil {
			return err
		}
		logger.Info("migrations applied")
	}

	// Prices are loaded before anything is served: a gateway that cannot
	// price a request has no business accepting one.
	table, err := pricing.LoadSeed()
	if err != nil {
		return fmt.Errorf("loading prices: %w", err)
	}
	prices := pricing.NewSnapshot(table)

	client := provider.NewHTTPClient(cfg.ProviderTimeout, 10*time.Second)
	router, err := buildRouter(cfg, client, table)
	if err != nil {
		return err
	}

	registry := obs.NewRegistry()
	metrics := obs.NewGatewayMetrics(registry)

	// The redactor knows this process's own credentials, so a provider
	// echoing one back in an error body cannot pass it on to a client.
	redactor := obs.NewRedactor(
		cfg.MasterKey, cfg.OpenAIKey, cfg.AnthropicKey, cfg.GoogleKey,
		databasePassword(cfg.DatabaseURL),
	)

	proxyOpts := proxy.Options{
		DB: db, Router: router, Pricing: prices, Logger: logger,
		Redactor:            redactor,
		Metrics:             metrics,
		LogPrompts:          cfg.LogPrompts,
		MaxBodyBytes:        cfg.MaxBodyBytes,
		DefaultMaxTokens:    cfg.DefaultMaxTokens,
		TierGuard:           cfg.TierGuard,
		ReservationTTL:      cfg.ReservationTTL,
		ProviderTimeout:     cfg.ProviderTimeout,
		DrainTimeout:        cfg.DrainTimeout,
		StreamWriteTimeout:  cfg.StreamWriteTimeout,
		MaxConcurrentDrains: cfg.MaxConcurrentDrains,
		StreamMetrics:       metrics,
	}

	// Every wrapper implements Unwrap, so http.ResponseController can still
	// reach the Flusher underneath. Without that, streaming degrades into
	// one buffered response and nothing says so.
	health := api.NewHealth(db, func() bool { return prices.Table() != nil })
	health.Version = version

	server := api.New(api.Options{
		DB:             db,
		MasterKey:      cfg.MasterKey,
		MaxDrainMS:     int(cfg.ProviderTimeout.Milliseconds()),
		Proxy:          proxy.New(proxyOpts),
		Models:         proxy.NewModelsHandler(proxyOpts),
		MaxQueryWindow: cfg.MaxQueryWindow,
		Health:         health,
		ReloadPricing: func(context.Context) error {
			fsys, dir := pricing.SeedFS()
			return prices.ReloadFS(fsys, dir)
		},
	})

	// Usage records leave the request path here. The buffer is bounded and
	// falls back to a synchronous write when full: a dropped record is an
	// accounting hole, and the loss this design accepts is a hard crash,
	// not a slow database.
	buffer := usage.NewBuffer(usage.BufferOptions{
		Writer: db, Logger: logger, Metrics: metrics,
		Capacity: cfg.UsageBufferSize, BatchSize: cfg.UsageBatchSize,
		FlushInterval: cfg.UsageFlushInterval,
	})
	bufferCtx, stopBuffer := context.WithCancel(context.Background())
	// Deferred as well as called below: an early return from a failed
	// listen would otherwise leave both goroutines running.
	defer stopBuffer()
	go buffer.Run(bufferCtx)
	proxyOpts.Usage = buffer

	reaper := budget.NewReaper(budget.Options{
		DB: db, Interval: cfg.ReaperInterval, Logger: logger, Metrics: metrics,
	})
	reaperCtx, stopReaper := context.WithCancel(context.Background())
	defer stopReaper()
	go reaper.Run(reaperCtx)

	// Retention is what makes the configured lifetime real. Without this
	// loop COSTLANE_PROMPT_RETENTION would be a promise the process never
	// keeps, which is worse than not offering it.
	if cfg.PromptRetention > 0 || cfg.UsageRetention > 0 {
		go runRetention(reaperCtx, db, logger, store.RetentionPolicy{
			RequestPayloads: cfg.PromptRetention,
			UsageRecords:    cfg.UsageRetention,
		}, cfg.RetentionInterval)
	}

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           proxy.WithRequestID(server.Handler()),
		ReadHeaderTimeout: 15 * time.Second,
		// No WriteTimeout: it would cut long streams short, and F6 sets a
		// deadline per write instead.
	}

	// Metrics live on their own address, so scraping never requires
	// exposing them beside the API.
	metricsServer := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           registry.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Info("serving metrics", "addr", cfg.MetricsAddr)
		if err := metricsServer.ListenAndServe(); err != nil &&
			!errors.Is(err, http.ErrServerClosed) {
			logger.Error("the metrics listener stopped", "error", err)
		}
	}()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = metricsServer.Shutdown(shutdown)
	}()

	errs := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	// The order below matters more than it looks.
	//
	// Readiness fails first, so a load balancer stops routing to this
	// process while it can still serve what it already has. Doing it the
	// other way round means refusing requests that were sent here
	// precisely because we still claimed to be ready.
	logger.Info("shutting down: reporting unready")
	health.BeginDraining()
	time.Sleep(cfg.DrainDelay)

	// Then stop accepting, and let in-flight requests finish: each one
	// still has a settle to run and a record to enqueue.
	logger.Info("shutting down: waiting for in-flight requests")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("waiting for in-flight requests", "error", err)
	}

	// Only now is it safe to stop the reaper and drain the buffer: until
	// the last request has settled, both are still receiving work.
	stopReaper()
	stopBuffer()
	if err := buffer.Close(shutdownCtx); err != nil {
		logger.Error("flushing the usage buffer", "error", err)
	}

	logger.Info("shutdown complete")
	return nil
}

// runRetention deletes rows past their configured lifetime, on a ticker.
//
// ponytail: one bounded batch per tick, so a large backlog drains over
// several hours rather than in one long-running delete. If that is ever too
// slow, shorten the interval before enlarging the batch — the batch size is
// what keeps the delete from holding locks against live traffic.
func runRetention(ctx context.Context, db *store.DB, logger *slog.Logger,
	policy store.RetentionPolicy, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			result, err := db.ApplyRetention(ctx, policy)
			if err != nil {
				logger.Error("applying retention", "error", err)
				continue
			}
			if result.RequestPayloads > 0 || result.UsageRecords > 0 {
				logger.Info("retention applied",
					"payloads_deleted", result.RequestPayloads,
					"usage_records_deleted", result.UsageRecords)
			}
		}
	}
}

// databasePassword extracts the credential from a connection string, so the
// redactor can remove it from anything it appears in.
func databasePassword(dsn obs.Secret) obs.Secret {
	u, err := url.Parse(dsn.Expose())
	if err != nil || u.User == nil {
		return ""
	}
	password, _ := u.User.Password()
	return obs.Secret(password)
}

// buildRouter wires up the providers that have credentials, and maps every
// priced model to the provider that serves it.
func buildRouter(cfg *config.Config, client *http.Client, table *pricing.Table) (*provider.Router, error) {
	opts := func(base string, key obs.Secret) provider.Options {
		return provider.Options{
			BaseURL: base, APIKey: key, Client: client,
			DefaultMaxTokens: cfg.DefaultMaxTokens,
		}
	}

	var providers []provider.Provider
	if cfg.OpenAIKey.IsSet() {
		providers = append(providers, provider.NewOpenAI(opts(cfg.OpenAIBaseURL, cfg.OpenAIKey)))
	}
	if cfg.AnthropicKey.IsSet() {
		providers = append(providers, provider.NewAnthropic(opts(cfg.AnthropicBaseURL, cfg.AnthropicKey)))
	}
	// Google was missing here from the first release to v0.2.0. Configuration
	// read the key, the redactor scrubbed it, the README documented it and the
	// price seed mapped every Gemini model to a provider named "google" — and
	// nothing ever constructed one, so Route looked up a provider that was not
	// in the map and returned 404 for every Gemini model.
	//
	// Nothing caught it because nothing crossed this seam: the adapters, the
	// streaming tests and the live verification all build a provider directly.
	// buildRouter is reached only from cmd/costlane, and the one end-to-end
	// test that goes through it asked for an OpenAI model.
	if cfg.GoogleKey.IsSet() {
		providers = append(providers, provider.NewGoogle(opts(cfg.GoogleBaseURL, cfg.GoogleKey)))
	}
	if len(providers) == 0 {
		return nil, errors.New("no provider credentials configured: set at least one of " +
			"COSTLANE_OPENAI_API_KEY, COSTLANE_ANTHROPIC_API_KEY or " +
			"COSTLANE_GOOGLE_API_KEY")
	}

	router := provider.NewRouter(providers...)
	// The routing table comes from the price list, so adding a model is a
	// pricing change rather than a code change.
	router.SetModelProviders(table.ModelProviders())
	return router, nil
}
