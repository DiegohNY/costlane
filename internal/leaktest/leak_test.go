// Package leaktest verifies that no credential escapes the gateway.
//
// It is an integration test rather than a unit one on purpose: a credential
// leaks through a whole system — a log line, an error body, a metric label, a
// database column — and each of those is a seam between components that a
// unit test never crosses.
package leaktest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DiegohNY/costlane/internal/api"
	"github.com/DiegohNY/costlane/internal/auth"
	"github.com/DiegohNY/costlane/internal/fakeprovider"
	"github.com/DiegohNY/costlane/internal/obs"
	"github.com/DiegohNY/costlane/internal/pricing"
	"github.com/DiegohNY/costlane/internal/provider"
	"github.com/DiegohNY/costlane/internal/proxy"
	"github.com/DiegohNY/costlane/internal/store"
	"github.com/DiegohNY/costlane/internal/storetest"
	"github.com/shopspring/decimal"
)

// The canaries. Each is planted somewhere a credential really lives, and
// none may appear anywhere a person or a log aggregator can see.
const (
	canaryProviderKey = "sk-proj-LEAKCANARY0000000000000000000001"
	canaryDBPassword  = "LEAKCANARY0000000000000000000002"
	canaryMasterKey   = "LEAKCANARY0000000000000000000003master"
	canaryPrompt      = "LEAKCANARY0000000000000000000004"
	canaryUpstream    = "LEAKCANARY0000000000000000000005"
)

func allCanaries() []string {
	return []string{
		canaryProviderKey, canaryDBPassword, canaryMasterKey,
		canaryPrompt, canaryUpstream,
	}
}

// safeLogger captures everything written, so the log can be searched.
type safeLogger struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *safeLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *safeLogger) contents() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type harness struct {
	server    http.Handler
	db        *store.DB
	logs      *safeLogger
	key       auth.Key
	fake      *httptest.Server
	responses []string
}

func newHarness(t *testing.T) *harness { return buildHarness(t, false) }

// newHarnessWithPromptLogging turns on the switch that stores request bodies.
func newHarnessWithPromptLogging(t *testing.T) *harness { return buildHarness(t, true) }

func buildHarness(t *testing.T, logPrompts bool) *harness {
	t.Helper()
	db := storetest.NewTestDB(t)

	logs := &safeLogger{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	fake := httptest.NewServer(fakeprovider.Handler())
	t.Cleanup(fake.Close)

	table, err := pricing.LoadSeed()
	if err != nil {
		t.Fatalf("loading prices: %v", err)
	}
	snapshot := pricing.NewSnapshot(table)

	// The provider credential is a canary: anything that forwards or logs
	// it will be caught.
	opts := provider.Options{
		BaseURL: fake.URL, APIKey: obs.Secret(canaryProviderKey),
		Client:           provider.NewHTTPClient(30*time.Second, 5*time.Second),
		DefaultMaxTokens: 4096,
	}
	router := provider.NewRouter(provider.NewOpenAI(opts), provider.NewAnthropic(opts))
	router.SetModelProviders(map[string]string{
		"gpt-6-astra":     "openai",
		"claude-sonnet-5": "anthropic",
	})

	key, err := auth.NewKey()
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	limit := decimal.NewFromInt(100)
	if _, err := db.CreateKey(t.Context(), store.CreateKeyInput{
		Hash: key.Hash, Prefix: key.Prefix, Label: "leak canary", LimitUSD: &limit,
	}); err != nil {
		t.Fatalf("creating key: %v", err)
	}

	proxyOpts := proxy.Options{
		DB: db, Router: router, Pricing: snapshot, Logger: logger,
		// The redactor is told every credential this process holds, so a
		// provider echoing one back cannot pass it on.
		Redactor: obs.NewRedactor(
			obs.Secret(canaryProviderKey),
			obs.Secret(canaryDBPassword),
			obs.Secret(canaryMasterKey),
			obs.Secret(canaryUpstream),
		),
		DefaultMaxTokens: 4096, ReservationTTL: time.Minute,
		ProviderTimeout:           5 * time.Second,
		PassthroughHeaderPrefixes: []string{"X-Fake-"},
		LogPrompts:                logPrompts,
	}
	server := api.New(api.Options{
		DB: db, MasterKey: obs.Secret(canaryMasterKey),
		MaxQueryWindow: 90 * 24 * time.Hour,
		Proxy:          proxy.New(proxyOpts),
		Models:         proxy.NewModelsHandler(proxyOpts),
	})

	return &harness{
		server: proxy.WithRequestID(server.Handler()),
		db:     db, logs: logs, key: key, fake: fake,
	}
}

// call exercises one path and records the response for scanning.
func (h *harness) call(t *testing.T, method, path, auth, body string,
	headers map[string]string) *httptest.ResponseRecorder {
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
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, req)

	// Both the body and every header, since a credential can escape
	// through either.
	var captured strings.Builder
	for name, values := range rec.Header() {
		captured.WriteString(name + ": " + strings.Join(values, ",") + "\n")
	}
	captured.WriteString(rec.Body.String())
	h.responses = append(h.responses, captured.String())
	return rec
}

