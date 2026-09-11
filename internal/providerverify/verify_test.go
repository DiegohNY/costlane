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
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DiegohNY/costlane/internal/obs"
	"github.com/DiegohNY/costlane/internal/provider"
	"github.com/DiegohNY/costlane/internal/proxy"
)

// target is one provider to verify, with the model to spend on.
//
// The adapter is built on demand rather than held, because one of the checks
// has to point it at a recording proxy instead of at the provider directly.
type target struct {
	name    string
	model   string
	baseURL string
	build   func(baseURL string) provider.Provider
}

// p returns the adapter talking straight to the provider.
func (t target) p() provider.Provider { return t.build(t.baseURL) }

// only reports whether a check should run.
//
// COSTLANE_VERIFY_ONLY names one of a, b or c. It exists because the free
// tier allows twenty requests per day per model and each check spends one:
// re-running a check that already has a recorded result costs a request that
// a retry might need later. Unset runs everything.
func only(t *testing.T, check string) {
	t.Helper()
	want := os.Getenv("COSTLANE_VERIFY_ONLY")
	if want != "" && want != check {
		t.Skipf("COSTLANE_VERIFY_ONLY=%s, so check (%s) is not being run", want, check)
	}
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
			name:    "openai",
			model:   env("COSTLANE_VERIFY_OPENAI_MODEL", "gpt-5.6-terra"),
			baseURL: env("COSTLANE_OPENAI_BASE_URL", "https://api.openai.com"),
			build: func(base string) provider.Provider {
				return provider.NewOpenAI(opts(base, key))
			},
		})
	}
	if key := os.Getenv("COSTLANE_ANTHROPIC_API_KEY"); key != "" {
		out = append(out, target{
			name:    "anthropic",
			model:   env("COSTLANE_VERIFY_ANTHROPIC_MODEL", "claude-sonnet-5"),
			baseURL: env("COSTLANE_ANTHROPIC_BASE_URL", "https://api.anthropic.com"),
			build: func(base string) provider.Provider {
				return provider.NewAnthropic(opts(base, key))
			},
		})
	}
	if key := os.Getenv("COSTLANE_GOOGLE_API_KEY"); key != "" {
		out = append(out, target{
			name:  "google",
			model: env("COSTLANE_VERIFY_GOOGLE_MODEL", "gemini-3.8-flash"),
			baseURL: env("COSTLANE_GOOGLE_BASE_URL",
				"https://generativelanguage.googleapis.com"),
			build: func(base string) provider.Provider {
				return provider.NewGoogle(opts(base, key))
			},
		})
	}
	if len(out) == 0 {
		t.Skip("no provider credential in the environment")
	}
	return out
}

// upstreamRecorder is a transparent proxy in front of a real provider that
// keeps a copy of the response body.
//
// It exists because of a hole this test had on its first run. Complete()
// returns a body already translated into the OpenAI dialect, so comparing our
// counts against that body compares costlane's arithmetic with costlane's own
// translation — two halves of the same code agreeing with each other, which is
// not verification of anything. Only OpenAI, whose dialect passes through
// untouched, was ever genuinely checked.
//
// Pointing the adapter at this proxy keeps the call exactly as it would be —
// the same translated request, the same endpoint, the same response — while
// giving the test the bytes the provider actually sent.
type upstreamRecorder struct {
	base   string
	client *http.Client

	mu   sync.Mutex
	last []byte
}

