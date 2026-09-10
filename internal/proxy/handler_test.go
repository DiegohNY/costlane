package proxy_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

type harness struct {
	handler http.Handler
	db      *store.DB
	fake    *httptest.Server
	key     auth.Key
}

func newHarness(t testing.TB, limit string) *harness {
	t.Helper()
	return newHarnessOn(t, limit, storetest.NewTestDB(t))
}

// newHarnessOn builds the same gateway over a database the caller supplies,
// so a test that needs an instrumented pool does not have to reproduce the
// wiring.
func newHarnessOn(t testing.TB, limit string, db *store.DB) *harness {
	t.Helper()
	fake := httptest.NewServer(fakeprovider.Handler())
	t.Cleanup(fake.Close)

	table, err := pricing.LoadSeed()
	if err != nil {
		t.Fatalf("loading prices: %v", err)
	}
	snapshot := pricing.NewSnapshot(table)

	client := provider.NewHTTPClient(30*time.Second, 5*time.Second)
	opts := provider.Options{BaseURL: fake.URL, APIKey: obs.Secret("test-key"),
		Client: client, DefaultMaxTokens: 4096}

	router := provider.NewRouter(
		provider.NewOpenAI(opts),
		provider.NewAnthropic(opts),
	)
	router.SetModelProviders(map[string]string{
		"gpt-6-astra":     "openai",
		"gpt-5.6-terra":   "openai",
		"claude-sonnet-5": "anthropic",
		"claude-opus-5":   "anthropic",
	})

	key, err := auth.NewKey()
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	in := store.CreateKeyInput{Hash: key.Hash, Prefix: key.Prefix, Label: "proxy test"}
	if limit != "" {
		l := decimal.RequireFromString(limit)
		in.LimitUSD = &l
	}
	if _, err := db.CreateKey(t.Context(), in); err != nil {
		t.Fatalf("creating key: %v", err)
	}

	h := proxy.New(proxy.Options{
		DB: db, Router: router, Pricing: snapshot,
		DefaultMaxTokens: 4096, ReservationTTL: time.Minute,
		PassthroughHeaderPrefixes: []string{"X-Fake-"},
	})
	return &harness{handler: h, db: db, fake: fake, key: key}
}

func (h *harness) post(t testing.TB, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.key.Secret.Expose())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	return rec
}

