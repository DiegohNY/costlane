package proxy

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// MaxFrameBytes caps a single SSE frame.
//
// A provider that never sends a blank line would otherwise grow the buffer
// without limit. Past the cap the bytes are handed on raw and the frame is
// counted as a parse error: we forward what we cannot understand, but we do
// not let it exhaust memory.
const MaxFrameBytes = 1 << 20

// Frame is one SSE event as it arrived.
//
// Raw holds the exact bytes, terminator included, so a frame can be relayed
// without re-serialising it. Nothing here re-encodes: a proxy that rewrites
// what it forwards is a proxy whose output cannot be trusted to match its
// input.
type Frame struct {
	Raw []byte
	// Event is the SSE event name, which Anthropic uses and OpenAI does not.
	Event []byte
	// Data is the payload of the data: field, without the prefix.
	Data []byte
	// Oversized reports a frame that exceeded the cap and was cut.
	Oversized bool
}

// IsDone reports the OpenAI end-of-stream sentinel.
func (f Frame) IsDone() bool { return bytes.Equal(bytes.TrimSpace(f.Data), []byte("[DONE]")) }

// FrameReader splits a stream into SSE frames.
//
// It is deliberately not a full SSE implementation: it recognises the frame
// boundary and the two fields that matter, and treats everything else as
// bytes to pass along.
type FrameReader struct {
	reader *bufio.Reader
	buf    bytes.Buffer
}

// NewFrameReader reads frames from r.
func NewFrameReader(r io.Reader) *FrameReader {
	return &FrameReader{reader: bufio.NewReaderSize(r, 16<<10)}
}

// ErrFrameTooLarge reports a frame that hit the cap. The frame is still
// returned, so the caller can forward it.
var ErrFrameTooLarge = errors.New("proxy: SSE frame exceeded the size cap")

// Next returns the next frame.
//
// A frame ends at a blank line. Both \n\n and \r\n\r\n are accepted, because
// providers differ and a stream that works against one must not break against
// another.
func (fr *FrameReader) Next() (Frame, error) {
	fr.buf.Reset()

	for {
		line, err := fr.readLine()
		if err != nil {
			if fr.buf.Len() > 0 {
				// A final frame without its terminator is still a frame:
				// dropping it would lose the last chunk of every stream a
				// provider ends abruptly.
				return fr.frame(false), nil
			}
			return Frame{}, err
		}

		if fr.buf.Len()+len(line) > MaxFrameBytes {
			fr.buf.Write(line[:min(len(line), MaxFrameBytes-fr.buf.Len())])
			return fr.frame(true), nil
		}
		fr.buf.Write(line)

		if isBlankLine(line) && fr.buf.Len() > len(line) {
			return fr.frame(false), nil
		}
	}
}

func (fr *FrameReader) readLine() ([]byte, error) {
	line, err := fr.reader.ReadBytes('\n')
	if len(line) > 0 {
		return line, nil
	}
	return nil, err
}

func (fr *FrameReader) frame(oversized bool) Frame {
	raw := make([]byte, fr.buf.Len())
	copy(raw, fr.buf.Bytes())

	f := Frame{Raw: raw, Oversized: oversized}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		switch {
		case bytes.HasPrefix(line, []byte("data:")):
			value := bytes.TrimPrefix(line, []byte("data:"))
			value = bytes.TrimPrefix(value, []byte(" "))
			if f.Data == nil {
				f.Data = value
			} else {
				// A multi-line data field is joined with newlines, per the
				// SSE specification.
				f.Data = append(append(f.Data, '\n'), value...)
			}
		case bytes.HasPrefix(line, []byte("event:")):
			value := bytes.TrimPrefix(line, []byte("event:"))
			f.Event = bytes.TrimSpace(value)
		}
	}
	return f
}

func isBlankLine(line []byte) bool {
	trimmed := bytes.TrimRight(line, "\r\n")
	return len(trimmed) == 0
}
