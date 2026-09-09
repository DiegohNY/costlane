package pricing

import (
	"strings"
	"sync"
	"testing"
	"testing/fstest"
)

const goodYAML = `
provider: anthropic
models:
  - model: claude-sonnet-5
    source_url: https://platform.claude.com/docs/en/about-claude/pricing
    fetched_at: 2026-09-09T00:00:00Z
    prices:
      - {kind: input,  usd_per_mtok: "2"}
      - {kind: output, usd_per_mtok: "10"}
aliases: []
`

func TestLoadFromYAML(t *testing.T) {
	fsys := fstest.MapFS{"pricing/anthropic.yaml": {Data: []byte(goodYAML)}}
	tbl, err := LoadFS(fsys, "pricing")
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	res, err := tbl.Cost(Request{
		Model: "claude-sonnet-5", Provider: "anthropic", Tier: TierStandard,
		At: at("2026-06-01T00:00:00Z"), Counts: Counts{KindInput: 1_000_000},
	})
	if err != nil {
		t.Fatalf("Cost: %v", err)
	}
	if !res.Cost.Equal(rate("2")) {
		t.Errorf("cost = %s, want 2", res.Cost)
	}
}

// A rate must survive parsing unchanged, which is why the YAML carries it as
// a string. Read as a YAML float it would go through float64 first.
func TestRatesSurviveParsingExactly(t *testing.T) {
	const y = `
provider: google
models:
  - model: gemini-3.8-flash
    source_url: https://ai.google.dev/gemini-api/docs/pricing
    fetched_at: 2026-09-09T00:00:00Z
    prices:
      - {kind: cached_read, usd_per_mtok: "0.075"}
`
	fsys := fstest.MapFS{"pricing/google.yaml": {Data: []byte(y)}}
	tbl, err := LoadFS(fsys, "pricing")
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	res, _ := tbl.Cost(Request{
		Model: "gemini-3.8-flash", Provider: "google", Tier: TierStandard,
		At: at("2026-06-01T00:00:00Z"), Counts: Counts{KindCachedRead: 1_000_000},
	})
	if res.Cost.String() != "0.075" {
		t.Errorf("rate came back as %s, want exactly 0.075", res.Cost)
	}
}

func TestSourceURLIsMandatory(t *testing.T) {
	const y = `
provider: openai
models:
  - model: some-model
    fetched_at: 2026-09-09T00:00:00Z
    prices:
      - {kind: input, usd_per_mtok: "1"}
`
	fsys := fstest.MapFS{"pricing/openai.yaml": {Data: []byte(y)}}
	_, err := LoadFS(fsys, "pricing")
	if err == nil {
		t.Fatal("a model without source_url must be rejected")
	}
	if !strings.Contains(err.Error(), "source_url") {
		t.Errorf("the error should name source_url, got: %v", err)
	}
}

// A typo in a key would otherwise silently drop a price, which is a wrong
// number rather than an error.
func TestUnknownFieldIsRejected(t *testing.T) {
	const y = `
provider: openai
models:
  - model: some-model
    source_url: https://example.test
    fetched_at: 2026-09-09T00:00:00Z
    prices:
      - {kind: input, usd_per_mtokk: "1"}
`
	fsys := fstest.MapFS{"pricing/openai.yaml": {Data: []byte(y)}}
	if _, err := LoadFS(fsys, "pricing"); err == nil {
		t.Fatal("a misspelled field must be rejected, not ignored")
	}
}

func TestOverlappingRowsAreRejectedAtLoad(t *testing.T) {
	const y = `
provider: openai
models:
  - model: some-model
    source_url: https://example.test
    fetched_at: 2026-09-09T00:00:00Z
    prices:
      - {kind: input, usd_per_mtok: "1"}
      - {kind: input, usd_per_mtok: "2"}
`
	fsys := fstest.MapFS{"pricing/openai.yaml": {Data: []byte(y)}}
	_, err := LoadFS(fsys, "pricing")
	if err == nil {
		t.Fatal("two rows for the same kind over the same window must be rejected")
	}
	if !strings.Contains(err.Error(), "overlap") {
		t.Errorf("the error should mention the overlap, got: %v", err)
	}
}

// An alias pointing at a model with no prices is a mistake worth catching at
// load rather than at the first request that hits it.
func TestAliasToAnUnpricedModelIsRejected(t *testing.T) {
	const y = `
provider: openai
models:
  - model: real-model
    source_url: https://example.test
    fetched_at: 2026-09-09T00:00:00Z
    prices:
      - {kind: input, usd_per_mtok: "1"}
aliases:
  - {alias: shorthand, canonical_model: model-that-does-not-exist}
`
	fsys := fstest.MapFS{"pricing/openai.yaml": {Data: []byte(y)}}
	_, err := LoadFS(fsys, "pricing")
	if err == nil {
		t.Fatal("an alias resolving to an unpriced model must be rejected")
	}
	if !strings.Contains(err.Error(), "no prices") {
		t.Errorf("the error should say the target has no prices, got: %v", err)
	}
}

