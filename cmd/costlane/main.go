// Command costlane runs the gateway.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/DiegohNY/costlane/internal/config"
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

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fmt.Printf("costlane: configuration valid: %s\n", cfg)

	db, err := store.Open(ctx, store.Options{
		DSN:                  cfg.DatabaseURL.Expose(),
		ReadStatementTimeout: cfg.ReadTimeout,
	})
	if err != nil {
		return err
	}
	defer db.Close()

	// In production, migrations run as a separate step before the rollout,
	// not from every replica that starts. The default suits development,
	// where one process owns its database.
	if cfg.MigrateOnBoot {
		if err := store.Migrate(ctx, db.Pool()); err != nil {
			return err
		}
		fmt.Println("costlane: migrations applied")
	} else {
		fmt.Println("costlane: skipping migrations (COSTLANE_MIGRATE_ON_BOOT=false)")
	}

	// Serving arrives in F5.
	fmt.Println("costlane: storage ready")
	return nil
}
