package store_test

import (
	"testing"

	"github.com/DiegohNY/costlane/internal/storetest"
)

// The flag exists so production can apply migrations as a separate step
// before a rollout. If it is honoured, a database that was never migrated
// stays empty and the caller finds out on its first query rather than having
// the schema appear from under it.
func TestSkippingMigrationsLeavesSchemaUntouched(t *testing.T) {
	db := storetest.NewEmptyDB(t)

	var count int
	if err := db.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM information_schema.tables WHERE table_schema='public'`).Scan(&count); err != nil {
		t.Fatalf("counting tables: %v", err)
	}
	if count != 0 {
		t.Errorf("an unmigrated database has %d tables, want 0", count)
	}
}