func (u *upstreamRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	target := u.base + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target,
		bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for name, values := range r.Header {
		if skipForward(name) {
			continue
		}
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}

	resp, err := u.client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	u.mu.Lock()
	u.last = raw
	u.mu.Unlock()

	for name, values := range resp.Header {
		if skipReturn(name) {
			continue
		}
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(raw)
}

// skipForward names the request headers that describe this hop rather than
// the call.
//
// Accept-Encoding is the one that matters and the one that cost an hour. Go's
// transport adds it and transparently decompresses the reply — but only when
// it added the header itself. Copying the client's Accept-Encoding forward
// makes the transport treat compression as the caller's business, so the
// recorder ends up holding gzip bytes instead of the JSON it exists to read.
// Dropping it lets each hop negotiate its own encoding, which is what a proxy
// should do anyway.
func skipForward(name string) bool {
	switch strings.ToLower(name) {
	case "host", "content-length", "accept-encoding",
		"connection", "transfer-encoding":
		return true
	}
	return false
}

// skipReturn names the response headers that describe the upstream hop's
// framing. The body being written here is already decoded, so passing on its
// original encoding or length would describe bytes that no longer exist.
func skipReturn(name string) bool {
	switch strings.ToLower(name) {
	case "content-encoding", "content-length",
		"connection", "transfer-encoding":
		return true
	}
	return false
}

func (u *upstreamRecorder) upstreamBody() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]byte(nil), u.last...)
}

