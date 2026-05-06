// Unit tests for the cross-batch trace breaker.

package incidentaryprocessor

import (
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
)

func mkTraceID(b byte) pcommon.TraceID {
	var t pcommon.TraceID
	t[0] = b
	return t
}

func TestTraceBreaker_NoTierBelowThreshold(t *testing.T) {
	b := newTraceBreaker(time.Hour)
	tid := mkTraceID(0x01)

	// 4 999 spans — under the warn threshold of 5 000.
	for i := int64(0); i < CircuitBreakerWarnThreshold-1; i++ {
		if got := b.observe(tid); got != tierNone {
			t.Fatalf("observe at i=%d returned %v, want tierNone", i, got)
		}
	}
}

func TestTraceBreaker_WarnTierFiresExactlyOnce(t *testing.T) {
	b := newTraceBreaker(time.Hour)
	tid := mkTraceID(0x02)

	// First 4 999 — no tier.
	for i := int64(0); i < CircuitBreakerWarnThreshold-1; i++ {
		_ = b.observe(tid)
	}
	// The 5 000th span crosses warn.
	if got := b.observe(tid); got != tierWarn {
		t.Errorf("5000th span: got %v, want tierWarn", got)
	}
	// Spans 5 001..5 005 should NOT re-fire warn.
	for i := 0; i < 5; i++ {
		if got := b.observe(tid); got != tierNone {
			t.Errorf("post-warn span %d: got %v, want tierNone", i, got)
		}
	}
}

func TestTraceBreaker_TruncateAndBreakerFireExactlyOnceEach(t *testing.T) {
	b := newTraceBreaker(time.Hour)
	tid := mkTraceID(0x03)

	// Walk up to the warn threshold first.
	for i := int64(0); i < CircuitBreakerWarnThreshold; i++ {
		_ = b.observe(tid)
	}
	// Now bridge from warn → truncate. We've already observed
	// CircuitBreakerWarnThreshold spans (5 000); we need
	// CircuitBreakerTruncateThreshold - CircuitBreakerWarnThreshold
	// = 45 000 more for the next transition.
	bridge := CircuitBreakerTruncateThreshold - CircuitBreakerWarnThreshold
	for i := int64(0); i < bridge-1; i++ {
		if got := b.observe(tid); got != tierNone {
			t.Fatalf("bridge to truncate at i=%d: got %v, want tierNone", i, got)
		}
	}
	if got := b.observe(tid); got != tierTruncate {
		t.Errorf("50 000th span: got %v, want tierTruncate", got)
	}

	// Bridge from truncate → breaker.
	bridge2 := CircuitBreakerHardThreshold - CircuitBreakerTruncateThreshold
	for i := int64(0); i < bridge2-1; i++ {
		if got := b.observe(tid); got != tierNone {
			t.Fatalf("bridge to breaker at i=%d: got %v, want tierNone", i, got)
		}
	}
	if got := b.observe(tid); got != tierBreaker {
		t.Errorf("500 000th span: got %v, want tierBreaker", got)
	}

	// Past the breaker, every subsequent observe is tierNone (no
	// double-fire) and shouldDrop returns true.
	for i := 0; i < 100; i++ {
		if got := b.observe(tid); got != tierNone {
			t.Errorf("post-breaker span %d: got %v, want tierNone", i, got)
		}
	}
	if !b.shouldDrop(tid) {
		t.Errorf("shouldDrop after breaker: got false, want true")
	}
}

// TestTraceBreaker_DistinctTraceIDsDontInteract is the cross-batch
// invariant — 200K spans across 3 distinct trace_ids must NOT trip
// any tier (each individual trace stays under 5K).
func TestTraceBreaker_DistinctTraceIDsDontInteract(t *testing.T) {
	b := newTraceBreaker(time.Hour)
	for traceByte := byte(0x10); traceByte < 0x13; traceByte++ {
		tid := mkTraceID(traceByte)
		for i := int64(0); i < CircuitBreakerWarnThreshold-1; i++ {
			if got := b.observe(tid); got != tierNone {
				t.Errorf("trace %x at span %d: got %v, want tierNone",
					traceByte, i, got)
			}
		}
	}
	if size := b.size(); size != 3 {
		t.Errorf("expected 3 trace IDs tracked, got %d", size)
	}
}

