package incidentaryprocessor

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor"
	"go.uber.org/zap"

	"github.com/incidentary/opentelemetry-collector-incidentary/internal/buffer"
	"github.com/incidentary/opentelemetry-collector-incidentary/internal/forward"
	"github.com/incidentary/opentelemetry-collector-incidentary/internal/triggers"
	"github.com/incidentary/opentelemetry-collector-incidentary/processor/incidentaryprocessor/internal/metadata"
)

// tracesProcessor is the runtime instance of the Bridge:
//   * Buffers every incoming span by trace_id with time + count eviction.
//   * Feeds completed-span signals to the four-trigger evaluator.
//   * On any trigger fire, drains the matching trace and forwards it.
//   * Always pass-through to the next consumer when configured to.
type tracesProcessor struct {
	cfg    *Config
	logger *zap.Logger

	buf       *buffer.Ring
	evaluator *triggers.Evaluator
	fwd       *forward.Client

	next consumer.Traces

	prune    *time.Ticker
	stopOnce sync.Once
	stopCh   chan struct{}

	// Bounded in-memory dead-letter queue. When `SendTraces` returns
	// an error the failed batch is pushed to this queue so a retry
	// loop can attempt to forward it later. The queue is capped at
	// `dlqMaxEntries` to prevent runaway memory growth during
	// sustained backend outages — once full, the oldest entry is
	// evicted (and a metric increments) rather than blocking the
	// ingest path.
	dlq        []dlqEntry
	dlqMu      sync.Mutex
	dlqRetry   *time.Ticker

	// Typed Prometheus telemetry. Construction happens in
	// `newTracesProcessor`; the DLQ size observable gauge callback is
	// registered in `Start` and unregistered in `Shutdown`. nil-safe
	// Every Record method on `*Telemetry` is a no-op when the
	// receiver is nil, so a failure during construction degrades to
	// "no metrics" rather than crashing the processor.
	tel       *metadata.Telemetry
	telSet    component.TelemetrySettings

	// Cumulative per-trace span counter for the cross-batch circuit
	// breaker. Pruned by `pruneLoop` every minute; entries older than
	// 2x pre-alert window are evicted so a long-running benign trace
	// eventually gets a fresh budget.
	breaker *traceBreaker
}

// dlqEntry is a single failed forward attempt awaiting retry.
type dlqEntry struct {
	td        ptrace.Traces
	enqueueAt time.Time
	attempts  int
}

// In-memory DLQ tuning.
//
// The capacity is intentionally small: the DLQ is a *short-term*
// resilience layer, not a durable queue. Beyond this many failed
// flushes the operator has bigger problems (sustained backend
// outage, misconfigured token), and durability is the correct
// response — not retaining megabytes of in-process span data.
const (
	dlqMaxEntries        = 32
	dlqRetryInterval     = 30 * time.Second
	dlqRetryMaxAttempts  = 5
)

func newTracesProcessor(
	_ context.Context,
	set processor.Settings,
	cfg *Config,
	next consumer.Traces,
) (processor.Traces, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	logger := set.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	ev := triggers.New(triggers.Config{
		ErrorRate5xxHigh:    cfg.Triggers.ErrorRate5xx.High,
		ErrorRate5xxLow:     cfg.Triggers.ErrorRate5xx.Low,
		SlowSuccessHigh:     cfg.Triggers.SlowSuccessRate.High,
		SlowSuccessMild:     cfg.Triggers.SlowSuccessRate.Mild,
		RetryHigh:           cfg.Triggers.RetryRate.High,
		RetryMild:           cfg.Triggers.RetryRate.Mild,
		InFlightMinAbsolute: cfg.Triggers.InFlightPileup.MinAbsoluteInFlight,
		InFlightMultiplier:  cfg.Triggers.InFlightPileup.BaselineMultiplier,
		InFlightNetGrowth:   cfg.Triggers.InFlightPileup.NetGrowthMin,
		SevereHoldSecs:      cfg.Triggers.InFlightPileup.SevereHoldSecs,
		MildHoldSecs:        cfg.Triggers.InFlightPileup.MildHoldSecs,
	}, nil)

	buf := buffer.New(buffer.Options{
		MaxSpans:       cfg.MaxBufferSize,
		PreAlertWindow: cfg.PreAlertWindow,
	})

	fwd, err := forward.NewClient(forward.Config{
		Endpoint:     cfg.Forward.IncidentaryEndpoint,
		APIKey:       cfg.Forward.IncidentaryToken,
		AgentVersion: "0.1.0",
		Timeout:      cfg.Forward.Timeout,
		Logger:       logger,
	})
	if err != nil {
		return nil, fmt.Errorf("incidentaryprocessor: forwarder: %w", err)
	}

	tel, telErr := metadata.NewTelemetry(set.TelemetrySettings)
	if telErr != nil {
		// Telemetry construction is non-fatal: the processor still
		// runs without metrics. The Record* methods on a nil receiver
		// are no-ops by design, so downstream call sites need no
		// branching.
		logger.Warn("incidentary processor telemetry init failed; metrics disabled",
			zap.Error(telErr),
		)
		tel = nil
	}

	return &tracesProcessor{
		cfg:       cfg,
		logger:    logger,
		buf:       buf,
		evaluator: ev,
		fwd:       fwd,
		next:      next,
		stopCh:    make(chan struct{}),
		tel:       tel,
		telSet:    set.TelemetrySettings,
		breaker:   newTraceBreaker(2 * cfg.PreAlertWindow),
	}, nil
}

