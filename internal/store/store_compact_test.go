package store_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/steve-weiland/kvstore/internal/store"
)

// TestCompactReducesFileSize writes 100 versions of the same key, compacts,
// and verifies the log shrinks to a single live record.
func TestCompactReducesFileSize(t *testing.T) {
	f, _ := os.CreateTemp("", "kv-compact-*.log")
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path); os.Remove(path + ".hint") })

	s, _ := store.Open(path)
	const N = 100
	for i := 0; i < N; i++ {
		if err := s.Put("key", []byte("value")); err != nil {
			t.Fatal(err)
		}
	}

	before, _ := os.Stat(path)
	if err := s.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	after, _ := os.Stat(path)

	if after.Size() >= before.Size() {
		t.Fatalf("compact did not shrink file: before=%d after=%d", before.Size(), after.Size())
	}
	// Single live record for "key"(3B) + "value"(5B) = 11+3+5 = 19 bytes.
	if after.Size() != int64(store.RecordHeaderSize+len("key")+len("value")) {
		t.Fatalf("expected single record after compact, got %d bytes", after.Size())
	}

	got, err := s.Get("key")
	if err != nil || string(got) != "value" {
		t.Fatalf("key unreadable after compact: err=%v got=%q", err, got)
	}
}

// TestCompactReclaimsTombstones writes N keys, deletes them all, then compacts.
// The resulting file must be empty (no live keys, no tombstones).
func TestCompactReclaimsTombstones(t *testing.T) {
	f, _ := os.CreateTemp("", "kv-compact-*.log")
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path); os.Remove(path + ".hint") })

	s, _ := store.Open(path)
	const N = 50
	for i := 0; i < N; i++ {
		_ = s.Put(fmt.Sprintf("k%d", i), []byte("v"))
	}
	for i := 0; i < N; i++ {
		_ = s.Delete(fmt.Sprintf("k%d", i))
	}

	if err := s.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	info, _ := os.Stat(path)
	if info.Size() != 0 {
		t.Fatalf("expected empty file after compacting all-deleted store, got %d bytes", info.Size())
	}
}

// TestCompactPreservesAllLiveKeys writes N unique keys, compacts, then reads
// every key back to confirm nothing was lost.
func TestCompactPreservesAllLiveKeys(t *testing.T) {
	f, _ := os.CreateTemp("", "kv-compact-*.log")
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path); os.Remove(path + ".hint") })

	s, _ := store.Open(path)
	const N = 200
	for i := 0; i < N; i++ {
		if err := s.Put(fmt.Sprintf("key-%d", i), []byte(fmt.Sprintf("val-%d", i))); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}

	for i := 0; i < N; i++ {
		key := fmt.Sprintf("key-%d", i)
		want := fmt.Sprintf("val-%d", i)
		got, err := s.Get(key)
		if err != nil || string(got) != want {
			t.Fatalf("%s: err=%v got=%q want=%q", key, err, got, want)
		}
	}
}

// TestCompactSurvivesReopen verifies that the compacted log (and hint file) can
// be used to restore all keys after a reopen.
func TestCompactSurvivesReopen(t *testing.T) {
	f, _ := os.CreateTemp("", "kv-compact-*.log")
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path); os.Remove(path + ".hint") })

	s, _ := store.Open(path)
	_ = s.Put("a", []byte("1"))
	_ = s.Put("b", []byte("2"))
	_ = s.Put("a", []byte("3")) // overwrite
	_ = s.Delete("b")
	if err := s.Compact(); err != nil {
		t.Fatalf("Compact: %v", err)
	}
	s.Close()

	s2, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open after compact: %v", err)
	}
	got, err := s2.Get("a")
	if err != nil || string(got) != "3" {
		t.Fatalf("a: err=%v got=%q", err, got)
	}
	_, err = s2.Get("b")
	if err == nil {
		t.Fatal("b should be gone after compact+reopen")
	}
}

// TestCompactionThresholdTriggersAutoCompact opens a store with a tiny threshold
// and verifies that auto-compact fires from inside Put at the right boundary.
// Each record for key="k", value="val" is RecordHeaderSize+1+3 = 15 bytes.
// With threshold=200: after 13 writes (195 B < 200) no compact; on the 14th
// write writePos hits 210 ≥ 200, compact fires and reduces the file to 15 bytes.
func TestCompactionThresholdTriggersAutoCompact(t *testing.T) {
	f, _ := os.CreateTemp("", "kv-compact-*.log")
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path); os.Remove(path + ".hint") })

	s, err := store.Open(path, store.WithCompactionThreshold(200))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	recordSize := int64(store.RecordHeaderSize + len("k") + len("val")) // 15 bytes

	// 13 writes: writePos = 195 < 200 — no compact yet.
	for i := 0; i < 13; i++ {
		if err := s.Put("k", []byte("val")); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	info, _ := os.Stat(path)
	if info.Size() != 13*recordSize {
		t.Fatalf("expected no compact before threshold: got %d bytes, want %d", info.Size(), 13*recordSize)
	}

	// 14th write: writePos hits 210 ≥ 200 → compact fires inside Put.
	if err := s.Put("k", []byte("val")); err != nil {
		t.Fatalf("Put 14: %v", err)
	}
	info, _ = os.Stat(path)
	if info.Size() != recordSize {
		t.Fatalf("expected compact after threshold: got %d bytes, want %d", info.Size(), recordSize)
	}

	got, err := s.Get("k")
	if err != nil || string(got) != "val" {
		t.Fatalf("Get after auto-compact: err=%v got=%q", err, got)
	}
}
