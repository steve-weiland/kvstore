package store_test

// Failure-mode tests for the V1 store.
//
// Each test demonstrates a known weakness of the V1 design (no fsync, no checksums,
// no compaction). All tests are expected to PASS against V1 — passing means the
// failure mode is present and observable. When V2 fixes a weakness, the corresponding
// test will need to be updated or inverted.

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/steve-weiland/kvstore/internal/store"
)

// TestDataLossOnTruncatedWrite simulates a process killed mid-write.
// A partial JSON entry (no trailing newline) is appended directly to the log.
// V1: replay silently drops the incomplete entry — data loss with no error.
func TestDataLossOnTruncatedWrite(t *testing.T) {
	f, err := os.CreateTemp("", "kv-failure-*.log")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path) })

	s, _ := store.Open(path)
	if err := s.Put("durable", []byte("survives")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("lost", []byte("world")); err != nil {
		t.Fatal(err)
	}

	// Simulate a write interrupted mid-entry: truncate the file through the last entry's bytes.
	info, _ := os.Stat(path)
	if err := os.Truncate(path, info.Size()-5); err != nil {
		t.Fatal(err)
	}

	s2, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open should succeed on truncated tail, got: %v", err)
	}

	got, err := s2.Get("durable")
	if err != nil || string(got) != "survives" {
		t.Fatalf("durable key lost after restart: err=%v got=%q", err, got)
	}

	// V1 data loss: the write-in-flight is gone after restart.
	_, err = s2.Get("lost")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for truncated entry, got: %v", err)
	}
}

// TestLogGrowsUnbounded shows that repeated writes to the same key inflate the log
// without bound. The index stays correct (points to the latest entry), but disk usage
// grows linearly with write count rather than with live key count.
func TestLogGrowsUnbounded(t *testing.T) {
	f, _ := os.CreateTemp("", "kv-failure-*.log")
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path) })

	s, _ := store.Open(path)
	const N = 100
	for i := 0; i < N; i++ {
		if err := s.Put("same-key", []byte("value")); err != nil {
			t.Fatal(err)
		}
	}

	info, _ := os.Stat(path)
	// Each entry is ~50 bytes; N entries → file must be far larger than one entry.
	const minExpected = 40 * N
	if info.Size() < minExpected {
		t.Fatalf("log too small: got %d bytes, want > %d (no compaction means N entries on disk)", info.Size(), minExpected)
	}
	t.Logf("after %d writes to same key: log is %d bytes, only 1 live entry in index", N, info.Size())

	got, err := s.Get("same-key")
	if err != nil || string(got) != "value" {
		t.Fatalf("latest value not accessible: err=%v got=%q", err, got)
	}
}

// TestCorruptEntryDroppedSilently writes a corrupt line between two valid entries.
// V1: replay skips the bad line with no error. The surrounding keys survive; the
// corrupted entry vanishes without any signal to the caller.
func TestCorruptEntryDroppedSilently(t *testing.T) {
	f, _ := os.CreateTemp("", "kv-failure-*.log")
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path) })

	raw, _ := os.OpenFile(path, os.O_WRONLY, 0)
	// "before" = base64("before"), "after" = base64("after")
	fmt.Fprintln(raw, `{"op":"put","key":"before","value":"YmVmb3Jl"}`)
	fmt.Fprintln(raw, `NOT VALID JSON — simulating a bit-flip or torn sector`)
	fmt.Fprintln(raw, `{"op":"put","key":"after","value":"YWZ0ZXI="}`)
	raw.Close()

	s, err := store.Open(path)
	// V1: no error returned — corruption is silent.
	if err != nil {
		t.Fatalf("Open should not fail on corrupt middle entry: %v", err)
	}

	got, err := s.Get("before")
	if err != nil || string(got) != "before" {
		t.Fatalf("key before corrupt entry: err=%v got=%q", err, got)
	}
	got, err = s.Get("after")
	if err != nil || string(got) != "after" {
		t.Fatalf("key after corrupt entry: err=%v got=%q", err, got)
	}
	t.Log("corrupt middle entry silently dropped; no error returned from Open")
}

// TestDeleteDoesNotReclaimDisk verifies that deleting keys grows the log (tombstones
// appended) rather than shrinking it. No compaction means disk is never reclaimed.
func TestDeleteDoesNotReclaimDisk(t *testing.T) {
	f, _ := os.CreateTemp("", "kv-failure-*.log")
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path) })

	s, _ := store.Open(path)
	const N = 50
	for i := 0; i < N; i++ {
		_ = s.Put(fmt.Sprintf("key-%d", i), []byte("value"))
	}
	afterPuts, _ := os.Stat(path)

	for i := 0; i < N; i++ {
		_ = s.Delete(fmt.Sprintf("key-%d", i))
	}
	afterDeletes, _ := os.Stat(path)

	if afterDeletes.Size() <= afterPuts.Size() {
		t.Fatalf("expected log to grow after %d deletes (tombstones appended): size %d → %d",
			N, afterPuts.Size(), afterDeletes.Size())
	}
	t.Logf("after %d puts + %d deletes: log grew from %d to %d bytes (no compaction)",
		N, N, afterPuts.Size(), afterDeletes.Size())

	for i := 0; i < N; i++ {
		_, err := s.Get(fmt.Sprintf("key-%d", i))
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("key-%d: expected ErrNotFound after delete, got %v", i, err)
		}
	}
}

// TestReplayScansFullLogHistory documents that startup time is O(total write history),
// not O(live keys). There is no compaction, no hint file, no segment boundary —
// Open must scan every byte ever written.
//
// No hard time assertion: this is an observability test. Run it before and after V2
// adds compaction and compare the t.Logf output.
func TestReplayScansFullLogHistory(t *testing.T) {
	f, _ := os.CreateTemp("", "kv-failure-*.log")
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path) })

	s, _ := store.Open(path)
	const N = 10_000
	for i := 0; i < N; i++ {
		_ = s.Put("key", []byte("value"))
	}
	info, _ := os.Stat(path)
	t.Logf("log size after %d writes to same key: %d bytes", N, info.Size())

	start := time.Now()
	s2, err := store.Open(path)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("replaying %d stale entries took %v", N, elapsed)

	got, err := s2.Get("key")
	if err != nil || string(got) != "value" {
		t.Fatalf("latest value wrong after replay: err=%v got=%q", err, got)
	}
}

// TestReplayTombstoneWithoutPut guards against a panic when a tombstone appears
// in the log with no preceding put. delete(map, missingKey) is a Go no-op, but
// this must be an explicit regression test for future replication scenarios.
func TestReplayTombstoneWithoutPut(t *testing.T) {
	f, _ := os.CreateTemp("", "kv-failure-*.log")
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path) })

	raw, _ := os.OpenFile(path, os.O_WRONLY, 0)
	fmt.Fprintln(raw, `{"op":"del","key":"ghost"}`)
	raw.Close()

	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("replay should handle tombstone without prior put: %v", err)
	}
	_, err = s.Get("ghost")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for ghost key, got: %v", err)
	}
}
