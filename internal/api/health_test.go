package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DiegohNY/costlane/internal/api"
)

type stubPinger struct{ err error }

func (p stubPinger) PingRead(context.Context) error { return p.err }

func probe(t *testing.T, handler http.HandlerFunc) int {
	t.Helper()
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return rec.Code
}

// Liveness answers only whether the process is running. Putting the database
// in it would restart every pod when the database went away, turning a
// recoverable outage into a crash loop.
func TestLivenessIgnoresDependencies(t *testing.T) {
	h := api.NewHealth(stubPinger{err: errors.New("database is gone")},
		func() bool { return false })

	if got := probe(t, h.Live); got != http.StatusOK {
		t.Errorf("liveness = %d with every dependency down, want 200", got)
	}
}

// Readiness answers whether this process can serve a request now.
func TestReadinessRequiresItsDependencies(t *testing.T) {
	t.Run("everything up", func(t *testing.T) {
		h := api.NewHealth(stubPinger{}, func() bool { return true })
		if got := probe(t, h.Ready); got != http.StatusOK {
			t.Errorf("readiness = %d, want 200", got)
		}
	})

	t.Run("database unreachable", func(t *testing.T) {
		h := api.NewHealth(stubPinger{err: errors.New("connection refused")},
			func() bool { return true })
		if got := probe(t, h.Ready); got != http.StatusServiceUnavailable {
			t.Errorf("readiness = %d, want 503", got)
		}
	})

	// A gateway that cannot price a request has no business accepting one:
	// it would meter nothing.
	t.Run("prices not loaded", func(t *testing.T) {
		h := api.NewHealth(stubPinger{}, func() bool { return false })
		if got := probe(t, h.Ready); got != http.StatusServiceUnavailable {
			t.Errorf("readiness = %d, want 503 with no price table", got)
		}
	})
}

// Readiness fails the moment shutdown begins, before anything stops
// accepting, so a load balancer stops routing here while requests can still
// be served.
func TestReadinessFailsAsSoonAsDrainingBegins(t *testing.T) {
	h := api.NewHealth(stubPinger{}, func() bool { return true })

	if got := probe(t, h.Ready); got != http.StatusOK {
		t.Fatalf("readiness = %d before shutdown, want 200", got)
	}

	h.BeginDraining()

	if got := probe(t, h.Ready); got != http.StatusServiceUnavailable {
		t.Errorf("readiness = %d after BeginDraining, want 503", got)
	}
	// Liveness stays up: the process is still working, it just should not
	// receive new traffic.
	if got := probe(t, h.Live); got != http.StatusOK {
		t.Errorf("liveness = %d while draining, want 200", got)
	}
	if !h.Draining() {
		t.Error("Draining() does not report the state")
	}
}

// The probes carry no credential, because a load balancer has none — and
// neither reveals anything worth protecting.
func TestProbesNeedNoCredential(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		t.Run(path, func(t *testing.T) {
			rec := srv.do(t, http.MethodGet, path, "", "")
			if rec.Code == http.StatusUnauthorized {
				t.Errorf("%s demanded a credential", path)
			}
		})
	}
}

// The build that answered is the first question of every incident, and
// asking it must not require a credential. Liveness is the one probe that
// stays up regardless of dependencies, so the version rides on that.
func TestLivenessReportsTheBuildVersion(t *testing.T) {
	h := api.NewHealth(stubPinger{}, func() bool { return true })
	h.Version = "v9.9.9"

	rec := httptest.NewRecorder()
	h.Live(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("liveness body is not JSON (%q): %v", rec.Body.String(), err)
	}
	if body.Status != "ok" || body.Version != "v9.9.9" {
		t.Errorf("liveness = %+v, want status ok and version v9.9.9", body)
	}
}

// An unstamped binary is a local build. Reporting "dev" says so; an empty
// string reads like a broken probe.
func TestLivenessReportsDevWhenUnstamped(t *testing.T) {
	h := api.NewHealth(stubPinger{}, func() bool { return true })

	rec := httptest.NewRecorder()
	h.Live(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if !strings.Contains(rec.Body.String(), `"version":"dev"`) {
		t.Errorf("unstamped liveness body = %q, want version dev", rec.Body.String())
	}
}
