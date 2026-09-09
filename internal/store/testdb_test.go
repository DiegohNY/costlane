package store_test

import (
	"testing"

	"github.com/DiegohNY/costlane/internal/store"
	"github.com/DiegohNY/costlane/internal/storetest"
)

func newTestDB(t *testing.T) *store.DB {
	t.Helper()
	return storetest.NewTestDB(t)
}