// recordingFront puts a recorder in front of one target and returns the
// adapter that goes through it.
func recordingFront(t *testing.T, tgt target) (provider.Provider, *upstreamRecorder) {
	t.Helper()
	rec := &upstreamRecorder{
		base:   tgt.baseURL,
		client: &http.Client{Timeout: 120 * time.Second},
	}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	return tgt.build(srv.URL), rec
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
			// OpenAI. prompt_tokens already includes cached reads.
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			// Anthropic. input_tokens EXCLUDES cache, which is reported
			// beside it, so the two have to be added back together to
			// arrive at what was read in.
			InputTokens          int64 `json:"input_tokens"`
			OutputTokens         int64 `json:"output_tokens"`
			CacheReadInputTokens int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
		// Google.
		UsageMetadata struct {
			PromptTokenCount     int64 `json:"promptTokenCount"`
			CandidatesTokenCount int64 `json:"candidatesTokenCount"`
			// Thinking tokens are billed as output and reported apart from
			// it, so output is the sum. This is the provider's documented
			// billing rule, read here from its own payload.
			ThoughtsTokenCount int64 `json:"thoughtsTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil
	}

	in := envelope.Usage.PromptTokens +
		envelope.Usage.InputTokens + envelope.Usage.CacheReadInputTokens +
		envelope.UsageMetadata.PromptTokenCount
	out := envelope.Usage.CompletionTokens + envelope.Usage.OutputTokens +
		envelope.UsageMetadata.CandidatesTokenCount +
		envelope.UsageMetadata.ThoughtsTokenCount
	if in == 0 && out == 0 {
		return nil
	}
	return map[string]int64{"input": in, "output": out}
}

// A non-streaming call must be counted exactly as the provider reports it.
// Not approximately, and not by our own tokeniser: the invoice is computed
// from the provider's figures, so the meter has to read the same ones.
func TestNonStreamingCountsMatchTheProviderExactly(t *testing.T) {
	only(t, "a")
	for _, tgt := range targets(t) {
		t.Run(tgt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
			defer cancel()

			// Through a recorder, so the comparison is against the bytes the
			// provider sent rather than against our own translation of them.
			p, rec := recordingFront(t, tgt)

			resp, err := p.Complete(ctx, provider.Request{
				Body:      body(tgt.model, false, "Reply with the single word: ok."),
				Model:     tgt.model,
				MaxTokens: 16,
			})
			if err != nil {
				// The recorder holds what the provider actually said, which
				// is the difference between "it failed" and knowing why.
				t.Fatalf("calling %s: %v\nupstream body: %s",
					tgt.name, err, truncate(rec.upstreamBody()))
			}

			upstream := rec.upstreamBody()
			if len(upstream) == 0 {
				t.Fatal("the recorder saw no upstream response; the adapter did " +
					"not go through it, and this check would be comparing " +
					"costlane against itself")
			}

			reported := reportedUsage(upstream)
			if reported == nil {
				t.Fatalf("no usage in the raw %s response; the dialect has changed:\n%s",
					tgt.name, truncate(upstream))
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
			t.Logf("VERIFIED %s non-stream at %s: model=%s provider_request_id=%q\n"+
				"  our counts:     %v\n"+
				"  provider says:  input=%d output=%d\n"+
				"  raw usage:      %s",
				tgt.name, time.Now().UTC().Format(time.RFC3339),
				resp.ServedModel, resp.ProviderRequestID, resp.Counts,
				reported["input"], reported["output"], usageFragment(upstream))
		})
	}
}

// The same, streamed. A streaming response reports its usage in a final event
// rather than in the body, and reading it correctly is what makes streaming
// accounting exact rather than estimated.
func TestStreamingCountsMatchTheProviderExactly(t *testing.T) {
	only(t, "b")
	for _, tgt := range targets(t) {
		streamer, ok := tgt.p().(provider.Streamer)
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

			// Both sides printed, as check (a) does. An assertion that
			// passed says the two agree; it does not put the provider's own
			// figures in the record, and a verification document is worth
			// what its evidence is worth.
			t.Logf(`VERIFIED %s stream at %s: model=%q provider_request_id=%q
  our counts:     %v
  provider says:  input=%d output=%d
  raw usage:      %s`,
				tgt.name, time.Now().UTC().Format(time.RFC3339),
				stream.ServedModel, stream.ProviderRequestID, counts,
				want["input"], want["output"], lastUsageFragment(raw.String()))
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
	only(t, "c")
	for _, tgt := range targets(t) {
		streamer, ok := tgt.p().(provider.Streamer)
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
			firstChunkAt := time.Now().UTC()

			// What the provider had already counted when we left.
			//
			// This is the reference the dashboard figure is compared
			// against, and without it the check reads its own result
			// wrongly. The first run assumed a cancel after one chunk meant
			// roughly one output token billed. On a model that thinks, the
			// first chunk arrives only once the thinking is done — thirty-
			// seven seconds in, on the run that made this obvious — so
			// several hundred tokens can already be on the meter before the
			// cancel is even possible. A dashboard figure of three hundred
			// is then neither the "stopped" case nor the "kept generating"
			// case until you know what had already been produced.
			firstChunkUsage := usageFragment(frame.Data)

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
  started (UTC):            %s
  first chunk (UTC):        %s   (%s after the start)
  cancelled (UTC):          %s
  upstream stopped within   %s of the cancel
  provider request id:      %s
  chunks read before cancel: %d (%d bytes of frame payload)
  max_tokens allowed:       %d
  ALREADY COUNTED at the moment of the cancel, by the provider's own
  figures on the last chunk we read:
      %s

  Read the dashboard as a difference, not as an absolute: total after this
  run minus the total you noted before it. Compare that difference with the
  figures above. Close to them means generation stopped at the disconnect.
  Close to %d means it did not, and the cancel default is wrong for this
  provider. Anything else is neither, and goes in the notes as the number it
  is.`,
				tgt.name,
				started.Format(time.RFC3339),
				firstChunkAt.Format(time.RFC3339),
				firstChunkAt.Sub(started).Round(time.Second),
				cancelledAt.Format(time.RFC3339),
				stoppedWithin.Round(time.Millisecond),
				stream.ProviderRequestID,
				chunks, delivered,
				cancelMaxTokens,
				firstChunkUsage,
				cancelMaxTokens)
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

// usageFragment extracts just the usage object from a provider payload, so the
// log line carries the provider's own figures without the completion text
// beside them. A verification record is worth what its evidence is worth.
func usageFragment(raw []byte) string {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "(unparseable)"
	}
	for _, key := range []string{"usage", "usageMetadata"} {
		if fragment, ok := envelope[key]; ok {
			compact := &bytes.Buffer{}
			if err := json.Compact(compact, fragment); err != nil {
				return string(fragment)
			}
			return key + ": " + compact.String()
		}
	}
	return "(no usage object)"
}

// lastUsageFragment returns the usage object of the last SSE frame that
// carried one, so the streaming record shows the provider's own figures
// rather than only our reading of them.
func lastUsageFragment(sse string) string {
	last := "(none)"
	for _, line := range strings.Split(sse, "\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok || data == "[DONE]" {
			continue
		}
		if reportedUsage([]byte(data)) != nil {
			last = usageFragment([]byte(data))
		}
	}
	return last
}
