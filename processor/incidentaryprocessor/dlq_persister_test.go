package incidentaryprocessor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/ptrace"
)

// makeTestTraces returns a ptrace.Traces with a single span carrying
// `name`. Sufficient for round-trip correctness — the persister does
// not interpret the proto contents, just round-trips them.
func makeTestTraces(name string) ptrace.Traces {
	td := ptrace.NewTraces()
	rs := td.ResourceSpans().AppendEmpty()
	ss := rs.ScopeSpans().AppendEmpty()
	span := ss.Spans().AppendEmpty()
	span.SetName(name)
	return td
}

func TestSaveLoad_RoundTripsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dlq.bin")

	if err := saveDLQ(path, nil); err != nil {
		t.Fatalf("save empty: %v", err)
	}
	// File should be removed (or never created) for empty save.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("empty save should not leave a file; stat err=%v", err)
	}

	out, err := loadDLQ(path)
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected 0 entries from missing file, got %d", len(out))
	}
}

func TestSaveLoad_RoundTripsSingleEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dlq.bin")

	now := time.Now().Truncate(time.Nanosecond)
	in := []dlqEntry{
		{
			td:        makeTestTraces("checkout"),
			enqueueAt: now,
			attempts:  2,
		},
	}
	if err := saveDLQ(path, in); err != nil {
		t.Fatalf("save: %v", err)
	}

	out, err := loadDLQ(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(out))
	}
	if got := out[0].attempts; got != 2 {
		t.Errorf("attempts: got %d, want 2", got)
	}
	if !out[0].enqueueAt.Equal(now) {
		t.Errorf("enqueueAt: got %v, want %v", out[0].enqueueAt, now)
	}
	// Verify the trace round-tripped — single span, name="checkout".
	rs := out[0].td.ResourceSpans()
	if rs.Len() != 1 || rs.At(0).ScopeSpans().Len() != 1 {
		t.Fatalf("malformed round-trip: rs.Len=%d", rs.Len())
	}
	if got := rs.At(0).ScopeSpans().At(0).Spans().At(0).Name(); got != "checkout" {
		t.Errorf("span name: got %q, want %q", got, "checkout")
	}
}

func TestSaveLoad_RoundTripsManyEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dlq.bin")

	in := make([]dlqEntry, 0, 10)
	for i := 0; i < 10; i++ {
		in = append(in, dlqEntry{
			td:        makeTestTraces("svc-" + string(rune('a'+i))),
			enqueueAt: time.Now().Add(time.Duration(-i) * time.Minute),
			attempts:  i,
		})
	}
	if err := saveDLQ(path, in); err != nil {
		t.Fatalf("save: %v", err)
	}

	out, err := loadDLQ(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(out) != 10 {
		t.Fatalf("expected 10 entries, got %d", len(out))
	}
	for i, e := range out {
		if e.attempts != i {
			t.Errorf("entry %d: attempts=%d want %d", i, e.attempts, i)
		}
		got := e.td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).Name()
		want := "svc-" + string(rune('a'+i))
		if got != want {
			t.Errorf("entry %d: name=%q want %q", i, got, want)
		}
	}
}

func TestSaveDLQ_AtomicallyReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dlq.bin")

	// Write an initial file.
	first := []dlqEntry{
		{td: makeTestTraces("first"), enqueueAt: time.Now(), attempts: 0},
	}
	if err := saveDLQ(path, first); err != nil {
		t.Fatalf("save first: %v", err)
	}

	// Overwrite with new contents — atomic rename should fully
	// replace, not append.
	second := []dlqEntry{
		{td: makeTestTraces("second-a"), enqueueAt: time.Now(), attempts: 0},
		{td: makeTestTraces("second-b"), enqueueAt: time.Now(), attempts: 1},
	}
	if err := saveDLQ(path, second); err != nil {
		t.Fatalf("save second: %v", err)
	}

	out, err := loadDLQ(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("after overwrite expected 2, got %d", len(out))
	}
	if got := out[0].td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).Name(); got != "second-a" {
		t.Errorf("first entry: got %q, want second-a", got)
	}
}

func TestLoadDLQ_RejectsCorruptHeaderVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dlq.bin")
	// Write a fake header with version=99.
	if err := os.WriteFile(path, []byte{99, 0, 0, 0, 0, 0, 0, 0, 0}, 0o600); err != nil {
		t.Fatalf("write fake: %v", err)
	}
	if _, err := loadDLQ(path); err == nil {
		t.Errorf("loadDLQ should reject version=99 header")
	}
}

func TestLoadDLQ_RejectsImplausibleEntryCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dlq.bin")
	// Header with version=1, count=1<<40 (way over 1M cap).
	header := []byte{1, 0, 0, 0x01, 0, 0, 0, 0, 0}
	if err := os.WriteFile(path, header, 0o600); err != nil {
		t.Fatalf("write fake: %v", err)
	}
	if _, err := loadDLQ(path); err == nil {
		t.Errorf("loadDLQ should reject implausible count")
	}
}

func TestLoadDLQ_PartialReadReturnsHeadOnTruncatedTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dlq.bin")

	// Save two entries.
	in := []dlqEntry{
		{td: makeTestTraces("survived"), enqueueAt: time.Now(), attempts: 0},
		{td: makeTestTraces("truncated"), enqueueAt: time.Now(), attempts: 1},
	}
	if err := saveDLQ(path, in); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Truncate the file by 5 bytes — should sever the tail of the
	// second record's payload while leaving the first record intact.
	// (Per-record overhead is 16 header bytes + payload; OTLP
	// payloads here are ~30+ bytes, so a 5-byte truncation lands
	// inside the second record's payload.)
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := os.Truncate(path, stat.Size()-5); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	out, loadErr := loadDLQ(path)
	if loadErr == nil {
		t.Errorf("loadDLQ should return error on truncated file")
	}
	// Should return the head (1 entry) along with the error so the
	// caller can choose to honour it.
	if len(out) != 1 {
		t.Errorf("expected 1 partially-recovered entry, got %d", len(out))
	}
	if len(out) >= 1 {
		got := out[0].td.ResourceSpans().At(0).ScopeSpans().At(0).Spans().At(0).Name()
		if got != "survived" {
			t.Errorf("first entry name: got %q want survived", got)
		}
	}
}
