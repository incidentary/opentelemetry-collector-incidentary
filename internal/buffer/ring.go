// Package buffer implements the per-trace_id pre-alert ring buffer.
//
// The buffer is a circular store of completed-or-in-progress traces.
// Two eviction rules apply:
//
//   1. Oldest-out when the total span count reaches MaxSpans.
//   2. Trace removed entirely when its newest span is older than
//      PreAlertWindow (time-eviction).
//
// Memory cap is a hard upper bound a misconfigured customer cannot
// exceed: insertion past MaxSpans always drops the *oldest* trace.
package buffer

import (
	"sync"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// Ring is a thread-safe per-trace_id buffer of resource spans.
//
// It stores cloned ptrace.ResourceSpans grouped by trace_id, plus the
// monotonic clock time at which each span was inserted (used by
// time-based eviction). Lookup, insert, drain, and prune are all O(1)
// amortized for typical workloads (HashMap of trace_id → SpanList +
// FIFO queue of trace_ids ordered by insertion).
type Ring struct {
	mu             sync.Mutex
	maxSpans       int
	preAlertWindow time.Duration
	now            func() time.Time

	traces map[pcommon.TraceID]*traceEntry
	order  []pcommon.TraceID
	count  int
}

type traceEntry struct {
	spans      []ptrace.ResourceSpans
	insertedAt []time.Time
	newestAt   time.Time
}

// Options configure a Ring.
type Options struct {
	// MaxSpans is the hard cap on total span count across all traces.
	// Insertion past this cap drops the oldest trace.
	MaxSpans int

	// PreAlertWindow is the wall-clock window. A trace is removed once
	// its newest span is older than this. Default 5m if zero.
	PreAlertWindow time.Duration

	// Now is the time source. Tests pass an injected clock; production
	// passes nil and gets time.Now.
	Now func() time.Time
}

// New constructs a Ring. It panics if MaxSpans <= 0.
func New(opts Options) *Ring {
	if opts.MaxSpans <= 0 {
		panic("buffer.New: MaxSpans must be > 0")
	}
	if opts.PreAlertWindow <= 0 {
		opts.PreAlertWindow = 5 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Ring{
		maxSpans:       opts.MaxSpans,
		preAlertWindow: opts.PreAlertWindow,
		now:            opts.Now,
		traces:         make(map[pcommon.TraceID]*traceEntry),
		order:          make([]pcommon.TraceID, 0, 64),
	}
}

// Insert appends one ResourceSpans (cloned) to the buffer slot for the
// given trace_id. Returns the post-insertion span count.
//
// If the cap is exceeded, the *oldest* trace is dropped wholesale. The
// per-span dropping option was rejected (plan §3.3.2) because dropping
// half a trace makes the post-flush incident artifact incoherent.
func (r *Ring) Insert(traceID pcommon.TraceID, rs ptrace.ResourceSpans) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	clone := ptrace.NewResourceSpans()
	rs.CopyTo(clone)
	spanCount := 0
	for i := 0; i < clone.ScopeSpans().Len(); i++ {
		spanCount += clone.ScopeSpans().At(i).Spans().Len()
	}

	entry, ok := r.traces[traceID]
	if !ok {
		entry = &traceEntry{
			spans:      make([]ptrace.ResourceSpans, 0, 1),
			insertedAt: make([]time.Time, 0, 1),
		}
		r.traces[traceID] = entry
		r.order = append(r.order, traceID)
	}
	entry.spans = append(entry.spans, clone)
	entry.insertedAt = append(entry.insertedAt, now)
	if now.After(entry.newestAt) {
		entry.newestAt = now
	}
	r.count += spanCount

	r.evictByTimeLocked(now)
	r.evictByCountLocked()

	return r.count
}

// Drain returns and removes all buffered spans for the given trace_id.
// Returns nil if the trace is unknown.
func (r *Ring) Drain(traceID pcommon.TraceID) []ptrace.ResourceSpans {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.traces[traceID]
	if !ok {
		return nil
	}
	delete(r.traces, traceID)
	r.removeFromOrderLocked(traceID)
	for _, rs := range entry.spans {
		spanCount := 0
		for i := 0; i < rs.ScopeSpans().Len(); i++ {
			spanCount += rs.ScopeSpans().At(i).Spans().Len()
		}
		r.count -= spanCount
	}
	if r.count < 0 {
		r.count = 0
	}
	return entry.spans
}

// Len returns the current total span count across all buffered traces.
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.count
}

// Traces returns the number of distinct trace_ids currently buffered.
func (r *Ring) Traces() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.traces)
}

// PruneByTime evicts trace_ids whose newest span is older than the
// pre-alert window. Useful when the processor has been idle and we
// want to reclaim memory between bursts.
func (r *Ring) PruneByTime() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.evictByTimeLocked(r.now())
}

func (r *Ring) evictByTimeLocked(now time.Time) int {
	cutoff := now.Add(-r.preAlertWindow)
	removed := 0
	survivors := r.order[:0]
	for _, id := range r.order {
		entry, ok := r.traces[id]
		if !ok {
			continue
		}
		if entry.newestAt.Before(cutoff) {
			delete(r.traces, id)
			for _, rs := range entry.spans {
				for i := 0; i < rs.ScopeSpans().Len(); i++ {
					r.count -= rs.ScopeSpans().At(i).Spans().Len()
				}
			}
			removed++
			continue
		}
		survivors = append(survivors, id)
	}
	r.order = survivors
	if r.count < 0 {
		r.count = 0
	}
	return removed
}

func (r *Ring) evictByCountLocked() int {
	removed := 0
	for r.count > r.maxSpans && len(r.order) > 0 {
		oldest := r.order[0]
		r.order = r.order[1:]
		entry, ok := r.traces[oldest]
		if !ok {
			continue
		}
		for _, rs := range entry.spans {
			for i := 0; i < rs.ScopeSpans().Len(); i++ {
				r.count -= rs.ScopeSpans().At(i).Spans().Len()
			}
		}
		delete(r.traces, oldest)
		removed++
	}
	return removed
}

func (r *Ring) removeFromOrderLocked(target pcommon.TraceID) {
	for i, id := range r.order {
		if id == target {
			r.order = append(r.order[:i], r.order[i+1:]...)
			return
		}
	}
}
