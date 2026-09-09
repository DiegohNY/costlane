package proxy_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/DiegohNY/costlane/internal/proxy"
)

// The frame reader is a byte-level parser over input from a third party,
// which is precisely what fuzzing is for. The invariants are narrow on
// purpose: it must not panic, and it must not buffer past the cap however
// hostile the input.
func FuzzFrameReader(f *testing.F) {
	seeds := []string{
		"data: hello\n\n",
		"data: [DONE]\n\n",
		"event: content_block_delta\ndata: {\"type\":\"text_delta\"}\n\n",
		"data: {\"broken\":\n\n",
		"data: one\ndata: two\n\n",
		": comment\n\n",
		"\n\n\n\n",
		"data:\n\n",
		"data: \r\n\r\n",
		"id: 1\nretry: 100\ndata: x\n\n",
		strings.Repeat("data: x\n\n", 100),
		"data: " + strings.Repeat("y", 2000),
		"\x00\x01\x02\n\n",
		"data: \xff\xfe invalid utf8\n\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, input string) {
		fr := proxy.NewFrameReader(strings.NewReader(input))
		total := 0
		for {
			frame, err := fr.Next()
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				return
			}

			if len(frame.Raw) > proxy.MaxFrameBytes {
				t.Fatalf("frame of %d bytes exceeds the %d cap",
					len(frame.Raw), proxy.MaxFrameBytes)
			}
			// Data is derived from Raw, so it cannot exceed it.
			if len(frame.Data) > len(frame.Raw) {
				t.Fatalf("data (%d bytes) longer than the frame it came from (%d)",
					len(frame.Data), len(frame.Raw))
			}

			total += len(frame.Raw)
			if total > len(input)+len(input)/2+1024 {
				t.Fatalf("returned %d bytes from %d of input: the reader is inventing data",
					total, len(input))
			}
		}
	})
}