func (p *tracesProcessor) Start(_ context.Context, _ component.Host) error {
	p.prune = time.NewTicker(time.Minute)
	go p.pruneLoop()
	// In-memory DLQ resilience — start the DLQ retry loop.
	p.dlqRetry = time.NewTicker(dlqRetryInterval)
	go p.dlqRetryLoop()

	// Register the DLQ size observable gauge
	// callback. Failures degrade the gauge to a no-op without
	// affecting other instruments.
	if p.tel != nil {
		if err := p.tel.RegisterDLQSizeCallback(metadata.Meter(p.telSet), p.dlqSize); err != nil {
			p.logger.Warn("dlq_size gauge callback registration failed",
				zap.Error(err),
			)
		}
	}

	// Restore any DLQ entries persisted from a
	// previous Shutdown. Failures here are non-fatal: we log and
	// continue with an empty in-memory queue.
	if path := p.cfg.DLQ.PersistPath; path != "" {
		entries, err := loadDLQ(path)
		if err != nil {
			p.logger.Warn("DLQ persistence load failed; starting with empty queue",
				zap.Error(err),
				zap.String("persist_path", path),
				zap.Int("partial_recovered", len(entries)),
			)
		}
		if len(entries) > 0 {
			p.dlqMu.Lock()
			// Cap-respecting append — operator could have changed
			// dlqMaxEntries between runs.
			for i := 0; i < len(entries) && len(p.dlq) < dlqMaxEntries; i++ {
				p.dlq = append(p.dlq, entries[i])
			}
			p.dlqMu.Unlock()
			p.logger.Info("DLQ restored from disk",
				zap.Int("recovered", len(entries)),
				zap.String("persist_path", path),
			)
		}
	}
	// Log only the host of the forward
	// endpoint at Info. Self-hosted deployments often point at an
	// internal ingress; replicating the full URL into structured
	// logs may hand network topology to anyone with log read access.
	endpointHost := forwardEndpointHost(p.cfg.Forward.IncidentaryEndpoint)
	p.logger.Info("incidentary processor started",
		zap.Duration("pre_alert_window", p.cfg.PreAlertWindow),
		zap.Int("max_buffer_size", p.cfg.MaxBufferSize),
		zap.String("forward_endpoint_host", endpointHost),
		zap.Bool("pass_through", p.cfg.Forward.PassThrough),
		zap.Int("dlq_max_entries", dlqMaxEntries),
	)
	p.logger.Debug("incidentary processor full forward endpoint",
		zap.String("forward_endpoint", p.cfg.Forward.IncidentaryEndpoint),
	)
	return nil
}

