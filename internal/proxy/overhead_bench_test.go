package proxy_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DiegohNY/costlane/internal/fakeprovider"
	"github.com/DiegohNY/costlane/internal/storetest"
)

// These benchmarks answer one question: what does putting costlane in front
// of a provider cost?
//
// Each pair measures the same work twice — once straight to the fake
// provider, once through the gateway to the same fake provider — so the
// difference is the gateway and nothing else. The absolute figures are
// meaningless on their own (they are dominated by a loopback socket and a
// provider that answers instantly); the delta is the number the README
// publishes.
//
// The harness has no usage buffer, so its usage INSERT happens on the request
// path. Production hands that record to a bounded buffer instead, which makes
// every gateway figure below an upper bound.

const (
	benchBody       = `{"model":"gpt-6-astra","messages":[{"role":"user","content":"benchmark"}]}`
	benchStreamBody = `{"model":"gpt-6-astra","messages":[{"role":"user","content":"benchmark"}],"stream":true}`
	// A budget large enough that no run exhausts it: a 402 halfway through
	// would measure the refusal path instead.
	benchLimit = "1000000"
)

func benchHeaders() map[string]string {
	return map[string]string{
		fakeprovider.HeaderPromptTokens:     "1000",
		fakeprovider.HeaderCompletionTokens: "200",
	}
}

// percentile returns the p-th percentile of a set of samples, using nearest
// rank. With the sample counts a benchmark produces, interpolating would be
// false precision.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted))*p) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

// reportLatency turns a set of samples into the metrics benchstat compares.
// Means hide the tail, and the tail is what an operator feels.
func reportLatency(b *testing.B, name string, samples []time.Duration) {
	b.Helper()
	slices.Sort(samples)
	b.ReportMetric(float64(percentile(samples, 0.50).Microseconds())/1000, name+"-p50-ms")
	b.ReportMetric(float64(percentile(samples, 0.99).Microseconds())/1000, name+"-p99-ms")
}

// benchRequest builds one request, ready to send.
func benchRequest(b *testing.B, url, auth, body string, headers map[string]string) *http.Request {
	b.Helper()
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, url+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		b.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

// runNonStream drives one non-streaming benchmark against a URL.
func runNonStream(b *testing.B, url, auth string) {
	b.Helper()
	client := &http.Client{Timeout: 30 * time.Second}
	samples := make([]time.Duration, 0, 1024)

	b.ResetTimer()
	for b.Loop() {
		start := time.Now()
		resp, err := client.Do(benchRequest(b, url, auth, benchBody, benchHeaders()))
		if err != nil {
			b.Fatalf("requesting: %v", err)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			b.Fatalf("reading the body: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("status = %d", resp.StatusCode)
		}
		samples = append(samples, time.Since(start))
	}
	b.StopTimer()
	reportLatency(b, "req", samples)
}

// BenchmarkNonStreamDirect is the baseline: the client talks to the provider
// with nothing in between. It needs no database, so it runs anywhere.
func BenchmarkNonStreamDirect(b *testing.B) {
	fake := httptest.NewServer(fakeprovider.Handler())
	b.Cleanup(fake.Close)
	runNonStream(b, fake.URL, "")
}

// BenchmarkNonStreamGateway is the same call through costlane: authenticate,
// price, reserve, forward, settle, account.
func BenchmarkNonStreamGateway(b *testing.B) {
	h := newHarnessOn(b, benchLimit, storetest.NewBenchDB(b))
	srv := httptest.NewServer(h.handler)
	b.Cleanup(srv.Close)
	runNonStream(b, srv.URL, h.key.Secret.Expose())
}

// runStream drives one streaming benchmark, separating the time to the first
// chunk from the cost of every chunk after it. They are different products:
// time to first token is what a user waits for, per-chunk cost is what a long
// completion multiplies.
func runStream(b *testing.B, url, auth string) {
	b.Helper()
	client := &http.Client{Timeout: 60 * time.Second}
	ttfts := make([]time.Duration, 0, 1024)
	perChunk := make([]time.Duration, 0, 1024)

	b.ResetTimer()
	for b.Loop() {
		start := time.Now()
		resp, err := client.Do(benchRequest(b, url, auth, benchStreamBody, benchHeaders()))
		if err != nil {
			b.Fatalf("requesting: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			b.Fatalf("status = %d", resp.StatusCode)
		}

		var (
			ttft   time.Duration
			chunks int
		)
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if !strings.HasPrefix(scanner.Text(), "data: ") {
				continue
			}
			if chunks == 0 {
				ttft = time.Since(start)
			}
			chunks++
		}
		_ = resp.Body.Close()
		if chunks == 0 {
			b.Fatal("the stream carried no data frames")
		}

		ttfts = append(ttfts, ttft)
		perChunk = append(perChunk, time.Since(start)/time.Duration(chunks))
	}
	b.StopTimer()
	reportLatency(b, "ttft", ttfts)
	reportLatency(b, "chunk", perChunk)
}

// BenchmarkStreamDirect is the baseline stream, straight from the provider.
func BenchmarkStreamDirect(b *testing.B) {
	fake := httptest.NewServer(fakeprovider.Handler())
	b.Cleanup(fake.Close)
	runStream(b, fake.URL, "")
}

// BenchmarkStreamGateway is the same stream relayed through costlane, which
// parses every frame to count tokens as they pass.
func BenchmarkStreamGateway(b *testing.B) {
	h := newHarnessOn(b, benchLimit, storetest.NewBenchDB(b))
	srv := httptest.NewServer(h.handler)
	b.Cleanup(srv.Close)
	runStream(b, srv.URL, h.key.Secret.Expose())
}
