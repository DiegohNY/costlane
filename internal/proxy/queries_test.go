package proxy_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/DiegohNY/costlane/internal/fakeprovider"
	"github.com/DiegohNY/costlane/internal/storetest"
	"github.com/jackc/pgx/v5"
)

// queryCounter records every statement both pools issue.
//
// It is a pgx.QueryTracer, so it sees exactly what crosses the wire: not what
// the code appears to do, but what Postgres was actually asked.
type queryCounter struct {
	mu         sync.Mutex
	statements []string
}

func (q *queryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn,
	data pgx.TraceQueryStartData) context.Context {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.statements = append(q.statements, normaliseSQL(data.SQL))
	return ctx
}

func (q *queryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (q *queryCounter) reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.statements = nil
}

func (q *queryCounter) seen() []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.statements...)
}

// work returns the statements that do something, with transaction control
// removed. begin and commit are round trips too, but they are the cost of
// atomicity rather than of a lookup, and counting them would obscure the
// question this test asks: what does the gateway ask the database about
// before it will forward a request?
func (q *queryCounter) work() []string {
	var out []string
	for _, s := range q.seen() {
		switch strings.ToLower(strings.Fields(s + " x")[0]) {
		case "begin", "commit", "rollback", "savepoint", "release":
			continue
		}
		out = append(out, s)
	}
	return out
}

// normaliseSQL reduces a statement to its first two words, which is enough to
// tell a reserve from a settle without pinning the test to whitespace.
func normaliseSQL(sql string) string {
	fields := strings.Fields(sql)
	if len(fields) > 2 {
		fields = fields[:2]
	}
	return strings.Join(fields, " ")
}

// The architecture claim in the README, measured rather than asserted: one
// statement stands between an accepted request and the provider call, and it
// is the reserve. Everything else — the settle, the remaining-budget read —
// happens after the provider has already answered, and the usage record
// leaves the path entirely.
//
// This test exists because the claim is the product's whole latency
// argument. A future change that adds a lookup before the upstream call is
// free to do so, but not silently.
func TestHappyPathMakesOneDatabaseRoundTripBeforeTheProvider(t *testing.T) {
	counter := &queryCounter{}
	db := storetest.NewTracedDB(t, counter)
	h := newHarnessOn(t, "100", db)

	// Key creation and migrations have run by now; only the request itself
	// is being counted.
	counter.reset()

	rec := h.post(t, `{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{
			fakeprovider.HeaderPromptTokens:     "1000",
			fakeprovider.HeaderCompletionTokens: "500",
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	statements := counter.work()

	// The reserve is one statement inside one transaction, so pgx reports
	// the UPDATE and the INSERT that records the reservation. What matters
	// is that nothing precedes them: no key lookup, no budget read, no
	// model check. Authentication, the model allowlist, the window
	// rotation and the limit are all predicates inside that UPDATE.
	if len(statements) == 0 {
		t.Fatal("no statements were traced; the tracer is not attached")
	}
	if first := statements[0]; !strings.HasPrefix(first, "UPDATE key_budgets") {
		t.Errorf("the first statement of a request is %q, want the reserve "+
			"(UPDATE key_budgets). Something now reads the database before "+
			"the request is authorised, which is the round trip the design "+
			"exists to avoid.", first)
	}

	// The whole path, for the record. This harness has no usage buffer, so
	// the usage INSERT appears here; in production it is handed to the
	// buffer and leaves the request path entirely.
	t.Logf("statements on the happy path (%d): %v", len(statements), statements)

	var selects int
	for _, s := range statements {
		if strings.HasPrefix(s, "SELECT") {
			selects++
		}
	}
	// One SELECT: the remaining budget for the response header, after the
	// provider has answered. A second one would mean a lookup crept back in.
	if selects > 1 {
		t.Errorf("the happy path runs %d SELECTs, want at most 1 (the remaining "+
			"budget read for the response header): %v", selects, statements)
	}
}

// A refused request must not reach the provider, and must not pay for a
// second round trip to find that out: the refusal is the reserve returning no
// rows. The diagnosis query that follows runs only on that path, which is
// exactly the trade the design makes.
func TestRefusalDiagnosesOnlyAfterTheReserveMatchesNothing(t *testing.T) {
	counter := &queryCounter{}
	db := storetest.NewTracedDB(t, counter)
	h := newHarnessOn(t, "0.0001", db)

	counter.reset()

	rec := h.post(t, `{"model":"gpt-6-astra","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{
			fakeprovider.HeaderPromptTokens:     "1000",
			fakeprovider.HeaderCompletionTokens: "500",
		})
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402: %s", rec.Code, rec.Body)
	}

	statements := counter.work()
	if len(statements) == 0 {
		t.Fatal("no statements were traced; the tracer is not attached")
	}
	if first := statements[0]; !strings.HasPrefix(first, "UPDATE key_budgets") {
		t.Errorf("the first statement of a refused request is %q, want the "+
			"reserve: the refusal must come from the reserve itself, not "+
			"from a check in front of it", first)
	}
	t.Logf("statements on the refusal path (%d): %v", len(statements), statements)
}