// TestTraceBreaker_AccumulatesAcrossInvocations is the headline gap
// closure — multiple observe() calls totaling 600K spans on the same
// trace_id MUST trip the breaker, even if each individual call would
// be under 50K.
func TestTraceBreaker_AccumulatesAcrossInvocations(t *testing.T) {
	b := newTraceBreaker(time.Hour)
	tid := mkTraceID(0xff)

	// Three batches of 200K = 600K total.
	const batchSize = 200_000
	transitions := []traceTier{}
	for batch := 0; batch < 3; batch++ {
		for i := 0; i < batchSize; i++ {
			if tier := b.observe(tid); tier != tierNone {
				transitions = append(transitions, tier)
			}
		}
	}
	// We expect three transitions, in order: warn → truncate → breaker.
	if len(transitions) != 3 {
		t.Fatalf("expected 3 tier transitions, got %d: %v", len(transitions), transitions)
	}
	want := []traceTier{tierWarn, tierTruncate, tierBreaker}
	for i, tier := range transitions {
		if tier != want[i] {
			t.Errorf("transition %d: got %v, want %v", i, tier, want[i])
		}
	}
}

// TestTraceBreaker_PruneStaleResetsCounters verifies the TTL behaviour:
// a trace that has not been observed for `> ttl` is fully evicted, and
// a fresh observation starts from zero (no leftover state).
func TestTraceBreaker_PruneStaleResetsCounters(t *testing.T) {
	b := newTraceBreaker(10 * time.Second)
	now := time.Now()
	b.nowFn = func() time.Time { return now }

	tid := mkTraceID(0x11)
	for i := int64(0); i < CircuitBreakerTruncateThreshold; i++ {
		_ = b.observe(tid)
	}
	if !b.shouldTruncate(tid) {
		t.Fatalf("should be in truncate tier before prune")
	}

	// Advance time past TTL.
	now = now.Add(20 * time.Second)
	evicted := b.pruneStale()
	if evicted != 1 {
		t.Errorf("expected 1 entry evicted, got %d", evicted)
	}
	if b.shouldTruncate(tid) {
		t.Errorf("should not be in truncate tier after prune")
	}
	if size := b.size(); size != 0 {
		t.Errorf("size after prune: got %d, want 0", size)
	}

	// Fresh observe starts from zero.
	if got := b.observe(tid); got != tierNone {
		t.Errorf("post-prune observe: got %v, want tierNone (fresh counter)", got)
	}
}

// TestTraceBreaker_PruneStaleKeepsRecentEntries pins the negative half
// — entries observed within the TTL window are NOT evicted.
func TestTraceBreaker_PruneStaleKeepsRecentEntries(t *testing.T) {
	b := newTraceBreaker(time.Hour)
	now := time.Now()
	b.nowFn = func() time.Time { return now }

	for traceByte := byte(0x20); traceByte < 0x23; traceByte++ {
		_ = b.observe(mkTraceID(traceByte))
	}
	now = now.Add(30 * time.Minute)
	if evicted := b.pruneStale(); evicted != 0 {
		t.Errorf("recent entries should not be evicted; got %d", evicted)
	}
	if size := b.size(); size != 3 {
		t.Errorf("size after pruneStale of recent entries: got %d, want 3", size)
	}
}

// TestTraceBreaker_ConcurrentObserveIsSafe pins the sync.Mutex
// contract — many goroutines hammering observe() on the same trace_id
// must produce the same total count as serial execution.
func TestTraceBreaker_ConcurrentObserveIsSafe(t *testing.T) {
	b := newTraceBreaker(time.Hour)
	tid := mkTraceID(0x77)

	const goroutines = 16
	const perGoroutine = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				_ = b.observe(tid)
			}
		}()
	}
	wg.Wait()

	expected := int64(goroutines * perGoroutine)
	b.mu.Lock()
	got := b.per[tid].count
	b.mu.Unlock()
	if got != expected {
		t.Errorf("concurrent count: got %d, want %d", got, expected)
	}
}
