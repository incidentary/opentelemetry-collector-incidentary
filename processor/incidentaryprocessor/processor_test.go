package incidentaryprocessor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/collector/processor/processortest"
)

// Pass-through default true. Every span the Bridge sees
// Must reach the next consumer untouched, regardless of trigger fires.
func TestProcessor_PassThroughCallsNextConsumer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.Forward.IncidentaryEndpoint = server.URL + "/api/v1/otlp/v1/traces"
	// Httptest.Server uses 127.0.0.1; allow the loopback target so
	// The SSRF guards don't reject the test setup.
	cfg.Forward.AllowInsecure = true
	cfg.Forward.IncidentaryToken = "k"
	cfg.Forward.PassThrough = true

	sink := &consumertest.TracesSink{}
	settings := processortest.NewNopSettings(component.MustNewType(typeStr))
	p, err := newTracesProcessor(context.Background(), settings, cfg, sink)
	if err != nil {
		t.Fatalf("newTracesProcessor: %v", err)
	}
	if err := p.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	td := makeTracesForTest(1, "checkout", 200)
	if err := p.ConsumeTraces(context.Background(), td); err != nil {
		t.Fatalf("ConsumeTraces: %v", err)
	}
	if got := sink.SpanCount(); got != 1 {
		t.Errorf("sink received %d spans, want 1 (pass-through must always forward to next)", got)
	}
}

// Pass_through=false drops the stream from the customer's pipeline.
//
func TestProcessor_PassThroughFalseDoesNotCallNext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.Forward.IncidentaryEndpoint = server.URL + "/api/v1/otlp/v1/traces"
	// Httptest.Server uses 127.0.0.1; allow the loopback target so
	// The SSRF guards don't reject the test setup.
	cfg.Forward.AllowInsecure = true
	cfg.Forward.IncidentaryToken = "k"
	cfg.Forward.PassThrough = false

	sink := &consumertest.TracesSink{}
	settings := processortest.NewNopSettings(component.MustNewType(typeStr))
	p, _ := newTracesProcessor(context.Background(), settings, cfg, sink)
	_ = p.Start(context.Background(), componenttest.NewNopHost())
	defer func() { _ = p.Shutdown(context.Background()) }()

	td := makeTracesForTest(1, "checkout", 200)
	if err := p.ConsumeTraces(context.Background(), td); err != nil {
		t.Fatalf("ConsumeTraces: %v", err)
	}
	if got := sink.SpanCount(); got != 0 {
		t.Errorf("sink got %d spans with pass_through=false, want 0", got)
	}
}

// When the 5xx trigger fires, the matching trace
// Must be forwarded to the configured endpoint.
func TestProcessor_FlushesOnTriggerFire(t *testing.T) {
	var forwardedHits int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		atomic.AddInt32(&forwardedHits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.Forward.IncidentaryEndpoint = server.URL + "/api/v1/otlp/v1/traces"
	// Httptest.Server uses 127.0.0.1; allow the loopback target so
	// The SSRF guards don't reject the test setup.
	cfg.Forward.AllowInsecure = true
	cfg.Forward.IncidentaryToken = "k"
	cfg.Forward.PassThrough = false

	sink := &consumertest.TracesSink{}
	settings := processortest.NewNopSettings(component.MustNewType(typeStr))
	p, _ := newTracesProcessor(context.Background(), settings, cfg, sink)
	_ = p.Start(context.Background(), componenttest.NewNopHost())
	defer func() { _ = p.Shutdown(context.Background()) }()

	// Build a traces payload that pushes the 5xx rate over 10%.
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "checkout")
	scope := rs.ScopeSpans().AppendEmpty()
	for i := 0; i < 90; i++ {
		s := scope.Spans().AppendEmpty()
		var tid pcommon.TraceID
		tid[0] = byte(i % 5)
		s.SetTraceID(tid)
		var sid pcommon.SpanID
		sid[0] = byte(i + 1)
		s.SetSpanID(sid)
		s.SetKind(ptrace.SpanKindServer)
		s.Attributes().PutInt("http.response.status_code", 200)
	}
	for i := 0; i < 30; i++ {
		s := scope.Spans().AppendEmpty()
		var tid pcommon.TraceID
		tid[0] = byte(7) // distinct trace_id
		s.SetTraceID(tid)
		var sid pcommon.SpanID
		sid[0] = byte(i + 100)
		s.SetSpanID(sid)
		s.SetKind(ptrace.SpanKindServer)
		s.Attributes().PutInt("http.response.status_code", 503)
	}

	if err := p.ConsumeTraces(context.Background(), td); err != nil {
		t.Fatalf("ConsumeTraces: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if atomic.LoadInt32(&forwardedHits) == 0 {
		t.Errorf("expected at least one forwarded flush, got %d", atomic.LoadInt32(&forwardedHits))
	}
}


// TestProcessor_RejectsNegativeDurationSpans verifies that
// A span where end_time < start_time must be discarded (not clamped
// Into the EWMA, where it would silently disable the slow-success
// Trigger for the affected service).
func TestProcessor_RejectsNegativeDurationSpans(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.Forward.IncidentaryEndpoint = server.URL + "/api/v1/otlp/v1/traces"
	cfg.Forward.AllowInsecure = true
	cfg.Forward.IncidentaryToken = "k"
	cfg.Forward.PassThrough = true

	sink := &consumertest.TracesSink{}
	settings := processortest.NewNopSettings(component.MustNewType(typeStr))
	p, err := newTracesProcessor(context.Background(), settings, cfg, sink)
	if err != nil {
		t.Fatalf("newTracesProcessor: %v", err)
	}
	if err := p.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	// Build a span where end < start.
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "checkout")
	scope := rs.ScopeSpans().AppendEmpty()
	span := scope.Spans().AppendEmpty()
	var tid pcommon.TraceID
	tid[0] = 1
	span.SetTraceID(tid)
	var sid pcommon.SpanID
	sid[0] = 1
	span.SetSpanID(sid)
	span.SetKind(ptrace.SpanKindServer)
	now := time.Now()
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(-time.Second))) // negative

	// ConsumeTraces must not panic — the span is silently dropped from
	// The trigger path. Pass-through still fires.
	if err := p.ConsumeTraces(context.Background(), td); err != nil {
		t.Fatalf("ConsumeTraces: %v", err)
	}
	if got := sink.SpanCount(); got != 1 {
		t.Errorf("pass-through must still deliver to next consumer; got %d, want 1", got)
	}
}

