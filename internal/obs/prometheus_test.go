package obs_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DiegohNY/costlane/internal/obs"
)

func TestCounterRendersInExpositionFormat(t *testing.T) {
	r := obs.NewRegistry()
	r.Counter("costlane_requests_total", "Requests completed.",
		map[string]string{"model": "gpt-6-astra", "status": "200"}, 3)

	out := r.Render()
	for _, want := range []string{
		"# HELP costlane_requests_total Requests completed.",
		"# TYPE costlane_requests_total counter",
		`costlane_requests_total{model="gpt-6-astra",status="200"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q from:\n%s", want, out)
		}
	}
}

// Labels are sorted so a series is identified consistently however the map
// was built; otherwise the same series would render two different ways.
func TestLabelOrderIsStable(t *testing.T) {
	first := obs.NewRegistry()
	first.Counter("m", "help", map[string]string{"b": "2", "a": "1"}, 1)

	second := obs.NewRegistry()
	second.Counter("m", "help", map[string]string{"a": "1", "b": "2"}, 1)

	if first.Render() != second.Render() {
		t.Errorf("label order changed the output:\n%s\nvs\n%s",
			first.Render(), second.Render())
	}
}

func TestGaugeMovesInBothDirections(t *testing.T) {
	r := obs.NewRegistry()
	r.Gauge("costlane_drains_in_flight", "Drains running.", nil, 3)
	r.Gauge("costlane_drains_in_flight", "Drains running.", nil, 1)

	if !strings.Contains(r.Render(), "costlane_drains_in_flight 1") {
		t.Errorf("the gauge did not take the later value:\n%s", r.Render())
	}
}

func TestHistogramRendersBucketsSumAndCount(t *testing.T) {
	r := obs.NewRegistry()
	buckets := []float64{0.01, 0.1, 1}
	for _, d := range []time.Duration{
		5 * time.Millisecond, 50 * time.Millisecond, 500 * time.Millisecond,
	} {
		r.ObserveDuration("costlane_ttft_seconds", "Time to first byte.",
			map[string]string{"model": "m"}, buckets, d)
	}

	out := r.Render()
	for _, want := range []string{
		`costlane_ttft_seconds_bucket{model="m",le="0.01"} 1`,
		`costlane_ttft_seconds_bucket{model="m",le="0.1"} 2`,
		`costlane_ttft_seconds_bucket{model="m",le="1"} 3`,
		`costlane_ttft_seconds_bucket{model="m",le="+Inf"} 3`,
		`costlane_ttft_seconds_count{model="m"} 3`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q from:\n%s", want, out)
		}
	}
}

// Values that would break the format must be escaped rather than emitted
// raw, or one odd model name corrupts the whole scrape.
func TestLabelValuesAreEscaped(t *testing.T) {
	r := obs.NewRegistry()
	r.Counter("m", "help", map[string]string{"model": `a "quoted" \ name`}, 1)

	out := r.Render()
	if strings.Contains(out, `"quoted"`) && !strings.Contains(out, `\"quoted\"`) {
		t.Errorf("a quote was not escaped:\n%s", out)
	}
}

// No metric may carry a key id: one series per key is how a metrics backend
// falls over, and per-key spend belongs in the read API.
func TestNoMetricCarriesAKeyIdentifier(t *testing.T) {
	r := obs.NewRegistry()
	m := obs.NewGatewayMetrics(r)

	m.RequestCompleted("gpt-6-astra", "openai", "200", 10*time.Millisecond)
	m.TokensCounted("gpt-6-astra", map[string]int64{"input": 100, "output": 20})
	m.Chunk("gpt-6-astra")
	m.TimeToFirstByte("gpt-6-astra", 5*time.Millisecond)
	m.ClientDisconnected("gpt-6-astra", "cancel")
	m.BudgetRejection()
	m.BufferDepth(7)
	m.SyncFallback()

	out := r.Render()
	for _, forbidden := range []string{"key_id", "key=", "cl_"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("a metric carries %q, which would give one series per key:\n%s",
				forbidden, out)
		}
	}
}

func TestHandlerServesTheExpositionFormat(t *testing.T) {
	r := obs.NewRegistry()
	r.Counter("costlane_requests_total", "Requests.", nil, 1)

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "costlane_requests_total 1") {
		t.Errorf("body = %s", rec.Body)
	}
}

// The drains gauge has to come back down, or a long-running gateway reports
// drains that finished hours ago.
func TestDrainGaugeReturnsToZero(t *testing.T) {
	r := obs.NewRegistry()
	m := obs.NewGatewayMetrics(r)

	m.DrainStarted()
	m.DrainStarted()
	if !strings.Contains(r.Render(), "costlane_drains_in_flight 2") {
		t.Errorf("gauge did not reach 2:\n%s", r.Render())
	}
	m.DrainFinished()
	m.DrainFinished()
	if !strings.Contains(r.Render(), "costlane_drains_in_flight 0") {
		t.Errorf("gauge did not return to 0:\n%s", r.Render())
	}
}

func TestConcurrentUpdatesAreSafe(t *testing.T) {
	r := obs.NewRegistry()
	m := obs.NewGatewayMetrics(r)

	done := make(chan struct{})
	for range 8 {
		go func() {
			for range 100 {
				m.RequestCompleted("m", "openai", "200", time.Millisecond)
				m.Chunk("m")
				m.BufferDepth(3)
			}
			done <- struct{}{}
		}()
	}
	for range 8 {
		<-done
	}

	if !strings.Contains(r.Render(), "costlane_stream_chunks_total") {
		t.Error("concurrent updates lost a series")
	}
}
