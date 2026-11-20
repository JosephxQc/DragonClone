package core

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Tracker keeps count of how far a job got. The UI polls Snapshot(), the
// engine only ever calls Add/BadAt, so this stays cheap.
type Tracker struct {
	mu    sync.Mutex
	phase string
	total int64
	start time.Time

	done atomic.Int64
	bad  atomic.Int64 // in bytes

	badAt []int64 // where the bad sectors are (first few thousand), for the sector map

	// Logf, if set, gets the odd interesting event (bad sectors and such).
	Logf func(format string, args ...any)
}

// Status is a point-in-time copy of the tracker.
type Status struct {
	Phase   string  `json:"phase"`
	Done    int64   `json:"done"`
	Total   int64   `json:"total"`
	Bad     int64   `json:"bad"`
	Elapsed float64 `json:"elapsed"` // seconds in this phase
	BadAt   []int64 `json:"badAt,omitempty"`
}

func (t *Tracker) Begin(phase string, total int64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.phase, t.total, t.start = phase, total, time.Now()
	t.mu.Unlock()
	t.done.Store(0)
}

func (t *Tracker) Add(n int64) {
	if t != nil {
		t.done.Add(n)
	}
}

// BadAt records an unreadable stretch.
func (t *Tracker) BadAt(off, n int64) {
	if t == nil {
		return
	}
	t.bad.Add(n)
	t.mu.Lock()
	if len(t.badAt) < 4000 {
		t.badAt = append(t.badAt, off)
	}
	t.mu.Unlock()
}

func (t *Tracker) log(format string, args ...any) {
	if t != nil && t.Logf != nil {
		t.Logf(format, args...)
	}
}

func (t *Tracker) Snapshot() Status {
	if t == nil {
		return Status{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := Status{Phase: t.phase, Total: t.total, Done: t.done.Load(), Bad: t.bad.Load(),
		BadAt: append([]int64(nil), t.badAt...)}
	if !t.start.IsZero() {
		s.Elapsed = time.Since(t.start).Seconds()
	}
	return s
}

func (s Status) String() string {
	return fmt.Sprintf("%s %s/%s", s.Phase, HumanBytes(s.Done), HumanBytes(s.Total))
}
