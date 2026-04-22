package store_test

// Tests for the WithSyncWrites option.
//
// We cannot directly assert that file.Sync() was or wasn't called without OS-level
// instrumentation, so these tests focus on what we can verify: that both modes
// produce correct results and that the option is wired through as expected.

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/steve-weiland/kvstore/internal/store"
)

// TestSyncWritesEnabledByDefault confirms that a store opened with no options
// (syncWrites=true) still produces correct results for all operations.
func TestSyncWritesEnabledByDefault(t *testing.T) {
	f, _ := os.CreateTemp("", "kv-sync-*.log")
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path); os.Remove(path + ".hint") })

	s, err := store.Open(path) // no options — syncWrites defaults to true
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Put("k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("k")
	if err != nil || string(got) != "v" {
		t.Fatalf("Get: err=%v got=%q", err, got)
	}
	if err := s.Delete("k"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Get("k")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
	s.Close()

	// Reopen — index must survive.
	s2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	_, err = s2.Get("k")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after reopen, got %v", err)
	}
}

// TestSyncWritesDisabledCorrectness confirms that WithSyncWrites(false) produces
// identical observable results to the default. This is the mode used under Raft.
func TestSyncWritesDisabledCorrectness(t *testing.T) {
	f, _ := os.CreateTemp("", "kv-nosync-*.log")
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path); os.Remove(path + ".hint") })

	s, err := store.Open(path, store.WithSyncWrites(false))
	if err != nil {
		t.Fatal(err)
	}

	// Put and read back.
	if err := s.Put("hello", []byte("world")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("hello")
	if err != nil || string(got) != "world" {
		t.Fatalf("Get: err=%v got=%q", err, got)
	}

	// Overwrite.
	if err := s.Put("hello", []byte("raft")); err != nil {
		t.Fatal(err)
	}
	got, err = s.Get("hello")
	if err != nil || string(got) != "raft" {
		t.Fatalf("overwrite: err=%v got=%q", err, got)
	}

	// Delete.
	if err := s.Delete("hello"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Get("hello")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	s.Close()

	// Clean reopen — data must survive (OS flushes on close).
	s2, err := store.Open(path, store.WithSyncWrites(false))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	_, err = s2.Get("hello")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after reopen, got %v", err)
	}
}

// TestSyncWritesDisabledReopen writes multiple keys with sync off, closes cleanly,
// and verifies all keys survive the reopen. Documents that sync=false is safe for
// clean shutdowns; crash safety under Raft comes from BoltDB, not this fsync.
func TestSyncWritesDisabledReopen(t *testing.T) {
	f, _ := os.CreateTemp("", "kv-nosync-reopen-*.log")
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path); os.Remove(path + ".hint") })

	s, err := store.Open(path, store.WithSyncWrites(false))
	if err != nil {
		t.Fatal(err)
	}
	const N = 50
	for i := 0; i < N; i++ {
		if err := s.Put(fmt.Sprintf("key-%d", i), []byte(fmt.Sprintf("val-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	s2, err := store.Open(path, store.WithSyncWrites(false))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	for i := 0; i < N; i++ {
		key := fmt.Sprintf("key-%d", i)
		want := fmt.Sprintf("val-%d", i)
		got, err := s2.Get(key)
		if err != nil || string(got) != want {
			t.Fatalf("%s: err=%v got=%q want=%q", key, err, got, want)
		}
	}
}

// TestSyncWritesDisabledCompact verifies that compaction (which always fsyncs its
// temp file before rename) works correctly when per-write sync is disabled.
func TestSyncWritesDisabledCompact(t *testing.T) {
	f, _ := os.CreateTemp("", "kv-nosync-compact-*.log")
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path); os.Remove(path + ".hint") })

	s, err := store.Open(path, store.WithSyncWrites(false))
	if err != nil {
		t.Fatal(err)
	}

	// Write 100 versions of the same key.
	for i := 0; i < 100; i++ {
		if err := s.Put("k", []byte("v")); err != nil {
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

	// Data must still be readable after compaction.
	got, err := s.Get("k")
	if err != nil || string(got) != "v" {
		t.Fatalf("Get after compact: err=%v got=%q", err, got)
	}

	// Reopen using the compacted file + hint.
	s.Close()
	s2, err := store.Open(path, store.WithSyncWrites(false))
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	got, err = s2.Get("k")
	if err != nil || string(got) != "v" {
		t.Fatalf("Get after compact+reopen: err=%v got=%q", err, got)
	}
}
