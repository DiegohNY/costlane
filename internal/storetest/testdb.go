// Package storetest brings up a real Postgres for integration tests.
//
// It lives apart from the store package so that testcontainers, the Docker
// client and the testing package never reach the production binary: a
// distroless image has no business carrying a container runtime client.
package storetest

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/DiegohNY/costlane/internal/store"
)

// postgresImage is pinned deliberately. `latest` would let a new major
// version change behaviour under a green build, which is the lesson a pinned
// linter release taught during F0.
const postgresImage = "postgres:17-alpine"

var (
	containerOnce sync.Once
	containerDSN  string
	containerErr  error
)

// startPostgres brings up one container for the whole test binary and hands
// each test its own database inside it, which is far cheaper than a
// container per test while keeping the tests isolated.
func startPostgres(t *testing.T) string {
	t.Helper()
	containerOnce.Do(func() {
		if !dockerAvailable() {
			return
		}
		ctx := context.Background()
		container, err := tcpostgres.Run(ctx, postgresImage,
			tcpostgres.WithDatabase("costlane_template"),
			tcpostgres.WithUsername("costlane"),
			tcpostgres.WithPassword("costlane"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(90*time.Second)),
		)
		if err != nil {
			containerErr = err
			return
		}
		containerDSN, containerErr = container.ConnectionString(ctx, "sslmode=disable")
	})

	if containerDSN == "" && containerErr == nil {
		t.Skip("docker is not available; skipping integration tests")
	}
	if containerErr != nil {
		t.Fatalf("starting postgres: %v", containerErr)
	}
	return containerDSN
}

func dockerAvailable() bool {
	if os.Getenv("COSTLANE_SKIP_DOCKER_TESTS") != "" {
		return false
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "info").Run() == nil
}

// PostgresDSN returns a connection string to the shared test container,
// skipping the calling test when Docker is unavailable. Use it when a test
// needs a database that NewTestDB's migrations have not been applied to.
func PostgresDSN(t *testing.T) string {
	t.Helper()
	return startPostgres(t)
}

// NewEmptyDB returns a connection to a fresh, unmigrated database. It exists
// so a test can observe what a deployment looks like when migrations are
// applied as a separate step rather than at boot.
func NewEmptyDB(t *testing.T) *store.DB {
	t.Helper()
	dsn := startPostgres(t)

	name := store.UniqueDBName()
	admin, err := store.Open(t.Context(), store.Options{DSN: dsn})
	if err != nil {
		t.Fatalf("connecting to the template database: %v", err)
	}
	if _, err := admin.Pool().Exec(t.Context(), `CREATE DATABASE `+store.QuoteIdent(name)); err != nil {
		admin.Close()
		t.Fatalf("creating %s: %v", name, err)
	}
	admin.Close()

	db, err := store.Open(t.Context(), store.Options{DSN: store.ReplaceDBName(dsn, name)})
	if err != nil {
		t.Fatalf("connecting to %s: %v", name, err)
	}
	t.Cleanup(db.Close)
	return db
}

// NewTestDB returns a fully migrated database, private to the calling test.
// It lives outside a _test.go file so that packages built on top of the
// store can use the same harness rather than inventing their own.
func NewTestDB(t *testing.T) *store.DB {
	t.Helper()
	dsn := startPostgres(t)

	name := store.UniqueDBName()
	admin, err := store.Open(t.Context(), store.Options{DSN: dsn})
	if err != nil {
		t.Fatalf("connecting to the template database: %v", err)
	}
	if _, err := admin.Pool().Exec(t.Context(), `CREATE DATABASE `+store.QuoteIdent(name)); err != nil {
		admin.Close()
		t.Fatalf("creating %s: %v", name, err)
	}
	admin.Close()

	db, err := store.Open(t.Context(), store.Options{
		DSN:                  store.ReplaceDBName(dsn, name),
		ReadStatementTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("connecting to %s: %v", name, err)
	}
	t.Cleanup(db.Close)

	if err := store.Migrate(t.Context(), db.Pool()); err != nil {
		t.Fatalf("migrating %s: %v", name, err)
	}
	return db
}
