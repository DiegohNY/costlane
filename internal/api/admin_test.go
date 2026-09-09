package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateKeyReturnsTheSecretExactlyOnce(t *testing.T) {
	srv, store := newTestServer(t)

	rec := srv.do(t, http.MethodPost, "/admin/keys", masterAuth, `{"label":"search team"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body)
	}

	var created struct {
		ID     string `json:"id"`
		Key    string `json:"key"`
		Prefix string `json:"key_prefix"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if created.Key == "" {
		t.Fatal("the creation response must carry the secret")
	}
	if !strings.HasPrefix(created.Key, "cl_") {
		t.Errorf("key = %q, want a cl_ prefix", created.Key)
	}

	// A response carrying a credential must not be stored by anything in
	// front of us.
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	// The secret belongs in the body only: a header is copied into far more
	// logs than a body is.
	for name, values := range rec.Header() {
		for _, v := range values {
			if strings.Contains(v, created.Key) {
				t.Errorf("header %s carried the secret", name)
			}
		}
	}

	// Fetching the key again must never return the secret.
	list := srv.do(t, http.MethodGet, "/admin/keys", masterAuth, "")
	if strings.Contains(list.Body.String(), created.Key) {
		t.Error("the listing returned the secret, which must exist only once")
	}
	if !strings.Contains(list.Body.String(), created.Prefix) {
		t.Error("the listing should identify keys by prefix")
	}
	_ = store
}

func TestAdminRoutesRequireTheMasterKey(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, tc := range []struct{ name, auth string }{
		{"no header", ""},
		{"wrong key", "Bearer not-the-master-key-but-long-enough"},
		{"virtual key", "Bearer cl_a-virtual-key-is-not-an-admin-000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := srv.do(t, http.MethodGet, "/admin/keys", tc.auth, "")
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

// A revoked key is gone for authentication but not for history.
func TestRevokeIsIdempotentOverHTTP(t *testing.T) {
	srv, _ := newTestServer(t)
	id := srv.createKey(t, `{"label":"doomed"}`)

	first := srv.do(t, http.MethodDelete, "/admin/keys/"+id, masterAuth, "")
	if first.Code != http.StatusNoContent {
		t.Fatalf("first revoke: status = %d, want 204", first.Code)
	}
	// The caller's intent is already satisfied, so a repeat is a success.
	second := srv.do(t, http.MethodDelete, "/admin/keys/"+id, masterAuth, "")
	if second.Code != http.StatusNoContent {
		t.Errorf("second revoke: status = %d, want 204, not 404", second.Code)
	}
}

func TestRevokingAnUnknownKeyIs404(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := srv.do(t, http.MethodDelete,
		"/admin/keys/00000000-0000-0000-0000-000000000001", masterAuth, "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestMalformedKeyIDIs400(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := srv.do(t, http.MethodDelete, "/admin/keys/not-a-uuid", masterAuth, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// The three intentions must survive the JSON boundary, which is where they
// are easiest to lose.
func TestPatchDistinguishesAbsentNullAndEmpty(t *testing.T) {
	srv, _ := newTestServer(t)
	id := srv.createKey(t, `{"label":"tiered","allowed_models":["gpt-5.6-terra"]}`)

	// Absent: untouched.
	rec := srv.do(t, http.MethodPatch, "/admin/keys/"+id, masterAuth, `{"label":"renamed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Label         string    `json:"label"`
		AllowedModels *[]string `json:"allowed_models"`
	}
	mustDecode(t, rec, &body)
	if body.Label != "renamed" {
		t.Errorf("label = %q, want renamed", body.Label)
	}
	if body.AllowedModels == nil || len(*body.AllowedModels) != 1 {
		t.Errorf("an absent field was modified: %v", body.AllowedModels)
	}

	// Empty: no model allowed.
	rec = srv.do(t, http.MethodPatch, "/admin/keys/"+id, masterAuth, `{"allowed_models":[]}`)
	mustDecode(t, rec, &body)
	if body.AllowedModels == nil || len(*body.AllowedModels) != 0 {
		t.Errorf("allowed_models = %v, want an empty list meaning none", body.AllowedModels)
	}

	// Null: every model allowed.
	rec = srv.do(t, http.MethodPatch, "/admin/keys/"+id, masterAuth, `{"allowed_models":null}`)
	mustDecode(t, rec, &body)
	if body.AllowedModels != nil {
		t.Errorf("allowed_models = %v, want null meaning all models", body.AllowedModels)
	}
}

func TestPatchLimitDistinguishesNullFromZero(t *testing.T) {
	srv, _ := newTestServer(t)
	id := srv.createKey(t, `{"label":"budgeted","limit_usd":"100"}`)

	var body struct {
		LimitUSD *string `json:"limit_usd"`
	}

	rec := srv.do(t, http.MethodPatch, "/admin/keys/"+id, masterAuth, `{"limit_usd":null}`)
	mustDecode(t, rec, &body)
	if body.LimitUSD != nil {
		t.Errorf("limit_usd = %v, want null meaning unlimited", body.LimitUSD)
	}

	rec = srv.do(t, http.MethodPatch, "/admin/keys/"+id, masterAuth, `{"limit_usd":"0"}`)
	mustDecode(t, rec, &body)
	if body.LimitUSD == nil {
		t.Fatal("limit_usd = null, want a limit of 0")
	}
	if !strings.HasPrefix(*body.LimitUSD, "0") {
		t.Errorf("limit_usd = %q, want 0", *body.LimitUSD)
	}
}

func TestMetadataMustBeAFlatStringMap(t *testing.T) {
	srv, _ := newTestServer(t)

	for _, body := range []string{
		`{"label":"x","metadata":{"team":{"nested":"object"}}}`,
		`{"label":"x","metadata":{"count":42}}`,
		`{"label":"x","metadata":{"tags":["a","b"]}}`,
	} {
		t.Run(body, func(t *testing.T) {
			rec := srv.do(t, http.MethodPost, "/admin/keys", masterAuth, body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 for %s", rec.Code, body)
			}
		})
	}
}

func TestLabelIsRequired(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := srv.do(t, http.MethodPost, "/admin/keys", masterAuth, `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: a key without a label cannot be attributed", rec.Code)
	}
}

// No response from any admin route may carry a stored hash: it is enough to
// verify a guess offline.
func TestNoAdminResponseCarriesAHash(t *testing.T) {
	srv, _ := newTestServer(t)
	id := srv.createKey(t, `{"label":"inspected"}`)

	for _, rec := range []*httptest.ResponseRecorder{
		srv.do(t, http.MethodGet, "/admin/keys", masterAuth, ""),
		srv.do(t, http.MethodPatch, "/admin/keys/"+id, masterAuth, `{"label":"still inspected"}`),
	} {
		if strings.Contains(rec.Body.String(), "hash") {
			t.Errorf("a response mentions a hash: %s", rec.Body)
		}
	}
}

func mustDecode(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body, err)
	}
}

var _ = bytes.NewReader