// The end-to-end path: a request crosses the gateway, reaches the provider,
// and is accounted for exactly.
func TestProxiesAndAccountsExactly(t *testing.T) {
	h := newHarness(t, "100")

	rec := h.post(t, `{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{
			fakeprovider.HeaderPromptTokens:     "1000",
			fakeprovider.HeaderCompletionTokens: "500",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	// gpt-6-astra short context: $10/Mtok in, $50/Mtok out.
	// 1000 * 10/1e6 + 500 * 50/1e6 = 0.01 + 0.025
	wantCost := decimal.RequireFromString("0.035")
	got := rec.Header().Get(proxy.HeaderCostUSD)
	if !decimal.RequireFromString(got).Equal(wantCost) {
		t.Errorf("cost header = %s, want %s", got, wantCost)
	}

	// The budget must show the same figure.
	var spent string
	if err := h.db.Pool().QueryRow(t.Context(),
		`SELECT spent_usd::text FROM key_budgets`).Scan(&spent); err != nil {
		t.Fatalf("reading budget: %v", err)
	}
	if !decimal.RequireFromString(spent).Equal(wantCost) {
		t.Errorf("spent_usd = %s, want %s", spent, wantCost)
	}

	// And what the caller sees of their remaining budget must agree.
	remaining := rec.Header().Get(proxy.HeaderBudgetRemaining)
	if !decimal.RequireFromString(remaining).Equal(decimal.RequireFromString("99.965")) {
		t.Errorf("remaining = %s, want 99.965", remaining)
	}

	assertReconciled(t, h.db)
}

// The cost of a call is visible in the reply itself, which is the detail a
// customer notices first.
func TestCostHeadersAreAlwaysPresent(t *testing.T) {
	h := newHarness(t, "100")
	rec := h.post(t, `{"model":"gpt-5.6-terra","messages":[]}`, nil)

	for _, header := range []string{
		proxy.HeaderRequestID, proxy.HeaderCostUSD, proxy.HeaderBudgetRemaining,
	} {
		if rec.Header().Get(header) == "" {
			t.Errorf("%s is missing", header)
		}
	}
}

// A body reaches an OpenAI-compatible provider byte for byte. Anything else
// would drop fields the gateway has never heard of.
func TestOpenAIBodyIsForwardedUnchanged(t *testing.T) {
	var received []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-6-astra","choices":[],
		                        "usage":{"prompt_tokens":10,"completion_tokens":2}}`))
	}))
	defer upstream.Close()

	h := newHarnessWithUpstream(t, upstream.URL)
	sent := `{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}],` +
		`"a_field_from_next_month":{"nested":[1,2,3]},"another":"value","third":42,` +
		`"fourth":null,"fifth":true,"sixth":1.5,"seventh":[],"eighth":{},"ninth":"éè"}`

	if rec := h.post(t, sent, nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var sentDoc, receivedDoc map[string]any
	if err := json.Unmarshal([]byte(sent), &sentDoc); err != nil {
		t.Fatalf("decoding sent: %v", err)
	}
	if err := json.Unmarshal(received, &receivedDoc); err != nil {
		t.Fatalf("decoding received: %v", err)
	}
	for name, want := range sentDoc {
		got, ok := receivedDoc[name]
		if !ok {
			t.Errorf("field %q never reached the provider", name)
			continue
		}
		wj, _ := json.Marshal(want)
		gj, _ := json.Marshal(got)
		if string(wj) != string(gj) {
			t.Errorf("field %q changed in transit: %s -> %s", name, wj, gj)
		}
	}
}

