package obs

import (
	"sync"
	"time"
)

// StreamMetrics counts what streams do.
//
// It is a small in-memory implementation rather than a Prometheus one: the
// registry arrives in F7, and the proxy needs somewhere to send numbers
// before then. The interface is what matters, so swapping the backing store
// changes nothing above it.
type StreamMetrics struct {
	mu sync.Mutex

	chunks         map[string]int64
	ttft           map[string][]time.Duration
	disconnects    map[string]int64
	drainsInFlight int64
	drainWaits     []time.Duration
}

// NewStreamMetrics builds a collector.
func NewStreamMetrics() *StreamMetrics {
	return &StreamMetrics{
		chunks:      map[string]int64{},
		ttft:        map[string][]time.Duration{},
		disconnects: map[string]int64{},
	}
}

// Chunk counts one relayed chunk.
func (m *StreamMetrics) Chunk(model string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chunks[model]++
}

// TimeToFirstByte records how long the client waited.
//
// Measured to the first byte written to the client rather than the first
// received from the provider: the difference between those two is this
// gateway's own overhead, and it is the number worth watching.
func (m *StreamMetrics) TimeToFirstByte(model string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ttft[model] = append(m.ttft[model], d)
}

// ClientDisconnected counts a departure, labelled by the policy that ran.
func (m *StreamMetrics) ClientDisconnected(_, policy string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.disconnects[policy]++
}

// DrainStarted and DrainFinished move the in-flight gauge.
func (m *StreamMetrics) DrainStarted() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drainsInFlight++
}

// DrainFinished decrements the gauge.
func (m *StreamMetrics) DrainFinished() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drainsInFlight--
}

// DrainWait records how long a drain queued for a slot. A rising value means
// the semaphore is the bottleneck rather than the provider.
func (m *StreamMetrics) DrainWait(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drainWaits = append(m.drainWaits, d)
}

// Snapshot returns the current values, for tests and for the metrics
// endpoint that arrives in F7.
type Snapshot struct {
	Chunks         map[string]int64
	TTFT           map[string][]time.Duration
	Disconnects    map[string]int64
	DrainsInFlight int64
	DrainWaits     []time.Duration
}

// Snapshot copies the counters.
func (m *StreamMetrics) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := Snapshot{
		Chunks:         make(map[string]int64, len(m.chunks)),
		TTFT:           make(map[string][]time.Duration, len(m.ttft)),
		Disconnects:    make(map[string]int64, len(m.disconnects)),
		DrainsInFlight: m.drainsInFlight,
		DrainWaits:     append([]time.Duration(nil), m.drainWaits...),
	}
	for k, v := range m.chunks {
		out.Chunks[k] = v
	}
	for k, v := range m.ttft {
		out.TTFT[k] = append([]time.Duration(nil), v...)
	}
	for k, v := range m.disconnects {
		out.Disconnects[k] = v
	}
	return out
}
