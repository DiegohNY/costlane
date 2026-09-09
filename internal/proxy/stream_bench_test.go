package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DiegohNY/costlane/internal/proxy"
)

func benchStream(chunks int) string {
	var b strings.Builder
	for range chunks {
		b.WriteString(`data: {"choices":[{"delta":{"content":"token "}}]}` + "\n\n")
	}
	b.WriteString(`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50}}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// BenchmarkStreamPassthrough measures the pump on the path where the dialect
// already matches: read a frame, count it, write it. This is the per-chunk
// cost the product advertises.
func BenchmarkStreamPassthrough(b *testing.B) {
	stream := benchStream(100)
	w := &discardWriter{header: http.Header{}}

	b.ResetTimer()
	b.SetBytes(int64(len(stream)))
	for b.Loop() {
		if _, err := proxy.Pump(w, strings.NewReader(stream), proxy.PumpOptions{
			ClientCtx: b.Context(),
		}); err != nil {
			b.Fatalf("Pump: %v", err)
		}
	}
}

// BenchmarkStreamWithUsageStripping adds the shape check that finds the
// injected usage chunk, which runs against every frame.
func BenchmarkStreamWithUsageStripping(b *testing.B) {
	stream := benchStream(100)
	w := &discardWriter{header: http.Header{}}

	b.ResetTimer()
	b.SetBytes(int64(len(stream)))
	for b.Loop() {
		if _, err := proxy.Pump(w, strings.NewReader(stream), proxy.PumpOptions{
			ClientCtx: b.Context(), StripUsageChunk: true,
		}); err != nil {
			b.Fatalf("Pump: %v", err)
		}
	}
}

// BenchmarkFrameReader isolates the parser from the writing, so a regression
// can be attributed to one or the other.
func BenchmarkFrameReader(b *testing.B) {
	stream := benchStream(100)

	b.ResetTimer()
	b.SetBytes(int64(len(stream)))
	for b.Loop() {
		fr := proxy.NewFrameReader(strings.NewReader(stream))
		for {
			if _, err := fr.Next(); err != nil {
				break
			}
		}
	}
}

// discardWriter accepts writes without doing anything with them, so the
// benchmark measures the pump rather than a socket.
type discardWriter struct {
	header http.Header
	status int
}

func (w *discardWriter) Header() http.Header         { return w.header }
func (w *discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *discardWriter) WriteHeader(status int)      { w.status = status }
func (w *discardWriter) Flush()                      {}

var _ io.Writer = (*discardWriter)(nil)
var _ http.Flusher = (*discardWriter)(nil)
var _ = httptest.NewRecorder