// enqueueDLQ pushes a failed-forward batch to the in-memory DLQ.
// When the queue is full the oldest entry is evicted (FIFO) and a
// loud Error is logged so on-call can attribute the data loss.
//
// In-memory DLQ resilience.
func (p *tracesProcessor) enqueueDLQ(td ptrace.Traces) {
	p.dlqMu.Lock()
	defer p.dlqMu.Unlock()
	if len(p.dlq) >= dlqMaxEntries {
		dropped := p.dlq[0]
		p.dlq = p.dlq[1:]
		p.logger.Error("DLQ full; dropping oldest failed batch",
			zap.Time("dropped_enqueue_at", dropped.enqueueAt),
			zap.Int("dropped_attempts", dropped.attempts),
			zap.Int("dlq_size", len(p.dlq)),
		)
		// Surface the eviction as a counter so the
		// dashboard can attribute data loss without parsing logs.
		p.tel.RecordDLQDropped(context.Background(), "fifo_evict")
	}
	p.dlq = append(p.dlq, dlqEntry{
		td:        td,
		enqueueAt: time.Now(),
		attempts:  0,
	})
	p.tel.RecordDLQEnqueued(context.Background(), "forward_failed")
}

// dlqRetryLoop drains the DLQ on every tick. Each entry is retried
// once per cycle; after `dlqRetryMaxAttempts` attempts an entry is
// abandoned with an Error log. Successful flushes are removed.
//
// In-memory DLQ resilience.
func (p *tracesProcessor) dlqRetryLoop() {
	for {
		select {
		case <-p.stopCh:
			return
		case <-p.dlqRetry.C:
			p.drainDLQOnce()
		}
	}
}

func (p *tracesProcessor) drainDLQOnce() {
	p.dlqMu.Lock()
	if len(p.dlq) == 0 {
		p.dlqMu.Unlock()
		return
	}
	// Snapshot the queue and clear it; we'll re-enqueue any retries
	// that still fail. Holding the lock during HTTP calls would
	// block ConsumeTraces, which is unacceptable on the hot path.
	pending := p.dlq
	p.dlq = nil
	p.dlqMu.Unlock()

	var stillFailing []dlqEntry
	for _, entry := range pending {
		entry.attempts++
		ctx, cancel := context.WithTimeout(context.Background(), p.cfg.Forward.Timeout)
		err := p.fwd.SendTraces(ctx, entry.td)
		cancel()
		if err == nil {
			p.logger.Info("DLQ retry succeeded",
				zap.Int("attempts", entry.attempts),
				zap.Duration("queued_for", time.Since(entry.enqueueAt)),
			)
			// Successful retry counter.
			p.tel.RecordDLQFlushed(context.Background(), "success")
			continue
		}
		if entry.attempts >= dlqRetryMaxAttempts {
			p.logger.Error("DLQ entry abandoned after max attempts",
				zap.Error(err),
				zap.Int("attempts", entry.attempts),
				zap.Duration("queued_for", time.Since(entry.enqueueAt)),
			)
			// Abandoned entry counter.
			p.tel.RecordDLQDropped(context.Background(), "max_attempts_exceeded")
			continue
		}
		stillFailing = append(stillFailing, entry)
	}

	if len(stillFailing) > 0 {
		p.dlqMu.Lock()
		// Prepend the still-failing entries so the FIFO eviction
		// policy targets the oldest first if new failures land.
		p.dlq = append(stillFailing, p.dlq...)
		// Cap again in case ConsumeTraces appended while we were
		// flushing.
		for len(p.dlq) > dlqMaxEntries {
			dropped := p.dlq[0]
			p.dlq = p.dlq[1:]
			p.logger.Error("DLQ full after retry merge; dropping oldest",
				zap.Time("dropped_enqueue_at", dropped.enqueueAt),
				zap.Int("dropped_attempts", dropped.attempts),
			)
		}
		p.dlqMu.Unlock()
	}
}

// forwardEndpointHost extracts the host (no scheme, no path, no
// query) from the configured endpoint URL. Returns "<unparseable>"
// if the URL is malformed; Validate already rejected those at config
// load, but the extraction is defensive.
func forwardEndpointHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "<unparseable>"
	}
	return u.Host
}

