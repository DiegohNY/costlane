package provider

import (
	"encoding/json"
	"fmt"

	"github.com/DiegohNY/costlane/internal/pricing"
)

// GoogleStreamTranslator turns streamGenerateContent frames into
// chat-completion chunks.
//
// It is written against captured bytes rather than against the documentation,
// which describes two other dialects for the same endpoint. See
// internal/provider/testdata/gemini/README.md for what the captures settled;
// the three facts that shape this type are:
//
//   - Every chunk carries the whole usageMetadata, cumulative. The last one
//     seen is the answer, and summing would multiply the count by the number
//     of chunks.
//   - A tool call arrives complete in a single chunk, args and all. There is
//     no partial-JSON accumulation of the kind Anthropic needs, and reusing
//     that machinery here would be inventing a problem.
//   - There is no [DONE]. The stream ends after a chunk carrying a
//     finishReason, and the connection closes.
type GoogleStreamTranslator struct {
	id    string
	model string

	// lastUsage is the raw frame that carried the most recent
	// usageMetadata, kept whole so that pricing.NormaliseGoogle can read
	// it. Re-deriving the counts here would duplicate the one rule most
	// expensive to get wrong: that thinking tokens fold into output.
	lastUsage []byte

	sawFinishReason bool
	sentRole        bool
	toolCalls       int
}

// NewGoogleStreamTranslator builds a translator.
func NewGoogleStreamTranslator() *GoogleStreamTranslator {
	return &GoogleStreamTranslator{}
}

// googleStreamChunk is the part of a GenerateContentResponse the translation
// reads. Fields it does not use — thoughtSignature, promptTokensDetails,
// serviceTier — are left out rather than parsed and dropped.
type googleStreamChunk struct {
	Candidates []struct {
		Content struct {
			Role  string `json:"role"`
			Parts []struct {
				Text         string `json:"text"`
				FunctionCall *struct {
					ID   string          `json:"id"`
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				} `json:"functionCall"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
		Index        int    `json:"index"`
	} `json:"candidates"`
	UsageMetadata json.RawMessage `json:"usageMetadata"`
	ModelVersion  string          `json:"modelVersion"`
	ResponseID    string          `json:"responseId"`
}

// Translate converts one Gemini chunk into zero or more OpenAI chunks.
//
// Zero is a normal outcome: the final chunk of a Gemini stream is often an
// empty text part carrying only a thought signature and a finishReason, and
// inventing a content delta for it would put an empty string in front of a
// client that never asked for one.
func (t *GoogleStreamTranslator) Translate(_, data []byte) ([][]byte, error) {
	var c googleStreamChunk
	if err := json.Unmarshal(data, &c); err != nil {
		// A malformed chunk is not fatal: the caller forwards what it
		// cannot parse and records that it could not.
		return nil, fmt.Errorf("provider: parsing a google stream chunk: %w", err)
	}

	if t.id == "" && c.ResponseID != "" {
		t.id = "chatcmpl-" + c.ResponseID
	}
	if c.ModelVersion != "" {
		t.model = c.ModelVersion
	}
	if len(c.UsageMetadata) > 0 {
		// Cumulative: keep the latest, never accumulate.
		t.lastUsage = append([]byte(nil), data...)
	}

	if len(c.Candidates) == 0 {
		return nil, nil
	}
	candidate := c.Candidates[0]

	var out [][]byte
	for _, part := range candidate.Content.Parts {
		switch {
		case part.FunctionCall != nil:
			// Whole in one chunk, so one delta carries the name and the
			// complete arguments. An OpenAI client concatenates argument
			// fragments; handing it the entire string at once is a
			// degenerate case of that and needs no state.
			args := string(part.FunctionCall.Args)
			if args == "" {
				args = "{}"
			}
			out = append(out, t.chunk(map[string]any{
				"tool_calls": []any{map[string]any{
					"index": t.toolCalls,
					"id":    part.FunctionCall.ID,
					"type":  "function",
					"function": map[string]any{
						"name":      part.FunctionCall.Name,
						"arguments": args,
					},
				}},
			}, nil))
			t.toolCalls++
		case part.Text != "":
			out = append(out, t.chunk(map[string]any{"content": part.Text}, nil))
		}
	}

	if candidate.FinishReason != "" {
		t.sawFinishReason = true
		finish := t.finishReason(candidate.FinishReason)
		out = append(out, t.chunk(map[string]any{}, &finish))
	}

	return out, nil
}

// finishReason maps Gemini's reason onto the OpenAI vocabulary.
//
// A tool call overrides whatever Gemini said, because Gemini reports STOP for
// a turn that ended in a function call and an OpenAI client keys its own
// control flow on tool_calls.
func (t *GoogleStreamTranslator) finishReason(reason string) string {
	if t.toolCalls > 0 {
		return "tool_calls"
	}
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	default:
		// An unrecognised reason is reported as a stop rather than
		// invented: the turn did end, and the record keeps the raw figure.
		return "stop"
	}
}

// ClosedCleanly reports whether a chunk carrying a finishReason went past.
//
// For Gemini this is the whole test. The connection closing is the ordinary
// end of a stream, not an interruption, so the question is whether the model
// said why it stopped before it did.
func (t *GoogleStreamTranslator) ClosedCleanly() bool { return t.sawFinishReason }

// Usage returns the counts from the most recent chunk, and whether any
// arrived at all.
//
// The figures go through pricing.NormaliseGoogle, the same function the
// non-streaming path uses, so the rule that thinking tokens are counted
// inside output lives in exactly one place. A second copy here is how the
// streaming and non-streaming meters would come to disagree.
func (t *GoogleStreamTranslator) Usage() (pricing.Counts, bool) {
	if len(t.lastUsage) == 0 {
		return pricing.Counts{}, false
	}
	normalised, err := pricing.NormaliseGoogle(t.lastUsage)
	if err != nil {
		return pricing.Counts{}, false
	}
	return normalised.Counts, true
}

// UsageChunk builds the final chunk an OpenAI client expects when it asked
// for usage. Gemini states its counts on every chunk in its own shape, so
// this restates the last of them in the shape the client is waiting for.
func (t *GoogleStreamTranslator) UsageChunk() []byte {
	counts, ok := t.Usage()
	if !ok {
		return nil
	}
	// Cached reads are part of what the prompt cost, and reasoning is
	// already inside output — see pricing.Kind.Billable.
	prompt := counts[pricing.KindInput] + counts[pricing.KindCachedRead]
	completion := counts[pricing.KindOutput]

	payload := map[string]any{
		"id":      t.id,
		"object":  "chat.completion.chunk",
		"model":   t.model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     prompt,
			"completion_tokens": completion,
			"total_tokens":      prompt + completion,
		},
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}

// chunk renders one chat-completion chunk.
func (t *GoogleStreamTranslator) chunk(delta map[string]any, finish *string) []byte {
	if !t.sentRole {
		// An OpenAI client expects the assistant role on the opening
		// delta. Gemini says "model" on every chunk instead, which is the
		// same fact in another vocabulary.
		delta["role"] = "assistant"
		t.sentRole = true
	}
	choice := map[string]any{"index": 0, "delta": delta}
	if finish != nil {
		choice["finish_reason"] = *finish
	}
	payload := map[string]any{
		"id":      t.id,
		"object":  "chat.completion.chunk",
		"model":   t.model,
		"choices": []any{choice},
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}
