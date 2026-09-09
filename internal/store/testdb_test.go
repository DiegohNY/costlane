package store

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
)

// postgresImage is pinned deliberately. `latest` would let a new major
// version change behaviour under a green build, which is how a pinned
// linter release taught us the same lesson in F0.
const postgresImage = "postgres:17-alpine"

var (
	containerOnce sync.Once
	containerDSN  string
	containerErr  error
)

// startPostgres brings up one container for the whole package and hands each
// test its own database inside it, which is far cheaper than a container per
// test while keeping the tests isolated from one another.
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

// newTestDB returns a fully migrated database, private to this test.
func newTestDB(t *testing.T) *DB {
	t.Helper()
	dsn := startPostgres(t)

	name := uniqueDBName()
	admin, err := Open(t.Context(), Options{DSN: dsn})
	if err != nil {
		t.Fatalf("connecting to the template database: %v", err)
	}
	if _, err := admin.Pool().Exec(t.Context(), `CREATE DATABASE `+quoteIdent(name)); err != nil {
		admin.Close()
		t.Fatalf("creating %s: %v", name, err)
	}
	admin.Close()

	db, err := Open(t.Context(), Options{
		DSN:                  replaceDBName(dsn, name),
		ReadStatementTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("connecting to %s: %v", name, err)
	}
	t.Cleanup(db.Close)

	if err := Migrate(t.Context(), db.Pool()); err != nil {
		t.Fatalf("migrating %s: %v", name, err)
	}
	return db
}
