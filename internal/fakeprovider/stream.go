package fakeprovider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// The streaming halves of each dialect. They exist here so the SSE pump of
// F6 can be exercised against the event shapes the providers actually emit,
// including the ways they go wrong.

// write emits to the stream, ignoring the error deliberately: once a client
// has gone there is nothing useful to do about a failed write, and this is a
// test double rather than something that has to report the fact.
func write(w http.ResponseWriter, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func prepareStream(w http.ResponseWriter) (http.Flusher, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return flusher, true
}

func streamOpenAI(w http.ResponseWriter, s Scenario, model string, includeUsage bool) {
	flusher, ok := prepareStream(w)
	if !ok {
		return
	}

	id := "chatcmpl-" + s.RequestID
	emit := func(payload any) {
		b, _ := json.Marshal(payload)
		write(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	for i := range s.CompletionTokens {
		if s.FailAfterChunks > 0 && i >= s.FailAfterChunks {
			// Stop without [DONE], as a provider dropping a connection
			// mid-stream does.
			return
		}
		if s.ChunkDelay > 0 {
			time.Sleep(s.ChunkDelay)
		}
		if i == s.MalformedChunkAt {
			// Not valid JSON: the gateway must forward it rather than
			// deciding on the client's behalf that it is unusable.
			write(w, "data: {\"choices\":[{\"delta\":{\"content\":\n\n")
			flusher.Flush()
			continue
		}
		emit(map[string]any{
			"id": id, "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"content": "token "},
			}},
		})
	}

	emit(map[string]any{
		"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{}, "finish_reason": "stop",
		}},
	})

	if includeUsage && !s.OmitUsage {
		emit(map[string]any{
			"id": id, "object": "chat.completion.chunk", "model": model,
			"choices": []any{},
			"usage":   openAIUsage(s),
		})
	}

	write(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func streamAnthropic(w http.ResponseWriter, s Scenario, model string) {
	flusher, ok := prepareStream(w)
	if !ok {
		return
	}

	// Anthropic names its events, and the usage arrives split between the
	// opening message_start and the closing message_delta.
	emit := func(event string, payload any) {
		b, _ := json.Marshal(payload)
		write(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_" + s.RequestID, "type": "message", "role": "assistant",
			"model": model, "content": []any{},
			"usage": map[string]any{
				"input_tokens":  s.PromptTokens,
				"output_tokens": 0,
			},
		},
	})
	emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})

	for i := range s.CompletionTokens {
		if s.FailAfterChunks > 0 && i >= s.FailAfterChunks {
			return
		}
		if s.ChunkDelay > 0 {
			time.Sleep(s.ChunkDelay)
		}
		if i == s.MalformedChunkAt {
			write(w, "event: content_block_delta\ndata: {\"type\":\n\n")
			flusher.Flush()
			continue
		}
		emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": "token "},
		})
	}

	emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})

	if !s.OmitUsage {
		emit("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn"},
			"usage": anthropicUsage(s),
		})
	}
	emit("message_stop", map[string]any{"type": "message_stop"})
}

func streamGemini(w http.ResponseWriter, s Scenario, model string) {
	flusher, ok := prepareStream(w)
	if !ok {
		return
	}

	emit := func(payload any) {
		b, _ := json.Marshal(payload)
		write(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	// Shaped after internal/provider/testdata/gemini/*.sse, captured from
	// the live API. Three things there are easy to get wrong from the
	// documentation alone, and all three matter to the meter:
	//
	//   - usageMetadata rides on EVERY chunk and is cumulative, not a
	//     final summary. A translator that sums would multiply the count.
	//   - responseId and modelVersion are on every chunk too.
	//   - there is no [DONE]. The stream ends after a chunk carrying a
	//     finishReason, and the connection closes.
	//
	// This function conforms to the captures. If the two ever disagree,
	// the captures are right and this is wrong.
	for i := range s.CompletionTokens {
		if s.FailAfterChunks > 0 && i >= s.FailAfterChunks {
			return
		}
		if s.ChunkDelay > 0 {
			time.Sleep(s.ChunkDelay)
		}
		if i == s.MalformedChunkAt {
			write(w, "data: {\"candidates\":\n\n")
			flusher.Flush()
			continue
		}
		emit(map[string]any{
			"modelVersion": model,
			"responseId":   s.RequestID,
			"candidates": []any{map[string]any{
				"content": map[string]any{
					"role":  "model",
					"parts": []any{map[string]any{"text": "token "}},
				},
				"index": 0,
			}},
			"usageMetadata": geminiUsageAt(s, i+1),
		})
	}

	// The closing chunk: an empty part, the finishReason, and the totals.
	// The real API sends exactly this shape, down to the empty text.
	final := map[string]any{
		"modelVersion": model,
		"responseId":   s.RequestID,
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": ""}}},
			"finishReason": "STOP",
			"index":        0,
		}},
	}
	if !s.OmitUsage {
		final["usageMetadata"] = geminiUsage(s)
	}
	emit(final)
}
