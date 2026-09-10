package provider_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DiegohNY/costlane/internal/pricing"
	"github.com/DiegohNY/costlane/internal/provider"
	"github.com/shopspring/decimal"
)

// The fixtures in testdata/gemini are captures from the live API, taken
// before this translator existed. They are the specification: where the
// translator and a capture disagree, the capture is right.
//
// See testdata/gemini/README.md for how they were taken and what they
// settled.

// geminiFrames reads a captured stream and returns its data payloads.
// Lines beginning with a colon are SSE comments, which is where the capture
// metadata lives.
func geminiFrames(t *testing.T, name string) [][]byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "gemini", name))
	if err != nil {
		t.Fatalf("reading the capture: %v", err)
	}
	var frames [][]byte
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		frames = append(frames, []byte(payload))
	}
	if len(frames) == 0 {
		t.Fatalf("%s carried no data frames", name)
	}
	return frames
}

// chatChunk is the little of an OpenAI chunk these assertions read.
type chatChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role      string `json:"role"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
}

// translateCapture runs a whole capture through a fresh translator.
func translateCapture(t *testing.T, name string) (*provider.GoogleStreamTranslator, []chatChunk) {
	t.Helper()
	tr := provider.NewGoogleStreamTranslator()

	var out []chatChunk
	for _, frame := range geminiFrames(t, name) {
		chunks, err := tr.Translate(nil, frame)
		if err != nil {
			t.Fatalf("%s: translating a captured frame failed: %v", name, err)
		}
		for _, c := range chunks {
			var parsed chatChunk
			if err := json.Unmarshal(c, &parsed); err != nil {
				t.Fatalf("%s: the translator emitted invalid JSON: %v", name, err)
			}
			if parsed.Object != "chat.completion.chunk" {
				t.Errorf("%s: object = %q, want chat.completion.chunk", name, parsed.Object)
			}
			out = append(out, parsed)
		}
	}
	return tr, out
}

// text joins the content deltas, which is what a client would display.
func text(chunks []chatChunk) string {
	var b strings.Builder
	for _, c := range chunks {
		for _, choice := range c.Choices {
			b.WriteString(choice.Delta.Content)
		}
	}
	return b.String()
}

// finishReason returns the last finish_reason seen, or "" if none was.
func finishReason(chunks []chatChunk) string {
	last := ""
	for _, c := range chunks {
		for _, choice := range c.Choices {
			if choice.FinishReason != nil {
				last = *choice.FinishReason
			}
		}
	}
	return last
}

// Each capture, end to end: what a client would see, and what the meter
// would charge.
func TestGoogleStreamTranslationAgainstCaptures(t *testing.T) {
	for _, tc := range []struct {
		fixture      string
		wantFinish   string
		wantInput    int64
		wantOutput   int64
		wantReason   int64
		wantTextPart string
	}{
		{
			// thinkingBudget 0, so usageMetadata carries no
			// thoughtsTokenCount at all and output is the visible count.
			fixture: "short-no-thinking.sse", wantFinish: "stop",
			wantInput: 6, wantOutput: 1, wantReason: 0, wantTextPart: "ok",
		},
		{
			// The case that decides the field names: 136 visible tokens
			// and 264 thought ones. Output must be the sum, because that
			// is what Google bills.
			fixture: "with-thinking.sse", wantFinish: "stop",
			wantInput: 45, wantOutput: 400, wantReason: 264, wantTextPart: "step-by-step",
		},
		{
			fixture: "tool-call.sse", wantFinish: "tool_calls",
			wantInput: 83, wantOutput: 78, wantReason: 60,
		},
		{
			// Ran into its own 2000-token ceiling, so it also covers the
			// truncation reason in a stream.
			fixture: "long-output.sse", wantFinish: "length",
			wantInput: 45, wantOutput: 1996, wantReason: 0, wantTextPart: "metric",
		},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			tr, chunks := translateCapture(t, tc.fixture)

			if got := finishReason(chunks); got != tc.wantFinish {
				t.Errorf("finish_reason = %q, want %q", got, tc.wantFinish)
			}
			if tc.wantTextPart != "" && !strings.Contains(text(chunks), tc.wantTextPart) {
				t.Errorf("the relayed text does not contain %q", tc.wantTextPart)
			}

			counts, reported := tr.Usage()
			if !reported {
				t.Fatal("no usage was reported, but every captured chunk carries it")
			}
			if got := counts[pricing.KindInput] + counts[pricing.KindCachedRead]; got != tc.wantInput {
				t.Errorf("input = %d, want %d", got, tc.wantInput)
			}
			if got := counts[pricing.KindOutput]; got != tc.wantOutput {
				t.Errorf("output = %d, want %d. Google bills thinking as output, "+
					"so this is candidatesTokenCount + thoughtsTokenCount",
					got, tc.wantOutput)
			}
			if got := counts[pricing.KindReasoning]; got != tc.wantReason {
				t.Errorf("reasoning = %d, want %d", got, tc.wantReason)
			}

			if !tr.ClosedCleanly() {
				t.Error("ClosedCleanly() is false for a capture that ran to a finishReason")
			}
		})
	}
}

// A tool call arrives whole in one chunk, so it becomes one delta carrying
// the name and the complete arguments. The Anthropic path accumulates
// partial JSON across chunks; reusing that here would be machinery for a
// problem this provider does not have.
func TestGoogleToolCallIsOneCompleteDelta(t *testing.T) {
	_, chunks := translateCapture(t, "tool-call.sse")

	var calls int
	for _, c := range chunks {
		for _, choice := range c.Choices {
			for _, call := range choice.Delta.ToolCalls {
				calls++
				if call.Index != 0 {
					t.Errorf("tool call index = %d, want 0", call.Index)
				}
				if call.Type != "function" {
					t.Errorf("tool call type = %q, want function", call.Type)
				}
				if call.ID == "" {
					t.Error("tool call has no id; a client cannot answer it")
				}
				if call.Function.Name != "get_weather" {
					t.Errorf("function name = %q, want get_weather", call.Function.Name)
				}
				var args map[string]any
				if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
					t.Fatalf("arguments are not valid JSON (%q): %v",
						call.Function.Arguments, err)
				}
				if args["city"] != "Bologna" {
					t.Errorf("arguments = %v, want city Bologna", args)
				}
			}
		}
	}
	if calls != 1 {
		t.Errorf("emitted %d tool call deltas, want exactly 1", calls)
	}
}

// Usage is cumulative on every chunk. Summing would multiply the count by the
// number of chunks, which on the long capture is a factor of eighty.
func TestGoogleUsageTakesTheLastChunkRatherThanSumming(t *testing.T) {
	tr, _ := translateCapture(t, "long-output.sse")
	counts, reported := tr.Usage()
	if !reported {
		t.Fatal("no usage reported")
	}

	// The capture's final usageMetadata. A summing translator would report
	// a number orders of magnitude larger.
	if counts[pricing.KindOutput] != 1996 {
		t.Errorf("output = %d, want 1996: the count must come from the last "+
			"chunk, not from adding up eighty cumulative snapshots",
			counts[pricing.KindOutput])
	}
	if counts[pricing.KindInput] != 45 {
		t.Errorf("input = %d, want 45", counts[pricing.KindInput])
	}
}

// A stream that stops before any finishReason was not a clean close, whatever
// the connection did. This is the half of the per-provider predicate that
// says something went wrong.
func TestGoogleStreamCutShortIsNotACleanClose(t *testing.T) {
	frames := geminiFrames(t, "long-output.sse")
	tr := provider.NewGoogleStreamTranslator()

	// Everything except the final chunk, which is the one that carries the
	// finishReason.
	for _, frame := range frames[:len(frames)-1] {
		if _, err := tr.Translate(nil, frame); err != nil {
			t.Fatalf("translating: %v", err)
		}
	}

	if tr.ClosedCleanly() {
		t.Error("ClosedCleanly() is true for a stream that stopped before any " +
			"finishReason. For Gemini the connection ending is the ordinary " +
			"close, so the finishReason is the only thing that separates a " +
			"complete stream from a truncated one.")
	}

	// The usage read so far is still the provider's own figure, which is
	// what lets a cancelled Gemini stream be accounted for exactly.
	counts, reported := tr.Usage()
	if !reported {
		t.Fatal("a cut-short stream reported no usage, though every chunk carries it")
	}
	if counts[pricing.KindOutput] == 0 {
		t.Error("output = 0 after reading most of a stream")
	}
}

// The failure matrix. None of these may panic, and none may be reported as a
// clean close.
func TestGoogleStreamFailureMatrix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		frames     []string
		wantErr    bool
		wantClean  bool
		wantChunks int
	}{
		{
			name:   "malformed JSON",
			frames: []string{`{"candidates":`},
			// An error tells the pump to forward the frame raw and count a
			// parse error rather than drop it.
			wantErr: true, wantClean: false,
		},
		{
			name:       "empty candidates",
			frames:     []string{`{"candidates":[],"modelVersion":"gemini-3.8-flash"}`},
			wantChunks: 0,
		},
		{
			name:       "candidate with no parts",
			frames:     []string{`{"candidates":[{"content":{"role":"model"},"index":0}]}`},
			wantChunks: 0,
		},
		{
			name:       "empty text part carries no content delta",
			frames:     []string{`{"candidates":[{"content":{"parts":[{"text":""}],"role":"model"}}]}`},
			wantChunks: 0,
		},
		{
			name: "unknown finishReason still closes the turn",
			frames: []string{
				`{"candidates":[{"content":{"parts":[{"text":"x"}]},"finishReason":"SOMETHING_NEW"}]}`,
			},
			wantClean: true, wantChunks: 2,
		},
		{
			name: "safety stop maps to content_filter",
			frames: []string{
				`{"candidates":[{"content":{"parts":[]},"finishReason":"SAFETY"}]}`,
			},
			wantClean: true, wantChunks: 1,
		},
		{
			name:       "no usageMetadata anywhere",
			frames:     []string{`{"candidates":[{"content":{"parts":[{"text":"x"}]}}]}`},
			wantChunks: 1,
		},
		{
			name:       "null candidates",
			frames:     []string{`{"candidates":null}`},
			wantChunks: 0,
		},
		{
			name:       "empty object",
			frames:     []string{`{}`},
			wantChunks: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := provider.NewGoogleStreamTranslator()
			var (
				chunks int
				sawErr bool
			)
			for _, frame := range tc.frames {
				out, err := tr.Translate(nil, []byte(frame))
				if err != nil {
					sawErr = true
					continue
				}
				chunks += len(out)
				for _, c := range out {
					var parsed chatChunk
					if err := json.Unmarshal(c, &parsed); err != nil {
						t.Fatalf("emitted invalid JSON: %v", err)
					}
				}
			}
			if sawErr != tc.wantErr {
				t.Errorf("error = %v, want %v", sawErr, tc.wantErr)
			}
			if !tc.wantErr && chunks != tc.wantChunks {
				t.Errorf("emitted %d chunks, want %d", chunks, tc.wantChunks)
			}
			if tr.ClosedCleanly() != tc.wantClean {
				t.Errorf("ClosedCleanly() = %v, want %v", tr.ClosedCleanly(), tc.wantClean)
			}
		})
	}
}

// A stream that reported nothing must say so, rather than presenting a zero
// as a measurement.
func TestGoogleUsageIsUnreportedWhenNoChunkCarriedIt(t *testing.T) {
	tr := provider.NewGoogleStreamTranslator()
	if _, err := tr.Translate(nil,
		[]byte(`{"candidates":[{"content":{"parts":[{"text":"x"}]}}]}`)); err != nil {
		t.Fatalf("translating: %v", err)
	}
	if _, reported := tr.Usage(); reported {
		t.Error("Usage() reported true with no usageMetadata in the stream: a " +
			"zero presented as a measurement is how a request gets billed at " +
			"nothing")
	}
	if chunk := tr.UsageChunk(); chunk != nil {
		t.Errorf("UsageChunk() = %s, want nil when there is nothing to report", chunk)
	}
}

// The usage chunk an OpenAI client waits for, assembled from Gemini's own
// last figures.
func TestGoogleUsageChunkRestatesTheProviderFigures(t *testing.T) {
	tr, _ := translateCapture(t, "with-thinking.sse")

	var parsed struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
		Choices []any `json:"choices"`
	}
	if err := json.Unmarshal(tr.UsageChunk(), &parsed); err != nil {
		t.Fatalf("the usage chunk is not valid JSON: %v", err)
	}

	if parsed.Usage.PromptTokens != 45 {
		t.Errorf("prompt_tokens = %d, want 45", parsed.Usage.PromptTokens)
	}
	if parsed.Usage.CompletionTokens != 400 {
		t.Errorf("completion_tokens = %d, want 400 (136 visible + 264 thought)",
			parsed.Usage.CompletionTokens)
	}
	if parsed.Usage.TotalTokens != 445 {
		t.Errorf("total_tokens = %d, want 445", parsed.Usage.TotalTokens)
	}
	if len(parsed.Choices) != 0 {
		t.Errorf("choices = %v, want empty on a usage chunk", parsed.Choices)
	}
}

// The whole point of the exercise, priced: the captured thinking stream must
// cost what Google charges for it, with no flag suggesting the figure might
// be incomplete.
//
// gemini-3.8-flash through 2026: $0.75/Mtok in, $3.75/Mtok out. The capture
// reports 45 prompt tokens, 136 visible output tokens and 264 thought ones,
// and Google bills thinking as output — so the charge is on 400 output
// tokens, not 136.
func TestGoogleThinkingStreamIsPricedInFull(t *testing.T) {
	tr, _ := translateCapture(t, "with-thinking.sse")
	counts, reported := tr.Usage()
	if !reported {
		t.Fatal("no usage reported")
	}

	table, err := pricing.LoadSeed()
	if err != nil {
		t.Fatalf("loading prices: %v", err)
	}
	result, err := table.Cost(pricing.Request{
		Model: "gemini-3.8-flash", Provider: "google", Tier: pricing.TierStandard,
		At: time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC), Counts: counts,
	})
	if err != nil {
		t.Fatalf("pricing: %v", err)
	}

	// 45 * 0.75/1e6 + 400 * 3.75/1e6
	want := decimal.RequireFromString("0.00153375")
	if !result.Cost.Equal(want) {
		t.Errorf("cost = %s, want %s (45 input at 0.75, 400 output at 3.75)",
			result.Cost, want)
	}
	if result.PartiallyPriced {
		t.Errorf("partially_priced = true with unpriced kinds %v. Thinking "+
			"tokens are a breakdown of output rather than a class of their "+
			"own, so nothing here is unpriced.", result.UnpricedKinds)
	}
	if result.Unpriced {
		t.Error("unpriced = true for a seeded model")
	}
}
