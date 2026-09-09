package obs

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Registry collects metrics and renders them in the Prometheus text format.
//
// It is written here rather than pulled in, because the client library brings
// a large dependency tree for what amounts to a map and a formatter. Should
// this need histograms with real quantiles, swapping it out changes only this
// file.
//
// Labels are deliberately few. A key id would give one series per key, which
// is how a metrics backend falls over: per-key spend belongs in the read API,
// where it can be queried rather than scraped.
type Registry struct {
	mu sync.RWMutex

	counters   map[string]*counter
	gauges     map[string]*gauge
	histograms map[string]*histogram
}

type counter struct {
	help   string
	values map[string]float64
}

type gauge struct {
	help   string
	values map[string]float64
}

type histogram struct {
	help    string
	buckets []float64
	series  map[string]*histogramSeries
}

type histogramSeries struct {
	counts []uint64
	sum    float64
	total  uint64
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		counters:   map[string]*counter{},
		gauges:     map[string]*gauge{},
		histograms: map[string]*histogram{},
	}
}

// Counter increments a monotonic series.
func (r *Registry) Counter(name, help string, labels map[string]string, delta float64) {
	key := labelKey(labels)
	r.mu.Lock()
	defer r.mu.Unlock()

	c, ok := r.counters[name]
	if !ok {
		c = &counter{help: help, values: map[string]float64{}}
		r.counters[name] = c
	}
	c.values[key] += delta
}

// Gauge sets a value that can move in either direction.
func (r *Registry) Gauge(name, help string, labels map[string]string, value float64) {
	key := labelKey(labels)
	r.mu.Lock()
	defer r.mu.Unlock()

	g, ok := r.gauges[name]
	if !ok {
		g = &gauge{help: help, values: map[string]float64{}}
		r.gauges[name] = g
	}
	g.values[key] = value
}

// Observe records a value in a histogram.
func (r *Registry) Observe(name, help string, labels map[string]string,
	buckets []float64, value float64) {
	key := labelKey(labels)
	r.mu.Lock()
	defer r.mu.Unlock()

	h, ok := r.histograms[name]
	if !ok {
		h = &histogram{help: help, buckets: buckets, series: map[string]*histogramSeries{}}
		r.histograms[name] = h
	}
	s, ok := h.series[key]
	if !ok {
		s = &histogramSeries{counts: make([]uint64, len(h.buckets))}
		h.series[key] = s
	}
	for i, upper := range h.buckets {
		if value <= upper {
			s.counts[i]++
		}
	}
	s.sum += value
	s.total++
}

// ObserveDuration records a duration in seconds.
func (r *Registry) ObserveDuration(name, help string, labels map[string]string,
	buckets []float64, d time.Duration) {
	r.Observe(name, help, labels, buckets, d.Seconds())
}

// Handler serves the metrics.
//
// It carries no credential, and is meant to be bound to a separate address so
// that scraping does not require exposing it alongside the API.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(r.Render()))
	})
}

// Render produces the exposition text.
func (r *Registry) Render() string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var b strings.Builder
	for _, name := range sortedKeys(r.counters) {
		c := r.counters[name]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s counter\n", name, c.help, name)
		for _, labels := range sortedValueKeys(c.values) {
			fmt.Fprintf(&b, "%s%s %g\n", name, labels, c.values[labels])
		}
	}
	for _, name := range sortedKeys(r.gauges) {
		g := r.gauges[name]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n", name, g.help, name)
		for _, labels := range sortedValueKeys(g.values) {
			fmt.Fprintf(&b, "%s%s %g\n", name, labels, g.values[labels])
		}
	}
	for _, name := range sortedKeys(r.histograms) {
		h := r.histograms[name]
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s histogram\n", name, h.help, name)
		for _, labels := range sortedSeriesKeys(h.series) {
			s := h.series[labels]
			for i, upper := range h.buckets {
				fmt.Fprintf(&b, "%s_bucket%s %d\n",
					name, withLabel(labels, "le", fmt.Sprintf("%g", upper)), s.counts[i])
			}
			fmt.Fprintf(&b, "%s_bucket%s %d\n", name, withLabel(labels, "le", "+Inf"), s.total)
			fmt.Fprintf(&b, "%s_sum%s %g\n", name, labels, s.sum)
			fmt.Fprintf(&b, "%s_count%s %d\n", name, labels, s.total)
		}
	}
	return b.String()
}

// labelKey renders labels in the exposition format, sorted so a series is
// identified consistently however the map was built.
func labelKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	names := make([]string, 0, len(labels))
	for name := range labels {
		names = append(names, name)
	}
	sort.Strings(names)

	var parts []string
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s=%q", name, escapeLabel(labels[name])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func withLabel(existing, name, value string) string {
	pair := fmt.Sprintf("%s=%q", name, value)
	if existing == "" {
		return "{" + pair + "}"
	}
	return existing[:len(existing)-1] + "," + pair + "}"
}

func escapeLabel(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return strings.ReplaceAll(v, "\n", `\n`)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedValueKeys(m map[string]float64) []string           { return sortedKeys(m) }
func sortedSeriesKeys(m map[string]*histogramSeries) []string { return sortedKeys(m) }
