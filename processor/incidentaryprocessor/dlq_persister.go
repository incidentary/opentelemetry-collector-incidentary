// File-based DLQ persistence.
//
// The in-memory DLQ (cap 32) is the right shape for transient backend
// outages, but it loses everything on operator restart. This file
// provides the simplest durable layer: serialize the queue to a single
// file on Shutdown, read it back on Start.
//
// We deliberately do NOT stream every enqueue/dequeue through the file
// — that would multiply the per-batch overhead by a disk write and add
// crash-safety obligations we don't owe (the queue is small and the
// outage window is bounded; an unclean shutdown losing 32 entries is
// equivalent to dropping a single OTLP batch from upstream).
//
// Format: a length-delimited stream of OTLP/protobuf-marshalled
// `ptrace.Traces`. One uint32 big-endian length prefix per entry. This
// mirrors the OTLP file format used by the contrib `fileexporter`,
// which means operators can pipe the DLQ file through `otlp-proto`
// tools to inspect what was queued.

package incidentaryprocessor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"go.opentelemetry.io/collector/pdata/ptrace"
)

// saveDLQ serializes `entries` to `path`, atomically replacing any
// existing file. Atomicity is achieved by writing to a sibling
// `.tmp` path and renaming on success.
//
// Returns an error if the directory does not exist, if the file
// system is read-only, or if marshalling fails. The caller (Shutdown)
// logs the error but does not propagate — losing the persistence file
// is recoverable next cycle.
func saveDLQ(path string, entries []dlqEntry) error {
	if path == "" {
		return errors.New("saveDLQ: empty path")
	}
	if len(entries) == 0 {
		// Empty queue — remove any stale file. Best-effort.
		_ = os.Remove(path)
		return nil
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".dlq-*.tmp")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}

	marshaler := &ptrace.ProtoMarshaler{}

	// File header: 1-byte version + 8-byte BE entry count. Allows
	// future format changes (currently version=1).
	header := make([]byte, 9)
	header[0] = 1
	binary.BigEndian.PutUint64(header[1:], uint64(len(entries)))
	if _, err := tmp.Write(header); err != nil {
		cleanup()
		return fmt.Errorf("write header: %w", err)
	}

	for _, e := range entries {
		buf, err := marshaler.MarshalTraces(e.td)
		if err != nil {
			cleanup()
			return fmt.Errorf("marshal traces: %w", err)
		}
		// Per-record: uint32 BE length + uint64 BE enqueueAt nanos +
		// uint32 BE attempts + payload.
		recHeader := make([]byte, 4+8+4)
		binary.BigEndian.PutUint32(recHeader[:4], uint32(len(buf)))
		binary.BigEndian.PutUint64(recHeader[4:12], uint64(e.enqueueAt.UnixNano()))
		binary.BigEndian.PutUint32(recHeader[12:], uint32(e.attempts)) //nolint:gosec
		if _, err := tmp.Write(recHeader); err != nil {
			cleanup()
			return fmt.Errorf("write record header: %w", err)
		}
		if _, err := tmp.Write(buf); err != nil {
			cleanup()
			return fmt.Errorf("write payload: %w", err)
		}
	}

	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// loadDLQ reads the persistence file at `path` and returns the
// recovered entries. Returns (nil, nil) when the file does not exist
// — this is the normal first-start case and not an error.
//
// On any parse error, returns the entries successfully read up to that
// point along with the error. The caller (Start) decides whether to
// honour the partial recovery (we recommend yes — a corrupt tail still
// retains the head).
func loadDLQ(path string) ([]dlqEntry, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()

	header := make([]byte, 9)
	if _, err := io.ReadFull(f, header); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	if header[0] != 1 {
		return nil, fmt.Errorf("unsupported dlq format version: %d", header[0])
	}
	count := binary.BigEndian.Uint64(header[1:])
	// Sanity bound: refuse implausibly large headers (could be a
	// truncated/corrupt file with garbage in the count field).
	if count > 1_000_000 {
		return nil, fmt.Errorf("dlq header count %d exceeds sanity bound", count)
	}

	out := make([]dlqEntry, 0, count)
	unmarshaler := &ptrace.ProtoUnmarshaler{}
	for i := uint64(0); i < count; i++ {
		recHeader := make([]byte, 4+8+4)
		if _, err := io.ReadFull(f, recHeader); err != nil {
			return out, fmt.Errorf("record %d header: %w", i, err)
		}
		payloadLen := binary.BigEndian.Uint32(recHeader[:4])
		enqueueNs := int64(binary.BigEndian.Uint64(recHeader[4:12])) //nolint:gosec
		attempts := int(binary.BigEndian.Uint32(recHeader[12:]))     //nolint:gosec

		// Sanity bound on per-record payload size — protobuf-encoded
		// traces are typically a few KB; 64 MB ceiling rejects
		// pathological corruption without restricting legitimate
		// large traces.
		if payloadLen > 64*1024*1024 {
			return out, fmt.Errorf("record %d payload size %d exceeds sanity bound", i, payloadLen)
		}
		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(f, payload); err != nil {
			return out, fmt.Errorf("record %d payload: %w", i, err)
		}
		td, err := unmarshaler.UnmarshalTraces(payload)
		if err != nil {
			return out, fmt.Errorf("record %d unmarshal: %w", i, err)
		}
		out = append(out, dlqEntry{
			td:        td,
			enqueueAt: time.Unix(0, enqueueNs),
			attempts:  attempts,
		})
	}
	return out, nil
}
