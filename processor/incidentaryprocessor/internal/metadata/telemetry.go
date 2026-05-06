// Package metadata exposes the Bridge processor's structured telemetry
// counters/gauges as a single typed struct.
//
// We hand-roll the struct rather than depending on `mdatagen` codegen
// because (a) the metrics list is small (6 series) and (b) the chart
// uses our own dependency surface — adding `mdatagen` to the build
// tooling for ~80 lines of plumbing is overhead we don't earn back.
// The shape mirrors what mdatagen would emit so a future migration is
// a drop-in replacement.
//
// Usage in the processor:
//
//	tel, err := metadata.NewTelemetry(set.TelemetrySettings)
//	if err != nil { return nil, err }
//	tel.RecordDLQEnqueued(ctx, "forward_failed")
package metadata

import (
	"context"
	"fmt"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// scopeName is the OTel meter scope for the Bridge processor's
// telemetry. Mirrors the import path so dashboards can group by it.
const scopeName = "github.com/incidentary/opentelemetry-collector-incidentary/processor/incidentaryprocessor"

// Telemetry holds the typed metric instruments the processor records
// against. Construction wires each instrument against the
// `MeterProvider` exposed via `component.TelemetrySettings`. Recording
// is a single Add()/Record() call from each instrumentation site.
type Telemetry struct {
	// DLQEnqueuedTotal — number of failed-forward batches pushed to
	// the in-memory DLQ. Labels: `reason` (`forward_failed`,
	// `circuit_breaker_open`, etc).
	DLQEnqueuedTotal metric.Int64Counter

	// DLQDroppedTotal — number of DLQ entries evicted (FIFO when
	// full) or abandoned (after `dlqRetryMaxAttempts`). Labels:
	// `reason` (`fifo_evict`, `max_attempts_exceeded`).
	DLQDroppedTotal metric.Int64Counter

	// DLQFlushedTotal — number of DLQ entries successfully forwarded
	// after retry. Labels: `outcome` (`success`).
	DLQFlushedTotal metric.Int64Counter

	// CircuitBreakerOpenTotal — fires when the per-trace span count
	// crosses one of the cumulative tier thresholds. Labels: `tier`
	// (`warn`, `truncate`, `breaker`).
	CircuitBreakerOpenTotal metric.Int64Counter

	// TriggerFiredTotal — fires when one of the four-trigger
	// evaluators decides a trace warrants forwarding. Labels:
	// `trigger` (`error_rate_5xx`, `slow_success_rate`,
	// `retry_rate`, `in_flight_pileup`), `severity` (`mild`,
	// `severe`).
	TriggerFiredTotal metric.Int64Counter

	// dlqSize — observable gauge sampled on the same cadence as the
	// DLQ retry tick. The processor sets a callback that returns the
	// current `len(p.dlq)` under lock.
	DLQSize metric.Int64ObservableGauge

	// dlqSizeFn is the callback returned by RegisterDLQSizeCallback.
	// It is owned by the processor and invoked synchronously by the
	// SDK each time a metric reader collects.
	dlqSizeRegistration metric.Registration
}

// NewTelemetry wires every counter/gauge against the MeterProvider in
// `set`. Pass `set` from `processor.Settings.TelemetrySettings`.
//
// The collector framework's default MeterProvider exports to whichever
// telemetry pipeline the operator configured (Prometheus by default in
// the contrib distribution); this function does not depend on the
// transport — it just registers instruments at the OTel SDK layer.
func NewTelemetry(set component.TelemetrySettings) (*Telemetry, error) {
	if set.MeterProvider == nil {
		return nil, fmt.Errorf("metadata.NewTelemetry: MeterProvider is nil")
	}
	meter := set.MeterProvider.Meter(scopeName)

	enq, err := meter.Int64Counter(
		"incidentary_processor_dlq_enqueued_total",
		metric.WithDescription("Failed-forward batches pushed to the in-memory DLQ."),
		metric.WithUnit("{batches}"),
	)
	if err != nil {
		return nil, fmt.Errorf("dlq_enqueued_total: %w", err)
	}
	dropped, err := meter.Int64Counter(
		"incidentary_processor_dlq_dropped_total",
		metric.WithDescription("DLQ entries evicted (FIFO) or abandoned after max attempts."),
		metric.WithUnit("{batches}"),
	)
	if err != nil {
		return nil, fmt.Errorf("dlq_dropped_total: %w", err)
	}
	flushed, err := meter.Int64Counter(
		"incidentary_processor_dlq_flushed_total",
		metric.WithDescription("DLQ entries successfully forwarded after retry."),
		metric.WithUnit("{batches}"),
	)
	if err != nil {
		return nil, fmt.Errorf("dlq_flushed_total: %w", err)
	}
	cb, err := meter.Int64Counter(
		"incidentary_processor_circuit_breaker_open_total",
		metric.WithDescription("Circuit breaker tier fires (warn/truncate/breaker)."),
		metric.WithUnit("{events}"),
	)
	if err != nil {
		return nil, fmt.Errorf("circuit_breaker_open_total: %w", err)
	}
	trig, err := meter.Int64Counter(
		"incidentary_processor_trigger_fired_total",
		metric.WithDescription("Four-trigger evaluator decided a trace warrants forwarding."),
		metric.WithUnit("{events}"),
	)
	if err != nil {
		return nil, fmt.Errorf("trigger_fired_total: %w", err)
	}
	dlqSize, err := meter.Int64ObservableGauge(
		"incidentary_processor_dlq_size",
		metric.WithDescription("Current DLQ depth (entries waiting for retry)."),
		metric.WithUnit("{batches}"),
	)
	if err != nil {
		return nil, fmt.Errorf("dlq_size: %w", err)
	}

	return &Telemetry{
		DLQEnqueuedTotal:        enq,
		DLQDroppedTotal:         dropped,
		DLQFlushedTotal:         flushed,
		CircuitBreakerOpenTotal: cb,
		TriggerFiredTotal:       trig,
		DLQSize:                 dlqSize,
	}, nil
}

// RecordDLQEnqueued emits one `dlq_enqueued_total` increment with the
// given reason.
func (t *Telemetry) RecordDLQEnqueued(ctx context.Context, reason string) {
	if t == nil {
		return
	}
	t.DLQEnqueuedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// RecordDLQDropped emits one `dlq_dropped_total` increment.
func (t *Telemetry) RecordDLQDropped(ctx context.Context, reason string) {
	if t == nil {
		return
	}
	t.DLQDroppedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

// RecordDLQFlushed emits one `dlq_flushed_total` increment.
func (t *Telemetry) RecordDLQFlushed(ctx context.Context, outcome string) {
	if t == nil {
		return
	}
	t.DLQFlushedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// RecordCircuitBreakerOpen emits one `circuit_breaker_open_total`
// increment for the given tier (`warn`, `truncate`, `breaker`).
func (t *Telemetry) RecordCircuitBreakerOpen(ctx context.Context, tier string) {
	if t == nil {
		return
	}
	t.CircuitBreakerOpenTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("tier", tier)))
}

// RecordTriggerFired emits one `trigger_fired_total` increment.
func (t *Telemetry) RecordTriggerFired(ctx context.Context, trigger, severity string) {
	if t == nil {
		return
	}
	t.TriggerFiredTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("trigger", trigger),
		attribute.String("severity", severity),
	))
}

