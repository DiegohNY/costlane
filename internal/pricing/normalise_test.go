package pricing

import (
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", name, err)
	}
	return b
}

// OpenAI reports cached and cache-write tokens as subsets of the prompt
// total, so they must be subtracted or every cached token is billed twice —
// once at the full input rate and once at the cached rate.
func TestNormaliseOpenAI(t *testing.T) {
	got, err := NormaliseOpenAI(fixture(t, "openai_nonstream.json"))
	if err != nil {
		t.Fatalf("NormaliseOpenAI: %v", err)
	}

	// prompt 15000 = 12000 cached + 3000 cache-write + 0 plain input.
	want := Counts{
		KindInput:        0,
		KindCachedRead:   12000,
		KindCacheWrite5m: 3000,
		KindOutput:       900,
		KindReasoning:    400,
	}
	assertCounts(t, got.Counts, want)

	// The parts must reconstruct the provider's own declared total.
	if got.Counts[KindInput]+got.Counts[KindCachedRead]+got.Counts[KindCacheWrite5m] != 15000 {
		t.Errorf("input parts do not reconstruct prompt_tokens: %v", got.Counts)
	}
	if got.ParseErrors != 0 {
		t.Errorf("a well-formed payload produced %d parse errors", got.ParseErrors)
	}
}

// Anthropic reports cache fields separately from input_tokens, so they must
// be added. This is the opposite convention from OpenAI, and getting it
// backwards would understate every cached request.
func TestNormaliseAnthropic(t *testing.T) {
	got, err := NormaliseAnthropic(fixture(t, "anthropic_nonstream.json"))
	if err != nil {
		t.Fatalf("NormaliseAnthropic: %v", err)
	}

	want := Counts{
		KindInput:        500,
		KindCachedRead:   8000,
		KindCacheWrite5m: 148,
		KindCacheWrite1h: 100,
		KindOutput:       1200,
	}
	assertCounts(t, got.Counts, want)

	// The TTL split must reconstruct the flat total the provider reports.
	if got.Counts[KindCacheWrite5m]+got.Counts[KindCacheWrite1h] != 248 {
		t.Errorf("the TTL split does not sum to cache_creation_input_tokens: %v", got.Counts)
	}
	if got.ParseErrors != 0 {
		t.Errorf("a well-formed payload produced %d parse errors", got.ParseErrors)
	}
}

// When the nested TTL breakdown disagrees with the flat total, the
// normaliser must say so rather than invent a split. A wrong TTL misprices
// by 1.6x.
func TestAnthropicTTLMismatchDegradesRatherThanGuesses(t *testing.T) {
	got, err := NormaliseAnthropic(fixture(t, "anthropic_ttl_mismatch.json"))
	if err != nil {
		t.Fatalf("a mismatch must not be a hard error: %v", err)
	}
	if got.ParseErrors == 0 {
		t.Error("a TTL breakdown that does not sum to the flat total must raise a parse error")
	}
	if got.Degraded != true {
		t.Error("the result must be marked degraded")
	}
	// The flat total is still the best available figure, so no tokens are
	// lost: charging nothing would be worse than charging at one TTL.
	if total := got.Counts[KindCacheWrite5m] + got.Counts[KindCacheWrite1h]; total != 1000 {
		t.Errorf("cache-write tokens total %d, want the flat 1000 preserved", total)
	}
}

// Google subtracts on input and adds on output: the only provider that does
// both, and the easiest to get wrong in one direction or the other.
func TestNormaliseGoogle(t *testing.T) {
	got, err := NormaliseGoogle(fixture(t, "google_nonstream.json"))
	if err != nil {
		t.Fatalf("NormaliseGoogle: %v", err)
	}

	// prompt 9000 includes 6000 cached, so plain input is 3000.
	// Output is candidates 700 plus thoughts 250, which are separate.
	want := Counts{
		KindInput:      3000,
		KindCachedRead: 6000,
		KindOutput:     950,
		KindReasoning:  250,
	}
	assertCounts(t, got.Counts, want)

	// prompt + thoughts + candidates is Google's own definition of the total.
	sum := got.Counts[KindInput] + got.Counts[KindCachedRead] + got.Counts[KindOutput]
	if sum != 9950 {
		t.Errorf("parts sum to %d, want the declared totalTokenCount of 9950", sum)
	}
}

// Reasoning tokens are billed as output by every provider, so they are
// recorded inside the output count rather than added to it. Counting them
// twice would overstate every reasoning-heavy request.
func TestReasoningIsInsideOutputNotAdditional(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func([]byte) (Normalised, error)
		file string
	}{
		{"openai", NormaliseOpenAI, "openai_nonstream.json"},
		{"google", NormaliseGoogle, "google_nonstream.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.fn(fixture(t, tc.file))
			if err != nil {
				t.Fatalf("normalising: %v", err)
			}
			if got.Counts[KindReasoning] > got.Counts[KindOutput] {
				t.Errorf("reasoning (%d) exceeds output (%d); it must be a subset",
					got.Counts[KindReasoning], got.Counts[KindOutput])
			}
		})
	}
}

// A payload with no usage at all is not zero usage: it is a payload we could
// not read.
func TestMissingUsageIsAnError(t *testing.T) {
	for name, fn := range map[string]func([]byte) (Normalised, error){
		"openai":    NormaliseOpenAI,
		"anthropic": NormaliseAnthropic,
		"google":    NormaliseGoogle,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := fn([]byte(`{"id":"x"}`)); err == nil {
				t.Error("a payload with no usage must be an error, not zero tokens")
			}
		})
	}
}

func TestMalformedJSONIsAnError(t *testing.T) {
	for name, fn := range map[string]func([]byte) (Normalised, error){
		"openai":    NormaliseOpenAI,
		"anthropic": NormaliseAnthropic,
		"google":    NormaliseGoogle,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := fn([]byte(`{"usage":`)); err == nil {
				t.Error("malformed JSON must be an error")
			}
		})
	}
}

// A provider reporting more cached tokens than prompt tokens is incoherent.
// Clamping silently would hide a real problem behind a plausible number.
func TestOpenAICachedExceedingPromptIsFlagged(t *testing.T) {
	payload := []byte(`{"usage":{"prompt_tokens":100,"completion_tokens":10,
	                    "prompt_tokens_details":{"cached_tokens":500}}}`)
	got, err := NormaliseOpenAI(payload)
	if err != nil {
		t.Fatalf("an incoherent payload must not be a hard error: %v", err)
	}
	if got.ParseErrors == 0 || !got.Degraded {
		t.Error("cached tokens exceeding the prompt total must be flagged")
	}
	if got.Counts[KindInput] < 0 {
		t.Errorf("input must never go negative, got %d", got.Counts[KindInput])
	}
}

func assertCounts(t *testing.T, got, want Counts) {
	t.Helper()
	for _, kind := range AllKinds {
		if got[kind] != want[kind] {
			t.Errorf("%s = %d, want %d", kind, got[kind], want[kind])
		}
	}
}