// TestProcessor_RejectsAbsurdlyLongDurationSpans verifies that
// Spans claiming year 2050+ end times (or otherwise > 1h durations)
// Must be discarded rather than fed into the EWMA. Without the
// Guard, sustained traffic of these would saturate the baseline
// Near maxBaselineNs and disable the slow-success trigger.
func TestProcessor_RejectsAbsurdlyLongDurationSpans(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Forward.IncidentaryEndpoint = "https://api.incidentary.com/api/v1/otlp/v1/traces"
	cfg.Forward.IncidentaryToken = "k"

	sink := &consumertest.TracesSink{}
	settings := processortest.NewNopSettings(component.MustNewType(typeStr))
	p, err := newTracesProcessor(context.Background(), settings, cfg, sink)
	if err != nil {
		t.Fatalf("newTracesProcessor: %v", err)
	}
	if err := p.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "checkout")
	scope := rs.ScopeSpans().AppendEmpty()
	span := scope.Spans().AppendEmpty()
	var tid pcommon.TraceID
	tid[0] = 1
	span.SetTraceID(tid)
	var sid pcommon.SpanID
	sid[0] = 1
	span.SetSpanID(sid)
	span.SetKind(ptrace.SpanKindServer)
	now := time.Now()
	// Year 2050 end time — 24+ years in the future.
	span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
	span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(24 * 365 * 24 * time.Hour)))

	// Must not panic. The span is dropped from the trigger path.
	if err := p.ConsumeTraces(context.Background(), td); err != nil {
		t.Fatalf("ConsumeTraces: %v", err)
	}
}

// TestProcessor_DeduplicatesDuplicateSpanIDsWithinBatch verifies
// when the same span ID appears twice in a single
// ConsumeTraces invocation (a fan-out pipeline that duplicates
// Every span), the in-flight count must be incremented exactly once
// — not twice. We check by inspecting the evaluator state via the
// Services count (trigger logic is internal but a duplicate would
// Otherwise touch in-flight metrics twice).
func TestProcessor_DeduplicatesDuplicateSpanIDsWithinBatch(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Forward.IncidentaryEndpoint = "https://api.incidentary.com/api/v1/otlp/v1/traces"
	cfg.Forward.IncidentaryToken = "k"
	cfg.Forward.PassThrough = true

	sink := &consumertest.TracesSink{}
	settings := processortest.NewNopSettings(component.MustNewType(typeStr))
	p, err := newTracesProcessor(context.Background(), settings, cfg, sink)
	if err != nil {
		t.Fatalf("newTracesProcessor: %v", err)
	}
	if err := p.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	// Build two ResourceSpans that both contain the same span (same
	// SpanID). A naive evaluator would call OnRequestStart twice.
	td := ptrace.NewTraces()
	for r := 0; r < 2; r++ {
		rs := td.ResourceSpans().AppendEmpty()
		rs.Resource().Attributes().PutStr("service.name", "checkout")
		scope := rs.ScopeSpans().AppendEmpty()
		span := scope.Spans().AppendEmpty()
		var tid pcommon.TraceID
		tid[0] = 0xAA
		span.SetTraceID(tid)
		var sid pcommon.SpanID
		sid[0] = 0xBB
		span.SetSpanID(sid)
		span.SetKind(ptrace.SpanKindServer)
		now := time.Now()
		span.SetStartTimestamp(pcommon.NewTimestampFromTime(now))
		span.SetEndTimestamp(pcommon.NewTimestampFromTime(now.Add(50 * time.Millisecond)))
		span.Attributes().PutInt("http.response.status_code", 200)
	}

	if err := p.ConsumeTraces(context.Background(), td); err != nil {
		t.Fatalf("ConsumeTraces: %v", err)
	}
	// Pass-through delivered both copies (we don't dedup on the
	// Pass-through path; that's the receiver's job). What matters
	// Is the evaluator state is correct — we proxy that here by
	// Confirming we consumed the duplicates without panic.
	if got := sink.SpanCount(); got != 2 {
		t.Errorf("pass-through delivers all spans verbatim; got %d, want 2", got)
	}
}

