package pricing

import (
	"testing"
	"time"
)

// The seed must load. A broken price file is a gateway that cannot price.
func TestSeedLoads(t *testing.T) {
	if _, err := LoadSeed(); err != nil {
		t.Fatalf("the shipped price files must load: %v", err)
	}
}

// Every rate below was read from the provider's own pricing page and is
// asserted here so an accidental edit fails a test rather than mis-billing
// silently. A rate change is a deliberate act: update the YAML and this
// table together, with the source that justifies it.
func TestSeededRatesMatchTheVerifiedFigures(t *testing.T) {
	tbl, err := LoadSeed()
	if err != nil {
		t.Fatalf("LoadSeed: %v", err)
	}
	when := at("2026-09-09T00:00:00Z")

	cases := []struct {
		model, provider string
		kind            Kind
		inputSize       int64
		want            string
	}{
		// OpenAI, short context (up to 272,000 input tokens).
		{"gpt-6-astra", "openai", KindInput, 1000, "10"},
		{"gpt-6-astra", "openai", KindCachedRead, 1000, "1"},
		{"gpt-6-astra", "openai", KindCacheWrite5m, 1000, "12.50"},
		{"gpt-6-astra", "openai", KindOutput, 1000, "50"},
		// OpenAI, long context: input and output double and 1.5x
		// respectively; this model's page also states cache rates double.
		{"gpt-6-astra", "openai", KindInput, 300_000, "20"},
		{"gpt-6-astra", "openai", KindCachedRead, 300_000, "2"},
		{"gpt-6-astra", "openai", KindOutput, 300_000, "75"},
		{"gpt-5.6-sol", "openai", KindInput, 1000, "4"},
		{"gpt-5.6-sol", "openai", KindOutput, 1000, "20"},
		{"gpt-5.6-terra", "openai", KindInput, 1000, "2"},
		{"gpt-5.6-terra", "openai", KindOutput, 1000, "12"},

		// Anthropic. Note Fable's cache read is 0.025x its input rate,
		// where every other model uses 0.1x — read, not derived.
		{"claude-fable-5-1", "anthropic", KindInput, 1000, "10"},
		{"claude-fable-5-1", "anthropic", KindCachedRead, 1000, "0.25"},
		{"claude-fable-5-1", "anthropic", KindCacheWrite5m, 1000, "12.50"},
		{"claude-fable-5-1", "anthropic", KindCacheWrite1h, 1000, "20"},
		{"claude-fable-5-1", "anthropic", KindOutput, 1000, "50"},
		{"claude-opus-5", "anthropic", KindInput, 1000, "5"},
		{"claude-opus-5", "anthropic", KindCachedRead, 1000, "0.50"},
		{"claude-opus-5", "anthropic", KindOutput, 1000, "25"},
		{"claude-sonnet-5", "anthropic", KindInput, 1000, "2"},
		{"claude-sonnet-5", "anthropic", KindCacheWrite5m, 1000, "2.50"},
		{"claude-sonnet-5", "anthropic", KindCacheWrite1h, 1000, "4"},
		{"claude-sonnet-5", "anthropic", KindOutput, 1000, "10"},

		// Google. The Pro tiers at 200,000 prompt tokens, with input
		// doubling but output rising only 1.5x.
		{"gemini-3.1-pro-preview", "google", KindInput, 1000, "2"},
		{"gemini-3.1-pro-preview", "google", KindOutput, 1000, "12"},
		{"gemini-3.1-pro-preview", "google", KindInput, 250_000, "4"},
		{"gemini-3.1-pro-preview", "google", KindOutput, 250_000, "18"},
		{"gemini-3.8-flash", "google", KindInput, 1000, "0.75"},
		{"gemini-3.8-flash", "google", KindOutput, 1000, "3.75"},
	}

	for _, c := range cases {
		name := c.model + "/" + string(c.kind)
		t.Run(name, func(t *testing.T) {
			got := ratePerMTok(t, tbl, c.model, c.provider, c.kind, c.inputSize, when)
			if got != normalise(c.want) {
				t.Errorf("%s at input size %d = %s, want %s",
					name, c.inputSize, got, c.want)
			}
		})
	}
}

