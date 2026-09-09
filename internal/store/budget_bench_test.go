package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/DiegohNY/costlane/internal/auth"
	"github.com/DiegohNY/costlane/internal/store"
	"github.com/DiegohNY/costlane/internal/storetest"
	"github.com/shopspring/decimal"
)

// BenchmarkReserveContention measures the reserve under the case the design
// is built around: many requests racing on one key's budget row.
//
// Postgres serialises updates to a single row, so this is where the fused
// single-statement reserve earns its keep — a SELECT-then-UPDATE would hold
// the row for a second round trip on every request.
func BenchmarkReserveContention(b *testing.B) {
	db := benchDB(b)
	hash := benchKey(b, db, "100000000")

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := db.Reserve(context.Background(), store.ReserveInput{
				KeyHash:      hash,
				Model:        "gpt-5.6-terra",
				EstimatedUSD: decimal.NewFromFloat(0.001),
				TTL:          time.Hour,
			}); err != nil {
				b.Fatalf("Reserve: %v", err)
			}
		}
	})
}

// BenchmarkReserveDistinctKeys is the contrast: the same work spread across
// keys, where no two requests contend for the same row. The gap between this
// and the contended case is the cost of the contention itself.
func BenchmarkReserveDistinctKeys(b *testing.B) {
	db := benchDB(b)

	const keys = 64
	hashes := make([][]byte, keys)
	for i := range keys {
		hashes[i] = benchKey(b, db, "100000000")
	}

	b.ResetTimer()
	var i int
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i++
			if _, err := db.Reserve(context.Background(), store.ReserveInput{
				KeyHash:      hashes[i%keys],
				Model:        "gpt-5.6-terra",
				EstimatedUSD: decimal.NewFromFloat(0.001),
				TTL:          time.Hour,
			}); err != nil {
				b.Fatalf("Reserve: %v", err)
			}
		}
	})
}

// BenchmarkSettle measures the other half of the critical path.
func BenchmarkSettle(b *testing.B) {
	db := benchDB(b)
	hash := benchKey(b, db, "100000000")

	ids := make([]struct{ id [16]byte }, 0)
	_ = ids

	b.ResetTimer()
	for b.Loop() {
		b.StopTimer()
		res, err := db.Reserve(context.Background(), store.ReserveInput{
			KeyHash: hash, Model: "m", EstimatedUSD: decimal.NewFromFloat(0.001), TTL: time.Hour,
		})
		if err != nil {
			b.Fatalf("Reserve: %v", err)
		}
		b.StartTimer()

		if _, err := db.Settle(context.Background(), store.SettleInput{
			ReservationID: res.ReservationID, ActualUSD: decimal.NewFromFloat(0.0008),
		}); err != nil {
			b.Fatalf("Settle: %v", err)
		}
	}
}

func benchDB(b *testing.B) *store.DB {
	b.Helper()
	return storetest.NewBenchDB(b)
}

func benchKey(b *testing.B, db *store.DB, limit string) []byte {
	b.Helper()
	key, err := auth.NewKey()
	if err != nil {
		b.Fatalf("generating: %v", err)
	}
	l := decimal.RequireFromString(limit)
	if _, err := db.CreateKey(context.Background(), store.CreateKeyInput{
		Hash: key.Hash, Prefix: key.Prefix, Label: "bench", LimitUSD: &l,
	}); err != nil {
		b.Fatalf("CreateKey: %v", err)
	}
	return key.Hash
}
