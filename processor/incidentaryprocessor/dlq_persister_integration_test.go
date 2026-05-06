// End-to-end restart-durability test.
//
// Construct a processor with a configured persist path, force an
// Entry into its in-memory DLQ, Shutdown, then construct a fresh
// Processor with the same persist path and assert it loaded the
// Queued entry on Start.
//
// This is the contract that operators care about: a Helm rollout
// Or pod eviction during a backend outage no longer drops the
// Queued retries.

package incidentaryprocessor

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/processor/processortest"
)

func TestDLQPersistence_RestartRestoresQueue(t *testing.T) {
	dir := t.TempDir()
	persistPath := filepath.Join(dir, "dlq.bin")

	// First processor — enqueue 3 entries directly into the DLQ
	// (we don't need to drive a real failure; the persister contract
	// Is what matters).
	{
		cfg := DefaultConfig()
		cfg.Forward.IncidentaryEndpoint = "https://example.com/api/v1/otlp/v1/traces"
		cfg.Forward.IncidentaryToken = "k"
		cfg.DLQ.PersistPath = persistPath
		settings := processortest.NewNopSettings(component.MustNewType(typeStr))
		p, err := newTracesProcessor(context.Background(), settings, cfg, &consumertest.TracesSink{})
		if err != nil {
			t.Fatalf("first newTracesProcessor: %v", err)
		}
		if err := p.Start(context.Background(), componenttest.NewNopHost()); err != nil {
			t.Fatalf("first Start: %v", err)
		}
		// Enqueue 3 entries.
		tp := p.(*tracesProcessor)
		tp.dlqMu.Lock()
		tp.dlq = append(tp.dlq,
			dlqEntry{td: makeTestTraces("a"), enqueueAt: time.Now(), attempts: 0},
			dlqEntry{td: makeTestTraces("b"), enqueueAt: time.Now(), attempts: 1},
			dlqEntry{td: makeTestTraces("c"), enqueueAt: time.Now(), attempts: 2},
		)
		tp.dlqMu.Unlock()
		// Shutdown should persist them.
		if err := p.Shutdown(context.Background()); err != nil {
			t.Fatalf("first Shutdown: %v", err)
		}
	}

	// Second processor — fresh, but pointing at the same persist
	// Path. After Start, the in-memory DLQ should hold the 3 entries.
	cfg := DefaultConfig()
	cfg.Forward.IncidentaryEndpoint = "https://example.com/api/v1/otlp/v1/traces"
	cfg.Forward.IncidentaryToken = "k"
	cfg.DLQ.PersistPath = persistPath
	settings := processortest.NewNopSettings(component.MustNewType(typeStr))
	p2, err := newTracesProcessor(context.Background(), settings, cfg, &consumertest.TracesSink{})
	if err != nil {
		t.Fatalf("second newTracesProcessor: %v", err)
	}
	if err := p2.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	defer func() { _ = p2.Shutdown(context.Background()) }()

	tp2 := p2.(*tracesProcessor)
	tp2.dlqMu.Lock()
	got := len(tp2.dlq)
	names := make([]string, 0, got)
	for _, e := range tp2.dlq {
		names = append(names, e.td.ResourceSpans().At(0).
			ScopeSpans().At(0).Spans().At(0).Name())
	}
	tp2.dlqMu.Unlock()

	if got != 3 {
		t.Errorf("after restart expected 3 DLQ entries, got %d (names: %v)", got, names)
	}
	for i, want := range []string{"a", "b", "c"} {
		if i < len(names) && names[i] != want {
			t.Errorf("entry %d: got %q, want %q", i, names[i], want)
		}
	}
}

func TestDLQPersistence_NoPathSkipsPersistAndLoad(t *testing.T) {
	// Shutdown without a persist path must succeed and not write any
	// Stray files. Start without a persist path is the existing
	// Behaviour and should remain unchanged.
	cfg := DefaultConfig()
	cfg.Forward.IncidentaryEndpoint = "https://example.com/api/v1/otlp/v1/traces"
	cfg.Forward.IncidentaryToken = "k"
	// Cfg.DLQ.PersistPath intentionally empty.
	settings := processortest.NewNopSettings(component.MustNewType(typeStr))
	p, err := newTracesProcessor(context.Background(), settings, cfg, &consumertest.TracesSink{})
	if err != nil {
		t.Fatalf("newTracesProcessor: %v", err)
	}
	if err := p.Start(context.Background(), componenttest.NewNopHost()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}
