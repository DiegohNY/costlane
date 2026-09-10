package pricing

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func rate(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		panic(err)
	}
	return d
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// A table shaped like Anthropic's: five kinds, no context tier.
func anthropicTable(t *testing.T) *Table {
	t.Helper()
	tbl, err := NewTable([]Row{
		{Model: "claude-sonnet-5", Provider: "anthropic", Kind: KindInput, Tier: TierStandard, USDPerMTok: rate("2"), From: at("2026-01-01T00:00:00Z"), SourceURL: "https://example.test"},
		{Model: "claude-sonnet-5", Provider: "anthropic", Kind: KindOutput, Tier: TierStandard, USDPerMTok: rate("10"), From: at("2026-01-01T00:00:00Z"), SourceURL: "https://example.test"},
		{Model: "claude-sonnet-5", Provider: "anthropic", Kind: KindCachedRead, Tier: TierStandard, USDPerMTok: rate("0.20"), From: at("2026-01-01T00:00:00Z"), SourceURL: "https://example.test"},
		{Model: "claude-sonnet-5", Provider: "anthropic", Kind: KindCacheWrite5m, Tier: TierStandard, USDPerMTok: rate("2.50"), From: at("2026-01-01T00:00:00Z"), SourceURL: "https://example.test"},
		{Model: "claude-sonnet-5", Provider: "anthropic", Kind: KindCacheWrite1h, Tier: TierStandard, USDPerMTok: rate("4"), From: at("2026-01-01T00:00:00Z"), SourceURL: "https://example.test"},
	}, nil)
	if err != nil {
		t.Fatalf("building table: %v", err)
	}
	return tbl
}

