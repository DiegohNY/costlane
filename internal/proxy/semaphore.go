package proxy

import "context"

// ChannelSemaphore bounds concurrent drains with a buffered channel.
type ChannelSemaphore chan struct{}

// NewSemaphore returns a semaphore admitting n holders.
func NewSemaphore(n int) ChannelSemaphore { return make(ChannelSemaphore, n) }

// Acquire takes a slot, or gives up when the context expires.
func (s ChannelSemaphore) Acquire(ctx context.Context) error {
	select {
	case s <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release returns a slot.
func (s ChannelSemaphore) Release() { <-s }

// InFlight reports how many drains are running, for the gauge.
func (s ChannelSemaphore) InFlight() int { return len(s) }