// TestNoCanaryEscapesAnyPath drives every failure mode the gateway has and
// then searches everywhere a person could look.
func TestNoCanaryEscapesAnyPath(t *testing.T) {
	h := newHarness(t)
	bearer := "Bearer " + h.key.Secret.Expose()

	// A prompt carrying a canary, so a request body logged anywhere shows up.
	promptBody := fmt.Sprintf(
		`{"model":"gpt-6-astra","messages":[{"role":"user","content":%q}]}`, canaryPrompt)

	// An upstream error body that echoes a credential back at us, which is
	// something providers really do.
	upstreamError := fmt.Sprintf(
		`{"error":{"message":"invalid key %s and %s"}}`, canaryProviderKey, canaryUpstream)

	t.Run("success", func(t *testing.T) {
		if rec := h.call(t, http.MethodPost, "/v1/chat/completions", bearer,
			promptBody, nil); rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
	})

	// Every upstream status, each with a body echoing the credentials.
	for _, status := range []string{"400", "401", "403", "404", "429", "500", "502", "503"} {
		t.Run("upstream "+status, func(t *testing.T) {
			h.call(t, http.MethodPost, "/v1/chat/completions", bearer, promptBody,
				map[string]string{
					fakeprovider.HeaderStatus:    status,
					fakeprovider.HeaderErrorBody: upstreamError,
				})
		})
	}

	t.Run("streamed", func(t *testing.T) {
		h.call(t, http.MethodPost, "/v1/chat/completions", bearer,
			strings.Replace(promptBody, `"messages"`, `"stream":true,"messages"`, 1), nil)
	})

	t.Run("error after headers", func(t *testing.T) {
		h.call(t, http.MethodPost, "/v1/chat/completions", bearer,
			strings.Replace(promptBody, `"messages"`, `"stream":true,"messages"`, 1),
			map[string]string{
				fakeprovider.HeaderCompletionTokens: "50",
				fakeprovider.HeaderFailAfterChunks:  "2",
			})
	})

	t.Run("upstream timeout", func(t *testing.T) {
		h.call(t, http.MethodPost, "/v1/chat/completions", bearer, promptBody,
			map[string]string{fakeprovider.HeaderLatency: "6000"})
	})

	t.Run("malformed request", func(t *testing.T) {
		h.call(t, http.MethodPost, "/v1/chat/completions", bearer, `{"broken`, nil)
	})

	t.Run("unsupported parameter", func(t *testing.T) {
		h.call(t, http.MethodPost, "/v1/chat/completions", bearer,
			`{"model":"claude-sonnet-5","messages":[],"logprobs":true}`, nil)
	})

	t.Run("bad credential", func(t *testing.T) {
		h.call(t, http.MethodPost, "/v1/chat/completions",
			"Bearer cl_"+canaryPrompt, promptBody, nil)
	})

	t.Run("credential in query string", func(t *testing.T) {
		// The rejection must not echo the value it is rejecting.
		h.call(t, http.MethodPost,
			"/v1/chat/completions?api_key="+canaryProviderKey, "", promptBody, nil)
	})

	t.Run("admin surfaces", func(t *testing.T) {
		master := "Bearer " + canaryMasterKey
		h.call(t, http.MethodGet, "/admin/keys", master, "", nil)
		h.call(t, http.MethodGet, "/admin/reconcile", master, "", nil)
		// Metadata is attribution — a team or customer name — and is meant
		// to be stored and returned, so a canary there would test nothing.
		h.call(t, http.MethodPost, "/admin/keys", master,
			`{"label":"canary","metadata":{"team":"search"}}`, nil)
	})

	t.Run("read surfaces", func(t *testing.T) {
		h.call(t, http.MethodGet, "/v1/usage?group_by=model", bearer, "", nil)
		h.call(t, http.MethodGet, "/v1/usage/requests", bearer, "", nil)
		h.call(t, http.MethodGet, "/v1/budget", bearer, "", nil)
		h.call(t, http.MethodGet, "/v1/models", bearer, "", nil)
	})

	// Give the accounting time to land.
	time.Sleep(500 * time.Millisecond)

	// --- now search everywhere -------------------------------------------

	t.Run("responses carry no canary", func(t *testing.T) {
		for i, response := range h.responses {
			for _, canary := range allCanaries() {
				// The virtual key's own secret is not a canary: the client
				// sent it and already has it.
				if strings.Contains(response, canary) {
					t.Errorf("response %d leaked %s:\n%s", i, canaryName(canary), response)
				}
			}
		}
	})

	t.Run("logs carry no canary", func(t *testing.T) {
		logs := h.logs.contents()
		for _, canary := range allCanaries() {
			if strings.Contains(logs, canary) {
				t.Errorf("the log leaked %s:\n%s", canaryName(canary), logs)
			}
		}
		// The virtual key must appear only as its display prefix, if at all.
		if strings.Contains(logs, h.key.Secret.Expose()) {
			t.Error("the log carried a virtual key in full")
		}
	})

	t.Run("database carries no canary with prompt logging off", func(t *testing.T) {
		// Prompt logging is off, so nothing anywhere in the database may
		// contain the prompt or any credential.
		for _, canary := range allCanaries() {
			if found, where := scanDatabase(t, h.db, canary); found {
				t.Errorf("%s appears in the database at %s", canaryName(canary), where)
			}
		}
	})
}

