package buffer

import (
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
)

// Helper: make a ResourceSpans containing n spans for the given trace.
func makeResourceSpans(traceID pcommon.TraceID, n int) ptrace.ResourceSpans {
	rs := ptrace.NewResourceSpans()
	rs.Resource().Attributes().PutStr("service.name", "checkout")
	scope := rs.ScopeSpans().AppendEmpty()
	for i := 0; i < n; i++ {
		span := scope.Spans().AppendEmpty()
		span.SetTraceID(traceID)
		var spanID pcommon.SpanID
		spanID[0] = byte(i + 1)
		span.SetSpanID(spanID)
		span.SetName("op")
	}
	return rs
}

func newTraceID(seed byte) pcommon.TraceID {
	var t pcommon.TraceID
	for i := range t {
		t[i] = seed + byte(i)
	}
	return t
}

// Basic insert + drain round-trip.
func TestRing_InsertDrain(t *testing.T) {
	r := New(Options{MaxSpans: 100, PreAlertWindow: time.Minute})
	tid := newTraceID(1)
	r.Insert(tid, makeResourceSpans(tid, 5))
	if r.Len() != 5 {
		t.Fatalf("Len = %d, want 5", r.Len())
	}
	if r.Traces() != 1 {
		t.Fatalf("Traces = %d, want 1", r.Traces())
	}
	got := r.Drain(tid)
	if len(got) != 1 {
		t.Fatalf("Drain returned %d ResourceSpans, want 1", len(got))
	}
	if r.Len() != 0 || r.Traces() != 0 {
		t.Fatalf("post-drain: Len=%d Traces=%d, want 0/0", r.Len(), r.Traces())
	}
}

// Eviction by count cap. The OLDEST trace (FIFO) drops.
func TestRing_EvictsOldestWhenCountExceedsMax(t *testing.T) {
	r := New(Options{MaxSpans: 10, PreAlertWindow: time.Minute})

	first := newTraceID(1)
	second := newTraceID(2)
	third := newTraceID(3)

	r.Insert(first, makeResourceSpans(first, 5))   // count=5
	r.Insert(second, makeResourceSpans(second, 5)) // count=10
	r.Insert(third, makeResourceSpans(third, 3))   // count=13 → evict first

	if got := r.Traces(); got != 2 {
		t.Fatalf("after over-cap insert, Traces = %d, want 2 (oldest dropped)", got)
	}
	if got := r.Drain(first); got != nil {
		t.Errorf("first trace should have been evicted, but Drain returned %v", got)
	}
	if got := r.Drain(second); got == nil {
		t.Errorf("second trace should still be present")
	}
	if got := r.Drain(third); got == nil {
		t.Errorf("third trace should still be present")
	}
}

// Eviction by age. A trace whose newest span is older
// Than the pre-alert window is removed wholesale on the next op.
func TestRing_EvictsByAge(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	r := New(Options{MaxSpans: 100, PreAlertWindow: time.Minute, Now: clock})

	stale := newTraceID(1)
	r.Insert(stale, makeResourceSpans(stale, 4))
	now = now.Add(2 * time.Minute) // age it past the window

	// Force a re-eviction by inserting another trace.
	fresh := newTraceID(2)
	r.Insert(fresh, makeResourceSpans(fresh, 3))

	if got := r.Drain(stale); got != nil {
		t.Errorf("stale trace should have been time-evicted, got %v", got)
	}
	if got := r.Drain(fresh); got == nil {
		t.Errorf("fresh trace should still be present")
	}
}

// PruneByTime is the explicit reclaim path for an idle processor.
func TestRing_PruneByTimeReclaims(t *testing.T) {
	now := time.Unix(0, 0)
	clock := func() time.Time { return now }
	r := New(Options{MaxSpans: 100, PreAlertWindow: time.Minute, Now: clock})

	for i := byte(1); i <= 5; i++ {
		id := newTraceID(i)
		r.Insert(id, makeResourceSpans(id, 2))
	}
	now = now.Add(2 * time.Minute)
	removed := r.PruneByTime()
	if removed != 5 {
		t.Errorf("PruneByTime removed %d, want 5", removed)
	}
	if r.Traces() != 0 {
		t.Errorf("post-prune Traces = %d, want 0", r.Traces())
	}
}

// Drain on unknown trace must be safe (no panic, returns nil).
func TestRing_DrainUnknownIsNil(t *testing.T) {
	r := New(Options{MaxSpans: 10, PreAlertWindow: time.Minute})
	if got := r.Drain(newTraceID(9)); got != nil {
		t.Errorf("Drain(unknown) = %v, want nil", got)
	}
}

// Inserting more spans for an existing trace must accumulate.
func TestRing_InsertMergesIntoExistingTrace(t *testing.T) {
	r := New(Options{MaxSpans: 100, PreAlertWindow: time.Minute})
	tid := newTraceID(1)
	r.Insert(tid, makeResourceSpans(tid, 3))
	r.Insert(tid, makeResourceSpans(tid, 4))
	if r.Traces() != 1 {
		t.Errorf("Traces after merging same trace_id = %d, want 1", r.Traces())
	}
	if r.Len() != 7 {
		t.Errorf("Len = %d, want 7 (3+4)", r.Len())
	}
	got := r.Drain(tid)
	if len(got) != 2 {
		t.Errorf("Drain returned %d ResourceSpans, want 2 (preserves insertion shape)", len(got))
	}
}