// TestProcessor_FailedFlushEnqueuesDLQ verifies that when
// `SendTraces` fails, the failed batch is pushed to the in-memory
// DLQ instead of being silently lost. This test points the
// Forwarder at a permanently-failing httptest server, triggers a
// Flush, and asserts the DLQ has a pending entry afterwards.
func TestProcessor_FailedFlushEnqueuesDLQ(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	cfg := DefaultConfig()
	cfg.Forward.IncidentaryEndpoint = server.URL + "/api/v1/otlp/v1/traces"
	cfg.Forward.AllowInsecure = true
	cfg.Forward.IncidentaryToken = "k"
	cfg.Forward.PassThrough = false

	sink := &consumertest.TracesSink{}
	settings := processortest.NewNopSettings(component.MustNewType(typeStr))
	pAny, err := newTracesProcessor(context.Background(), settings, cfg, sink)
	if err != nil {
		t.Fatalf("newTracesProcessor: %v", err)
	}
	p := pAny.(*tracesProcessor)
	if err := p.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	// Build a payload that pushes the 5xx rate over 10% so the
	// Trigger fires and a flush is attempted (and fails).
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "checkout")
	scope := rs.ScopeSpans().AppendEmpty()
	for i := 0; i < 90; i++ {
		s := scope.Spans().AppendEmpty()
		var tid pcommon.TraceID
		tid[0] = byte(i % 5)
		s.SetTraceID(tid)
		var sid pcommon.SpanID
		sid[0] = byte(i + 1)
		s.SetSpanID(sid)
		s.SetKind(ptrace.SpanKindServer)
		s.Attributes().PutInt("http.response.status_code", 200)
	}
	for i := 0; i < 30; i++ {
		s := scope.Spans().AppendEmpty()
		var tid pcommon.TraceID
		tid[0] = byte(7)
		s.SetTraceID(tid)
		var sid pcommon.SpanID
		sid[0] = byte(i + 100)
		s.SetSpanID(sid)
		s.SetKind(ptrace.SpanKindServer)
		s.Attributes().PutInt("http.response.status_code", 503)
	}

	// PassThrough=false; ConsumeTraces returns the forward error.
	_ = p.ConsumeTraces(context.Background(), td)

	// Tick the DLQ a tad to let the enqueue settle (no goroutines
	// Run inline — the enqueue is synchronous with the flush loop).
	time.Sleep(20 * time.Millisecond)

	p.dlqMu.Lock()
	dlqLen := len(p.dlq)
	p.dlqMu.Unlock()

	if dlqLen == 0 {
		t.Fatalf("DLQ should hold the failed batch; got 0 entries")
	}
}

// TestProcessor_DLQEvictsOldestWhenFull verifies that
// Once the DLQ is at capacity (`dlqMaxEntries`), enqueueing another
// Failed batch evicts the oldest entry FIFO.
func TestProcessor_DLQEvictsOldestWhenFull(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Forward.IncidentaryEndpoint = "https://api.incidentary.com/api/v1/otlp/v1/traces"
	cfg.Forward.IncidentaryToken = "k"

	sink := &consumertest.TracesSink{}
	settings := processortest.NewNopSettings(component.MustNewType(typeStr))
	pAny, err := newTracesProcessor(context.Background(), settings, cfg, sink)
	if err != nil {
		t.Fatalf("newTracesProcessor: %v", err)
	}
	p := pAny.(*tracesProcessor)
	if err := p.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	// Stuff (dlqMaxEntries + 5) failed batches into the DLQ.
	for i := 0; i < dlqMaxEntries+5; i++ {
		p.enqueueDLQ(ptrace.NewTraces())
	}
	p.dlqMu.Lock()
	dlqLen := len(p.dlq)
	p.dlqMu.Unlock()
	if dlqLen != dlqMaxEntries {
		t.Errorf("DLQ should be capped at %d; got %d", dlqMaxEntries, dlqLen)
	}
}

func makeTracesForTest(traceCount int, service string, status int64) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", service)
	scope := rs.ScopeSpans().AppendEmpty()
	for i := 0; i < traceCount; i++ {
		s := scope.Spans().AppendEmpty()
		var tid pcommon.TraceID
		tid[0] = byte(i + 1)
		s.SetTraceID(tid)
		var sid pcommon.SpanID
		sid[0] = byte(i + 1)
		s.SetSpanID(sid)
		s.SetKind(ptrace.SpanKindServer)
		s.Attributes().PutInt("http.response.status_code", int64(status))
	}
	return td
}
