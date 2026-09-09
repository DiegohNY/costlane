package api_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/DiegohNY/costlane/internal/pricing"
	"github.com/DiegohNY/costlane/internal/store"
	"github.com/DiegohNY/costlane/internal/usage"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// seedUsage writes records for a key, spread over the given hours ago.
func seedUsage(t *testing.T, db *store.DB, keyID uuid.UUID, model string,
	hoursAgo []int, costEach string) {
	t.Helper()
	cost := decimal.RequireFromString(costEach)
	records := make([]usage.Record, 0, len(hoursAgo))
	for _, h := range hoursAgo {
		records = append(records, usage.Record{
			ID: uuid.New(), KeyID: keyID, RequestID: uuid.NewString(),
			RequestedModel: model, ServedModel: model, Provider: "openai",
			WindowStart: time.Now().UTC().Truncate(24 * time.Hour),
			TokenDetail: pricing.Counts{
				pricing.KindInput: 1000, pricing.KindOutput: 100,
			},
			CostUSD: &cost, UsageSource: "provider", StatusCode: 200,
			CreatedAt: time.Now().UTC().Add(-time.Duration(h) * time.Hour),
		})
	}
	if err := db.WriteBatchAt(t.Context(), records); err != nil {
		t.Fatalf("seeding usage: %v", err)
	}
}