// Gemini Flash reprices on 1 January 2027. Both windows are seeded, so the
// change lands without anyone remembering to act.
func TestGeminiFlashRepricesOnSchedule(t *testing.T) {
	tbl, err := LoadSeed()
	if err != nil {
		t.Fatalf("LoadSeed: %v", err)
	}
	before := ratePerMTok(t, tbl, "gemini-3.8-flash", "google", KindInput, 1000,
		at("2026-12-31T23:59:59Z"))
	after := ratePerMTok(t, tbl, "gemini-3.8-flash", "google", KindInput, 1000,
		at("2027-01-01T00:00:00Z"))

	if before != "0.75" {
		t.Errorf("before the increase: %s, want 0.75", before)
	}
	if after != "1.5" {
		t.Errorf("from 2027-01-01: %s, want 1.50", after)
	}
}

// The gpt-5.6-sol and gpt-5.6-terra pages state only that input and output
// double above 272K; neither mentions cache. Inferring a doubled cache rate
// would be inventing one, so long-context cached usage on those models is
// deliberately unpriced — and being partially priced, it still charges for
// the kinds that do have a rate.
func TestUndocumentedLongContextCacheIsPartiallyPriced(t *testing.T) {
	tbl, err := LoadSeed()
	if err != nil {
		t.Fatalf("LoadSeed: %v", err)
	}
	res, err := tbl.Cost(Request{
		Model: "gpt-5.6-sol", Provider: "openai", Tier: TierStandard,
		At: at("2026-09-09T00:00:00Z"),
		Counts: Counts{
			KindInput:      300_000,
			KindCachedRead: 100_000,
			KindOutput:     1_000,
		},
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if !res.PartiallyPriced {
		t.Error("long-context cached usage on sol must be reported as partially priced")
	}
	if res.Cost.IsZero() {
		t.Error("a partially priced request must still charge for its priced kinds")
	}
	// Astra documents the doubling, so it prices fully.
	astra, err := tbl.Cost(Request{
		Model: "gpt-6-astra", Provider: "openai", Tier: TierStandard,
		At: at("2026-09-09T00:00:00Z"),
		Counts: Counts{
			KindInput:      300_000,
			KindCachedRead: 100_000,
			KindOutput:     1_000,
		},
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if astra.PartiallyPriced {
		t.Errorf("astra documents long-context cache rates and must price fully, unpriced kinds: %v",
			astra.UnpricedKinds)
	}
}

// No aliases are seeded: OpenAI publishes its flagships under bare ids with
// no documented alias, and from Claude 4.6 on a dateless id is itself the
// pinned snapshot. Inventing aliases for convenience would make the resolver
// lie about what a name means.
func TestSeedInventsNoAliases(t *testing.T) {
	tbl, err := LoadSeed()
	if err != nil {
		t.Fatalf("LoadSeed: %v", err)
	}
	if n := len(tbl.aliases); n != 0 {
		t.Errorf("the seed declares %d aliases, want none", n)
	}
	// A canonical id resolves to itself.
	if got := tbl.Resolve("claude-opus-5", "anthropic", at("2026-09-09T00:00:00Z")); got != "claude-opus-5" {
		t.Errorf("Resolve(claude-opus-5) = %q, want itself", got)
	}
}

// Every seeded row must carry the page it came from.
func TestEverySeededRowCitesItsSource(t *testing.T) {
	tbl, err := LoadSeed()
	if err != nil {
		t.Fatalf("LoadSeed: %v", err)
	}
	for key, rows := range tbl.rows {
		for _, r := range rows {
			if r.SourceURL == "" {
				t.Errorf("%s/%s %s has no source_url", key.provider, key.model, r.Kind)
			}
			if r.FetchedAt.IsZero() {
				t.Errorf("%s/%s %s has no fetched_at", key.provider, key.model, r.Kind)
			}
		}
	}
}

// --- helpers --------------------------------------------------------------

// ratePerMTok returns the published rate that applies to a kind at a given
// context size and instant.
func ratePerMTok(t *testing.T, tbl *Table, model, provider string, kind Kind,
	inputSize int64, when time.Time) string {
	t.Helper()
	row, ok := pick(tbl.rows[modelKey{model, provider}], kind, TierStandard, when, inputSize)
	if !ok {
		t.Fatalf("no %s row for %s/%s at input size %d", kind, provider, model, inputSize)
	}
	return row.USDPerMTok.String()
}

// normalise strips trailing zeros so "12.50" and "12.5" compare equal.
func normalise(s string) string {
	d, err := decimalFromString(s)
	if err != nil {
		return s
	}
	return d.String()
}