func TestNegativeRateIsRejectedAtLoad(t *testing.T) {
	const y = `
provider: openai
models:
  - model: some-model
    source_url: https://example.test
    fetched_at: 2026-09-09T00:00:00Z
    prices:
      - {kind: input, usd_per_mtok: "-1"}
`
	fsys := fstest.MapFS{"pricing/openai.yaml": {Data: []byte(y)}}
	if _, err := LoadFS(fsys, "pricing"); err == nil {
		t.Fatal("a negative rate must be rejected")
	}
}

func TestUnknownKindIsRejectedAtLoad(t *testing.T) {
	const y = `
provider: openai
models:
  - model: some-model
    source_url: https://example.test
    fetched_at: 2026-09-09T00:00:00Z
    prices:
      - {kind: vibes, usd_per_mtok: "1"}
`
	fsys := fstest.MapFS{"pricing/openai.yaml": {Data: []byte(y)}}
	if _, err := LoadFS(fsys, "pricing"); err == nil {
		t.Fatal("an unknown token kind must be rejected")
	}
}

// The reload contract: all or nothing. A broken file must leave the running
// table intact and still serving.
func TestFailedReloadLeavesTheOldTableServing(t *testing.T) {
	good := fstest.MapFS{"pricing/anthropic.yaml": {Data: []byte(goodYAML)}}
	tbl, err := LoadFS(good, "pricing")
	if err != nil {
		t.Fatalf("initial load: %v", err)
	}
	snap := NewSnapshot(tbl)

	broken := fstest.MapFS{"pricing/anthropic.yaml": {Data: []byte("this: [is not: valid")}}
	if err := snap.ReloadFS(broken, "pricing"); err == nil {
		t.Fatal("reloading a broken file must fail")
	}

	// The old table must still be there, and still correct.
	res, err := snap.Table().Cost(Request{
		Model: "claude-sonnet-5", Provider: "anthropic", Tier: TierStandard,
		At: at("2026-06-01T00:00:00Z"), Counts: Counts{KindInput: 1_000_000},
	})
	if err != nil {
		t.Fatalf("the surviving table must still price: %v", err)
	}
	if !res.Cost.Equal(rate("2")) {
		t.Errorf("after a failed reload the cost is %s, want the old 2", res.Cost)
	}
}

func TestSuccessfulReloadSwapsTheTable(t *testing.T) {
	good := fstest.MapFS{"pricing/anthropic.yaml": {Data: []byte(goodYAML)}}
	tbl, _ := LoadFS(good, "pricing")
	snap := NewSnapshot(tbl)

	repriced := strings.Replace(goodYAML, `{kind: input,  usd_per_mtok: "2"}`,
		`{kind: input,  usd_per_mtok: "3"}`, 1)
	next := fstest.MapFS{"pricing/anthropic.yaml": {Data: []byte(repriced)}}
	if err := snap.ReloadFS(next, "pricing"); err != nil {
		t.Fatalf("a sound reload must succeed: %v", err)
	}

	res, _ := snap.Table().Cost(Request{
		Model: "claude-sonnet-5", Provider: "anthropic", Tier: TierStandard,
		At: at("2026-06-01T00:00:00Z"), Counts: Counts{KindInput: 1_000_000},
	})
	if !res.Cost.Equal(rate("3")) {
		t.Errorf("after reload the cost is %s, want the new 3", res.Cost)
	}
}

// Readers must never see a half-applied table while a reload runs.
func TestConcurrentReadsDuringReload(t *testing.T) {
	good := fstest.MapFS{"pricing/anthropic.yaml": {Data: []byte(goodYAML)}}
	tbl, _ := LoadFS(good, "pricing")
	snap := NewSnapshot(tbl)

	repriced := strings.Replace(goodYAML, `{kind: input,  usd_per_mtok: "2"}`,
		`{kind: input,  usd_per_mtok: "3"}`, 1)
	next := fstest.MapFS{"pricing/anthropic.yaml": {Data: []byte(repriced)}}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				res, err := snap.Table().Cost(Request{
					Model: "claude-sonnet-5", Provider: "anthropic", Tier: TierStandard,
					At: at("2026-06-01T00:00:00Z"), Counts: Counts{KindInput: 1_000_000},
				})
				if err != nil {
					t.Errorf("Cost during reload: %v", err)
					return
				}
				// Either the old rate or the new one, never anything else.
				if !res.Cost.Equal(rate("2")) && !res.Cost.Equal(rate("3")) {
					t.Errorf("saw a half-applied table: cost = %s", res.Cost)
					return
				}
			}
		}()
	}

	for range 50 {
		if err := snap.ReloadFS(next, "pricing"); err != nil {
			t.Errorf("reload: %v", err)
			break
		}
		if err := snap.ReloadFS(good, "pricing"); err != nil {
			t.Errorf("reload back: %v", err)
			break
		}
	}
	close(stop)
	wg.Wait()
}