func (p *tracesProcessor) Shutdown(_ context.Context) error {
	p.stopOnce.Do(func() {
		if p.prune != nil {
			p.prune.Stop()
		}
		if p.dlqRetry != nil {
			p.dlqRetry.Stop()
		}
		// Release the DLQ size gauge callback so
		// the SDK does not invoke it after the processor's queue is
		// gone.
		if p.tel != nil {
			_ = p.tel.Unregister()
		}
		// Persist any pending DLQ entries so the
		// next Start() can restore them. Best-effort: a failed save
		// only loses the queue (which would have been lost anyway
		// without persistence), it does not block shutdown.
		if path := p.cfg.DLQ.PersistPath; path != "" {
			p.dlqMu.Lock()
			snapshot := make([]dlqEntry, len(p.dlq))
			copy(snapshot, p.dlq)
			p.dlqMu.Unlock()
			if err := saveDLQ(path, snapshot); err != nil {
				p.logger.Warn("DLQ persistence save failed",
					zap.Error(err),
					zap.String("persist_path", path),
					zap.Int("entries", len(snapshot)),
				)
			} else if len(snapshot) > 0 {
				p.logger.Info("DLQ persisted to disk",
					zap.Int("entries", len(snapshot)),
					zap.String("persist_path", path),
				)
			}
		}
		close(p.stopCh)
	})
	return nil
}

// dlqSize snapshots the current DLQ depth under lock for the
// observable gauge callback.
func (p *tracesProcessor) dlqSize() int64 {
	p.dlqMu.Lock()
	defer p.dlqMu.Unlock()
	return int64(len(p.dlq))
}

// Capabilities reports whether ConsumeTraces mutates its input.
//
// The Bridge does not mutate spans — it only inspects them. Returning
// MutatesData=false lets the collector skip the defensive deep-copy.
func (p *tracesProcessor) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

// ConsumeTraces is the hot path: per-span buffering + trigger eval.
//
// We walk the resource/scope/span tree once. Per ResourceSpans, we
// extract service.name (used as the per-service trigger key). Per span,
// we (a) update the trigger evaluator and (b) record the span in the
// per-trace_id ring buffer. After the walk, any trace whose triggers
// fired is drained and sent via the forwarder.
func (p *tracesProcessor) ConsumeTraces(ctx context.Context, td ptrace.Traces) error {
	rss := td.ResourceSpans()
	flushTargets := make(map[pcommon.TraceID]struct{})
	var lastForwardErr error
	// Track span IDs we have already
	// evaluated within THIS ConsumeTraces invocation. Fan-out
	// pipelines (two receivers feeding the same processor) may
	// duplicate every span; without dedup the in-flight pileup
	// trigger fires on doubled signals.
	seenSpans := make(map[pcommon.SpanID]struct{})

	for i := 0; i < rss.Len(); i++ {
		rs := rss.At(i)
		service := serviceName(rs.Resource().Attributes())

		// Buffer every span by trace_id. Iterate scope/spans to find
		// the trace IDs present in this ResourceSpans.
		traceSet := traceIDsInResource(rs)
		for tid := range traceSet {
			single := isolateTrace(rs, tid)
			p.buf.Insert(tid, single)
		}

		// Evaluate triggers on every span (server/client kinds).
		ss := rs.ScopeSpans()
		for j := 0; j < ss.Len(); j++ {
			scope := ss.At(j)
			spans := scope.Spans()
			for k := 0; k < spans.Len(); k++ {
				span := spans.At(k)
				// Skip span IDs we
				// already evaluated this invocation. Duplicate
				// ResourceSpans (fan-out pipelines) would otherwise
				// double-count toward in-flight metrics.
				sid := span.SpanID()
				if _, dup := seenSpans[sid]; dup {
					continue
				}
				seenSpans[sid] = struct{}{}

				// Cross-batch breaker. Observe
				// the span before evaluation so even spans that
				// would not fire a trigger still count toward the
				// per-trace cumulative cap. Tier transitions
				// (warn/truncate/breaker) emit a Prometheus counter.
				if p.breaker != nil {
					if tier := p.breaker.observe(span.TraceID()); tier != tierNone {
						p.recordTraceBreakerTransition(ctx, tier)
						p.logger.Warn("trace circuit breaker tier crossed",
							zap.String("tier", tier.String()),
							zap.String("trace_id", traceIDHex(span.TraceID())),
						)
					}
					if p.breaker.shouldDrop(span.TraceID()) {
						// Hard breaker tier — silently skip this
						// span. The metric was already emitted on
						// the transition; per-span emissions would
						// flood the dashboard.
						continue
					}
				}

				reason := p.evaluateSpan(service, span)
				if reason != nil {
					flushTargets[span.TraceID()] = struct{}{}
					p.logger.Debug("trigger fired",
						zap.String("service", reason.Service),
						zap.String("trigger", string(reason.TriggerType)),
						zap.String("severity", string(reason.Severity)),
					)
					// Surface trigger fires as
					// dashboard-visible counters.
					p.tel.RecordTriggerFired(ctx,
						string(reason.TriggerType),
						string(reason.Severity),
					)
				}
			}
		}
	}

	for tid := range flushTargets {
		drained := p.buf.Drain(tid)
		if len(drained) == 0 {
			continue
		}
		spanCount := 0
		out := ptrace.NewTraces()
		for _, rs := range drained {
			rs.CopyTo(out.ResourceSpans().AppendEmpty())
			ss := rs.ScopeSpans()
			for j := 0; j < ss.Len(); j++ {
				spanCount += ss.At(j).Spans().Len()
			}
		}
		if err := p.fwd.SendTraces(ctx, out); err != nil {
			// In-memory DLQ resilience — push the failed batch to
			// the in-memory DLQ for asynchronous retry, AND log at
			// Error level so on-call sees the failure immediately.
			// The DLQ is bounded; sustained outages will eventually
			// drop the oldest entries (also logged Error).
			p.logger.Error("flush forward failed; queued for DLQ retry",
				zap.Error(err),
				zap.String("trace_id", traceIDHex(tid)),
				zap.Int("queued_spans", spanCount),
			)
			p.enqueueDLQ(out)
			// Hardening: — record the most recent
			// forward error so the post-loop decision (whether to
			// signal back-pressure to the upstream receiver) can
			// take it into account.
			lastForwardErr = err
		}
	}

	if p.cfg.Forward.PassThrough && p.next != nil {
		// PassThrough=true is the SLA contract — the upstream
		// receiver only cares that the next consumer accepts the
		// batch. Forward failures are best-effort and are already
		// logged at Error above, so we return whatever the next
		// consumer says (success or its own error).
		return p.next.ConsumeTraces(ctx, td)
	}

	// PassThrough=false: the forwarder is the sole delivery path.
	// Hardening: — return the forward error so the
	// upstream collector framework can apply its retry/back-pressure
	// instead of marking the batch as silently delivered.
	if lastForwardErr != nil {
		return lastForwardErr
	}
	return nil
}

