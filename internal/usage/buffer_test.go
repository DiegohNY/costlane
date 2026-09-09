package usage_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DiegohNY/costlane/internal/usage"
	"github.com/google/uuid"
)

// recordingWriter stands in for the database, and can be made to fail or
// stall on demand.
type recordingWriter struct {
	mu      sync.Mutex
	records []usage.Record
	batches int

	failUntil atomic.Int64 // unix nanos
	stall     atomic.Int64 // nanoseconds per write
}

func (w *recordingWriter) WriteBatch(ctx context.Context, records []usage.Record) error {
	if d := time.Duration(w.stall.Load()); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if time.Now().UnixNano() < w.failUntil.Load() {
		return errors.New("database unavailable")
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	w.records = append(w.records, records...)
	w.batches++
	return nil
}

func (w *recordingWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.records)
}

func (w *recordingWriter) batchCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.batches
}

type countingMetrics struct {
	mu           sync.Mutex
	depth        int
	full         int
	syncFallback int
	retries      int
	lost         int
	batches      int
}

func (m *countingMetrics) BufferDepth(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if n > m.depth {
		m.depth = n
	}
}
func (m *countingMetrics) BufferFull() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.full++
}
func (m *countingMetrics) SyncFallback() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.syncFallback++
}
func (m *countingMetrics) BatchWritten(int, time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batches++
}
func (m *countingMetrics) BatchRetried() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retries++
}
func (m *countingMetrics) RecordsLost(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lost += n
}
func (m *countingMetrics) snapshot() (full, sync, retries, lost int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.full, m.syncFallback, m.retries, m.lost
}

func aRecord() usage.Record {
	return usage.Record{ID: uuid.New(), RequestID: uuid.NewString()}
}

func TestBufferWritesEveryRecord(t *testing.T) {
	w := &recordingWriter{}
	b := usage.NewBuffer(usage.BufferOptions{
		Writer: w, BatchSize: 10, FlushInterval: 20 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)

	const n = 137
	for range n {
		if err := b.Add(context.Background(), aRecord()); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	cancel()
	if err := b.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := w.count(); got != n {
		t.Errorf("%d records written, want %d", got, n)
	}
}

// Batching is the point: a per-request insert would put a round trip on the
// critical path.
func TestRecordsAreBatched(t *testing.T) {
	w := &recordingWriter{}
	b := usage.NewBuffer(usage.BufferOptions{
		Writer: w, BatchSize: 50, FlushInterval: time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)

	for range 200 {
		_ = b.Add(context.Background(), aRecord())
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	_ = b.Close(t.Context())

	if w.count() != 200 {
		t.Fatalf("%d records, want 200", w.count())
	}
	// Four full batches, plus whatever the shutdown flush wrote.
	if batches := w.batchCount(); batches > 8 {
		t.Errorf("%d batches for 200 records: batching is not happening", batches)
	}
}

// A quiet gateway must not leave records sitting in memory indefinitely.
func TestPartialBatchFlushesOnTime(t *testing.T) {
	w := &recordingWriter{}
	b := usage.NewBuffer(usage.BufferOptions{
		Writer: w, BatchSize: 500, FlushInterval: 30 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	_ = b.Add(context.Background(), aRecord())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if w.count() == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("a single record was never flushed: the interval is not firing")
}

// The rule that makes a bounded buffer safe: when the queue is full the
// record is written synchronously rather than dropped. A dropped record is an
// accounting hole, and the risk we accepted was a hard crash, not congestion.
func TestFullBufferFallsBackToSynchronousWrites(t *testing.T) {
	w := &recordingWriter{}
	m := &countingMetrics{}

	// A tiny queue and no drainer, so every Add after the first few finds
	// it full.
	b := usage.NewBuffer(usage.BufferOptions{
		Writer: w, Capacity: 2, BatchSize: 100,
		FlushInterval: time.Hour, Metrics: m,
	})

	const n = 20
	for range n {
		if err := b.Add(context.Background(), aRecord()); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}

	full, syncFallback, _, lost := m.snapshot()
	if full == 0 || syncFallback == 0 {
		t.Errorf("the fallback was never taken: full=%d sync=%d", full, syncFallback)
	}
	if lost != 0 {
		t.Errorf("%d records lost; a full buffer must degrade latency, not drop", lost)
	}
	// Everything past the queue's capacity reached the writer directly.
	if got := w.count(); got != n-2 {
		t.Errorf("%d records written synchronously, want %d", got, n-2)
	}
}

// A database that goes away for a few seconds must cost latency, not records.
func TestNoRecordsLostWhileTheDatabaseIsDown(t *testing.T) {
	w := &recordingWriter{}
	m := &countingMetrics{}
	b := usage.NewBuffer(usage.BufferOptions{
		Writer: w, Capacity: 1000, BatchSize: 20,
		FlushInterval: 20 * time.Millisecond,
		RetryBackoff:  10 * time.Millisecond, RetryMaxBackoff: 100 * time.Millisecond,
		Metrics: m,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)

	// The database is unavailable for the next three seconds.
	w.failUntil.Store(time.Now().Add(3 * time.Second).UnixNano())

	const n = 300
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := b.Add(context.Background(), aRecord()); err != nil {
				t.Errorf("Add: %v", err)
			}
		}()
		time.Sleep(5 * time.Millisecond)
	}
	wg.Wait()

	// Give the retries time to succeed once the database returns.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && w.count() < n {
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	_ = b.Close(t.Context())

	if got := w.count(); got != n {
		t.Errorf("%d of %d records survived a three-second outage", got, n)
	}
	if _, _, retries, lost := m.snapshot(); retries == 0 {
		t.Error("no retries were recorded during the outage")
	} else if lost != 0 {
		t.Errorf("%d records reported lost", lost)
	}
}

// Shutdown must not strand what is still queued.
func TestCloseFlushesWhatIsQueued(t *testing.T) {
	w := &recordingWriter{}
	b := usage.NewBuffer(usage.BufferOptions{
		Writer: w, BatchSize: 1000, FlushInterval: time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)

	for range 50 {
		_ = b.Add(context.Background(), aRecord())
	}

	cancel()
	if err := b.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := w.count(); got != 50 {
		t.Errorf("%d records survived shutdown, want 50", got)
	}
}

// A record arriving after shutdown began is written rather than refused: it
// represents spend that already happened.
func TestAddAfterCloseWritesSynchronously(t *testing.T) {
	w := &recordingWriter{}
	b := usage.NewBuffer(usage.BufferOptions{Writer: w, FlushInterval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)
	cancel()
	_ = b.Close(t.Context())

	if err := b.Add(context.Background(), aRecord()); err != nil {
		t.Fatalf("Add after close: %v", err)
	}
	if w.count() != 1 {
		t.Errorf("%d records written, want the late one to survive", w.count())
	}
}

// A caller whose own context is already cancelled still gets its record
// written: the tokens were consumed regardless.
func TestSynchronousWriteSurvivesACancelledCaller(t *testing.T) {
	w := &recordingWriter{}
	b := usage.NewBuffer(usage.BufferOptions{
		Writer: w, Capacity: 1, FlushInterval: time.Hour,
	})

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	// Fill the queue, then force the fallback with a dead context.
	_ = b.Add(context.Background(), aRecord())
	if err := b.Add(cancelled, aRecord()); err != nil {
		t.Fatalf("Add with a cancelled caller: %v", err)
	}
	if w.count() != 1 {
		t.Errorf("%d records written, want the synchronous one to survive", w.count())
	}
}
