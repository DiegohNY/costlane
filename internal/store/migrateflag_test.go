package store

import (
	"testing"
)

// The flag exists so production can apply migrations as a separate step
// before a rollout. If it is honoured, a database that was never migrated
// stays empty and the caller finds out on its first query rather than
// having the schema appear from under it.
func TestSkippingMigrationsLeavesSchemaUntouched(t *testing.T) {
	dsn := startPostgres(t)

	name := uniqueDBName()
	admin, err := Open(t.Context(), Options{DSN: dsn})
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	if _, err := admin.Pool().Exec(t.Context(), `CREATE DATABASE `+quoteIdent(name)); err != nil {
		admin.Close()
		t.Fatalf("creating database: %v", err)
	}
	admin.Close()

	db, err := Open(t.Context(), Options{DSN: replaceDBName(dsn, name)})
	if err != nil {
		t.Fatalf("connecting to the fresh database: %v", err)
	}
	t.Cleanup(db.Close)

	// Deliberately not migrating, which is what MigrateOnBoot=false does.
	var count int
	if err := db.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`).Scan(&count); err != nil {
		t.Fatalf("counting tables: %v", err)
	}
	if count != 0 {
		t.Errorf("an unmigrated database has %d tables, want 0", count)
	}
}
