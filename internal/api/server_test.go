package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DiegohNY/costlane/internal/obs"
	"github.com/DiegohNY/costlane/internal/store"
	"github.com/DiegohNY/costlane/internal/storetest"
)

const (
	masterKey  = "master-key-for-tests-at-least-32ch"
	masterAuth = "Bearer " + masterKey
)

type testServer struct {
	handler http.Handler
}

func newTestServer(t *testing.T) (*testServer, *store.DB) {
	t.Helper()
	db := storetest.NewTestDB(t)
	srv := New(Options{
		DB:        db,
		MasterKey: obs.Secret(masterKey),
	})
	return &testServer{handler: srv.Handler()}, db
}

func (s *testServer) do(t *testing.T, method, path, auth, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

func (s *testServer) createKey(t *testing.T, body string) string {
	t.Helper()
	rec := s.do(t, http.MethodPost, "/admin/keys", masterAuth, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating a key: status %d: %s", rec.Code, rec.Body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return created.ID
}
