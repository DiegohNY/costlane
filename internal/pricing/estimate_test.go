package pricing

import (
	"testing"
)

func TestEstimateInputTokens(t *testing.T) {
	cases := []struct {
		bytes int
		want  int64
	}{
		{0, 0},
		{-5, 0},
		{1, 1},
		{4, 2},
		{4000, 1001},
	}
	for _, c := range cases {
		if got := EstimateInputTokens(c.bytes); got != c.want {
			t.Errorf("EstimateInputTokens(%d) = %d, want %d", c.bytes, got, c.want)
		}
	}
}

func astraTable(t *testing.T) *Table {
	t.Helper()
	tbl, err := NewTable([]Row{
		{Model: "gpt-6-astra", Provider: "openai", Kind: KindInput, Tier: TierStandard,
			USDPerMTok: rate("10"), TierTo: 272_000, From: at("2026-01-01T00:00:00Z"),
			SourceURL: "https://example.test"},
		{Model: "gpt-6-astra", Provider: "openai", Kind: KindOutput, Tier: TierStandard,
			USDPerMTok: rate("50"), TierTo: 272_000, From: at("2026-01-01T00:00:00Z"),
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
	return tbl
}

// The client's own ceiling bounds the output far better than any guess.
func TestEstimateUsesTheClientsMaxTokens(t *testing.T) {
	tbl := astraTable(t)
	withCeiling, _, err := tbl.Estimate(EstimateRequest{
		Model: "gpt-6-astra", Provider: "openai", Tier: TierStandard,
		At: at("2026-06-01T00:00:00Z"), PromptBytes: 4000, MaxTokens: 100,
		DefaultMaxTokens: 4096,
	})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	withDefault, _, err := tbl.Estimate(EstimateRequest{
		Model: "gpt-6-astra", Provider: "openai", Tier: TierStandard,
		At: at("2026-06-01T00:00:00Z"), PromptBytes: 4000, DefaultMaxTokens: 4096,
	})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if !withCeiling.LessThan(withDefault) {
		t.Errorf("a stated max_tokens (%s) must reserve less than the default (%s)",
			withCeiling, withDefault)
	}
}

// An estimate landing near a cliff reserves at the far side, because the
// estimator errs downward and crossing doubles the whole request.
func TestEstimateGuardsAgainstTheContextCliff(t *testing.T) {
	tbl := astraTable(t)

	// 250,000 estimated tokens is inside the guard band of 272,000.
	near, _, err := tbl.Estimate(EstimateRequest{
		Model: "gpt-6-astra", Provider: "openai", Tier: TierStandard,
		At: at("2026-06-01T00:00:00Z"), PromptBytes: 1_000_000, DefaultMaxTokens: 100,
	})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}

	// The same token count priced without the guard would use the short
	// tier; the guarded estimate must be at least the long-tier price of
	// the lifted count.
	unguarded, err := tbl.Cost(Request{
		Model: "gpt-6-astra", Provider: "openai", Tier: TierStandard,
		At:     at("2026-06-01T00:00:00Z"),
		Counts: Counts{KindInput: 250_001, KindOutput: 100},
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if !near.GreaterThan(unguarded.Cost) {
		t.Errorf("guarded estimate %s is not above the unguarded %s", near, unguarded.Cost)
	}
}

// An unpriced model yields an unpriced result rather than a zero estimate: a
// reservation of nothing would let unbudgeted spend through.
func TestEstimateOfAnUnpricedModelIsUnpriced(t *testing.T) {
	tbl := astraTable(t)
	cost, res, err := tbl.Estimate(EstimateRequest{
		Model: "unknown-model", Provider: "openai", Tier: TierStandard,
		At: at("2026-06-01T00:00:00Z"), PromptBytes: 100, DefaultMaxTokens: 100,
	})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if !res.Unpriced {
		t.Error("an unknown model must produce an unpriced result")
	}
	if !cost.IsZero() {
		t.Errorf("cost = %s; an unpriced estimate carries no amount", cost)
	}
}