// Anthropic requires max_tokens, so one is injected — and the client is told,
// because a ceiling it did not ask for changes what it gets back.
func TestInjectedMaxTokensIsDeclared(t *testing.T) {
	h := newHarness(t, "100")

	rec := h.post(t, `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get(proxy.HeaderInjectedMaxTokens); got != "4096" {
		t.Errorf("%s = %q, want 4096", proxy.HeaderInjectedMaxTokens, got)
	}

	// A client that set its own ceiling is not told about an injection
	// that did not happen.
	rec = h.post(t, `{"model":"claude-sonnet-5","messages":[],"max_tokens":50}`, nil)
	if got := rec.Header().Get(proxy.HeaderInjectedMaxTokens); got != "" {
		t.Errorf("%s = %q, want it absent", proxy.HeaderInjectedMaxTokens, got)
	}
}

// A parameter with no equivalent is named in the refusal rather than dropped.
func TestUnsupportedParameterIsRefusedByName(t *testing.T) {
	h := newHarness(t, "100")
	rec := h.post(t, `{"model":"claude-sonnet-5","messages":[],"logprobs":true}`, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "logprobs") {
		t.Errorf("the error should name the parameter: %s", rec.Body)
	}
}

// A provider echoing a credential back in an error must not have it
// forwarded. This has happened in the wild.
func TestUpstreamErrorBodyIsRedacted(t *testing.T) {
	h := newHarness(t, "100")
	const planted = `{"error":{"message":"invalid key sk-proj-abcdefghijklmnopqrstuvwxyz012345"}}`

	rec := h.post(t, `{"model":"gpt-6-astra","messages":[]}`, map[string]string{
		fakeprovider.HeaderStatus:    "401",
		fakeprovider.HeaderErrorBody: planted,
	})

	if strings.Contains(rec.Body.String(), "sk-proj-abcdefghijklmnopqrstuvwxyz012345") {
		t.Errorf("the credential was forwarded to the client: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), obs.Redacted) {
		t.Errorf("the body should show it was redacted: %s", rec.Body)
	}
}

// Retry-After tells a client's SDK what to do; withholding it would make a
// retryable failure look permanent.
func TestRetryAfterIsPreserved(t *testing.T) {
	h := newHarness(t, "100")
	rec := h.post(t, `{"model":"gpt-6-astra","messages":[]}`, map[string]string{
		fakeprovider.HeaderStatus:     "429",
		fakeprovider.HeaderRetryAfter: "42",
	})

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want the upstream 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "42" {
		t.Errorf("Retry-After = %q, want 42", got)
	}
}

// Pricing follows what the provider says it served.
func TestCostFollowsTheServedModel(t *testing.T) {
	h := newHarness(t, "100")
	rec := h.post(t, `{"model":"gpt-6-astra","messages":[]}`, map[string]string{
		fakeprovider.HeaderServedModel:      "gpt-5.6-terra",
		fakeprovider.HeaderPromptTokens:     "100000",
		fakeprovider.HeaderCompletionTokens: "0",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	// 100,000 tokens is below the 272,000 cliff, so terra's short-context
	// rate of $2/Mtok applies: 0.2, not astra's $10 giving 1.0.
	if got := rec.Header().Get(proxy.HeaderCostUSD); got != "0.2" {
		t.Errorf("cost = %s, want 0.2 (the served model's rate, short context)", got)
	}
}

// Crossing the context threshold reprices the whole request, so a million
// input tokens on the same model costs the long-context rate.
func TestCostCrossesTheContextCliff(t *testing.T) {
	h := newHarness(t, "100")
	rec := h.post(t, `{"model":"gpt-5.6-terra","messages":[]}`, map[string]string{
		fakeprovider.HeaderPromptTokens:     "1000000",
		fakeprovider.HeaderCompletionTokens: "0",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	// Above 272,000 the rate doubles to $4/Mtok, for the entire request.
	if got := rec.Header().Get(proxy.HeaderCostUSD); got != "4" {
		t.Errorf("cost = %s, want 4 (long-context rate applied to the whole request)", got)
	}
}

// A budget that cannot cover the estimate refuses before the provider is
// called at all.
func TestExhaustedBudgetRefusesBeforeCalling(t *testing.T) {
	h := newHarness(t, "0.000001")
	rec := h.post(t, `{"model":"gpt-6-astra","messages":[]}`, nil)

	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402: %s", rec.Code, rec.Body)
	}
	var body struct {
		Error struct {
			Type           string    `json:"type"`
			WindowResetsAt time.Time `json:"window_resets_at"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body.Error.Type != "budget_exceeded" {
		t.Errorf("type = %q", body.Error.Type)
	}
	if body.Error.WindowResetsAt.IsZero() {
		t.Error("the refusal must say when the budget resets")
	}
}

// A malformed body must not reach the database: a reservation taken and then
// released is a wasted round trip and a spurious audit row.
func TestValidationHappensBeforeReserving(t *testing.T) {
	h := newHarness(t, "100")

	for _, body := range []string{`not json`, `{}`, `{"model":""}`} {
		t.Run(body, func(t *testing.T) {
			rec := h.post(t, body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}

	var reservations int
	if err := h.db.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM budget_reservations`).Scan(&reservations); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if reservations != 0 {
		t.Errorf("%d reservations were taken for requests that never ran", reservations)
	}
}

func TestUnknownModelIs404(t *testing.T) {
	h := newHarness(t, "100")
	rec := h.post(t, `{"model":"a-model-nobody-serves","messages":[]}`, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}

func TestMissingCredentialIs401(t *testing.T) {
	h := newHarness(t, "100")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-6-astra","messages":[]}`))
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// Every reservation is closed, whatever the outcome: one left pending holds
// budget until the reaper.
func TestEveryPathSettlesItsReservation(t *testing.T) {
	h := newHarness(t, "100")

	h.post(t, `{"model":"gpt-6-astra","messages":[]}`, nil)
	h.post(t, `{"model":"gpt-6-astra","messages":[]}`,
		map[string]string{fakeprovider.HeaderStatus: "500"})
	h.post(t, `{"model":"claude-sonnet-5","messages":[],"seed":1}`, nil)

	var pending int
	if err := h.db.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM budget_reservations WHERE state = 'pending'`).Scan(&pending); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if pending != 0 {
		t.Errorf("%d reservations left pending", pending)
	}
	assertReconciled(t, h.db)
}

// modelsHandler builds a models endpoint over the same router and prices.
func (h *harness) modelsHandler(t *testing.T) http.Handler {
	t.Helper()
	table, err := pricing.LoadSeed()
	if err != nil {
		t.Fatalf("loading prices: %v", err)
	}
	client := provider.NewHTTPClient(30*time.Second, 5*time.Second)
	opts := provider.Options{BaseURL: h.fake.URL, APIKey: obs.Secret("test-key"),
		Client: client, DefaultMaxTokens: 4096}
	router := provider.NewRouter(provider.NewOpenAI(opts), provider.NewAnthropic(opts))
	router.SetModelProviders(map[string]string{
		"gpt-6-astra":     "openai",
		"claude-sonnet-5": "anthropic",
	})
	return proxy.NewModelsHandler(proxy.Options{
		Router: router, Pricing: pricing.NewSnapshot(table),
	})
}

func newHarnessWithUpstream(t *testing.T, baseURL string) *harness {
	t.Helper()
	h := newHarness(t, "100")
	table, _ := pricing.LoadSeed()
	client := provider.NewHTTPClient(30*time.Second, 5*time.Second)
	opts := provider.Options{BaseURL: baseURL, APIKey: obs.Secret("test-key"),
		Client: client, DefaultMaxTokens: 4096}
	router := provider.NewRouter(provider.NewOpenAI(opts))
	router.SetModelProviders(map[string]string{"gpt-6-astra": "openai"})
	h.handler = proxy.New(proxy.Options{
		DB: h.db, Router: router, Pricing: pricing.NewSnapshot(table),
		DefaultMaxTokens: 4096, ReservationTTL: time.Minute,
	})
	return h
}

func assertReconciled(t *testing.T, db *store.DB) {
	t.Helper()
	report, err := db.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(report.BalanceDrift) != 0 {
		t.Errorf("balance drifted from the log: %+v", report.BalanceDrift)
	}
}

// The models endpoint answers locally: many clients call it at startup, and
// an upstream round trip there would make this gateway's availability depend
// on three others.
func TestModelsListsWhatCanBePricedAndRouted(t *testing.T) {
	h := newHarness(t, "100")
	handler := h.modelsHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+h.key.Secret.Expose())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var body struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body.Object != "list" {
		t.Errorf("object = %q, want list", body.Object)
	}

	seen := map[string]string{}
	for _, m := range body.Data {
		seen[m.ID] = m.OwnedBy
	}
	for model, wantProvider := range map[string]string{
		"gpt-6-astra":     "openai",
		"claude-sonnet-5": "anthropic",
	} {
		if seen[model] != wantProvider {
			t.Errorf("model %q owned by %q, want %q", model, seen[model], wantProvider)
		}
	}
}

func TestModelsRequiresACredential(t *testing.T) {
	h := newHarness(t, "100")
	rec := httptest.NewRecorder()
	h.modelsHandler(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// Every completed request leaves a record whose aggregates reconstruct its
// detail, since the detail is what cost was computed from.
func TestUsageRecordIsWrittenAndConsistent(t *testing.T) {
	h := newHarness(t, "100")

	rec := h.post(t, `{"model":"gpt-6-astra","messages":[]}`, map[string]string{
		fakeprovider.HeaderPromptTokens:     "5000",
		fakeprovider.HeaderCompletionTokens: "300",
		fakeprovider.HeaderCachedTokens:     "2000",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var (
		requested, served, providerName string
		input, cachedRead, output       int64
		cost                            *string
		unpriced                        bool
		detail                          []byte
	)
	if err := h.db.Pool().QueryRow(t.Context(), `
		SELECT requested_model, served_model, provider,
		       input_tokens, cached_read_tokens, output_tokens,
		       cost_usd::text, unpriced, token_detail
		  FROM usage_records`).
		Scan(&requested, &served, &providerName, &input, &cachedRead, &output,
			&cost, &unpriced, &detail); err != nil {
		t.Fatalf("reading the record: %v", err)
	}

	if requested != "gpt-6-astra" || served != "gpt-6-astra" || providerName != "openai" {
		t.Errorf("record identifies %s/%s on %s", requested, served, providerName)
	}
	// OpenAI reports cached inside the prompt total, so plain input is
	// 5000 - 2000.
	if input != 3000 || cachedRead != 2000 || output != 300 {
		t.Errorf("aggregates: input=%d cached=%d output=%d, want 3000/2000/300",
			input, cachedRead, output)
	}
	if unpriced {
		t.Error("a priced model must not be recorded as unpriced")
	}
	if cost == nil {
		t.Fatal("a priced request must record a cost")
	}

	// The aggregates must reconstruct the detail they were derived from.
	var counts map[string]int64
	if err := json.Unmarshal(detail, &counts); err != nil {
		t.Fatalf("decoding detail: %v", err)
	}
	if counts["input"] != input || counts["cached_read"] != cachedRead ||
		counts["output"] != output {
		t.Errorf("aggregates disagree with the detail: %v", counts)
	}

	// And the recorded cost must equal what the client was told.
	if rec.Header().Get(proxy.HeaderCostUSD) != decimal.RequireFromString(*cost).String() {
		t.Errorf("header says %s, record says %s",
			rec.Header().Get(proxy.HeaderCostUSD), *cost)
	}
}

// An unpriced model on a budgeted key cannot be reserved, so it is refused
// explicitly rather than let through untracked.
func TestUnpricedModelWithABudgetIsRefused(t *testing.T) {
	h := newHarnessWithUnpricedModel(t, "10")

	rec := h.post(t, `{"model":"unpriced-model","messages":[]}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body.Error.Type != "model_not_priced" {
		t.Errorf("type = %q, want model_not_priced", body.Error.Type)
	}
}

// Without a budget there is nothing to reserve against, so the request goes
// through and is recorded with an unknown cost — never a zero one.
func TestUnpricedModelWithoutABudgetIsRecordedAsUnpriced(t *testing.T) {
	h := newHarnessWithUnpricedModel(t, "")

	rec := h.post(t, `{"model":"unpriced-model","messages":[]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
	}
	// No cost header, because there is no cost to report.
	if got := rec.Header().Get(proxy.HeaderCostUSD); got != "" {
		t.Errorf("%s = %q, want it absent for an unpriced model",
			proxy.HeaderCostUSD, got)
	}

	var (
		cost     *string
		unpriced bool
	)
	if err := h.db.Pool().QueryRow(t.Context(),
		`SELECT cost_usd::text, unpriced FROM usage_records`).Scan(&cost, &unpriced); err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	if !unpriced {
		t.Error("the record must be marked unpriced")
	}
	if cost != nil {
		t.Errorf("cost_usd = %v, want NULL: unknown is never free", *cost)
	}
}

// newHarnessWithUnpricedModel routes a model the price table does not cover.
func newHarnessWithUnpricedModel(t *testing.T, limit string) *harness {
	t.Helper()
	h := newHarness(t, limit)

	table, err := pricing.LoadSeed()
	if err != nil {
		t.Fatalf("loading prices: %v", err)
	}
	client := provider.NewHTTPClient(30*time.Second, 5*time.Second)
	opts := provider.Options{BaseURL: h.fake.URL, APIKey: obs.Secret("test-key"),
		Client: client, DefaultMaxTokens: 4096}
	router := provider.NewRouter(provider.NewOpenAI(opts))
	router.SetModelProviders(map[string]string{"unpriced-model": "openai"})

	h.handler = proxy.New(proxy.Options{
		DB: h.db, Router: router, Pricing: pricing.NewSnapshot(table),
		DefaultMaxTokens: 4096, ReservationTTL: time.Minute,
		PassthroughHeaderPrefixes: []string{"X-Fake-"},
	})
	return h
}
