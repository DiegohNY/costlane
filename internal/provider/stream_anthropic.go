package provider

import (
	"encoding/json"
	"fmt"
)

// AnthropicStreamTranslator turns Messages events into chat-completion
// chunks.
//
// The two protocols disagree about more than field names. Anthropic opens
// with the full input token count and closes with the output count, while
// OpenAI sends usage once at the end. Anthropic streams tool arguments as
// partial JSON inside a numbered content block; OpenAI streams them as
// fragments of a string, under an index of its own. The translator holds the
// little state needed to bridge that, and no more.
type AnthropicStreamTranslator struct {
	id    string
	model string

	// inputTokens arrives in message_start and is not repeated, so it has
	// to be carried to the end to build a single usage chunk.
	inputTokens      int
	cacheReadTokens  int
	cacheWriteTokens int
	outputTokens     int

	// blockKind remembers what each content block index is, because a
	// delta says only that it is a delta.
	blockKind map[int]string
	// toolIndex maps a content block to the tool_calls index OpenAI
	// expects, which counts only tool blocks and not text ones.
	toolIndex map[int]int
	nextTool  int

	sawUsage bool
}

// NewAnthropicStreamTranslator builds a translator.
func NewAnthropicStreamTranslator() *AnthropicStreamTranslator {
	return &AnthropicStreamTranslator{
		blockKind: map[int]string{},
		toolIndex: map[int]int{},
	}
}

// anthropicEvent is the union of the event shapes that carry anything the
// translation needs.
type anthropicEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// Translate converts one Anthropic event into zero or more OpenAI chunks.
//
// Zero is a normal outcome: several Anthropic events exist only to frame
// others, and inventing a chunk for each would produce a stream no OpenAI
// client expects.
func (t *AnthropicStreamTranslator) Translate(event, data []byte) ([][]byte, error) {
	var e anthropicEvent
	if err := json.Unmarshal(data, &e); err != nil {
		// A malformed event is not fatal: the caller forwards what it
		// cannot parse and records that it could not.
		return nil, fmt.Errorf("provider: parsing anthropic event: %w", err)
	}
	if e.Type == "" {
		e.Type = string(event)
	}

	switch e.Type {
	case "message_start":
		if e.Message != nil {
			t.id, t.model = e.Message.ID, e.Message.Model
			if u := e.Message.Usage; u != nil {
				t.inputTokens = u.InputTokens
				t.cacheReadTokens = u.CacheReadInputTokens
				t.cacheWriteTokens = u.CacheCreationInputTokens
			}
		}
		// The opening chunk announces the assistant role, as OpenAI does.
		return [][]byte{t.chunk(map[string]any{"role": "assistant"}, nil)}, nil

	case "content_block_start":
		if e.ContentBlock == nil {
			return nil, nil
		}
		t.blockKind[e.Index] = e.ContentBlock.Type
		if e.ContentBlock.Type != "tool_use" {
			return nil, nil
		}
		// OpenAI indexes tool calls separately from content blocks, and
		// announces name and id once before the arguments start arriving.
		index := t.nextTool
		t.toolIndex[e.Index] = index
		t.nextTool++
		return [][]byte{t.chunk(map[string]any{
			"tool_calls": []any{map[string]any{
				"index": index,
				"id":    e.ContentBlock.ID,
				"type":  "function",
				"function": map[string]any{
					"name":      e.ContentBlock.Name,
					"arguments": "",
				},
			}},
		}, nil)}, nil

	case "content_block_delta":
		if e.Delta == nil {
			return nil, nil
		}
		switch e.Delta.Type {
		case "text_delta":
			return [][]byte{t.chunk(map[string]any{"content": e.Delta.Text}, nil)}, nil
		case "input_json_delta":
			index, ok := t.toolIndex[e.Index]
			if !ok {
				// A delta for a block we never saw start: forwarding a
				// tool call with no name would be worse than dropping it.
				return nil, nil
			}
			return [][]byte{t.chunk(map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    index,
					"function": map[string]any{"arguments": e.Delta.PartialJSON},
				}},
			}, nil)}, nil
		}
		return nil, nil

	case "message_delta":
		if e.Usage != nil {
			t.outputTokens = e.Usage.OutputTokens
			t.sawUsage = true
		}
		if e.Delta == nil || e.Delta.StopReason == "" {
			return nil, nil
		}
		finish := anthropicFinishReason(e.Delta.StopReason)
		return [][]byte{t.chunk(map[string]any{}, &finish)}, nil

	case "message_stop", "content_block_stop", "ping":
		return nil, nil
	}
	return nil, nil
}

// Usage returns the counts gathered across the stream, and whether the
// provider actually reported them.
func (t *AnthropicStreamTranslator) Usage() (input, cachedRead, cacheWrite, output int, reported bool) {
	return t.inputTokens, t.cacheReadTokens, t.cacheWriteTokens, t.outputTokens, t.sawUsage
}

// UsageChunk builds the final chunk an OpenAI client expects when it asked
// for usage. Anthropic never sends one, so it is assembled from the counts
// collected along the way.
func (t *AnthropicStreamTranslator) UsageChunk() []byte {
	payload := map[string]any{
		"id":      t.id,
		"object":  "chat.completion.chunk",
		"model":   t.model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     t.inputTokens + t.cacheReadTokens + t.cacheWriteTokens,
			"completion_tokens": t.outputTokens,
			"total_tokens": t.inputTokens + t.cacheReadTokens +
				t.cacheWriteTokens + t.outputTokens,
		},
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}

func (t *AnthropicStreamTranslator) chunk(delta map[string]any, finish *string) []byte {
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

func anthropicFinishReason(stop string) string {
	switch stop {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}