func TestUsageAggregatesByModel(t *testing.T) {
	srv, db := newTestServer(t)
	keyID := uuid.MustParse(srv.createKey(t, `{"label":"reporting"}`))

	seedUsage(t, db, keyID, "gpt-6-astra", []int{1, 2, 3}, "0.10")
	seedUsage(t, db, keyID, "claude-sonnet-5", []int{1, 2}, "0.25")

	rec := srv.do(t, http.MethodGet, "/v1/usage?group_by=model", masterAuth, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var body struct {
		Data []struct {
			Group    map[string]string `json:"group"`
			Requests int64             `json:"requests"`
			CostUSD  string            `json:"cost_usd"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(body.Data) != 2 {
		t.Fatalf("%d buckets, want 2: %s", len(body.Data), rec.Body)
	}

	byModel := map[string]struct {
		requests int64
		cost     string
	}{}
	for _, b := range body.Data {
		byModel[b.Group["model"]] = struct {
			requests int64
			cost     string
		}{b.Requests, b.CostUSD}
	}
	if got := byModel["gpt-6-astra"]; got.requests != 3 ||
		!decimal.RequireFromString(got.cost).Equal(decimal.RequireFromString("0.30")) {
		t.Errorf("astra: %+v, want 3 requests at 0.30", got)
	}
	if got := byModel["claude-sonnet-5"]; got.requests != 2 ||
		!decimal.RequireFromString(got.cost).Equal(decimal.RequireFromString("0.50")) {
		t.Errorf("sonnet: %+v, want 2 requests at 0.50", got)
	}
}

// Money must never become a float on the way out: a JSON number would be
// parsed as a double by most clients and rounded.
func TestAmountsAreDecimalStrings(t *testing.T) {
	srv, db := newTestServer(t)
	keyID := uuid.MustParse(srv.createKey(t, `{"label":"precision"}`))
	// A figure that float64 cannot represent exactly.
	seedUsage(t, db, keyID, "m", []int{1}, "0.1000000001")

	rec := srv.do(t, http.MethodGet, "/v1/usage", masterAuth, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"cost_usd":"0.1000000001"`) {
		t.Errorf("the amount is not an exact decimal string: %s", rec.Body)
	}
}

// A key sees its own spend and nobody else's, and the repository is what
// enforces that.
func TestAKeyCannotReadAnotherKeysUsage(t *testing.T) {
	srv, db := newTestServer(t)

	mineSecret, mineID := srv.createKeyWithSecret(t, `{"label":"mine"}`)
	_, theirsID := srv.createKeyWithSecret(t, `{"label":"theirs"}`)

	seedUsage(t, db, mineID, "gpt-6-astra", []int{1}, "1.00")
	seedUsage(t, db, theirsID, "gpt-6-astra", []int{1, 2, 3}, "5.00")

	rec := srv.do(t, http.MethodGet, "/v1/usage", "Bearer "+mineSecret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var body struct {
		Data []struct {
			Requests int64  `json:"requests"`
			CostUSD  string `json:"cost_usd"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(body.Data) != 1 {
		t.Fatalf("%d buckets, want 1", len(body.Data))
	}
	if body.Data[0].Requests != 1 {
		t.Errorf("%d requests visible, want only the caller's own 1",
			body.Data[0].Requests)
	}
	if !decimal.RequireFromString(body.Data[0].CostUSD).
		Equal(decimal.RequireFromString("1.00")) {
		t.Errorf("cost = %s, want the caller's own 1.00", body.Data[0].CostUSD)
	}
}

// The grouping vocabulary is closed, so no caller can put a column name into
// a query.
func TestGroupByRejectsAnythingOutsideTheWhitelist(t *testing.T) {
	srv, _ := newTestServer(t)

	// Percent-encoded, so the value reaches the handler intact rather than
	// breaking the request line. The point is that a column name or an
	// injection attempt is rejected by the whitelist, not by the URL parser.
	for _, group := range []string{
		"key_hash",
		"created_at",
		"nonsense",
		url.QueryEscape("1;DROP TABLE usage_records"),
		url.QueryEscape("u.key_id::text, (SELECT key_hash FROM virtual_keys)"),
	} {
		t.Run(group, func(t *testing.T) {
			rec := srv.do(t, http.MethodGet, "/v1/usage?group_by="+group, masterAuth, "")
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 for %q: %s", rec.Code, group, rec.Body)
			}
		})
	}
}

func TestGroupByAcceptsEveryValidDimension(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, group := range []string{"key", "model", "provider", "day", "hour"} {
		t.Run(group, func(t *testing.T) {
			rec := srv.do(t, http.MethodGet, "/v1/usage?group_by="+group, masterAuth, "")
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d for %q: %s", rec.Code, group, rec.Body)
			}
		})
	}
}

func TestAtMostTwoDimensions(t *testing.T) {
	srv, _ := newTestServer(t)

	if rec := srv.do(t, http.MethodGet,
		"/v1/usage?group_by=model,day", masterAuth, ""); rec.Code != http.StatusOK {
		t.Errorf("two dimensions must be allowed, got %d: %s", rec.Code, rec.Body)
	}
	if rec := srv.do(t, http.MethodGet,
		"/v1/usage?group_by=model,day,provider", masterAuth, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("three dimensions must be refused, got %d", rec.Code)
	}
}

// Days are cut on UTC boundaries, matching the budget windows. A day that
// moved with the caller's timezone would give totals that never reconcile
// with spent_usd.
func TestDaysAreCutOnUTCBoundaries(t *testing.T) {
	srv, db := newTestServer(t)
	keyID := uuid.MustParse(srv.createKey(t, `{"label":"days"}`))

	// Two records either side of a UTC midnight, 30 hours apart.
	seedUsage(t, db, keyID, "m", []int{1, 31}, "1.00")

	rec := srv.do(t, http.MethodGet,
		"/v1/usage?group_by=day&from="+
			time.Now().UTC().Add(-72*time.Hour).Format(time.RFC3339), masterAuth, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var body struct {
		Data []struct {
			Group map[string]string `json:"group"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	for _, bucket := range body.Data {
		day := bucket.Group["day"]
		if _, err := time.Parse("2006-01-02", day); err != nil {
			t.Errorf("day %q is not a plain UTC date", day)
		}
	}
}

// A range wider than the gateway allows is refused, so no query becomes a
// full scan.
func TestOverlyWideRangeIsRefused(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := srv.do(t, http.MethodGet,
		"/v1/usage?from=2020-01-01T00:00:00Z&to=2030-01-01T00:00:00Z", masterAuth, "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a ten-year range", rec.Code)
	}
}

func TestMalformedTimestampsAreRefused(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, query := range []string{
		"?from=yesterday", "?to=soon", "?from=2026-09-01", // no time zone
	} {
		t.Run(query, func(t *testing.T) {
			rec := srv.do(t, http.MethodGet, "/v1/usage"+query, masterAuth, "")
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

// --- pagination -----------------------------------------------------------

func TestRequestsPaginateWithAStableCursor(t *testing.T) {
	srv, db := newTestServer(t)
	keyID := uuid.MustParse(srv.createKey(t, `{"label":"paging"}`))

	hours := make([]int, 25)
	for i := range hours {
		hours[i] = i + 1
	}
	seedUsage(t, db, keyID, "m", hours, "0.01")

	// The records span 25 hours, so the default 24-hour window would leave
	// two of them out and make this look like a pagination bug.
	from := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)

	seen := map[string]bool{}
	cursor := ""
	pages := 0

	for {
		url := "/v1/usage/requests?limit=10&from=" + from
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		rec := srv.do(t, http.MethodGet, url, masterAuth, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}

		var page struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			NextCursor *string `json:"next_cursor"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("decoding: %v", err)
		}

		for _, row := range page.Data {
			if seen[row.ID] {
				t.Fatalf("row %s appeared on two pages", row.ID)
			}
			seen[row.ID] = true
		}
		pages++
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}

	if len(seen) != 25 {
		t.Errorf("%d distinct rows across %d pages, want 25", len(seen), pages)
	}
}

// A cursor the endpoint did not issue is the caller's mistake, and reporting
// 500 would send someone hunting for a fault in the gateway.
func TestMalformedCursorIs400(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, cursor := range []string{
		"not-base64!!", "aGVsbG8", "", "MjAyNi0wOS0wOQ",
	} {
		if cursor == "" {
			continue
		}
		t.Run(cursor, func(t *testing.T) {
			rec := srv.do(t, http.MethodGet,
				"/v1/usage/requests?cursor="+cursor, masterAuth, "")
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body)
			}
		})
	}
}

// --- budget ---------------------------------------------------------------

func TestBudgetReportsTheCallersOwnState(t *testing.T) {
	srv, _ := newTestServer(t)
	secret, _ := srv.createKeyWithSecret(t, `{"label":"budgeted","limit_usd":"25"}`)

	rec := srv.do(t, http.MethodGet, "/v1/budget", "Bearer "+secret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	var body struct {
		LimitUSD       *string   `json:"limit_usd"`
		SpentUSD       string    `json:"spent_usd"`
		RemainingUSD   *string   `json:"remaining_usd"`
		WindowResetsAt time.Time `json:"window_resets_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body.LimitUSD == nil ||
		!decimal.RequireFromString(*body.LimitUSD).Equal(decimal.NewFromInt(25)) {
		t.Errorf("limit = %v, want 25", body.LimitUSD)
	}
	if body.RemainingUSD == nil {
		t.Fatal("remaining is null on a budgeted key")
	}
	if body.WindowResetsAt.IsZero() || !body.WindowResetsAt.After(time.Now()) {
		t.Errorf("window_resets_at = %v, want a future instant", body.WindowResetsAt)
	}
}

// An unlimited key has no remaining figure, and null must not become zero.
func TestUnlimitedKeyReportsNullRemaining(t *testing.T) {
	srv, _ := newTestServer(t)
	secret, _ := srv.createKeyWithSecret(t, `{"label":"unlimited"}`)

	rec := srv.do(t, http.MethodGet, "/v1/budget", "Bearer "+secret, "")
	var body struct {
		LimitUSD     *string `json:"limit_usd"`
		RemainingUSD *string `json:"remaining_usd"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body.LimitUSD != nil || body.RemainingUSD != nil {
		t.Errorf("limit=%v remaining=%v, want both null", body.LimitUSD, body.RemainingUSD)
	}
}

func TestReadEndpointsRequireACredential(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, path := range []string{"/v1/usage", "/v1/usage/requests", "/v1/budget"} {
		t.Run(path, func(t *testing.T) {
			if rec := srv.do(t, http.MethodGet, path, "", ""); rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
		})
	}
}

// A revoked key reads nothing, immediately.
func TestRevokedKeyCannotRead(t *testing.T) {
	srv, _ := newTestServer(t)
	secret, id := srv.createKeyWithSecret(t, `{"label":"doomed"}`)

	if rec := srv.do(t, http.MethodGet, "/v1/usage", "Bearer "+secret, ""); rec.Code != http.StatusOK {
		t.Fatalf("the key should work before revocation: %d", rec.Code)
	}
	if rec := srv.do(t, http.MethodDelete, "/admin/keys/"+id.String(), masterAuth, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoking: %d", rec.Code)
	}
	if rec := srv.do(t, http.MethodGet, "/v1/usage", "Bearer "+secret, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 immediately after revocation", rec.Code)
	}
}