func (p *tracesProcessor) evaluateSpan(service string, span ptrace.Span) *triggers.FlushReason {
	kind := mapSpanKind(span.Kind())
	if kind == triggers.SignalOther || service == "" {
		return nil
	}
	durationNs := int64(span.EndTimestamp().AsTime().Sub(span.StartTimestamp().AsTime()))
	// Reject spans whose duration is
	// negative (end < start, broken instrumentation) or absurdly
	// large (year 2050+ start/end timestamps from a misconfigured
	// SDK clock). Without this, the slow-success EWMA in
	// triggers.SuccessWindow saturates at maxBaselineNs (1 hour) and
	// the slow-success trigger silently disables itself for the
	// affected service.
	if durationNs < 0 || durationNs > int64(time.Hour) {
		p.logger.Warn("rejecting span with out-of-range duration",
			zap.Int64("duration_ns", durationNs),
			zap.String("service", service),
		)
		return nil
	}
	sig := triggers.RequestSignal{
		Kind:                 kind,
		StatusCode:           statusCode(span),
		DurationNs:           durationNs,
		OutboundRetryKeyHash: outboundRetryKey(span),
		NowUnixMs:            time.Now().UnixMilli(),
	}
	if kind == triggers.SignalServer {
		p.evaluator.OnRequestStart(service)
	}
	return p.evaluator.OnRequestComplete(service, sig)
}

func (p *tracesProcessor) pruneLoop() {
	for {
		select {
		case <-p.stopCh:
			return
		case <-p.prune.C:
			p.buf.PruneByTime()
			p.evaluator.PruneStale()
			// Evict trace IDs whose cumulative
			// counter has gone idle past 2 * pre_alert_window.
			if p.breaker != nil {
				if evicted := p.breaker.pruneStale(); evicted > 0 {
					p.logger.Debug("pruned stale trace breaker entries",
						zap.Int("evicted", evicted),
					)
				}
			}
		}
	}
}

