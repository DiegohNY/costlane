package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DiegohNY/costlane/internal/obs"
	"github.com/DiegohNY/costlane/internal/store"
	"github.com/DiegohNY/costlane/internal/storetest"
	"github.com/google/uuid"

	"github.com/DiegohNY/costlane/internal/api"
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
	srv := api.New(api.Options{
		DB:             db,
		MasterKey:      obs.Secret(masterKey),
		MaxQueryWindow: 90 * 24 * time.Hour,
		Health:         api.NewHealth(db, func() bool { return true }),
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

// createKeyWithSecret returns both halves, for tests that need to call as
// the key rather than about it.
func (s *testServer) createKeyWithSecret(t *testing.T, body string) (secret string, id uuid.UUID) {
	t.Helper()
	rec := s.do(t, http.MethodPost, "/admin/keys", masterAuth, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating a key: status %d: %s", rec.Code, rec.Body)
	}
	var created struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return created.Key, uuid.MustParse(created.ID)
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
