package provider_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DiegohNY/costlane/internal/provider"
)

// The golden streams pin the hardest translation in the gateway. Anthropic
// sends tool arguments as partial JSON inside a numbered content block;
// OpenAI expects fragments of a string under an index that counts only tool
// calls. Getting this approximately right would produce tool calls that
// parse but carry the wrong arguments, which is worse than not supporting it.
func TestAnthropicStreamTranslation(t *testing.T) {
	dir := filepath.Join("testdata", "stream")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading fixtures: %v", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".events") {
			continue
		}
		base := strings.TrimSuffix(name, ".events")

		t.Run(base, func(t *testing.T) {
			events := readLines(t, filepath.Join(dir, base+".events"))
			want := readLines(t, filepath.Join(dir, base+".expected"))

			translator := provider.NewAnthropicStreamTranslator()
			var got []string
			for _, line := range events {
				eventName, data, found := strings.Cut(line, "\t")
				if !found {
					t.Fatalf("fixture line has no tab separator: %q", line)
				}
				chunks, err := translator.Translate([]byte(eventName), []byte(data))
				if err != nil {
					t.Fatalf("translating %s: %v", eventName, err)
				}
				for _, c := range chunks {
					got = append(got, string(c))
				}
			}

			if len(got) != len(want) {
				t.Fatalf("%d chunks, want %d\n--- got ---\n%s\n--- want ---\n%s",
					len(got), len(want), strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			for i := range got {
				assertJSONEqual(t, []byte(got[i]), []byte(want[i]))
			}
		})
	}
}

// Reassembling the argument fragments must yield the JSON the model meant to
// send. This is the property a client actually depends on.
func TestToolArgumentFragmentsReassemble(t *testing.T) {
	events := readLines(t, filepath.Join("testdata", "stream", "tool_call.events"))
	translator := provider.NewAnthropicStreamTranslator()

	var arguments strings.Builder
	for _, line := range events {
		eventName, data, _ := strings.Cut(line, "\t")
		chunks, err := translator.Translate([]byte(eventName), []byte(data))
		if err != nil {
			t.Fatalf("translating: %v", err)
		}
		for _, c := range chunks {
			var chunk struct {
				Choices []struct {
					Delta struct {
						ToolCalls []struct {
							Function struct {
								Arguments string `json:"arguments"`
							} `json:"function"`
						} `json:"tool_calls"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(c, &chunk); err != nil {
				t.Fatalf("decoding chunk: %v", err)
			}
			for _, call := range chunk.Choices[0].Delta.ToolCalls {
				arguments.WriteString(call.Function.Arguments)
			}
		}
	}

	var args map[string]string
	if err := json.Unmarshal([]byte(arguments.String()), &args); err != nil {
		t.Fatalf("the reassembled arguments are not valid JSON: %q: %v",
			arguments.String(), err)
	}
	if args["city"] != "Rome" {
		t.Errorf("arguments = %v, want city Rome", args)
	}
}

// Anthropic sends the input count at the start and the output count at the
// end, so a single OpenAI usage chunk has to be assembled from both.
func TestUsageIsGatheredAcrossTheStream(t *testing.T) {
	events := readLines(t, filepath.Join("testdata", "stream", "text.events"))
	translator := provider.NewAnthropicStreamTranslator()
	for _, line := range events {
		eventName, data, _ := strings.Cut(line, "\t")
		if _, err := translator.Translate([]byte(eventName), []byte(data)); err != nil {
			t.Fatalf("translating: %v", err)
		}
	}

	input, _, _, output, reported := translator.Usage()
	if !reported {
		t.Error("the provider did report usage")
	}
	if input != 25 || output != 4 {
		t.Errorf("usage: input=%d output=%d, want 25 and 4", input, output)
	}

	var chunk struct {
		Choices []any `json:"choices"`
		Usage   struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(translator.UsageChunk(), &chunk); err != nil {
		t.Fatalf("decoding usage chunk: %v", err)
	}
	// An OpenAI usage chunk carries no choices, which is how a client
	// distinguishes it from content.
	if len(chunk.Choices) != 0 {
		t.Errorf("the usage chunk carries %d choices, want none", len(chunk.Choices))
	}
	if chunk.Usage.PromptTokens != 25 || chunk.Usage.CompletionTokens != 4 {
		t.Errorf("usage chunk = %+v", chunk.Usage)
	}
}

// A malformed event is reported rather than turned into a plausible chunk.
func TestMalformedEventIsReportedNotInvented(t *testing.T) {
	translator := provider.NewAnthropicStreamTranslator()
	chunks, err := translator.Translate([]byte("content_block_delta"), []byte(`{"type":`))
	if err == nil {
		t.Error("a malformed event must be reported")
	}
	if len(chunks) != 0 {
		t.Errorf("%d chunks produced from an unparseable event", len(chunks))
	}
}

// An argument delta for a block that never started would become a tool call
// with no name, which a client cannot act on.
func TestOrphanToolDeltaIsDropped(t *testing.T) {
	translator := provider.NewAnthropicStreamTranslator()
	chunks, err := translator.Translate([]byte("content_block_delta"),
		[]byte(`{"type":"content_block_delta","index":7,"delta":{"type":"input_json_delta","partial_json":"{}"}}`))
	if err != nil {
		t.Fatalf("translating: %v", err)
	}
	if len(chunks) != 0 {
		t.Errorf("%d chunks from a delta with no matching block start", len(chunks))
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// A client that asks for usage on a translated stream must receive the chunk.
//
// It did not. ClientWantsUsage defaulted to false for every provider except
// OpenAI, and the proxy drops the trailing chunks unless it is set — so the
// usage chunk Anthropic's translator carefully assembles was built and thrown
// away on every request. Found while wiring the same field for Gemini, which
// had inherited the gap.
func TestTranslatedStreamsReportWhetherTheClientAskedForUsage(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"asked", `{"model":"m","stream":true,"stream_options":{"include_usage":true}}`, true},
		{"asked for false", `{"model":"m","stream":true,"stream_options":{"include_usage":false}}`, false},
		{"did not ask", `{"model":"m","stream":true}`, false},
		{"empty options", `{"model":"m","stream":true,"stream_options":{}}`, false},
		{"options are not an object", `{"model":"m","stream_options":"yes"}`, false},
		{"not JSON at all", `not json`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := provider.ClientAskedForUsage([]byte(tc.body)); got != tc.want {
				t.Errorf("ClientAskedForUsage(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
