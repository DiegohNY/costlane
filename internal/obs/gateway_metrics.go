package obs

import (
	"sync"
	"time"
)

// Latency buckets, in seconds. They are dense below a second because that is
// where a gateway's own overhead lives; a request that takes ten seconds is
// waiting on a model, and knowing whether it was eleven does not help.
var (
	latencyBuckets = []float64{
		0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60,
	}
	overheadBuckets = []float64{
		0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1,
	}
)

// GatewayMetrics is the whole metric surface, in one place so the label sets
// can be seen together and kept low-cardinality.
//
// No metric carries a key id. One series per key is how a metrics backend
// falls over, and per-key spend is a question for the read API — where it can
// be queried, filtered and paginated — not for a scrape.
type GatewayMetrics struct {
	registry *Registry
	// drains tracks the gauge, which needs a running total rather than a
	// counter's monotonic sum.
	mu     sync.Mutex
	drains float64
}

// NewGatewayMetrics builds the collector.
func NewGatewayMetrics(registry *Registry) *GatewayMetrics {
	return &GatewayMetrics{registry: registry}
}

// Registry exposes the underlying registry, for the metrics handler.
func (m *GatewayMetrics) Registry() *Registry { return m.registry }

// RequestCompleted records a finished request.
func (m *GatewayMetrics) RequestCompleted(model, provider, status string, d time.Duration) {
	labels := map[string]string{"model": model, "provider": provider, "status": status}
	m.registry.Counter("costlane_requests_total",
		"Requests completed, by model, provider and status.", labels, 1)
	m.registry.ObserveDuration("costlane_request_duration_seconds",
		"Time from receiving a request to finishing its response.",
		map[string]string{"model": model, "provider": provider}, latencyBuckets, d)
}

// ProxyOverhead records the time spent in the gateway rather than upstream.
func (m *GatewayMetrics) ProxyOverhead(model string, d time.Duration) {
	m.registry.ObserveDuration("costlane_proxy_overhead_seconds",
		"Time spent in costlane rather than waiting on a provider.",
		map[string]string{"model": model}, overheadBuckets, d)
}

// TokensCounted records tokens by billing class.
func (m *GatewayMetrics) TokensCounted(model string, counts map[string]int64) {
	for kind, n := range counts {
		if n <= 0 {
			continue
		}
		m.registry.Counter("costlane_tokens_total",
			"Tokens metered, by model and billing class.",
			map[string]string{"model": model, "kind": kind}, float64(n))
	}
}

// Unpriced counts a request whose cost could not be computed.
func (m *GatewayMetrics) Unpriced(model string) {
	m.registry.Counter("costlane_unpriced_requests_total",
		"Requests for a model with no active price.",
		map[string]string{"model": model}, 1)
}

// ModelMismatch counts a response served by a model other than the one asked
// for, which means pricing rested on a wrong assumption until it was noticed.
func (m *GatewayMetrics) ModelMismatch(requested, served string) {
	m.registry.Counter("costlane_model_mismatch_total",
		"Responses served by a model other than the one requested.",
		map[string]string{"requested": requested, "served": served}, 1)
}

// BudgetRejection counts a refusal.
func (m *GatewayMetrics) BudgetRejection() {
	m.registry.Counter("costlane_budget_rejections_total",
		"Requests refused because a key's budget was exhausted.", nil, 1)
}

// BudgetOvershoot counts a settle above its reservation.
func (m *GatewayMetrics) BudgetOvershoot() {
	m.registry.Counter("costlane_budget_overshoot_total",
		"Settles larger than the reservation that preceded them.", nil, 1)
}

// BudgetDrift reports how far the materialised balances have moved from the
// reservation log. It should be zero; anything else needs a person.
func (m *GatewayMetrics) BudgetDrift(usd float64) {
	m.registry.Gauge("costlane_budget_drift_usd",
		"Difference between materialised balances and the reservation log.", nil, usd)
}

// ReservationsExpired counts reservations the reaper reclaimed.
func (m *GatewayMetrics) ReservationsExpired(n int) {
	m.registry.Counter("costlane_reservations_expired_total",
		"Reservations released by the reaper after their TTL.", nil, float64(n))
}

// UsageRecordsDropped counts records that could not be persisted at all. It
// is the one counter here that should never move.
func (m *GatewayMetrics) UsageRecordsDropped(n int) {
	m.registry.Counter("costlane_usage_records_dropped_total",
		"Usage records lost, which should never happen.", nil, float64(n))
}

