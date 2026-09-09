package proxy_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DiegohNY/costlane/internal/provider"
	"github.com/DiegohNY/costlane/internal/proxy"
)

func openAIStream(chunks ...string) string {
	var b strings.Builder
	for _, c := range chunks {
		b.WriteString("data: " + c + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func TestPumpForwardsEveryChunk(t *testing.T) {
	rec := httptest.NewRecorder()
	stream := openAIStream(`{"choices":[{"delta":{"content":"a"}}]}`,
		`{"choices":[{"delta":{"content":"b"}}]}`)

	result, err := proxy.Pump(rec, strings.NewReader(stream), proxy.PumpOptions{
		ClientCtx: t.Context(),
	})
	if err != nil {
		t.Fatalf("Pump: %v", err)
	}
	if result.Chunks != 3 {
		t.Errorf("%d chunks, want 3 including [DONE]", result.Chunks)
	}
	if !result.SawDone {
		t.Error("the stream ended without [DONE]")
	}
	if got := rec.Body.String(); got != stream {
		t.Errorf("the relayed stream differs:\n%q\nwant\n%q", got, stream)
	}
}

// A writer with no Flusher would buffer every chunk and deliver the stream in
// one piece. That degradation is invisible in a test that only checks the
// final body, so it has to fail loudly instead.
func TestPumpRefusesAWriterThatCannotFlush(t *testing.T) {
	_, err := proxy.Pump(unflushableWriter{header: http.Header{}},
		strings.NewReader(openAIStream(`{}`)), proxy.PumpOptions{ClientCtx: t.Context()})
	if err == nil {
		t.Fatal("a writer that cannot flush must be refused, not silently buffered")
	}
	if !strings.Contains(err.Error(), "flush") {
		t.Errorf("the error should name the problem, got: %v", err)
	}
}

// Every wrapper in the production chain must implement Unwrap, or
// ResponseController cannot find the Flusher underneath and streaming
// degrades in silence.
func TestMiddlewareChainPreservesFlushing(t *testing.T) {
	var (
		mu       sync.Mutex
		flushes  int
		received []string
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := range 3 {
			_, _ = w.Write([]byte("data: {\"i\":" + string(rune('0'+i)) + "}\n\n"))
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	// The handler under test, wrapped exactly as production wraps it.
	handler := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp, err := http.Get(upstream.URL) //nolint:bodyclose,noctx // closed below
		if err != nil {
			t.Errorf("calling upstream: %v", err)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := proxy.Pump(w, resp.Body, proxy.PumpOptions{
			ClientCtx: r.Context(), WriteTimeout: 5 * time.Second,
		}); err != nil {
			t.Errorf("Pump: %v", err)
		}
	}))
	handler = proxy.WithRequestID(handler)
	handler = proxy.WithMetrics(handler, nil)

	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL) //nolint:noctx // test client
	if err != nil {
		t.Fatalf("requesting: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read chunk by chunk, recording when each arrives. If the wrappers
	// broke flushing, everything would land at once at the end.
	buf := make([]byte, 256)
	start := time.Now()
	var arrivals []time.Duration
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			mu.Lock()
			received = append(received, string(buf[:n]))
			arrivals = append(arrivals, time.Since(start))
			flushes++
			mu.Unlock()
		}
		if err != nil {
			break
		}
	}

	if len(arrivals) < 2 {
		t.Fatalf("received %d reads, want the chunks to arrive separately: %v",
			len(arrivals), received)
	}
	// The first chunk must arrive well before the last, which only happens
	// if each was flushed as it was written.
	if arrivals[len(arrivals)-1]-arrivals[0] < 20*time.Millisecond {
		t.Errorf("all chunks arrived together (%v): flushing is not reaching the socket",
			arrivals)
	}
}

// The default on disconnect is to cancel, and it must happen within a chunk
// of the client going away rather than eventually.
func TestClientDisconnectCancelsPromptly(t *testing.T) {
	ctx, disconnect := context.WithCancel(context.Background())
	cancelled := make(chan struct{})

	// A stream that keeps producing until someone stops it.
	body := &blockingReader{chunks: 1000, delay: time.Millisecond}

	rec := httptest.NewRecorder()
	go func() {
		time.Sleep(30 * time.Millisecond)
		disconnect()
	}()

	result, err := proxy.Pump(rec, body, proxy.PumpOptions{
		ClientCtx:      ctx,
		CancelUpstream: func() { close(cancelled) },
	})
	if err != nil {
		t.Fatalf("Pump: %v", err)
	}
	if !result.ClientDisconnected {
		t.Fatal("the disconnect was not detected")
	}

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("the upstream call was not cancelled")
	}

	// It must stop promptly, not after draining a thousand chunks.
	if result.Chunks > 100 {
		t.Errorf("%d chunks relayed after the client left; cancellation was not prompt",
			result.Chunks)
	}
}

// Under the drain policy the stream is read to the end, to learn the exact
// usage, at the cost of tokens nobody reads.
func TestDrainPolicyReadsToTheEnd(t *testing.T) {
	ctx, disconnect := context.WithCancel(context.Background())
	disconnect()

	stream := openAIStream(`{"choices":[{"delta":{"content":"a"}}]}`,
		`{"choices":[]," usage":{}}`)
	rec := httptest.NewRecorder()

	result, err := proxy.Pump(rec, strings.NewReader(stream), proxy.PumpOptions{
		ClientCtx:    ctx,
		Drain:        true,
		DrainTimeout: 5 * time.Second,
		DrainSlot:    proxy.NewSemaphore(4),
	})
	if err != nil {
		t.Fatalf("Pump: %v", err)
	}
	if !result.Drained {
		t.Error("the stream was not drained")
	}
	if !result.SawDone {
		t.Error("draining must read through to [DONE]")
	}
	// Nothing reaches a client that has gone.
	if rec.Body.Len() != 0 {
		t.Errorf("wrote %d bytes to a departed client", rec.Body.Len())
	}
}

// A client that opens and abandons many streams must not become many
// upstream streams.
func TestDrainsAreBoundedBySemaphore(t *testing.T) {
	sem := proxy.NewSemaphore(2)

	// Fill the semaphore, so the pump cannot acquire.
	if err := sem.Acquire(t.Context()); err != nil {
		t.Fatalf("acquiring: %v", err)
	}
	if err := sem.Acquire(t.Context()); err != nil {
		t.Fatalf("acquiring: %v", err)
	}
	defer sem.Release()
	defer sem.Release()

	ctx, disconnect := context.WithCancel(context.Background())
	disconnect()
	cancelled := make(chan struct{})

	result, err := proxy.Pump(httptest.NewRecorder(),
		&blockingReader{chunks: 1000, delay: time.Millisecond},
		proxy.PumpOptions{
			ClientCtx:      ctx,
			Drain:          true,
			DrainTimeout:   100 * time.Millisecond,
			DrainSlot:      sem,
			CancelUpstream: func() { close(cancelled) },
		})
	if err != nil {
		t.Fatalf("Pump: %v", err)
	}
	if result.Drained {
		t.Error("the drain proceeded despite the semaphore being full")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("with no drain slot the upstream call must be cancelled")
	}
}

// A reader that stops reading must not hold a stream open indefinitely.
func TestSlowClientHitsTheWriteDeadline(t *testing.T) {
	stream := strings.Repeat("data: {\"x\":1}\n\n", 200)

	// A writer that never accepts a write, standing in for a client whose
	// buffer is full.
	rec := &stallingWriter{ResponseRecorder: httptest.NewRecorder()}

	result, err := proxy.Pump(rec, strings.NewReader(stream), proxy.PumpOptions{
		ClientCtx:    t.Context(),
		WriteTimeout: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Pump: %v", err)
	}
	if !result.ClientDisconnected {
		t.Error("a client that will not accept writes must be treated as gone")
	}
}

// The usage chunk is ours when the client did not ask for it, so removing it
// restores exactly what the client would otherwise have received.
func TestInjectedUsageChunkIsStrippedByShape(t *testing.T) {
	stream := "data: " + `{"choices":[{"delta":{"content":"a"}}]}` + "\n\n" +
		"data: " + `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2}}` + "\n\n" +
		"data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	if _, err := proxy.Pump(rec, strings.NewReader(stream), proxy.PumpOptions{
		ClientCtx: t.Context(), StripUsageChunk: true,
	}); err != nil {
		t.Fatalf("Pump: %v", err)
	}

	body := rec.Body.String()
	if strings.Contains(body, "prompt_tokens") {
		t.Errorf("the usage chunk was not stripped:\n%s", body)
	}
	if !strings.Contains(body, `"content":"a"`) {
		t.Error("content chunks must survive")
	}
	if !strings.Contains(body, "[DONE]") {
		t.Error("[DONE] must survive")
	}
}

