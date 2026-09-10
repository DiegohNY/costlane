package proxy_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DiegohNY/costlane/internal/fakeprovider"
	"github.com/shopspring/decimal"
)

// serve runs the harness over a real listener, so streaming is exercised
// across an actual socket rather than a recorder.
func (h *harness) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h.handler)
	t.Cleanup(srv.Close)
	return srv
}

// the response is returned unread because a stream must be consumed
// incrementally to prove it is not buffered.
//
// streamRequest builds a streaming request. The caller performs it and owns
// the body, because a stream has to be consumed incrementally to prove it is
// not being buffered — reading it inside a helper would defeat the tests.
//
//nolint:bodyclose // the body is closed by the t.Cleanup this registers;
func (h *harness) streamRequest(t *testing.T, srv *httptest.Server,
	body string, headers map[string]string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.key.Secret.Expose())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

// send performs a prepared request; the caller closes the body.
func (h *harness) send(t *testing.T, srv *httptest.Server, req *http.Request) *http.Response {
	t.Helper()
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("requesting: %v", err)
	}
	return resp
}

// A streamed request is proxied, relayed and accounted for like any other.
func TestStreamEndToEnd(t *testing.T) {
	h := newHarness(t, "100")
	srv := h.serve(t)

	req := h.streamRequest(t, srv,
		`{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		map[string]string{
			fakeprovider.HeaderPromptTokens:     "1000",
			fakeprovider.HeaderCompletionTokens: "5",
		})
	resp := h.send(t, srv, req)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Errorf("Content-Type = %q", got)
	}
	// Any reverse proxy in front of us would otherwise buffer the response
	// and undo the streaming entirely.
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q, want no", got)
	}

	body := readStream(t, resp)
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("the stream did not terminate:\n%s", body)
	}
	if n := strings.Count(body, `"delta"`); n < 5 {
		t.Errorf("%d delta chunks, want at least the 5 requested", n)
	}

	// The accounting must match: 1000 in and 5 out on astra's short-context
	// rates, 10 and 50 per million.
	waitForSettle(t, h)
	var spent string
	if err := h.db.Pool().QueryRow(t.Context(),
		`SELECT spent_usd::text FROM key_budgets`).Scan(&spent); err != nil {
		t.Fatalf("reading budget: %v", err)
	}
	want := decimal.RequireFromString("0.01025") // 0.01 + 0.00025
	if !decimal.RequireFromString(spent).Equal(want) {
		t.Errorf("spent_usd = %s, want %s", spent, want)
	}

	var streamed bool
	var source string
	if err := h.db.Pool().QueryRow(t.Context(),
		`SELECT streamed, usage_source FROM usage_records`).Scan(&streamed, &source); err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	if !streamed {
		t.Error("the record does not say it was streamed")
	}
	if source != "provider" {
		t.Errorf("usage_source = %q, want provider: the fake reported usage", source)
	}
}

// The chunks must reach the client as they are produced, not in one piece at
// the end. This is what the whole pump exists for.
func TestChunksArriveProgressively(t *testing.T) {
	h := newHarness(t, "100")
	srv := h.serve(t)

	req := h.streamRequest(t, srv,
		`{"model":"gpt-6-astra","messages":[],"stream":true}`,
		map[string]string{
			fakeprovider.HeaderCompletionTokens: "5",
			fakeprovider.HeaderChunkDelay:       "40",
		})
	resp := h.send(t, srv, req)
	defer func() { _ = resp.Body.Close() }()

	start := time.Now()
	var arrivals []time.Duration
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data:") {
			arrivals = append(arrivals, time.Since(start))
		}
	}

	if len(arrivals) < 3 {
		t.Fatalf("%d chunks arrived, want several", len(arrivals))
	}
	// With 40ms between chunks upstream, the first must arrive well before
	// the last. If anything buffered, they would land together.
	spread := arrivals[len(arrivals)-1] - arrivals[0]
	if spread < 80*time.Millisecond {
		t.Errorf("chunks spanned only %v: the stream is being buffered somewhere", spread)
	}
	if arrivals[0] > 100*time.Millisecond {
		t.Errorf("the first chunk took %v, longer than the upstream delay", arrivals[0])
	}
}

// A client that goes away mid-stream is still accounted for: the tokens were
// produced and billed by the provider whether or not anyone read them.
func TestClientDisconnectMidStreamIsStillAccounted(t *testing.T) {
	h := newHarness(t, "100")
	srv := h.serve(t)

	ctx, disconnect := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-6-astra","messages":[],"stream":true}`))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.key.Secret.Expose())
	req.Header.Set(fakeprovider.HeaderCompletionTokens, "200")
	req.Header.Set(fakeprovider.HeaderChunkDelay, "10")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("requesting: %v", err)
	}

	// Read a couple of chunks, then vanish.
	buf := make([]byte, 128)
	for range 2 {
		if _, err := resp.Body.Read(buf); err != nil {
			break
		}
	}
	disconnect()
	_ = resp.Body.Close()

	waitForSettle(t, h)

	var (
		disconnected bool
		source       string
		streamed     bool
	)
	if err := h.db.Pool().QueryRow(t.Context(),
		`SELECT client_disconnected, usage_source, streamed FROM usage_records`).
		Scan(&disconnected, &source, &streamed); err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	if !disconnected {
		t.Error("the record does not show the client disconnected")
	}
	if !streamed {
		t.Error("the record does not say it was streamed")
	}
	// The provider's own total never arrived, and the record must say so
	// rather than presenting a count as a measurement.
	if source == "provider" {
		t.Error("usage_source claims provider figures for a cancelled stream")
	}

	// The reservation is closed either way: one left pending would hold
	// budget until the reaper.
	var pending int
	if err := h.db.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM budget_reservations WHERE state = 'pending'`).Scan(&pending); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if pending != 0 {
		t.Errorf("%d reservations left pending after a disconnect", pending)
	}
}

// A provider that stops without [DONE] has truncated the stream, and the
// record must say so while still charging for what was produced.
func TestTruncatedStreamIsRecorded(t *testing.T) {
	h := newHarness(t, "100")
	srv := h.serve(t)

	req := h.streamRequest(t, srv,
		`{"model":"gpt-6-astra","messages":[],"stream":true}`,
		map[string]string{
			fakeprovider.HeaderCompletionTokens: "20",
			fakeprovider.HeaderFailAfterChunks:  "3",
		})
	resp := h.send(t, srv, req)
	defer func() { _ = resp.Body.Close() }()
	body := readStream(t, resp)
	if strings.Contains(body, "[DONE]") {
		t.Error("a truncated upstream stream must not gain a [DONE] it never sent")
	}

	waitForSettle(t, h)
	var errorCode *string
	if err := h.db.Pool().QueryRow(t.Context(),
		`SELECT error_code FROM usage_records`).Scan(&errorCode); err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	if errorCode == nil || *errorCode != "stream_truncated" {
		t.Errorf("error_code = %v, want stream_truncated", errorCode)
	}
}

// A malformed chunk reaches the client: the gateway forwards what it cannot
// parse rather than deciding the chunk is unusable.
func TestMalformedChunkReachesTheClient(t *testing.T) {
	h := newHarness(t, "100")
	srv := h.serve(t)

	req := h.streamRequest(t, srv,
		`{"model":"gpt-6-astra","messages":[],"stream":true}`,
		map[string]string{
			fakeprovider.HeaderCompletionTokens: "5",
			fakeprovider.HeaderMalformedChunkAt: "2",
		})
	resp := h.send(t, srv, req)
	defer func() { _ = resp.Body.Close() }()
	body := readStream(t, resp)

	if !strings.Contains(body, `{"choices":[{"delta":{"content":`) {
		t.Errorf("the malformed chunk did not reach the client:\n%s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Error("the stream should still finish")
	}
}

// A client that did not ask for usage must not receive the chunk the gateway
// injected on its behalf.
func TestInjectedUsageChunkIsNotLeakedToTheClient(t *testing.T) {
	h := newHarness(t, "100")
	srv := h.serve(t)

	req := h.streamRequest(t, srv,
		`{"model":"gpt-6-astra","messages":[],"stream":true}`,
		map[string]string{fakeprovider.HeaderCompletionTokens: "3"})
	resp := h.send(t, srv, req)
	defer func() { _ = resp.Body.Close() }()
	body := readStream(t, resp)

	if strings.Contains(body, "prompt_tokens") {
		t.Errorf("the injected usage chunk reached a client that did not ask:\n%s", body)
	}

	// The gateway still used it: the accounting is exact.
	waitForSettle(t, h)
	var source string
	if err := h.db.Pool().QueryRow(t.Context(),
		`SELECT usage_source FROM usage_records`).Scan(&source); err != nil {
		t.Fatalf("reading: %v", err)
	}
	if source != "provider" {
		t.Errorf("usage_source = %q, want provider", source)
	}
}

// A client that did ask for usage receives it.
func TestRequestedUsageChunkReachesTheClient(t *testing.T) {
	h := newHarness(t, "100")
	srv := h.serve(t)

	req := h.streamRequest(t, srv,
		`{"model":"gpt-6-astra","messages":[],"stream":true,`+
			`"stream_options":{"include_usage":true}}`,
		map[string]string{fakeprovider.HeaderCompletionTokens: "3"})
	resp := h.send(t, srv, req)
	defer func() { _ = resp.Body.Close() }()
	body := readStream(t, resp)

	if !strings.Contains(body, "prompt_tokens") {
		t.Errorf("a client that asked for usage did not receive it:\n%s", body)
	}
}

// Anthropic's events become OpenAI chunks, so an OpenAI client sees the
// dialect it expects.
func TestTranslatedStreamEndToEnd(t *testing.T) {
	h := newHarness(t, "100")
	srv := h.serve(t)

	req := h.streamRequest(t, srv,
		`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		map[string]string{
			fakeprovider.HeaderPromptTokens:     "500",
			fakeprovider.HeaderCompletionTokens: "4",
		})
	resp := h.send(t, srv, req)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	body := readStream(t, resp)

	// The client must see chat.completion.chunk, never Anthropic's own
	// event names.
	if !strings.Contains(body, "chat.completion.chunk") {
		t.Errorf("the stream was not translated:\n%s", body)
	}
	if strings.Contains(body, "content_block_delta") {
		t.Errorf("Anthropic event names leaked to the client:\n%s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Error("a translated stream must be terminated with [DONE]")
	}

	waitForSettle(t, h)
	var spent string
	if err := h.db.Pool().QueryRow(t.Context(),
		`SELECT spent_usd::text FROM key_budgets`).Scan(&spent); err != nil {
		t.Fatalf("reading budget: %v", err)
	}
	// sonnet-5: 500 in at $2, 4 out at $10 per million.
	want := decimal.RequireFromString("0.00104")
	if !decimal.RequireFromString(spent).Equal(want) {
		t.Errorf("spent_usd = %s, want %s", spent, want)
	}
}

func readStream(t *testing.T, resp *http.Response) string {
	t.Helper()
	var b strings.Builder
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for scanner.Scan() {
		b.WriteString(scanner.Text())
		b.WriteString("\n")
	}
	return b.String()
}

// waitForSettle waits for the usage record a stream writes after its last
// chunk, since the client sees the body before the accounting lands.
func waitForSettle(t *testing.T, h *harness) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := h.db.Pool().QueryRow(context.Background(),
			`SELECT count(*) FROM usage_records`).Scan(&n); err == nil && n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no usage record was written within five seconds")
}

// Time to first byte is the number this gateway is judged on, and on a fast
// path it is under a millisecond — which is exactly where rounding to whole
// milliseconds would throw the measurement away.
func TestTimeToFirstByteIsRecorded(t *testing.T) {
	h := newHarness(t, "100")
	srv := h.serve(t)

	req := h.streamRequest(t, srv,
		`{"model":"gpt-6-astra","messages":[],"stream":true}`,
		map[string]string{fakeprovider.HeaderCompletionTokens: "3"})
	resp := h.send(t, srv, req)
	defer func() { _ = resp.Body.Close() }()
	readStream(t, resp)

	waitForSettle(t, h)

	var ttft *int64
	if err := h.db.Pool().QueryRow(t.Context(),
		`SELECT ttft_us FROM usage_records`).Scan(&ttft); err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	if ttft == nil {
		t.Fatal("no time to first byte was recorded")
	}
	if *ttft <= 0 {
		t.Errorf("ttft_us = %d, want a positive figure", *ttft)
	}
	// A local fake answers in well under a second; anything larger means
	// the measurement is not what it claims to be.
	if *ttft > 5_000_000 {
		t.Errorf("ttft_us = %d, implausibly large for a local provider", *ttft)
	}
}

// A non-streaming request has no first byte to time, and must not record a
// figure that would be meaningless.
func TestNonStreamingRecordsNoTTFT(t *testing.T) {
	h := newHarness(t, "100")
	if rec := h.post(t, `{"model":"gpt-6-astra","messages":[]}`, nil); rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}

	var ttft *int64
	if err := h.db.Pool().QueryRow(t.Context(),
		`SELECT ttft_us FROM usage_records`).Scan(&ttft); err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	if ttft != nil {
		t.Errorf("ttft_us = %d on a non-streamed request", *ttft)
	}
}

// Once the headers are out the status cannot change, so a failure has to be
// reported inside the stream. The record keeps status 200 — what the client
// saw — with error_code carrying what actually happened.
func TestErrorAfterHeadersBecomesAnSSEEvent(t *testing.T) {
	h := newHarness(t, "100")
	srv := h.serve(t)

	req := h.streamRequest(t, srv,
		`{"model":"gpt-6-astra","messages":[],"stream":true}`,
		map[string]string{
			fakeprovider.HeaderCompletionTokens: "50",
			fakeprovider.HeaderFailAfterChunks:  "2",
		})
	resp := h.send(t, srv, req)
	defer func() { _ = resp.Body.Close() }()

	// The client already received 200: nothing can change that now.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: headers had already been sent", resp.StatusCode)
	}
	readStream(t, resp)

	waitForSettle(t, h)
	var (
		status    int
		errorCode *string
	)
	if err := h.db.Pool().QueryRow(t.Context(),
		`SELECT status_code, error_code FROM usage_records`).Scan(&status, &errorCode); err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status_code = %d, want the 200 the client saw", status)
	}
	if errorCode == nil {
		t.Fatal("error_code is null: the failure left no trace")
	}
	if *errorCode == "" {
		t.Error("error_code is empty")
	}
}

