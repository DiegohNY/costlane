package pricing

import (
	"io/fs"
	"sync/atomic"
)

// Snapshot holds the table a request prices against.
//
// The lookup must not touch the database: a query for a price before every
// reservation would put a round-trip on the critical path for data that
// changes once a month. Prices can be cached precisely because they are
// versioned and immutable — unlike virtual keys, where a cache would create
// a window in which a revoked key still works.
//
// The pointer is swapped wholesale, never mutated, so a request sees either
// the old table or the new one and never a half-applied mixture.
type Snapshot struct {
	current atomic.Pointer[Table]
}

// NewSnapshot returns a snapshot holding the given table.
func NewSnapshot(t *Table) *Snapshot {
	s := &Snapshot{}
	s.current.Store(t)
	return s
}

// Table returns the table in force. It never returns nil once the snapshot
// has been built.
func (s *Snapshot) Table() *Table { return s.current.Load() }

// ReloadFS parses and validates a whole price directory, and swaps it in only
// if every file is sound. A broken file leaves the running table untouched
// and serving, which is the point: reloading prices must never be able to
// take pricing down.
func (s *Snapshot) ReloadFS(fsys fs.FS, dir string) error {
	next, err := LoadFS(fsys, dir)
	if err != nil {
		return err
	}
	s.current.Store(next)
	return nil
}
