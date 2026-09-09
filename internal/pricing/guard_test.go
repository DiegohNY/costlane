package pricing

import (
	"slices"
	"testing"
)

// chars/4 understates token counts, always downward, so an estimate near a
// cliff must reserve at the far side of it.
func TestEstimateNearTheCliffReservesHigh(t *testing.T) {
	thresholds := []int64{272_000}

	cases := []struct {
		name     string
		estimate int64
		want     int64
	}{
		{"far below the cliff", 100_000, 100_000},
		{"just below the guard band", 203_999, 203_999},
		{"exactly at the guard band", 204_000, 272_000}, // 0.75 x 272000
		{"inside the guard band", 250_000, 272_000},
		{"one token below the cliff", 271_999, 272_000},
		{"exactly at the cliff", 272_000, 272_000},
		{"beyond the cliff", 400_000, 400_000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := GuardedTierTotal(c.estimate, thresholds, TierGuardFactor)
			if got != c.want {
				t.Errorf("GuardedTierTotal(%d) = %d, want %d", c.estimate, got, c.want)
			}
		})
	}
}

// The guard only ever raises an estimate. Reserving lower than the estimate
// would defeat the point.
func TestGuardNeverLowersAnEstimate(t *testing.T) {
	thresholds := []int64{200_000, 272_000}
	for estimate := int64(0); estimate < 500_000; estimate += 1_777 {
		got := GuardedTierTotal(estimate, thresholds, TierGuardFactor)
		if got < estimate {
			t.Fatalf("GuardedTierTotal(%d) = %d, which is lower", estimate, got)
		}
	}
}

// With several cliffs, the estimate is lifted to the nearest one it is
// close to, not to the highest.
func TestGuardLiftsToTheNearestCliff(t *testing.T) {
	thresholds := []int64{200_000, 272_000}
	// 160_000 is 0.8 x 200_000 but only 0.59 x 272_000.
	if got := GuardedTierTotal(160_000, thresholds, TierGuardFactor); got != 200_000 {
		t.Errorf("GuardedTierTotal(160000) = %d, want 200000", got)
	}
}

func TestNoThresholdsLeavesTheEstimateAlone(t *testing.T) {
	if got := GuardedTierTotal(999_999, nil, TierGuardFactor); got != 999_999 {
		t.Errorf("with no cliffs the estimate must pass through, got %d", got)
	}
}

// A settle uses the real token count and no guard, so the two must be able
// to disagree — that is what the overshoot flag records.
func TestThresholdsComeFromTheTable(t *testing.T) {
	tbl, err := NewTable([]Row{
		{Model: "gpt-6-astra", Provider: "openai", Kind: KindInput, Tier: TierStandard,
			USDPerMTok: rate("10"), TierTo: 272_000, From: at("2026-01-01T00:00:00Z"),
			SourceURL: "https://example.test"},
		{Model: "gpt-6-astra", Provider: "openai", Kind: KindInput, Tier: TierStandard,
			USDPerMTok: rate("20"), TierFrom: 272_000, From: at("2026-01-01T00:00:00Z"),
			SourceURL: "https://example.test"},
		{Model: "gpt-6-astra", Provider: "openai", Kind: KindOutput, Tier: TierStandard,
			USDPerMTok: rate("75"), TierFrom: 272_000, From: at("2026-01-01T00:00:00Z"),
			SourceURL: "https://example.test"},
	}, nil)
	if err != nil {
		t.Fatalf("building table: %v", err)
	}

	got := tbl.Thresholds("gpt-6-astra", "openai", TierStandard)
	if !slices.Contains(got, 272_000) {
		t.Errorf("Thresholds = %v, want it to contain 272000", got)
	}
	// Each boundary appears once even though two kinds declare it.
	if len(got) != 1 {
		t.Errorf("Thresholds = %v, want exactly one distinct boundary", got)
	}

	// A model with flat pricing has no cliffs at all.
	if n := len(tbl.Thresholds("nothing", "openai", TierStandard)); n != 0 {
		t.Errorf("an unknown model reported %d thresholds, want 0", n)
	}
}
