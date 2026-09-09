package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DiegohNY/costlane/internal/obs"
)

// The defect found in a surveyed project: credentials accepted from the
// query string, where every proxy, browser history and access log records
// them. Rejected here, and the value is neither logged nor echoed.
func TestCredentialsInQueryStringAreRejected(t *testing.T) {
	const leaked = "cl_secret-that-must-not-be-echoed"

	for _, param := range []string{"api_key", "key", "token", "access_token", "apikey"} {
		t.Run(param, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost,
				"/v1/chat/completions?"+param+"="+leaked, nil)
			rec := httptest.NewRecorder()

			RejectQueryCredentials(noopHandler()).ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, "Authorization header") {
				t.Errorf("the response should say where credentials belong, got: %s", body)
			}
			// Echoing the value back would put it in the client's own logs.
			if strings.Contains(body, leaked) {
				t.Errorf("the response echoed the credential: %s", body)
			}
			if strings.Contains(strings.Join(rec.Header().Values("Location"), ""), leaked) {
				t.Error("the credential reached a response header")
			}
		})
	}
}

// An ordinary query string must pass through untouched.
func TestHarmlessQueryParametersPassThrough(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/usage?group_by=model&limit=50", nil)
	rec := httptest.NewRecorder()

	RejectQueryCredentials(noopHandler()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200: ordinary parameters are not credentials", rec.Code)
	}
}

// The check is on the parameter name, so case and surrounding parameters
// must not let one slip past.
func TestQueryCredentialDetectionIsCaseInsensitive(t *testing.T) {
	for _, raw := range []string{
		"/v1/usage?API_KEY=cl_x",
		"/v1/usage?Api_Key=cl_x",
		"/v1/usage?group_by=model&apiKey=cl_x",
	} {
		t.Run(raw, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, raw, nil)
			rec := httptest.NewRecorder()
			RejectQueryCredentials(noopHandler()).ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 for %q", rec.Code, raw)
			}
		})
	}
}

// --- bearer extraction ----------------------------------------------------

func TestBearerTokenIsExtracted(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer cl_the-key")

	got, ok := BearerToken(req)
	if !ok {
		t.Fatal("a well-formed bearer header must be accepted")
	}
	if got.Expose() != "cl_the-key" {
		t.Errorf("token = %q, want cl_the-key", got.Expose())
	}
	// The extracted token is a Secret, so it cannot be logged by accident.
	if strings.Contains(got.String(), "cl_the-key") {
		t.Error("the extracted token renders its value")
	}
}

func TestMalformedAuthorizationHeadersAreRejected(t *testing.T) {
	for _, header := range []string{
		"",
		"cl_the-key",         // no scheme
		"Basic dXNlcjpwYXNz", // wrong scheme
		"Bearer",             // no value
		"Bearer ",
	} {
		t.Run(header, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if header != "" {
				req.Header.Set("Authorization", header)
			}
			if _, ok := BearerToken(req); ok {
				t.Errorf("header %q must not yield a token", header)
			}
		})
	}
}

// The scheme is case-insensitive per RFC 7235, and clients do vary.
func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", scheme+" cl_the-key")
		if _, ok := BearerToken(req); !ok {
			t.Errorf("scheme %q must be accepted", scheme)
		}
	}
}

func noopHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

var _ = obs.Redacted
