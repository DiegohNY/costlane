// Package providerverify checks costlane's assumptions against live provider
// APIs.
//
// Everything else in this repository is tested against a fake provider, which
// proves the gateway does what it intends. It cannot prove that what it
// intends matches what OpenAI, Anthropic and Google actually do — that a
// cancelled stream stops the meter, or that the token fields we read are the
// ones they send. Those are the two assumptions the whole product rests on,
// and they can only be settled by spending real money.
//
// Nothing here runs unless it is asked to:
//
//	COSTLANE_VERIFY_PROVIDERS=1 \
//	COSTLANE_OPENAI_API_KEY=... \
//	COSTLANE_ANTHROPIC_API_KEY=... \
//	COSTLANE_GOOGLE_API_KEY=... \
//	go test ./internal/providerverify/ -v
//
// The results belong in docs/provider-verification.md, with the date and the
// provider request ids printed below, because the billing half of the check
// happens on a dashboard that no test can read.
package providerverify_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DiegohNY/costlane/internal/obs"
	"github.com/DiegohNY/costlane/internal/provider"
	"github.com/DiegohNY/costlane/internal/proxy"
)

// target is one provider to verify, with the model to spend on.
type target struct {
	name  string
	model string
	p     provider.Provider
}

// targets builds the set of providers that have a credential in the
// environment. A provider with no key is skipped rather than failed: the
// point is to verify what can be paid for.
func targets(t *testing.T) []target {
	t.Helper()
	if os.Getenv("COSTLANE_VERIFY_PROVIDERS") != "1" {
		t.Skip("set COSTLANE_VERIFY_PROVIDERS=1 to spend real money against live APIs")
	}

	client := provider.NewHTTPClient(120*time.Second, 10*time.Second)
	opts := func(base, key string) provider.Options {
		return provider.Options{
			BaseURL: base, APIKey: obs.Secret(key), Client: client,
			DefaultMaxTokens: 256,
		}
	}

	var out []target
	if key := os.Getenv("COSTLANE_OPENAI_API_KEY"); key != "" {
		out = append(out, target{
			name:  "openai",
			model: env("COSTLANE_VERIFY_OPENAI_MODEL", "gpt-5.6-terra"),
			p: provider.NewOpenAI(opts(
				env("COSTLANE_OPENAI_BASE_URL", "https://api.openai.com"), key)),
		})
	}
	if key := os.Getenv("COSTLANE_ANTHROPIC_API_KEY"); key != "" {
		out = append(out, target{
			name:  "anthropic",
			model: env("COSTLANE_VERIFY_ANTHROPIC_MODEL", "claude-sonnet-5"),
			p: provider.NewAnthropic(opts(
				env("COSTLANE_ANTHROPIC_BASE_URL", "https://api.anthropic.com"), key)),
		})
	}
	if key := os.Getenv("COSTLANE_GOOGLE_API_KEY"); key != "" {
		out = append(out, target{
			name:  "google",
			model: env("COSTLANE_VERIFY_GOOGLE_MODEL", "gemini-3.8-flash"),
			p: provider.NewGoogle(opts(
				env("COSTLANE_GOOGLE_BASE_URL",
					"https://generativelanguage.googleapis.com"), key)),
		})
	}
	if len(out) == 0 {
		t.Skip("no provider credential in the environment")
	}
	return out
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// body builds a request small enough to be cheap and long enough to stream.
func body(model string, stream bool, prompt string) []byte {
	req := map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
		"stream":   stream,
	}
	if stream {
		req["stream_options"] = map[string]any{"include_usage": true}
	}
	b, _ := json.Marshal(req)
	return b
}

// reportedUsage pulls the token counts out of a raw provider payload, by the
// field names each dialect documents. It exists to be a second opinion: the
// adapter's own parse is what is under test, so the comparison has to come
// from somewhere else.
func reportedUsage(raw []byte) map[string]int64 {
	var envelope struct {
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			InputTokens      int64 `json:"input_tokens"`
			OutputTokens     int64 `json:"output_tokens"`
		} `json:"usage"`
		UsageMetadata struct {
			PromptTokenCount     int64 `json:"promptTokenCount"`
			CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil
	}

	in := envelope.Usage.PromptTokens + envelope.Usage.InputTokens +
		envelope.UsageMetadata.PromptTokenCount
	out := envelope.Usage.CompletionTokens + envelope.Usage.OutputTokens +
		envelope.UsageMetadata.CandidatesTokenCount
	if in == 0 && out == 0 {
		return nil
	}
	return map[string]int64{"input": in, "output": out}
}

