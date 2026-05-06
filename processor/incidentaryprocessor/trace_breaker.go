// Cross-batch trace circuit breaker.
//
// The Bridge already enforces a per-trace span ceiling (50K) inside
// the buffer itself. That ceiling is per-`ConsumeTraces` invocation,
// so a malicious or runaway emitter that fragments a 1M-span trace
// across many small ResourceSpans batches sails past it: each batch
// looks innocent on its own.
//
// This file adds an *accumulator* keyed on trace_id that survives
// across batches. Whenever the cumulative span count for a trace
// crosses one of three tiers, the processor:
//
//   - 5K  (warn):     log + Prometheus counter, no behaviour change
//   - 50K (truncate): drop subsequent spans for this trace_id
//   - 500K (breaker): blacklist the trace until TTL expiry
//
// The map is pruned by the existing pruneLoop every minute. Entries
// older than `2 * pre_alert_window` are removed — long enough that a
// legitimate slow trace finishes and reports cleanly, short enough
// that a runaway ID does not consume memory indefinitely.
//
// We mirror the upstream tail-sampling processor's shape
// (`map[pcommon.TraceID]*counter`) rather than reaching for a
// general-purpose circuit breaker library: the per-traceID
// accumulation is not what gobreaker / cep21/circuit model.

package incidentaryprocessor

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
)

const (
	// CircuitBreakerWarnThreshold — surface a metric, take no action.
	// Operators investigating "why is my trace assembly slow?" see
	// this counter increment and know which trace IDs are big.
	CircuitBreakerWarnThreshold int64 = 5_000

	// CircuitBreakerTruncateThreshold — stop adding spans to the
	// per-trace buffer. The first 50K spans are still forwarded if a
	// trigger fires; the rest are silently dropped.
	CircuitBreakerTruncateThreshold int64 = 50_000

	// CircuitBreakerHardThreshold — fully drop subsequent spans for
	// this trace_id. The trace is blacklisted in the breaker map
	// until pruning evicts it.
	CircuitBreakerHardThreshold int64 = 500_000
)

// traceTier describes which threshold a trace has crossed.
type traceTier int

const (
	tierNone traceTier = iota
	tierWarn
	tierTruncate
	tierBreaker
)

// String formats the tier as the metric label value.
func (t traceTier) String() string {
	switch t {
	case tierWarn:
		return "warn"
	case tierTruncate:
		return "truncate"
	case tierBreaker:
		return "breaker"
	}
	return "none"
}

// traceCounterEntry is one row in the breaker's per-trace map.
type traceCounterEntry struct {
	count    int64
	lastSeen time.Time
	// highestTier — the maximum tier this trace has crossed. We use
	// this so each transition (none→warn, warn→truncate,
	// truncate→breaker) emits a metric exactly once per trace per
	// TTL window, not once per span past the threshold.
	highestTier traceTier
}

// traceBreaker accumulates span counts per trace_id across
// ConsumeTraces invocations. Methods are safe for concurrent use.
type traceBreaker struct {
	mu  sync.Mutex
	per map[pcommon.TraceID]*traceCounterEntry
	ttl time.Duration
	// nowFn is the time source. Tests inject a mock; production uses
	// time.Now.
	nowFn func() time.Time
}

// newTraceBreaker returns a breaker with the given TTL. `ttl` is
// typically 2 * the pre-alert window so legitimate slow traces have
// enough time to finish before their counter resets.
func newTraceBreaker(ttl time.Duration) *traceBreaker {
	return &traceBreaker{
		per:   make(map[pcommon.TraceID]*traceCounterEntry),
		ttl:   ttl,
		nowFn: time.Now,
	}
}

// observe increments the cumulative span count for `tid` and returns
// the tier transition (if any). Returns `tierNone` when no new
// threshold was crossed by this call.
func (b *traceBreaker) observe(tid pcommon.TraceID) traceTier {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.nowFn()
	e, ok := b.per[tid]
	if !ok {
		e = &traceCounterEntry{}
		b.per[tid] = e
	}
	e.count++
	e.lastSeen = now

	tier := tierFromCount(e.count)
	if tier > e.highestTier {
		// Transition — caller should emit the corresponding metric.
		e.highestTier = tier
		return tier
	}
	return tierNone
}

// shouldDrop returns true if the trace has crossed the hard
// (breaker) threshold and subsequent spans should be silently
// dropped.
func (b *traceBreaker) shouldDrop(tid pcommon.TraceID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.per[tid]
	if !ok {
		return false
	}
	return e.highestTier >= tierBreaker
}

// shouldTruncate returns true if the trace has crossed the truncate
// threshold but not yet the breaker threshold. Spans past this point
// are dropped from the buffer but counted/metric'd.
func (b *traceBreaker) shouldTruncate(tid pcommon.TraceID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.per[tid]
	if !ok {
		return false
	}
	return e.highestTier >= tierTruncate
}

// pruneStale removes entries whose lastSeen is older than `ttl`.
// Called from the processor's pruneLoop. Returns the number of
// entries evicted (useful for logging).
func (b *traceBreaker) pruneStale() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.nowFn()
	evicted := 0
	for tid, e := range b.per {
		if now.Sub(e.lastSeen) > b.ttl {
			delete(b.per, tid)
			evicted++
		}
	}
	return evicted
}

// size returns the current number of tracked trace IDs. Useful for
// the dlq_size-equivalent observability gauge if we want to expose
// it later.
func (b *traceBreaker) size() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.per)
}

// tierFromCount maps a cumulative count to the highest tier crossed.
func tierFromCount(count int64) traceTier {
	switch {
	case count >= CircuitBreakerHardThreshold:
		return tierBreaker
	case count >= CircuitBreakerTruncateThreshold:
		return tierTruncate
	case count >= CircuitBreakerWarnThreshold:
		return tierWarn
	default:
		return tierNone
	}
}

// recordTransition emits the Prometheus metric for the given tier
// crossing. Wraps the nil-safe Telemetry call so the call sites in
// processor.go don't have to know about the metric details.
func (p *tracesProcessor) recordTraceBreakerTransition(ctx context.Context, tier traceTier) {
	switch tier {
	case tierWarn, tierTruncate, tierBreaker:
		p.tel.RecordCircuitBreakerOpen(ctx, tier.String())
	}
}
