package provider_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/DiegohNY/costlane/internal/provider"
)

// The golden files are the translation contract: one file per case, so what
// the gateway supports is enumerable rather than folded into prose.
func TestAnthropicRequestTranslation(t *testing.T) {
	dir := filepath.Join("testdata", "anthropic")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading fixtures: %v", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".in.json") {
			continue
		}
		base := strings.TrimSuffix(name, ".in.json")

		t.Run(base, func(t *testing.T) {
			in := readFixture(t, filepath.Join(dir, base+".in.json"))
			want := readFixture(t, filepath.Join(dir, base+".out.json"))

			got, _, err := provider.TranslateAnthropicRequest(in, 4096)
			if err != nil {
				t.Fatalf("translating: %v", err)
			}
			assertJSONEqual(t, got, want)
		})
	}
}

// A parameter with no equivalent is refused by name, never dropped: a client
// receiving a response without the logprobs it asked for cannot tell whether
// the model declined or the gateway discarded the request.
func TestAnthropicRefusesUnsupportedParameters(t *testing.T) {
	for _, param := range []string{
		"logprobs", "top_logprobs", "n", "presence_penalty",
		"frequency_penalty", "logit_bias", "seed", "response_format",
	} {
		t.Run(param, func(t *testing.T) {
			body := []byte(`{"model":"claude-sonnet-5","messages":[],"max_tokens":10,"` +
				param + `":1}`)
			_, _, err := provider.TranslateAnthropicRequest(body, 4096)
			unsupported, ok := provider.AsUnsupported(err)
			if !ok {
				t.Fatalf("err = %v, want an unsupported-parameter error", err)
			}
			if unsupported.Parameter != param {
				t.Errorf("Parameter = %q, want %q", unsupported.Parameter, param)
			}
		})
	}
}

// OpenAI accepts temperature up to 2 and Anthropic only to 1. Rescaling
// would change what the client asked for, so the request is refused instead.
func TestAnthropicRefusesOutOfRangeTemperature(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","messages":[],"max_tokens":10,"temperature":1.5}`)
	_, _, err := provider.TranslateAnthropicRequest(body, 4096)
	if _, ok := provider.AsUnsupported(err); !ok {
		t.Fatalf("err = %v, want an unsupported-parameter error", err)
	}
}

// The one deliberate exception to not injecting a ceiling, reported so the
// client knows it happened.
func TestAnthropicReportsAnInjectedCeiling(t *testing.T) {
	withCeiling := []byte(`{"model":"m","messages":[],"max_tokens":123}`)
	_, injected, err := provider.TranslateAnthropicRequest(withCeiling, 4096)
	if err != nil {
		t.Fatalf("translating: %v", err)
	}
	if injected != 0 {
		t.Errorf("injected = %d, want 0 when the client set its own ceiling", injected)
	}

	without := []byte(`{"model":"m","messages":[]}`)
	_, injected, err = provider.TranslateAnthropicRequest(without, 4096)
	if err != nil {
		t.Fatalf("translating: %v", err)
	}
	if injected != 4096 {
		t.Errorf("injected = %d, want 4096", injected)
	}
}

func TestAnthropicResponseTranslation(t *testing.T) {
	raw := []byte(`{
		"id": "msg_01",
		"model": "claude-sonnet-5",
		"content": [{"type": "text", "text": "hello there"}],
		"stop_reason": "end_turn",
		"usage": {"input_tokens": 12, "output_tokens": 3}
	}`)

	body, served, finish, err := provider.TranslateAnthropicResponse(raw)
	if err != nil {
		t.Fatalf("translating: %v", err)
	}
	if served != "claude-sonnet-5" {
		t.Errorf("served model = %q", served)
	}
	if finish != "stop" {
		t.Errorf("finish_reason = %q, want stop", finish)
	}

	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if out.Object != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", out.Object)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "hello there" {
		t.Errorf("choices = %+v", out.Choices)
	}
}

// A tool call has to come back in the shape an OpenAI client expects, with
// arguments as a JSON string rather than an object.
func TestAnthropicToolCallTranslation(t *testing.T) {
	raw := []byte(`{
		"id": "msg_02",
		"model": "claude-sonnet-5",
		"content": [{"type":"tool_use","id":"tu_1","name":"get_weather","input":{"city":"Rome"}}],
		"stop_reason": "tool_use",
		"usage": {"input_tokens": 20, "output_tokens": 8}
	}`)

	body, _, finish, err := provider.TranslateAnthropicResponse(raw)
	if err != nil {
		t.Fatalf("translating: %v", err)
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", finish)
	}

	var out struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	calls := out.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("%d tool calls, want 1", len(calls))
	}
	if calls[0].Function.Name != "get_weather" {
		t.Errorf("name = %q", calls[0].Function.Name)
	}
	// Arguments travel as a string in the OpenAI shape, not as an object.
	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments are not a JSON string containing an object: %v", err)
	}
	if args["city"] != "Rome" {
		t.Errorf("arguments = %v", args)
	}
}