// --- helpers -----------------------------------------------------------------

func serviceName(attrs pcommon.Map) string {
	v, ok := attrs.Get("service.name")
	if !ok {
		return ""
	}
	return v.AsString()
}

func mapSpanKind(k ptrace.SpanKind) triggers.SignalKind {
	switch k {
	case ptrace.SpanKindServer:
		return triggers.SignalServer
	case ptrace.SpanKindClient:
		return triggers.SignalClient
	}
	return triggers.SignalOther
}

func statusCode(span ptrace.Span) int {
	for _, key := range []string{"http.response.status_code", "http.status_code", "rpc.grpc.status_code"} {
		v, ok := span.Attributes().Get(key)
		if !ok {
			continue
		}
		if v.Type() == pcommon.ValueTypeInt {
			return int(v.Int())
		}
		if v.Type() == pcommon.ValueTypeStr {
			n := 0
			_, _ = fmt.Sscanf(v.Str(), "%d", &n)
			if n > 0 {
				return n
			}
		}
	}
	// SpanStatus.Code is a coarse mapping when no HTTP/RPC code is set.
	if span.Status().Code() == ptrace.StatusCodeError {
		return 500
	}
	return 0
}

// outboundRetryKey hashes (http.request.method, server.address, url.path)
// to a single 64-bit value the retry-onset trigger uses for collision
// detection. Returns 0 when the span lacks the necessary attributes.
func outboundRetryKey(span ptrace.Span) uint64 {
	method := getAttrString(span.Attributes(), "http.request.method", "http.method")
	server := getAttrString(span.Attributes(), "server.address")
	path := getAttrString(span.Attributes(), "url.path")
	if method == "" && server == "" && path == "" {
		return 0
	}
	return fnv1a64(method + "|" + server + "|" + path)
}

// getAttrString returns the first non-empty attribute value among keys,
// or "" if none is present. pdata.Value.Get() can return a zero Value
// whose AsString() panics on an empty internal pointer.
func getAttrString(attrs pcommon.Map, keys ...string) string {
	for _, k := range keys {
		v, ok := attrs.Get(k)
		if !ok {
			continue
		}
		s := v.AsString()
		if s != "" {
			return s
		}
	}
	return ""
}

func fnv1a64(s string) uint64 {
	const (
		offset = 1469598103934665603
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	if h == 0 {
		return 1 // 0 means "no key"
	}
	return h
}

func traceIDHex(tid pcommon.TraceID) string {
	const hex = "0123456789abcdef"
	out := make([]byte, len(tid)*2)
	for i, b := range tid {
		out[i*2] = hex[b>>4]
		out[i*2+1] = hex[b&0x0f]
	}
	return string(out)
}

func traceIDsInResource(rs ptrace.ResourceSpans) map[pcommon.TraceID]struct{} {
	out := make(map[pcommon.TraceID]struct{})
	ss := rs.ScopeSpans()
	for i := 0; i < ss.Len(); i++ {
		spans := ss.At(i).Spans()
		for j := 0; j < spans.Len(); j++ {
			out[spans.At(j).TraceID()] = struct{}{}
		}
	}
	return out
}

// isolateTrace returns a copy of rs containing only spans for the given
// trace_id. Used so the buffer keys per-trace independently of how the
// pipeline batched them.
func isolateTrace(rs ptrace.ResourceSpans, tid pcommon.TraceID) ptrace.ResourceSpans {
	out := ptrace.NewResourceSpans()
	rs.Resource().CopyTo(out.Resource())
	out.SetSchemaUrl(rs.SchemaUrl())
	ss := rs.ScopeSpans()
	for i := 0; i < ss.Len(); i++ {
		scope := ss.At(i)
		var dst ptrace.ScopeSpans
		var dstInit bool
		spans := scope.Spans()
		for j := 0; j < spans.Len(); j++ {
			span := spans.At(j)
			if span.TraceID() != tid {
				continue
			}
			if !dstInit {
				dst = out.ScopeSpans().AppendEmpty()
				scope.Scope().CopyTo(dst.Scope())
				dst.SetSchemaUrl(scope.SchemaUrl())
				dstInit = true
			}
			span.CopyTo(dst.Spans().AppendEmpty())
		}
	}
	return out
}
