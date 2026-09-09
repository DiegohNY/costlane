package usage

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Writer persists a batch of records.
type Writer interface {
	WriteBatch(ctx context.Context, records []Record) error
}

// Metrics reports what the buffer is doing.
//
// Depth and the two counters together answer the only question that matters
// here: is the buffer sized correctly. A rising SyncFallback means it is not,
// and says so before anyone notices latency.
type Metrics interface {
	BufferDepth(n int)
	BufferFull()
	SyncFallback()
	BatchWritten(n int, d time.Duration)
	BatchRetried()
	RecordsLost(n int)
}

// BufferOptions configure the writer.
type BufferOptions struct {
	Writer Writer

	// Capacity bounds the queue. Unbounded would trade a lost record for
	// an exhausted process, which is a worse failure.
	Capacity int
	// BatchSize and FlushInterval decide when a batch goes out, whichever
	// comes first: size keeps throughput up, the interval keeps a quiet
	// gateway's records from sitting indefinitely.
	BatchSize     int
	FlushInterval time.Duration

	// SyncTimeout bounds the synchronous write used when the queue is
	// full.
	SyncTimeout time.Duration

	// RetryBackoff is the first delay after a failed batch; it doubles up
	// to RetryMaxBackoff.
	RetryBackoff    time.Duration
	RetryMaxBackoff time.Duration

	Metrics Metrics
	Logger  Logger
}

// Logger is the little the buffer needs.
type Logger interface {
	Error(msg string, args ...any)
	Warn(msg string, args ...any)
}

// Buffer takes usage records off the request path.
//
// The write itself is batched and asynchronous, because a per-request INSERT
// would put a round trip on the critical path of a product that advertises
// low overhead. What is not negotiable is losing a record: a dropped one is
// an accounting hole, and the risk accepted in the design was a hard crash,
// not a slow database. So when the queue is full the record is written
// synchronously instead — that request pays for the congestion, and nothing
// disappears.
type Buffer struct {
	opts    BufferOptions
	queue   chan Record
	done    chan struct{}
	closed  chan struct{}
	closeMu sync.Mutex
	stopped bool
}

// ErrBufferClosed reports an enqueue after shutdown.
var ErrBufferClosed = errors.New("usage: buffer is closed")

// NewBuffer builds a buffer. Run must be called to start draining it.
func NewBuffer(opts BufferOptions) *Buffer {
	if opts.Capacity <= 0 {
		opts.Capacity = 10_000
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 500
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = 200 * time.Millisecond
	}
	if opts.SyncTimeout <= 0 {
		opts.SyncTimeout = 2 * time.Second
	}
	if opts.RetryBackoff <= 0 {
		opts.RetryBackoff = 50 * time.Millisecond
	}
	if opts.RetryMaxBackoff <= 0 {
		opts.RetryMaxBackoff = 5 * time.Second
	}
	return &Buffer{
		opts:   opts,
		queue:  make(chan Record, opts.Capacity),
		done:   make(chan struct{}),
		closed: make(chan struct{}),
	}
}

// Add queues a record, falling back to a synchronous write when the queue is
// full.
//
// The fallback is what makes the buffer safe to bound. Dropping instead would
// turn a transient database problem into missing money.
func (b *Buffer) Add(ctx context.Context, record Record) error {
	select {
	case <-b.closed:
		// Shutting down: write it now rather than lose it.
		return b.writeSynchronously(ctx, record)
	default:
	}

	select {
	case b.queue <- record:
		if b.opts.Metrics != nil {
			b.opts.Metrics.BufferDepth(len(b.queue))
		}
		return nil
	default:
	}

	if b.opts.Metrics != nil {
		b.opts.Metrics.BufferFull()
		b.opts.Metrics.SyncFallback()
	}
	if b.opts.Logger != nil {
		b.opts.Logger.Warn("usage buffer full, writing synchronously",
			"capacity", b.opts.Capacity)
	}
	return b.writeSynchronously(ctx, record)
}

func (b *Buffer) writeSynchronously(ctx context.Context, record Record) error {
	// Detached from the caller: a client that has gone still consumed
	// tokens, and its record must not vanish with its connection.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.opts.SyncTimeout)
	defer cancel()
	return b.opts.Writer.WriteBatch(writeCtx, []Record{record})
}

// Run drains the queue until the context is cancelled, then flushes.
func (b *Buffer) Run(ctx context.Context) {
	defer close(b.done)

	ticker := time.NewTicker(b.opts.FlushInterval)
	defer ticker.Stop()

	batch := make([]Record, 0, b.opts.BatchSize)

	for {
		select {
		case <-ctx.Done():
			b.drainAndFlush(batch)
			return

		case record := <-b.queue:
			batch = append(batch, record)
			if b.opts.Metrics != nil {
				b.opts.Metrics.BufferDepth(len(b.queue))
			}
			if len(batch) >= b.opts.BatchSize {
				batch = b.flush(ctx, batch)
			}

		case <-ticker.C:
			if len(batch) > 0 {
				batch = b.flush(ctx, batch)
			}
		}
	}
}

// flush writes a batch, retrying transient failures while holding it in
// memory. The queue keeps filling behind it up to its bound, at which point
// callers fall back to synchronous writes — so a database outage degrades
// latency rather than losing records.
func (b *Buffer) flush(ctx context.Context, batch []Record) []Record {
	if len(batch) == 0 {
		return batch
	}

	backoff := b.opts.RetryBackoff
	for attempt := 0; ; attempt++ {
		started := time.Now()
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		err := b.opts.Writer.WriteBatch(writeCtx, batch)
		cancel()

		if err == nil {
			if b.opts.Metrics != nil {
				b.opts.Metrics.BatchWritten(len(batch), time.Since(started))
			}
			return batch[:0]
		}

		if b.opts.Metrics != nil {
			b.opts.Metrics.BatchRetried()
		}
		if b.opts.Logger != nil {
			b.opts.Logger.Error("writing a usage batch failed, retrying",
				"error", err, "records", len(batch), "attempt", attempt+1)
		}

		select {
		case <-ctx.Done():
			// Shutting down and still failing: one last attempt on a
			// fresh context, then report what was lost rather than
			// letting it disappear quietly.
			return b.lastAttempt(batch)
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > b.opts.RetryMaxBackoff {
			backoff = b.opts.RetryMaxBackoff
		}
	}
}

func (b *Buffer) lastAttempt(batch []Record) []Record {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := b.opts.Writer.WriteBatch(ctx, batch); err != nil {
		if b.opts.Metrics != nil {
			b.opts.Metrics.RecordsLost(len(batch))
		}
		if b.opts.Logger != nil {
			b.opts.Logger.Error("usage records lost at shutdown",
				"error", err, "records", len(batch))
		}
	}
	return batch[:0]
}

// drainAndFlush empties the queue at shutdown.
func (b *Buffer) drainAndFlush(batch []Record) {
	for {
		select {
		case record := <-b.queue:
			batch = append(batch, record)
			if len(batch) >= b.opts.BatchSize {
				batch = b.flush(context.Background(), batch)
			}
		default:
			if len(batch) > 0 {
				b.flush(context.Background(), batch)
			}
			return
		}
	}
}

// Close stops accepting records and waits for the queue to drain.
func (b *Buffer) Close(ctx context.Context) error {
	b.closeMu.Lock()
	if !b.stopped {
		b.stopped = true
		close(b.closed)
	}
	b.closeMu.Unlock()

	select {
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Depth reports the current queue length, for the gauge.
func (b *Buffer) Depth() int { return len(b.queue) }