// A client that asked for usage gets it.
func TestRequestedUsageChunkIsKept(t *testing.T) {
	stream := "data: " + `{"choices":[],"usage":{"prompt_tokens":10}}` + "\n\n" +
		"data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	if _, err := proxy.Pump(rec, strings.NewReader(stream), proxy.PumpOptions{
		ClientCtx: t.Context(), StripUsageChunk: false,
	}); err != nil {
		t.Fatalf("Pump: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "prompt_tokens") {
		t.Error("a client that asked for usage must receive it")
	}
}

// Stripping must match the shape, not the position: a chunk that happens to
// be last is not necessarily the injected one.
func TestStrippingMatchesShapeNotPosition(t *testing.T) {
	// A final content chunk with no usage must survive stripping.
	stream := "data: " + `{"choices":[{"delta":{"content":"last"}}]}` + "\n\n" +
		"data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	if _, err := proxy.Pump(rec, strings.NewReader(stream), proxy.PumpOptions{
		ClientCtx: t.Context(), StripUsageChunk: true,
	}); err != nil {
		t.Fatalf("Pump: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"content":"last"`) {
		t.Errorf("a content chunk was stripped as though it were usage:\n%s", rec.Body)
	}
}

// A chunk we cannot parse is forwarded anyway: refusing to relay what we do
// not understand would make this a breaking point for every format change.
func TestMalformedChunkIsForwardedAndCounted(t *testing.T) {
	stream := "data: " + `{"choices":[{"delta":{"content":"ok"}}]}` + "\n\n" +
		"data: {\"broken\":\n\n" +
		"data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	result, err := proxy.Pump(rec, strings.NewReader(stream), proxy.PumpOptions{
		ClientCtx: t.Context(),
	})
	if err != nil {
		t.Fatalf("Pump: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `{"broken":`) {
		t.Errorf("the malformed chunk was withheld from the client:\n%s", rec.Body)
	}
	_ = result
}