func TestCostSumsEveryKind(t *testing.T) {
	tbl := anthropicTable(t)
	res, err := tbl.Cost(Request{
		Model: "claude-sonnet-5", Provider: "anthropic", Tier: TierStandard,
		At: at("2026-06-01T00:00:00Z"),
		Counts: Counts{
			KindInput:        1_000_000,
			KindOutput:       1_000_000,
			KindCachedRead:   1_000_000,
			KindCacheWrite5m: 1_000_000,
		},
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	// 2 + 10 + 0.20 + 2.50
	if !res.Cost.Equal(rate("14.7")) {
		t.Errorf("cost = %s, want 14.7", res.Cost)
	}
	if res.PartiallyPriced || res.Unpriced {
		t.Error("a fully priced request must be neither partial nor unpriced")
	}
}

// The two cache-write TTLs are 1.6x apart, and the provider reports a flat
// total that does not say which was used. Pricing them separately is the
// only way to be right.
func TestCacheWriteTTLsArePricedSeparately(t *testing.T) {
	tbl := anthropicTable(t)
	short, err := tbl.Cost(Request{
		Model: "claude-sonnet-5", Provider: "anthropic", Tier: TierStandard,
		At:     at("2026-06-01T00:00:00Z"),
		Counts: Counts{KindCacheWrite5m: 1_000_000},
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	long, err := tbl.Cost(Request{
		Model: "claude-sonnet-5", Provider: "anthropic", Tier: TierStandard,
		At:     at("2026-06-01T00:00:00Z"),
		Counts: Counts{KindCacheWrite1h: 1_000_000},
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if !short.Cost.Equal(rate("2.5")) || !long.Cost.Equal(rate("4")) {
		t.Errorf("5m=%s 1h=%s, want 2.5 and 4", short.Cost, long.Cost)
	}
	ratio := long.Cost.Div(short.Cost)
	if !ratio.Equal(rate("1.6")) {
		t.Errorf("the two TTLs differ by %sx, want 1.6x", ratio)
	}
}

// The hole this closes: a request that spent real money must not be charged
// zero because one of its token kinds had no published rate.
func TestPartiallyPricedChargesTheKnownPart(t *testing.T) {
	// A table shaped like gpt-5.6-sol above the threshold: input and output
	// are priced, cached read is not.
	tbl, err := NewTable([]Row{
		{Model: "m", Provider: "openai", Kind: KindInput, Tier: TierStandard, USDPerMTok: rate("8"), From: at("2026-01-01T00:00:00Z"), SourceURL: "https://example.test"},
		{Model: "m", Provider: "openai", Kind: KindOutput, Tier: TierStandard, USDPerMTok: rate("30"), From: at("2026-01-01T00:00:00Z"), SourceURL: "https://example.test"},
	}, nil)
	if err != nil {
		t.Fatalf("building table: %v", err)
	}

	res, err := tbl.Cost(Request{
		Model: "m", Provider: "openai", Tier: TierStandard,
		At: at("2026-06-01T00:00:00Z"),
		Counts: Counts{
			KindInput:      1_000_000,
			KindOutput:     1_000_000,
			KindCachedRead: 1_000_000, // no rate for this kind
		},
	})
	if err != nil {
		t.Fatalf("a partially priced request must not be an error: %v", err)
	}

	// Charging zero here would be the largest budget hole in the system.
	if !res.Cost.Equal(rate("38")) {
		t.Errorf("cost = %s, want 38 (the priced kinds only)", res.Cost)
	}
	if !res.PartiallyPriced {
		t.Error("the result must declare itself partially priced")
	}
	if res.Unpriced {
		t.Error("partially priced is not the same as unpriced")
	}
	if len(res.UnpricedKinds) != 1 || res.UnpricedKinds[0] != KindCachedRead {
		t.Errorf("UnpricedKinds = %v, want [cached_read]", res.UnpricedKinds)
	}
}

// A kind with no rate but also no usage is not a gap.
func TestZeroCountOfAnUnpricedKindIsNotPartial(t *testing.T) {
	tbl, err := NewTable([]Row{
		{Model: "m", Provider: "openai", Kind: KindInput, Tier: TierStandard, USDPerMTok: rate("8"), From: at("2026-01-01T00:00:00Z"), SourceURL: "https://example.test"},
	}, nil)
	if err != nil {
		t.Fatalf("building table: %v", err)
	}
	res, err := tbl.Cost(Request{
		Model: "m", Provider: "openai", Tier: TierStandard,
		At:     at("2026-06-01T00:00:00Z"),
		Counts: Counts{KindInput: 1000, KindCachedRead: 0},
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if res.PartiallyPriced {
		t.Error("a kind with no tokens used is not an unpriced kind")
	}
}

// No rows at all for a model: unknown, never free.
func TestNoPricesAtAllIsUnpriced(t *testing.T) {
	tbl := anthropicTable(t)
	res, err := tbl.Cost(Request{
		Model: "mystery-model", Provider: "anthropic", Tier: TierStandard,
		At:     at("2026-06-01T00:00:00Z"),
		Counts: Counts{KindInput: 1000},
	})
	if err != nil {
		t.Fatalf("an unpriced model must not be an error at this layer: %v", err)
	}
	if !res.Unpriced {
		t.Error("a model with no rows must be reported unpriced")
	}
	if !res.Cost.IsZero() {
		t.Errorf("an unpriced result carries no cost, got %s", res.Cost)
	}
}

// Rates are effective-dated, and history is immutable: a request from before
// a repricing must resolve to the older rate forever.
func TestRateIsResolvedAtTheRequestInstant(t *testing.T) {
	tbl, err := NewTable([]Row{
		{Model: "gemini-3.8-flash", Provider: "google", Kind: KindInput, Tier: TierStandard,
			USDPerMTok: rate("0.75"), From: at("2026-01-01T00:00:00Z"), Until: at("2027-01-01T00:00:00Z"), SourceURL: "https://example.test"},
		{Model: "gemini-3.8-flash", Provider: "google", Kind: KindInput, Tier: TierStandard,
			USDPerMTok: rate("1.50"), From: at("2027-01-01T00:00:00Z"), SourceURL: "https://example.test"},
	}, nil)
	if err != nil {
		t.Fatalf("building table: %v", err)
	}

	before, _ := tbl.Cost(Request{Model: "gemini-3.8-flash", Provider: "google", Tier: TierStandard,
		At: at("2026-12-31T23:59:59Z"), Counts: Counts{KindInput: 1_000_000}})
	after, _ := tbl.Cost(Request{Model: "gemini-3.8-flash", Provider: "google", Tier: TierStandard,
		At: at("2027-01-01T00:00:00Z"), Counts: Counts{KindInput: 1_000_000}})

	if !before.Cost.Equal(rate("0.75")) {
		t.Errorf("before the repricing: %s, want 0.75", before.Cost)
	}
	// effective_from is inclusive, so the new rate applies at the instant.
	if !after.Cost.Equal(rate("1.5")) {
		t.Errorf("at the repricing instant: %s, want 1.50", after.Cost)
	}
}

// The context tier is chosen on the input total and applied to every kind,
// output included. Crossing OpenAI's 272k threshold reprices the whole
// request.
func TestTierSelectedOnInputAppliesToOutput(t *testing.T) {
	tbl, err := NewTable([]Row{
		{Model: "gpt-6-astra", Provider: "openai", Kind: KindInput, Tier: TierStandard,
			USDPerMTok: rate("10"), TierTo: 272_000, From: at("2026-01-01T00:00:00Z"), SourceURL: "https://example.test"},
		{Model: "gpt-6-astra", Provider: "openai", Kind: KindOutput, Tier: TierStandard,
			USDPerMTok: rate("50"), TierTo: 272_000, From: at("2026-01-01T00:00:00Z"), SourceURL: "https://example.test"},
		{Model: "gpt-6-astra", Provider: "openai", Kind: KindInput, Tier: TierStandard,
			USDPerMTok: rate("20"), TierFrom: 272_000, From: at("2026-01-01T00:00:00Z"), SourceURL: "https://example.test"},
		{Model: "gpt-6-astra", Provider: "openai", Kind: KindOutput, Tier: TierStandard,
			USDPerMTok: rate("75"), TierFrom: 272_000, From: at("2026-01-01T00:00:00Z"), SourceURL: "https://example.test"},
	}, nil)
	if err != nil {
		t.Fatalf("building table: %v", err)
	}

	ask := func(in, out int64) decimal.Decimal {
		res, err := tbl.Cost(Request{
			Model: "gpt-6-astra", Provider: "openai", Tier: TierStandard,
			At:     at("2026-06-01T00:00:00Z"),
			Counts: Counts{KindInput: in, KindOutput: out},
		})
		if err != nil {
			t.Fatalf("Cost: %v", err)
		}
		return res.Cost
	}

	// 271,999 in + 1,000 out at short rates.
	below := ask(271_999, 1_000)
	// 272,000 in + 1,000 out: the whole request reprices, output included.
	above := ask(272_000, 1_000)

	if !above.GreaterThan(below.Mul(rate("1.9"))) {
		t.Errorf("crossing the cliff: %s then %s; the jump is too small", below, above)
	}
	// 272000*20/1e6 + 1000*75/1e6 = 5.44 + 0.075
	if !above.Equal(rate("5.515")) {
		t.Errorf("above the cliff = %s, want 5.515", above)
	}
}

// Cached and cache-write tokens are part of what the model reads, so they
// count towards the threshold that selects the tier.
func TestCachedTokensCountTowardTheInputTier(t *testing.T) {
	tbl, err := NewTable([]Row{
		{Model: "m", Provider: "openai", Kind: KindInput, Tier: TierStandard,
			USDPerMTok: rate("10"), TierTo: 272_000, From: at("2026-01-01T00:00:00Z"), SourceURL: "https://example.test"},
		{Model: "m", Provider: "openai", Kind: KindCachedRead, Tier: TierStandard,
			USDPerMTok: rate("1"), TierTo: 272_000, From: at("2026-01-01T00:00:00Z"), SourceURL: "https://example.test"},
		{Model: "m", Provider: "openai", Kind: KindInput, Tier: TierStandard,
			USDPerMTok: rate("20"), TierFrom: 272_000, From: at("2026-01-01T00:00:00Z"), SourceURL: "https://example.test"},
		{Model: "m", Provider: "openai", Kind: KindCachedRead, Tier: TierStandard,
			USDPerMTok: rate("2"), TierFrom: 272_000, From: at("2026-01-01T00:00:00Z"), SourceURL: "https://example.test"},
	}, nil)
	if err != nil {
		t.Fatalf("building table: %v", err)
	}
	// 200k plain + 100k cached = 300k of input, which is over the cliff even
	// though neither count alone would be.
	res, err := tbl.Cost(Request{
		Model: "m", Provider: "openai", Tier: TierStandard,
		At:     at("2026-06-01T00:00:00Z"),
		Counts: Counts{KindInput: 200_000, KindCachedRead: 100_000},
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	// 200000*20/1e6 + 100000*2/1e6 = 4 + 0.2
	if !res.Cost.Equal(rate("4.2")) {
		t.Errorf("cost = %s, want 4.2 (long-context rates)", res.Cost)
	}
}

// A tier the table does not price is not silently served at standard rates.
func TestUnknownServiceTierIsUnpricedNotStandard(t *testing.T) {
	tbl := anthropicTable(t)
	res, err := tbl.Cost(Request{
		Model: "claude-sonnet-5", Provider: "anthropic", Tier: TierBatch,
		At:     at("2026-06-01T00:00:00Z"),
		Counts: Counts{KindInput: 1_000_000},
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if !res.Unpriced {
		t.Error("a tier with no rows must be unpriced, not billed at standard")
	}
}

// Thinking tokens are a breakdown of output, not a charge of their own.
//
// Every provider that reports them counts them inside its output total:
// OpenAI's completion_tokens includes reasoning_tokens, and Google's output
// is candidatesTokenCount + thoughtsTokenCount. Pricing the reasoning count
// separately would bill those tokens twice; leaving it in the loop with no
// rate behind it marked a complete, correct cost as partially priced.
//
// This is a v0.1.0 bug: every Gemini request that thought, and every OpenAI
// reasoning request, carried partially_priced = true while its figure was
// right all along.
func TestReasoningIsABreakdownRatherThanACharge(t *testing.T) {
	table, err := LoadSeed()
	if err != nil {
		t.Fatalf("loading prices: %v", err)
	}
	at := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name     string
		model    string
		provider string
		thinking Counts
		plain    Counts
	}{
		{
			name: "google", model: "gemini-3.8-flash", provider: "google",
			// The with-thinking capture: 45 in, 136 visible out, 264 thought.
			// Google's normalisation folds thoughts into output, so output
			// is 400 and reasoning repeats 264 of it.
			thinking: Counts{
				KindInput: 45, KindOutput: 400, KindReasoning: 264,
			},
			plain: Counts{KindInput: 45, KindOutput: 400},
		},
		{
			name: "openai", model: "gpt-6-astra", provider: "openai",
			thinking: Counts{
				KindInput: 100, KindOutput: 500, KindReasoning: 300,
			},
			plain: Counts{KindInput: 100, KindOutput: 500},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			with, err := table.Cost(Request{
				Model: tc.model, Provider: tc.provider, Tier: TierStandard,
				At: at, Counts: tc.thinking,
			})
			if err != nil {
				t.Fatalf("pricing with reasoning: %v", err)
			}
			without, err := table.Cost(Request{
				Model: tc.model, Provider: tc.provider, Tier: TierStandard,
				At: at, Counts: tc.plain,
			})
			if err != nil {
				t.Fatalf("pricing without reasoning: %v", err)
			}

			if !with.Cost.Equal(without.Cost) {
				t.Errorf("cost with reasoning = %s, without = %s: the reasoning "+
					"count changed the figure, which means those tokens are "+
					"being charged twice", with.Cost, without.Cost)
			}
			if with.PartiallyPriced {
				t.Errorf("partially_priced = true with unpriced kinds %v: the cost "+
					"is complete, and a flag saying otherwise sends someone "+
					"looking for a hole that is not there", with.UnpricedKinds)
			}
			if with.Unpriced {
				t.Error("unpriced = true: the model is priced")
			}
		})
	}
}
