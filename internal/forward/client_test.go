package forward

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/zap"
)

// Every flushed batch must carry the
// X-Incidentary-Surface: processor header. Backend depends on it
// to stamp surface=processor on the resulting CEs.
func TestClient_SetsSurfaceProcessorHeader(t *testing.T) {
	var gotSurface, gotAuth, gotContentType string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSurface = r.Header.Get("X-Incidentary-Surface")
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c, err := NewClient(Config{
		Endpoint:     server.URL + "/api/v1/otlp/v1/traces",
		APIKey:       "sk_test_abc",
		AgentVersion: "0.1.0",
		Timeout:      2 * time.Second,
		Logger:       zap.NewNop(),
		HTTPClient:   server.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	td := makeTraces()
	if err := c.SendTraces(context.Background(), td); err != nil {
		t.Fatalf("SendTraces: %v", err)
	}

	if gotSurface != "processor" {
		t.Errorf("X-Incidentary-Surface = %q, want %q (plan §3.5.2)", gotSurface, "processor")
	}
	if !strings.HasPrefix(gotAuth, "Bearer sk_test_") {
		t.Errorf("Authorization = %q, want Bearer sk_test_…", gotAuth)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
}

// Empty traces are a no-op (avoid wasting a request).
func TestClient_SendEmptyTracesIsNoOp(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c, err := NewClient(Config{
		Endpoint:     server.URL + "/api/v1/otlp/v1/traces",
		APIKey:       "k",
		AgentVersion: "0.1.0",
		HTTPClient:   server.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if err := c.SendTraces(context.Background(), ptrace.NewTraces()); err != nil {
		t.Fatalf("SendTraces: %v", err)
	}
	if called {
		t.Errorf("server should not have been called for empty traces")
	}
}

// Server 5xx → retry exhausts → error returned (do not silently swallow).
func TestClient_PropagatesPermanentFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Permanent 4xx — incidentary-go-core's ingest treats this as no-retry.
		http.Error(w, "bad token", http.StatusUnauthorized)
	}))
	defer server.Close()

	c, _ := NewClient(Config{
		Endpoint:     server.URL + "/api/v1/otlp/v1/traces",
		APIKey:       "bad",
		AgentVersion: "0.1.0",
		HTTPClient:   server.Client(),
	})
	err := c.SendTraces(context.Background(), makeTraces())
	if err == nil {
		t.Fatalf("expected error from 401, got nil")
	}
}

func makeTraces() ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	rs.Resource().Attributes().PutStr("service.name", "checkout")
	scope := rs.ScopeSpans().AppendEmpty()
	span := scope.Spans().AppendEmpty()
	var tid pcommon.TraceID
	tid[0] = 1
	span.SetTraceID(tid)
	var sid pcommon.SpanID
	sid[0] = 2
	span.SetSpanID(sid)
	span.SetName("GET /checkout")
	span.SetKind(ptrace.SpanKindServer)
	return td
}