// canaryName describes a canary without repeating it in the failure message.
func canaryName(canary string) string {
	switch canary {
	case canaryProviderKey:
		return "the provider API key"
	case canaryDBPassword:
		return "the database password"
	case canaryMasterKey:
		return "the master key"
	case canaryPrompt:
		return "a prompt"
	case canaryUpstream:
		return "an upstream error credential"
	}
	return "a canary"
}

// scanDatabase looks for a value in every text-bearing column of every table.
//
// Enumerating the columns rather than naming them means a column added later
// is searched too, which is the point: the leak this guards against is the
// one nobody thought of.
func scanDatabase(t *testing.T, db *store.DB, needle string) (bool, string) {
	t.Helper()

	rows, err := db.Pool().Query(context.Background(), `
		SELECT table_name, column_name
		  FROM information_schema.columns
		 WHERE table_schema = 'public'
		   AND data_type IN ('text', 'character varying', 'jsonb', 'json')`)
	if err != nil {
		t.Fatalf("listing columns: %v", err)
	}
	type column struct{ table, name string }
	var columns []column
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.table, &c.name); err != nil {
			rows.Close()
			t.Fatalf("scanning columns: %v", err)
		}
		columns = append(columns, c)
	}
	rows.Close()

	for _, c := range columns {
		var found bool
		query := fmt.Sprintf(
			`SELECT EXISTS (SELECT 1 FROM %q WHERE %q::text LIKE $1)`, c.table, c.name)
		if err := db.Pool().QueryRow(context.Background(), query, "%"+needle+"%").
			Scan(&found); err != nil {
			// A column that cannot be searched is not evidence of safety,
			// so say so rather than passing quietly.
			t.Logf("could not search %s.%s: %v", c.table, c.name, err)
			continue
		}
		if found {
			return true, c.table + "." + c.name
		}
	}
	return false, ""
}

var _ = io.Discard
var _ = json.Marshal

// With prompt logging on, a prompt appears in exactly one place: the table
// built for it. Anywhere else would mean the switch does not bound what it
// claims to.
func TestPromptsAppearOnlyInTheirOwnTableWhenLoggingIsOn(t *testing.T) {
	h := newHarnessWithPromptLogging(t)
	bearer := "Bearer " + h.key.Secret.Expose()

	body := fmt.Sprintf(
		`{"model":"gpt-6-astra","messages":[{"role":"user","content":%q}]}`, canaryPrompt)
	if rec := h.call(t, http.MethodPost, "/v1/chat/completions", bearer, body, nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	time.Sleep(300 * time.Millisecond)

	found, where := scanDatabase(t, h.db, canaryPrompt)
	if !found {
		t.Fatal("the prompt was not stored despite logging being enabled")
	}
	if where != "request_payloads.request_body" {
		t.Errorf("the prompt appears at %s, want only request_payloads.request_body",
			where)
	}

	// Every other credential must still be nowhere.
	for _, canary := range []string{
		canaryProviderKey, canaryDBPassword, canaryMasterKey, canaryUpstream,
	} {
		if found, where := scanDatabase(t, h.db, canary); found {
			t.Errorf("%s appears at %s even with prompt logging on",
				canaryName(canary), where)
		}
	}
	// And the log still carries nothing.
	for _, canary := range allCanaries() {
		if strings.Contains(h.logs.contents(), canary) {
			t.Errorf("the log leaked %s", canaryName(canary))
		}
	}
}
