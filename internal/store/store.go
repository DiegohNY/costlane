// Package store provides Postgres access, connection pools and migrations.
package store

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockID guards concurrent migration by several replicas. The value
// is arbitrary but must never change, or two versions of the binary would
// take different locks and migrate simultaneously.
const migrationLockID int64 = 8_215_403_119_774_002

// Options configures the connection pools.
type Options struct {
	DSN string

	// ReadStatementTimeout caps every query on the read pool. Reporting
	// queries are the ones that can turn into a full scan, and a cap is
	// cheaper to reason about than a query planner hint.
	ReadStatementTimeout time.Duration

	MaxWriteConns int32
	MaxReadConns  int32
}

// DB owns a write pool and a read pool over the same database.
//
// The split exists so that the read side can carry a statement timeout that
// the write side must not have: a settle or a migration has to finish, while
// a reporting query that runs for minutes is a bug either way.
type DB struct {
	write *pgxpool.Pool
	read  *pgxpool.Pool
}

// Open connects both pools and verifies each one.
func Open(ctx context.Context, opts Options) (*DB, error) {
	if opts.DSN == "" {
		return nil, errors.New("store: DSN is required")
	}
	if opts.MaxWriteConns == 0 {
		opts.MaxWriteConns = 10
	}
	if opts.MaxReadConns == 0 {
		opts.MaxReadConns = 5
	}

	write, err := openPool(ctx, opts.DSN, opts.MaxWriteConns, 0)
	if err != nil {
		return nil, fmt.Errorf("store: write pool: %w", err)
	}

	read, err := openPool(ctx, opts.DSN, opts.MaxReadConns, opts.ReadStatementTimeout)
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("store: read pool: %w", err)
	}

	return &DB{write: write, read: read}, nil
}

func openPool(ctx context.Context, dsn string, maxConns int32, stmtTimeout time.Duration) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing DSN: %w", err)
	}
	cfg.MaxConns = maxConns
	if stmtTimeout > 0 {
		if cfg.ConnConfig.RuntimeParams == nil {
			cfg.ConnConfig.RuntimeParams = map[string]string{}
		}
		cfg.ConnConfig.RuntimeParams["statement_timeout"] =
			fmt.Sprintf("%d", stmtTimeout.Milliseconds())
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pinging: %w", err)
	}
	return pool, nil
}

// Pool returns the write pool, which carries no statement timeout.
func (db *DB) Pool() *pgxpool.Pool { return db.write }

// ReadPool returns the read pool, whose queries are capped.
func (db *DB) ReadPool() *pgxpool.Pool { return db.read }

// Close releases both pools.
func (db *DB) Close() {
	if db == nil {
		return
	}
	db.read.Close()
	db.write.Close()
}

// Migrate applies every pending migration under an advisory lock, so that N
// replicas starting together serialise instead of racing.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return withMigrationLock(ctx, pool, func(g *goose.Provider) error {
		_, err := g.Up(ctx)
		return err
	})
}

// MigrateDownAll rolls every migration back. It exists so the down path is
// exercised by tests: a down migration that has never run is a broken one.
func MigrateDownAll(ctx context.Context, pool *pgxpool.Pool) error {
	return withMigrationLock(ctx, pool, func(g *goose.Provider) error {
		_, err := g.DownTo(ctx, 0)
		return err
	})
}

func withMigrationLock(ctx context.Context, pool *pgxpool.Pool, fn func(*goose.Provider) error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("store: acquiring a connection: %w", err)
	}
	defer conn.Release()

	// pg_advisory_lock blocks until the lock is free, which is the
	// behaviour we want: a second replica waits rather than failing.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("store: taking the migration lock: %w", err)
	}
	defer func() {
		// Use a fresh context: the caller's may already be cancelled, and
		// leaving the lock held would block every future migration.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(releaseCtx, `SELECT pg_advisory_unlock($1)`, migrationLockID)
	}()

	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()

	// goose reads from the root of the filesystem it is given, while
	// go:embed keeps the directory prefix.
	root, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("store: locating migrations: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, db, root)
	if err != nil {
		return fmt.Errorf("store: preparing migrations: %w", err)
	}
	if err := fn(provider); err != nil {
		return fmt.Errorf("store: applying migrations: %w", err)
	}
	return nil
}

// quoteIdent quotes an identifier for use in DDL, where placeholders are not
// accepted.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// uniqueDBName produces a collision-free database name for a test.
func uniqueDBName() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "costlane_test_" + hex.EncodeToString(b[:])
}

// replaceDBName rewrites the database portion of a DSN, so tests can reuse
// one container across many databases.
func replaceDBName(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}