// RegisterDLQSizeCallback wires a value-source for the DLQ size
// observable gauge. Pass a function that snapshots `len(p.dlq)` under
// lock; the SDK invokes it on every metric collection. Returns an
// error from the underlying registration; the processor logs but
// continues on failure (the gauge becomes a no-op without affecting
// other instruments).
func (t *Telemetry) RegisterDLQSizeCallback(
	meter metric.Meter,
	getSize func() int64,
) error {
	if t == nil {
		return nil
	}
	reg, err := meter.RegisterCallback(
		func(_ context.Context, observer metric.Observer) error {
			observer.ObserveInt64(t.DLQSize, getSize())
			return nil
		},
		t.DLQSize,
	)
	if err != nil {
		return fmt.Errorf("register dlq_size callback: %w", err)
	}
	t.dlqSizeRegistration = reg
	return nil
}

// Unregister releases the DLQ size callback. Call from `Shutdown`.
func (t *Telemetry) Unregister() error {
	if t == nil || t.dlqSizeRegistration == nil {
		return nil
	}
	return t.dlqSizeRegistration.Unregister()
}

// Meter returns the meter scoped to this processor — useful for
// callers that need to register additional callbacks (e.g. the
// processor's RegisterDLQSizeCallback).
func Meter(set component.TelemetrySettings) metric.Meter {
	return set.MeterProvider.Meter(scopeName)
}