// A streamed Gemini request, end to end.
//
// Gemini was the provider costlane could not stream in v0.1.0, and its
// protocol differs from the other two in three ways that all touch the
// meter: usage rides cumulatively on every chunk, thinking tokens are billed
// as output but reported apart from it, and the stream ends by closing the
// connection rather than by sending [DONE].
func TestGeminiStreamEndToEnd(t *testing.T) {
	h := newHarness(t, "100")
	srv := h.serve(t)

	req := h.streamRequest(t, srv,
		`{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		map[string]string{
			fakeprovider.HeaderPromptTokens:     "45",
			fakeprovider.HeaderCompletionTokens: "136",
			fakeprovider.HeaderReasoningTokens:  "264",
		})
	resp := h.send(t, srv, req)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	var (
		chunks     int
		sawDone    bool
		completion int64
	)
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if payload == "[DONE]" {
			sawDone = true
			continue
		}
		chunks++
		var parsed struct {
			Object string `json:"object"`
			Usage  *struct {
				CompletionTokens int64 `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
			t.Fatalf("the client received a chunk that is not valid JSON: %q", payload)
		}
		if parsed.Object != "chat.completion.chunk" {
			t.Errorf("chunk object = %q: the client is not seeing the OpenAI dialect",
				parsed.Object)
		}
		if parsed.Usage != nil {
			completion = parsed.Usage.CompletionTokens
		}
	}

	if chunks == 0 {
		t.Fatal("no chunks reached the client")
	}
	// The gateway appends [DONE] for a translated stream, because an OpenAI
	// client waits for one even though Gemini never sends it.
	if !sawDone {
		t.Error("the client never received [DONE]; an OpenAI client would hang")
	}
	// 136 visible plus 264 thought: Google bills thinking as output.
	if completion != 400 {
		t.Errorf("completion_tokens in the usage chunk = %d, want 400", completion)
	}

	// The accounting: exact, from the provider's own figures, and priced
	// without a partially-priced flag even though thinking was involved.
	waitForSettle(t, h)

	var (
		source          string
		errorCode       string
		partiallyPriced bool
		outputTokens    int64
		reasoning       int64
	)
	if err := h.db.Pool().QueryRow(t.Context(), `
		SELECT usage_source, error_code, partially_priced,
		       output_tokens, reasoning_tokens
		  FROM usage_records`).
		Scan(&source, &errorCode, &partiallyPriced, &outputTokens, &reasoning); err != nil {
		t.Fatalf("reading the record: %v", err)
	}

	if source != "provider" {
		t.Errorf("usage_source = %q, want provider", source)
	}
	if errorCode != "" {
		t.Errorf("error_code = %q, want empty: a Gemini stream ends by closing "+
			"the connection after a finishReason, which is a clean close",
			errorCode)
	}
	if partiallyPriced {
		t.Error("partially_priced = true: thinking tokens are a breakdown of " +
			"output, not an unpriced kind of their own")
	}
	if outputTokens != 400 {
		t.Errorf("recorded output = %d, want 400", outputTokens)
	}
	if reasoning != 264 {
		t.Errorf("recorded reasoning = %d, want 264", reasoning)
	}

	assertReconciled(t, h.db)
}
