package provider_test

import (
	"encoding/json"
	"testing"

	"github.com/DiegohNY/costlane/internal/provider"
)

// The stream translator consumes events from a third party and holds state
// across them, which is where a malformed or out-of-order sequence could put
// it somewhere its authors never considered. The invariants are narrow: it
// must not panic, and every chunk it emits must be valid JSON in the shape an
// OpenAI client can parse — a chunk that parses but is malformed inside would
// be worse than no chunk at all.
func FuzzAnthropicStreamTranslator(f *testing.F) {
	seeds := []string{
		`{"type":"message_start","message":{"id":"m","model":"x","usage":{"input_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"f"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		`{"type":"message_stop"}`,
		`{"type":"ping"}`,
		`{}`,
		`{"type":"content_block_delta","index":999999,"delta":{"type":"input_json_delta","partial_json":"x"}}`,
		`{"type":"message_start","message":null}`,
		`{"type":"unknown_future_event","payload":{"anything":1}}`,
	}
	for _, s := range seeds {
		f.Add("content_block_delta", s)
	}

	f.Fuzz(func(t *testing.T, event, data string) {
		translator := provider.NewAnthropicStreamTranslator()
		chunks, err := translator.Translate([]byte(event), []byte(data))
		if err != nil {
			// A malformed event is reported, not turned into a chunk.
			if len(chunks) != 0 {
				t.Fatalf("%d chunks produced alongside an error", len(chunks))
			}
			return
		}

		for _, chunk := range chunks {
			var parsed struct {
				Object  string `json:"object"`
				Choices []struct {
					Index int             `json:"index"`
					Delta json.RawMessage `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(chunk, &parsed); err != nil {
				t.Fatalf("emitted a chunk that is not valid JSON: %s: %v", chunk, err)
			}
			if parsed.Object != "chat.completion.chunk" {
				t.Fatalf("chunk object = %q, want chat.completion.chunk: %s",
					parsed.Object, chunk)
			}
			if len(parsed.Choices) != 1 {
				t.Fatalf("chunk carries %d choices, want exactly 1: %s",
					len(parsed.Choices), chunk)
			}
		}

		// The usage chunk must stay well formed whatever was fed in.
		var usage struct {
			Choices []json.RawMessage `json:"choices"`
			Usage   struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(translator.UsageChunk(), &usage); err != nil {
			t.Fatalf("the usage chunk is not valid JSON: %v", err)
		}
		if len(usage.Choices) != 0 {
			t.Fatal("a usage chunk must carry no choices, or a client reads it as content")
		}
		if usage.Usage.PromptTokens < 0 || usage.Usage.CompletionTokens < 0 {
			t.Fatalf("negative token counts: %+v", usage.Usage)
		}
	})
}