func TestAnthropicStopReasonMapping(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "stop",
		"max_tokens":    "length",
		"stop_sequence": "stop",
		"tool_use":      "tool_calls",
		"":              "stop",
	}
	for reason, want := range cases {
		t.Run(reason, func(t *testing.T) {
			raw := []byte(`{"id":"m","model":"x","content":[],"stop_reason":"` + reason +
				`","usage":{"input_tokens":1,"output_tokens":1}}`)
			_, _, got, err := provider.TranslateAnthropicResponse(raw)
			if err != nil {
				t.Fatalf("translating: %v", err)
			}
			if got != want {
				t.Errorf("stop_reason %q became %q, want %q", reason, got, want)
			}
		})
	}
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return b
}

func assertJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("decoding result: %v\n%s", err, got)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("decoding expectation: %v", err)
	}
	gj, _ := json.MarshalIndent(g, "", "  ")
	wj, _ := json.MarshalIndent(w, "", "  ")
	if string(gj) != string(wj) {
		t.Errorf("translation mismatch\n--- got ---\n%s\n--- want ---\n%s", gj, wj)
	}
}

// Anthropic has a documented equivalent, so reasoning_effort is mapped
// rather than refused: output_config.effort takes the same three level names
// on every model costlane prices. The refusals below are the levels Anthropic
// does not publish, which stay refusals.
func TestAnthropicMapsReasoningEffort(t *testing.T) {
	for _, c := range []struct{ model, effort, want string }{
		{"claude-sonnet-5", "low", "low"},
		{"claude-sonnet-5", "medium", "medium"},
		{"claude-opus-5", "high", "high"},
		{"claude-fable-5-1", "medium", "medium"},
	} {
		t.Run(c.model+"/"+c.effort, func(t *testing.T) {
			body := []byte(`{"model":"` + c.model + `","messages":[],"max_tokens":10,` +
				`"reasoning_effort":"` + c.effort + `"}`)

			got, _, err := provider.TranslateAnthropicRequest(body, 4096)
			if err != nil {
				t.Fatalf("translating: %v", err)
			}

			var sent struct {
				OutputConfig struct {
					Effort string `json:"effort"`
				} `json:"output_config"`
			}
			if err := json.Unmarshal(got, &sent); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			if sent.OutputConfig.Effort != c.want {
				t.Errorf("output_config.effort = %q, want %q\n%s",
					sent.OutputConfig.Effort, c.want, got)
			}
		})
	}
}

// Absent means absent: the API default is high, and costlane does not decide
// on the caller's behalf which level they meant.
func TestAnthropicAbsentReasoningEffortSendsNoOutputConfig(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5","messages":[],"max_tokens":10}`)

	got, _, err := provider.TranslateAnthropicRequest(body, 4096)
	if err != nil {
		t.Fatalf("translating: %v", err)
	}
	if strings.Contains(string(got), "output_config") {
		t.Errorf("a request with no reasoning_effort grew an output_config:\n%s", got)
	}
}

// Fail closed, the same way Gemini does: a level Anthropic does not publish
// is refused by name rather than rounded to the nearest one we do have.
func TestAnthropicRefusesUnmappableReasoningEffort(t *testing.T) {
	for _, c := range []struct{ name, model, effort string }{
		{"none", "claude-sonnet-5", "none"},
		{"minimal", "claude-sonnet-5", "minimal"},
		{"unknown_model", "claude-not-a-model", "low"},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := []byte(`{"model":"` + c.model + `","messages":[],"max_tokens":10,` +
				`"reasoning_effort":"` + c.effort + `"}`)

			_, _, err := provider.TranslateAnthropicRequest(body, 4096)
			unsupported, ok := provider.AsUnsupported(err)
			if !ok {
				t.Fatalf("err = %v, want an unsupported-parameter error", err)
			}
			if unsupported.Parameter != "reasoning_effort" {
				t.Errorf("Parameter = %q, want reasoning_effort", unsupported.Parameter)
			}
			if unsupported.Detail == "" {
				t.Error("the refusal carries no reason")
			}
		})
	}
}
