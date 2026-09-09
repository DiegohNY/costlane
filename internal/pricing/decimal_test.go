package pricing

import (
	"math/rand/v2"
	"testing"

	"github.com/shopspring/decimal"
)

// The property that matters most: pricing the kinds separately and pricing
// them together must agree exactly, for any combination. float64 drifts
// here — measurably, at the twelfth decimal after a hundred thousand
// additions — and decimal does not.
func TestSumOfPartsEqualsWhole(t *testing.T) {
	tbl := anthropicTable(t)
	when := at("2026-06-01T00:00:00Z")
	rng := rand.New(rand.NewPCG(1, 2))

	kinds := []Kind{KindInput, KindOutput, KindCachedRead, KindCacheWrite5m, KindCacheWrite1h}

	for range 2000 {
		counts := Counts{}
		for _, k := range kinds {
			counts[k] = rng.Int64N(5_000_000)
		}

		whole, err := tbl.Cost(Request{
			Model: "claude-sonnet-5", Provider: "anthropic", Tier: TierStandard,
			At: when, Counts: counts,
		})
		if err != nil {
			t.Fatalf("Cost: %v", err)
		}

		parts := decimal.Zero
		for _, k := range kinds {
			one, err := tbl.Cost(Request{
				Model: "claude-sonnet-5", Provider: "anthropic", Tier: TierStandard,
				At: when, Counts: Counts{k: counts[k]},
			})
			if err != nil {
				t.Fatalf("Cost(%s): %v", k, err)
			}
			parts = parts.Add(one.Cost)
		}

		if !whole.Cost.Equal(parts) {
			t.Fatalf("drift for %v: whole=%s parts=%s (difference %s)",
				counts, whole.Cost, parts, whole.Cost.Sub(parts))
		}
	}
}

// One token must produce an exact fraction, not a rounded zero. At $2/Mtok
// a single token is 0.000002 exactly.
func TestSingleTokenKeepsFullPrecision(t *testing.T) {
	tbl := anthropicTable(t)
	res, err := tbl.Cost(Request{
		Model: "claude-sonnet-5", Provider: "anthropic", Tier: TierStandard,
		At: at("2026-06-01T00:00:00Z"), Counts: Counts{KindInput: 1},
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if !res.Cost.Equal(rate("0.000002")) {
		t.Errorf("one input token = %s, want exactly 0.000002", res.Cost)
	}
	if res.Cost.IsZero() {
		t.Error("a single token must not round to zero")
	}
}

// Accumulating many small charges must not drift: a gateway adds up millions
// of these, and the total is what gets compared against the provider's
// invoice.
func TestAccumulationDoesNotDrift(t *testing.T) {
	tbl := anthropicTable(t)
	res, err := tbl.Cost(Request{
		Model: "claude-sonnet-5", Provider: "anthropic", Tier: TierStandard,
		At: at("2026-06-01T00:00:00Z"), Counts: Counts{KindInput: 7},
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}

	total := decimal.Zero
	for range 100_000 {
		total = total.Add(res.Cost)
	}
	want := res.Cost.Mul(decimal.NewFromInt(100_000))
	if !total.Equal(want) {
		t.Errorf("accumulated %s, want %s (drift %s)", total, want, total.Sub(want))
	}
}

// Each kind is charged at its own rate. Averaging input and output — as one
// surveyed project does — would give 6 here instead of 12.
func TestRatesAreNotAveraged(t *testing.T) {
	tbl := anthropicTable(t)
	res, err := tbl.Cost(Request{
		Model: "claude-sonnet-5", Provider: "anthropic", Tier: TierStandard,
		At:     at("2026-06-01T00:00:00Z"),
		Counts: Counts{KindInput: 1_000_000, KindOutput: 1_000_000},
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if !res.Cost.Equal(rate("12")) {
		t.Errorf("1M in + 1M out = %s, want 12 (2 + 10, not an average)", res.Cost)
	}
}

func TestNegativeCountIsRejected(t *testing.T) {
	tbl := anthropicTable(t)
	_, err := tbl.Cost(Request{
		Model: "claude-sonnet-5", Provider: "anthropic", Tier: TierStandard,
		At: at("2026-06-01T00:00:00Z"), Counts: Counts{KindInput: -1},
	})
	if err == nil {
		t.Error("a negative token count must be rejected")
	}
}

func TestZeroUsageCostsZero(t *testing.T) {
	tbl := anthropicTable(t)
	res, err := tbl.Cost(Request{
		Model: "claude-sonnet-5", Provider: "anthropic", Tier: TierStandard,
		At: at("2026-06-01T00:00:00Z"), Counts: Counts{},
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if !res.Cost.IsZero() || res.Unpriced || res.PartiallyPriced {
		t.Errorf("empty usage: cost=%s unpriced=%t partial=%t; want 0, false, false",
			res.Cost, res.Unpriced, res.PartiallyPriced)
	}
}
