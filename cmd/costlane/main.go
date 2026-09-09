// Command costlane runs the gateway.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
)

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
	logger.Info("configuration loaded", "config", cfg.String())

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

	proxyOpts := proxy.Options{
		DB: db, Router: router, Pricing: prices, Logger: logger,
		MaxBodyBytes:        cfg.MaxBodyBytes,
		DefaultMaxTokens:    cfg.DefaultMaxTokens,
		TierGuard:           cfg.TierGuard,
		ReservationTTL:      cfg.ReservationTTL,
		ProviderTimeout:     cfg.ProviderTimeout,
		DrainTimeout:        cfg.DrainTimeout,
		StreamWriteTimeout:  cfg.StreamWriteTimeout,
		MaxConcurrentDrains: cfg.MaxConcurrentDrains,
		StreamMetrics:       obs.NewStreamMetrics(),
	}

	// Every wrapper implements Unwrap, so http.ResponseController can still
	// reach the Flusher underneath. Without that, streaming degrades into
	// one buffered response and nothing says so.
	server := api.New(api.Options{
		DB:         db,
		MasterKey:  cfg.MasterKey,
		MaxDrainMS: int(cfg.ProviderTimeout.Milliseconds()),
		Proxy:      proxy.New(proxyOpts),
		Models:     proxy.NewModelsHandler(proxyOpts),
	})

	reaper := budget.NewReaper(budget.Options{
		DB: db, Interval: cfg.ReaperInterval, Logger: logger,
	})
	go reaper.Run(ctx)

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           proxy.WithRequestID(server.Handler()),
		ReadHeaderTimeout: 15 * time.Second,
		// No WriteTimeout: it would cut long streams short, and F6 sets a
		// deadline per write instead.
	}

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

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return httpServer.Shutdown(shutdownCtx)
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
	if len(providers) == 0 {
		return nil, errors.New("no provider credentials configured: set at least one of " +
			"COSTLANE_OPENAI_API_KEY or COSTLANE_ANTHROPIC_API_KEY")
	}

	router := provider.NewRouter(providers...)
	// The routing table comes from the price list, so adding a model is a
	// pricing change rather than a code change.
	router.SetModelProviders(table.ModelProviders())
	return router, nil
}