// --- streaming ------------------------------------------------------------

// Chunk counts one relayed stream chunk.
func (m *GatewayMetrics) Chunk(model string) {
	m.registry.Counter("costlane_stream_chunks_total",
		"Stream chunks relayed to clients.", map[string]string{"model": model}, 1)
}

// TimeToFirstByte records the wait before a client saw anything.
func (m *GatewayMetrics) TimeToFirstByte(model string, d time.Duration) {
	m.registry.ObserveDuration("costlane_ttft_seconds",
		"Time from receiving a request to writing its first byte to the client.",
		map[string]string{"model": model}, latencyBuckets, d)
}

// ClientDisconnected counts a departure, labelled by the policy that ran.
func (m *GatewayMetrics) ClientDisconnected(_, policy string) {
	m.registry.Counter("costlane_client_disconnects_total",
		"Clients that went away mid-stream, by disconnect policy.",
		map[string]string{"policy": policy}, 1)
}

// DrainStarted and DrainFinished move the in-flight gauge.
func (m *GatewayMetrics) DrainStarted() { m.adjustDrains(1) }

// DrainFinished decrements the gauge.
func (m *GatewayMetrics) DrainFinished() { m.adjustDrains(-1) }

func (m *GatewayMetrics) adjustDrains(delta float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if delta > 0 {
		m.registry.Counter("costlane_drains_started_total",
			"Streams drained after a client disconnected.", nil, delta)
	}
	m.drains += delta
	m.registry.Gauge("costlane_drains_in_flight",
		"Streams currently being drained.", nil, m.drains)
}

// DrainWait records how long a drain queued for a semaphore slot. A rising
// value means the semaphore is the bottleneck rather than the provider.
func (m *GatewayMetrics) DrainWait(d time.Duration) {
	m.registry.ObserveDuration("costlane_drain_semaphore_wait_seconds",
		"Time a drain waited for a slot.", nil, overheadBuckets, d)
}

// --- usage buffer ---------------------------------------------------------

// BufferDepth reports how much is queued.
func (m *GatewayMetrics) BufferDepth(n int) {
	m.registry.Gauge("costlane_usage_buffer_depth",
		"Usage records waiting to be written.", nil, float64(n))
}

// BufferFull counts an enqueue that found no room.
func (m *GatewayMetrics) BufferFull() {
	m.registry.Counter("costlane_usage_buffer_full_total",
		"Times the usage buffer had no room.", nil, 1)
}

// SyncFallback counts a record written on the request path because the
// buffer was full. If this rises, the buffer is undersized — which is the
// whole reason it is a metric rather than a silent behaviour.
func (m *GatewayMetrics) SyncFallback() {
	m.registry.Counter("costlane_usage_sync_fallback_total",
		"Usage records written synchronously because the buffer was full.", nil, 1)
}

// BatchWritten records a persisted batch.
func (m *GatewayMetrics) BatchWritten(n int, d time.Duration) {
	m.registry.Counter("costlane_usage_records_written_total",
		"Usage records persisted.", nil, float64(n))
	m.registry.ObserveDuration("costlane_usage_batch_seconds",
		"Time to write one batch of usage records.", nil, latencyBuckets, d)
}

// BatchRetried counts a failed batch that will be retried.
func (m *GatewayMetrics) BatchRetried() {
	m.registry.Counter("costlane_usage_batch_retries_total",
		"Usage batches retried after a write failure.", nil, 1)
}

// RecordsLost counts records abandoned at shutdown.
func (m *GatewayMetrics) RecordsLost(n int) { m.UsageRecordsDropped(n) }

// --- reaper ---------------------------------------------------------------

// ReservationsReaped counts what one sweep released.
func (m *GatewayMetrics) ReservationsReaped(n int) { m.ReservationsExpired(n) }

// ReapDuration records how long a sweep took.
func (m *GatewayMetrics) ReapDuration(d time.Duration) {
	m.registry.ObserveDuration("costlane_reaper_duration_seconds",
		"Time for one reaper sweep.", nil, latencyBuckets, d)
}

// DeadlockRetries counts sweeps that hit a deadlock and retried.
func (m *GatewayMetrics) DeadlockRetries(n int) {
	m.registry.Counter("costlane_reaper_deadlock_retries_total",
		"Reaper sweeps retried after a deadlock.", nil, float64(n))
}