// A non-streaming call must be counted exactly as the provider reports it.
// Not approximately, and not by our own tokeniser: the invoice is computed
// from the provider's figures, so the meter has to read the same ones.
func TestNonStreamingCountsMatchTheProviderExactly(t *testing.T) {
	for _, tgt := range targets(t) {
		t.Run(tgt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
			defer cancel()

			resp, err := tgt.p.Complete(ctx, provider.Request{
				Body:      body(tgt.model, false, "Reply with the single word: ok."),
				Model:     tgt.model,
				MaxTokens: 16,
			})
			if err != nil {
				t.Fatalf("calling %s: %v", tgt.name, err)
			}

			reported := reportedUsage(resp.Body)
			if reported == nil {
				t.Fatalf("no usage in the %s response; the dialect has changed:\n%s",
					tgt.name, truncate(resp.Body))
			}

			gotInput := resp.Counts["input"] + resp.Counts["cached_read"]
			gotOutput := resp.Counts["output"]
			if gotInput != reported["input"] || gotOutput != reported["output"] {
				t.Errorf("counted input=%d output=%d, %s reported input=%d output=%d",
					gotInput, gotOutput, tgt.name, reported["input"], reported["output"])
			}

			// Printed for docs/provider-verification.md: a claim about a
			// provider is worth nothing without the request id that
			// supports it.
			t.Logf("VERIFIED %s non-stream at %s: model=%s provider_request_id=%s "+
				"counts=%v", tgt.name, time.Now().UTC().Format(time.RFC3339),
				resp.ServedModel, resp.ProviderRequestID, resp.Counts)
		})
	}
}

// The same, streamed. A streaming response reports its usage in a final event
// rather than in the body, and reading it correctly is what makes streaming
// accounting exact rather than estimated.
func TestStreamingCountsMatchTheProviderExactly(t *testing.T) {
	for _, tgt := range targets(t) {
		streamer, ok := tgt.p.(provider.Streamer)
		if !ok {
			t.Logf("SKIP %s: this adapter does not stream", tgt.name)
			continue
		}

		t.Run(tgt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
			defer cancel()

			stream, err := streamer.Stream(ctx, provider.Request{
				Body:      body(tgt.model, true, "Count from one to twenty, one number per line."),
				Model:     tgt.model,
				Stream:    true,
				MaxTokens: 256,
			})
			if err != nil {
				t.Fatalf("streaming from %s: %v", tgt.name, err)
			}
			defer func() { _ = stream.Body.Close() }()

			// The raw upstream bytes are kept so the provider's own final
			// usage event can be read independently of our accumulator.
			var raw bytes.Buffer
			rec := httptest.NewRecorder()
			if _, err := proxy.Pump(rec, io.TeeReader(stream.Body, &raw), proxy.PumpOptions{
				ClientCtx:      ctx,
				Translate:      stream.Translate,
				TrailingChunks: stream.TrailingChunks,
			}); err != nil {
				t.Fatalf("relaying the %s stream: %v", tgt.name, err)
			}

			counts, reported := stream.Usage()
			if !reported {
				t.Fatalf("%s streamed without reporting usage; the accounting "+
					"would fall back to an estimate:\n%s", tgt.name, truncate(raw.Bytes()))
			}

			want := lastReportedUsage(raw.String())
			if want == nil {
				t.Fatalf("no usage event in the %s stream; the dialect has changed:\n%s",
					tgt.name, truncate(raw.Bytes()))
			}

			gotInput := counts["input"] + counts["cached_read"]
			gotOutput := counts["output"]
			if gotInput != want["input"] || gotOutput != want["output"] {
				t.Errorf("streamed count input=%d output=%d, %s reported input=%d output=%d",
					gotInput, gotOutput, tgt.name, want["input"], want["output"])
			}

			t.Logf("VERIFIED %s stream at %s: model=%s provider_request_id=%s counts=%v",
				tgt.name, time.Now().UTC().Format(time.RFC3339),
				stream.ServedModel, stream.ProviderRequestID, counts)
		})
	}
}

