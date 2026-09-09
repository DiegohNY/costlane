package proxy

import (
	"io"
	"net/http"
)

// readBody reads a request body under a size cap.
//
// An unbounded read is an easy way to exhaust memory, and a gateway is a
// tempting place to try it: the body is forwarded whole, so whatever arrives
// is held at least twice.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	return io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
}
