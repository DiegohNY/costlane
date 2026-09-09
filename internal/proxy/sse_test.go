package proxy_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/DiegohNY/costlane/internal/proxy"
)

func readAllFrames(t *testing.T, input string) []proxy.Frame {
	t.Helper()
	fr := proxy.NewFrameReader(strings.NewReader(input))
	var frames []proxy.Frame
	for {
		f, err := fr.Next()
		if errors.Is(err, io.EOF) {
			return frames
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		frames = append(frames, f)
	}
}

func TestFramesSplitOnBlankLines(t *testing.T) {
	frames := readAllFrames(t, "data: one\n\ndata: two\n\ndata: [DONE]\n\n")

	if len(frames) != 3 {
		t.Fatalf("%d frames, want 3", len(frames))
	}
	if string(frames[0].Data) != "one" || string(frames[1].Data) != "two" {
		t.Errorf("payloads: %q, %q", frames[0].Data, frames[1].Data)
	}
	if !frames[2].IsDone() {
		t.Error("the last frame should be recognised as [DONE]")
	}
}

// Providers differ on line endings, and a stream that works against one must
// not break against another.
func TestCarriageReturnsAreAccepted(t *testing.T) {
	frames := readAllFrames(t, "data: one\r\n\r\ndata: two\r\n\r\n")
	if len(frames) != 2 {
		t.Fatalf("%d frames, want 2", len(frames))
	}
	if string(frames[0].Data) != "one" {
		t.Errorf("payload = %q, want one", frames[0].Data)
	}
}

// Anthropic names its events; OpenAI does not. Both must parse.
func TestNamedEventsAreParsed(t *testing.T) {
	frames := readAllFrames(t,
		"event: content_block_delta\ndata: {\"type\":\"x\"}\n\n")
	if len(frames) != 1 {
		t.Fatalf("%d frames, want 1", len(frames))
	}
	if string(frames[0].Event) != "content_block_delta" {
		t.Errorf("event = %q", frames[0].Event)
	}
	if string(frames[0].Data) != `{"type":"x"}` {
		t.Errorf("data = %q", frames[0].Data)
	}
}

// A frame arriving in pieces across several reads must reassemble: the
// network decides where the boundaries fall, not the provider.
func TestFramesSplitAcrossReadsReassemble(t *testing.T) {
	fr := proxy.NewFrameReader(&trickleReader{data: []byte("data: hello\n\ndata: world\n\n")})
	first, err := fr.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if string(first.Data) != "hello" {
		t.Errorf("data = %q, want hello", first.Data)
	}
	second, err := fr.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if string(second.Data) != "world" {
		t.Errorf("data = %q, want world", second.Data)
	}
}

// A provider that ends without a blank line still sent that last chunk, and
// dropping it would lose the end of every abruptly closed stream.
func TestUnterminatedFinalFrameIsReturned(t *testing.T) {
	frames := readAllFrames(t, "data: one\n\ndata: truncated")
	if len(frames) != 2 {
		t.Fatalf("%d frames, want 2", len(frames))
	}
	if string(frames[1].Data) != "truncated" {
		t.Errorf("final payload = %q", frames[1].Data)
	}
}

// The cap is what stops a provider that never sends a terminator from
// growing the buffer without limit.
func TestOversizedFrameIsCappedNotUnbounded(t *testing.T) {
	huge := "data: " + strings.Repeat("x", proxy.MaxFrameBytes*2)
	frames := readAllFrames(t, huge)

	if len(frames) == 0 {
		t.Fatal("no frames returned")
	}
	if !frames[0].Oversized {
		t.Error("the frame must be reported as oversized")
	}
	if len(frames[0].Raw) > proxy.MaxFrameBytes {
		t.Errorf("frame is %d bytes, over the %d cap", len(frames[0].Raw), proxy.MaxFrameBytes)
	}
}

// Malformed JSON is still a frame. The gateway forwards what it cannot
// parse, because deciding on a client's behalf that a chunk is unusable
// makes this a breaking point for every future format change.
func TestMalformedPayloadIsStillAFrame(t *testing.T) {
	frames := readAllFrames(t, "data: {\"broken\":\n\ndata: [DONE]\n\n")
	if len(frames) != 2 {
		t.Fatalf("%d frames, want 2", len(frames))
	}
	if string(frames[0].Data) != `{"broken":` {
		t.Errorf("data = %q, want the malformed payload intact", frames[0].Data)
	}
}

func TestMultiLineDataIsJoined(t *testing.T) {
	frames := readAllFrames(t, "data: first\ndata: second\n\n")
	if len(frames) != 1 {
		t.Fatalf("%d frames, want 1", len(frames))
	}
	if string(frames[0].Data) != "first\nsecond" {
		t.Errorf("data = %q, want the lines joined", frames[0].Data)
	}
}

func TestCommentsAndUnknownFieldsPassThrough(t *testing.T) {
	frames := readAllFrames(t, ": keep-alive comment\n\nid: 42\ndata: payload\n\n")
	if len(frames) != 2 {
		t.Fatalf("%d frames, want 2", len(frames))
	}
	// The comment frame carries no data but is still relayed verbatim.
	if len(frames[0].Data) != 0 {
		t.Errorf("a comment frame should have no data, got %q", frames[0].Data)
	}
	if !strings.Contains(string(frames[0].Raw), "keep-alive") {
		t.Error("the comment must survive in the raw bytes")
	}
	if string(frames[1].Data) != "payload" {
		t.Errorf("data = %q", frames[1].Data)
	}
}

// trickleReader hands over one byte at a time, so frames necessarily span
// several reads.
type trickleReader struct {
	data []byte
	pos  int
}

func (r *trickleReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}