// lastReportedUsage scans an SSE body for the final usage payload, whichever
// frame carries it.
func lastReportedUsage(sse string) map[string]int64 {
	var last map[string]int64
	for _, line := range strings.Split(sse, "\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		if usage := reportedUsage([]byte(data)); usage != nil {
			last = usage
		}
	}
	return last
}

// cancelMaxTokens is the ceiling the cancelled request asks for, and the
// prompt is written to reach it. The gap between what a cancelled stream
// produces and what it was allowed to produce is what makes the result legible
// on a usage dashboard.
//
// A dashboard reports a day in aggregate. A cancelled request that was only
// ever going to yield thirty tokens disappears into the noise of everything
// else billed that day, and the check would prove nothing. Asking for four
// thousand and taking one makes the two outcomes unmistakable: roughly one
// token means generation stopped, roughly four thousand means it did not.
const cancelMaxTokens = 4000

// The cancel policy is the one assumption with real money attached: costlane
// closes the upstream connection when a client disappears, on the belief that
// the provider stops generating and stops charging.
//
// A test can prove the connection closed and record what had been produced by
// then. It cannot read an invoice — that half is a human opening the
// provider's usage dashboard and confirming the figure stopped where this log
// line says it did.
func TestCancellingMidStreamStopsTheUpstream(t *testing.T) {
	for _, tgt := range targets(t) {
		streamer, ok := tgt.p.(provider.Streamer)
		if !ok {
			continue
		}

		t.Run(tgt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
			defer cancel()

			started := time.Now().UTC()
			stream, err := streamer.Stream(ctx, provider.Request{
				Body: body(tgt.model, true,
					"Write a 3000 word essay on the history of the metric system, "+
						"from the French Revolution to the present day. Cover the "+
						"original definitions, the 1875 Metre Convention, the SI "+
						"redefinitions of 1960, 1983 and 2019, and the countries "+
						"that never adopted it. Write it in full."),
				Model:     tgt.model,
				Stream:    true,
				MaxTokens: cancelMaxTokens,
			})
			if err != nil {
				t.Fatalf("streaming from %s: %v", tgt.name, err)
			}
			defer func() { _ = stream.Body.Close() }()

			// One chunk, then leave — exactly what a client closing its
			// connection does, and as early as it can be done.
			reader := proxy.NewFrameReader(stream.Body)
			frame, err := reader.Next()
			if err != nil {
				t.Fatalf("the %s stream ended before its first chunk, so there "+
					"was nothing to cancel: %v", tgt.name, err)
			}
			chunks := 1
			delivered := len(frame.Data)

			// Two clocks on purpose: the UTC stamp is what a dashboard
			// filters on, and time.Now().UTC() drops the monotonic reading
			// that measuring an elapsed interval needs.
			cancelMark := time.Now()
			cancelledAt := cancelMark.UTC()
			cancel()

			// The upstream read must end promptly. A provider that kept
			// generating would keep the body open and keep charging.
			done := make(chan error, 1)
			go func() {
				_, err := io.Copy(io.Discard, stream.Body)
				done <- err
			}()
			var stoppedWithin time.Duration
			select {
			case <-done:
				stoppedWithin = time.Since(cancelMark)
			case <-time.After(10 * time.Second):
				t.Errorf("the %s stream was still delivering ten seconds after "+
					"cancellation: the cancel policy does not stop this provider",
					tgt.name)
			}

			// Everything docs/provider-verification.md needs, in one place. The
			// timestamps are UTC because that is what every provider dashboard
			// filters on.
			t.Logf(`CHECK THE DASHBOARD — %s
  started (UTC):          %s
  cancelled (UTC):        %s
  upstream stopped within %s of the cancel
  provider request id:    %s
  chunks read before cancel: %d (%d bytes of frame payload)
  max_tokens the request allowed: %d
  Expect the day's billed output for this request to sit near the chunk count,
  not near %d. Near %d means this provider kept generating after the
  connection closed, and the cancel default is wrong for it.`,
				tgt.name,
				started.Format(time.RFC3339),
				cancelledAt.Format(time.RFC3339),
				stoppedWithin.Round(time.Millisecond),
				stream.ProviderRequestID,
				chunks, delivered,
				cancelMaxTokens, cancelMaxTokens, cancelMaxTokens)
		})
	}
}

func truncate(b []byte) string {
	const limit = 2000
	if len(b) > limit {
		return fmt.Sprintf("%s… (%d bytes)", b[:limit], len(b))
	}
	return string(b)
}
