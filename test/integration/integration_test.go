// Integration test: synthesize a high-rate span stream with a 5xx
// burst, run the Bridge end-to-end (config → factory → processor →
// forward), and assert that the Incidentary backend receives at least
// one flushed batch with the X-Incidentary-Surface: processor header.
//
// We avoid the heavyweight `testbed` framework: spinning a real OTLP
// server in-process here would only validate the receiver, which is
// upstream code we don't own. The unit + this end-to-end gate covers
// the contract we DO own.
package integration

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

	"github.com/incidentary/opentelemetry-collector-incidentary/processor/incidentaryprocessor"
)

// TestIntegration_SyntheticStreamFiresFlushWithSurfaceHeader is the
// acceptance gate for `make integration`.
//
//   - Stream: 1,000 spans across 50 trace_ids, 5% 5xx baseline → spike
//     to 25% 5xx for the last 200 spans.
//   - Backend: httptest server captures every POST.
//   - Assertion: at least one POST arrives with the surface header.
//
// The test is self-contained — no external services, no kind cluster.
func TestIntegration_SyntheticStreamFiresFlushWithSurfaceHeader(t *testing.T) {
	var (
		hits      int32
		surfaceOk int32
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Header.Get("X-Incidentary-Surface") == "processor" {
			atomic.AddInt32(&surfaceOk, 1)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := incidentaryprocessor.DefaultConfig()
	cfg.Forward.IncidentaryEndpoint = server.URL + "/api/v1/otlp/v1/traces"
	// httptest.Server is loopback-bound; allow the SSRF guard to
	// accept it for the integration scenario.
	cfg.Forward.AllowInsecure = true
	cfg.Forward.IncidentaryToken = "sk_test_integration"
	cfg.Forward.PassThrough = false
	cfg.MaxBufferSize = 100_000

	sink := &consumertest.TracesSink{}
	settings := processortest.NewNopSettings(component.MustNewType("incidentary"))

	factory := incidentaryprocessor.NewFactory()
	procAny, err := factory.CreateTraces(context.Background(), settings, cfg, sink)
	if err != nil {
		t.Fatalf("CreateTraces: %v", err)
	}
	if err := procAny.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = procAny.Shutdown(context.Background()) }()

	stream := generateStream(1000, 50, 0.05, 0.25)
	if err := procAny.ConsumeTraces(context.Background(), stream); err != nil {
		t.Fatalf("ConsumeTraces: %v", err)
	}

	// Forwarder is synchronous in v0.1; defensive sleep covers any
	// future move to async.
	time.Sleep(50 * time.Millisecond)

	if got := atomic.LoadInt32(&hits); got == 0 {
		t.Fatalf("backend received zero requests; expected at least 1 flush")
	}
	if got := atomic.LoadInt32(&surfaceOk); got == 0 {
		t.Fatalf("no request carried X-Incidentary-Surface: processor")
	}
}

// generateStream synthesizes spans across distinct trace_ids with a
// baseline 5xx fraction, then a spike fraction at the tail.
func generateStream(total, traces int, baseline5xx, spike5xx float64) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "checkout")
	scope := rs.ScopeSpans().AppendEmpty()

	spikeStart := total * 4 / 5
	for i := 0; i < total; i++ {
		s := scope.Spans().AppendEmpty()
		var tid pcommon.TraceID
		tid[0] = byte(i % traces)
		tid[1] = byte((i / traces) % 256)
		s.SetTraceID(tid)
		var sid pcommon.SpanID
		sid[0] = byte(i % 256)
		sid[1] = byte((i / 256) % 256)
		s.SetSpanID(sid)
		s.SetKind(ptrace.SpanKindServer)
		s.SetStartTimestamp(pcommon.Timestamp(i * 1_000_000))
		s.SetEndTimestamp(pcommon.Timestamp(i*1_000_000 + 50_000_000))

		errorFraction := baseline5xx
		if i >= spikeStart {
			errorFraction = spike5xx
		}
		statusCode := int64(200)
		// Deterministic pseudo-random: every Nth becomes 5xx.
		threshold := int(1.0 / errorFraction)
		if threshold > 0 && i%threshold == 0 {
			statusCode = 503
		}
		s.Attributes().PutInt("http.response.status_code", statusCode)
	}
	return td
}
