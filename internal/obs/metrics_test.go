package obs_test

import (
	"testing"
	"time"

	"github.com/DiegohNY/costlane/internal/obs"
)

func TestStreamMetricsRecordWhatStreamsDo(t *testing.T) {
	m := obs.NewStreamMetrics()

	for range 5 {
		m.Chunk("gpt-6-astra")
	}
	m.Chunk("claude-sonnet-5")
	m.TimeToFirstByte("gpt-6-astra", 42*time.Millisecond)
	m.ClientDisconnected("gpt-6-astra", "cancel")
	m.ClientDisconnected("gpt-6-astra", "cancel")
	m.ClientDisconnected("gpt-6-astra", "drain")
	m.DrainWait(7 * time.Millisecond)

	snap := m.Snapshot()
	if snap.Chunks["gpt-6-astra"] != 5 || snap.Chunks["claude-sonnet-5"] != 1 {
		t.Errorf("chunks = %v", snap.Chunks)
	}
	if len(snap.TTFT["gpt-6-astra"]) != 1 || snap.TTFT["gpt-6-astra"][0] != 42*time.Millisecond {
		t.Errorf("ttft = %v", snap.TTFT)
	}
	// The policy label is what distinguishes a cheap cancellation from a
	// drain that paid for tokens nobody read.
	if snap.Disconnects["cancel"] != 2 || snap.Disconnects["drain"] != 1 {
		t.Errorf("disconnects = %v", snap.Disconnects)
	}
	if len(snap.DrainWaits) != 1 {
		t.Errorf("drain waits = %v", snap.DrainWaits)
	}
}

// The gauge has to come back down, or a long-running gateway reports drains
// that finished hours ago.
func TestDrainGaugeReturnsToZero(t *testing.T) {
	m := obs.NewStreamMetrics()

	m.DrainStarted()
	m.DrainStarted()
	if got := m.Snapshot().DrainsInFlight; got != 2 {
		t.Errorf("in flight = %d, want 2", got)
	}
	m.DrainFinished()
	m.DrainFinished()
	if got := m.Snapshot().DrainsInFlight; got != 0 {
		t.Errorf("in flight = %d, want 0 once every drain has finished", got)
	}
}

// A snapshot must not alias the live maps, or a reader would see them change
// underneath it.
func TestSnapshotIsACopy(t *testing.T) {
	m := obs.NewStreamMetrics()
	m.Chunk("model")

	snap := m.Snapshot()
	m.Chunk("model")

	if snap.Chunks["model"] != 1 {
		t.Errorf("the snapshot changed after it was taken: %v", snap.Chunks)
	}
}