// A provider that stops without [DONE] has cut the stream short, and the
// result must say so.
func TestStreamWithoutDoneIsReported(t *testing.T) {
	stream := "data: {\"choices\":[]}\n\ndata: {\"choices\":[]}\n\n"

	result, err := proxy.Pump(httptest.NewRecorder(), strings.NewReader(stream),
		proxy.PumpOptions{ClientCtx: t.Context()})
	if err != nil {
		t.Fatalf("Pump: %v", err)
	}
	if result.SawDone {
		t.Error("a stream with no [DONE] must not be reported as complete")
	}
	if result.Chunks != 2 {
		t.Errorf("%d chunks, want 2", result.Chunks)
	}
}

// A translated stream has no [DONE] of its own, and an OpenAI client waits
// for one.
func TestTranslatedStreamGetsADoneSentinel(t *testing.T) {
	stream := "event: message_start\ndata: " +
		`{"type":"message_start","message":{"id":"m","model":"claude-sonnet-5","usage":{"input_tokens":5}}}` +
		"\n\n"

	translator := provider.NewAnthropicStreamTranslator()
	rec := httptest.NewRecorder()
	result, err := proxy.Pump(rec, strings.NewReader(stream), proxy.PumpOptions{
		ClientCtx: t.Context(),
		Translate: translator.Translate,
	})
	if err != nil {
		t.Fatalf("Pump: %v", err)
	}
	if !result.SawDone {
		t.Error("a translated stream must be terminated with [DONE]")
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("the client never received [DONE]:\n%s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"role":"assistant"`) {
		t.Errorf("the translated opening chunk is missing:\n%s", rec.Body)
	}
}

// --- helpers --------------------------------------------------------------

// unflushableWriter implements only the bare ResponseWriter, standing in for
// a middleware wrapper that forgot Unwrap: the controller cannot reach a
// Flusher through it, which is the failure this must catch.
type unflushableWriter struct{ header http.Header }

func (w unflushableWriter) Header() http.Header {
	if w.header == nil {
		return http.Header{}
	}
	return w.header
}
func (w unflushableWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w unflushableWriter) WriteHeader(int)             {}

type stallingWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *stallingWriter) Write(p []byte) (int, error) {
	if !w.deadline.IsZero() && time.Now().After(w.deadline) {
		return 0, context.DeadlineExceeded
	}
	// Simulate a client that never drains its buffer.
	time.Sleep(60 * time.Millisecond)
	return 0, context.DeadlineExceeded
}

func (w *stallingWriter) SetWriteDeadline(t time.Time) error {
	w.deadline = t
	return nil
}

func (w *stallingWriter) FlushError() error { return nil }

// blockingReader emits frames on a delay until it is exhausted.
type blockingReader struct {
	chunks int
	delay  time.Duration
	sent   int
	buf    []byte
}

func (r *blockingReader) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		if r.sent >= r.chunks {
			return 0, context.Canceled
		}
		time.Sleep(r.delay)
		r.sent++
		r.buf = []byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}
