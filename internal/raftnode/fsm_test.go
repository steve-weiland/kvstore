package raftnode

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"

	"github.com/steve-weiland/kvstore/internal/store"
)

// testSnapshotSink is a minimal raft.SnapshotSink backed by a bytes.Buffer.
type testSnapshotSink struct {
	buf bytes.Buffer
}

func (s *testSnapshotSink) Write(p []byte) (int, error) { return s.buf.Write(p) }
func (s *testSnapshotSink) Close() error                { return nil }
func (s *testSnapshotSink) ID() string                  { return "test" }
func (s *testSnapshotSink) Cancel() error               { return nil }

// TestFSMSnapshotRestore verifies that a full Snapshot → Persist → Restore cycle
// produces an identical store on the receiving side.
func TestFSMSnapshotRestore(t *testing.T) {
	// Source store with known entries.
	src, err := store.Open(filepath.Join(t.TempDir(), "kv.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	entries := map[string][]byte{
		"alpha": []byte("one"),
		"beta":  []byte("two"),
		"gamma": []byte("three"),
	}
	for k, v := range entries {
		if err := src.Put(k, v); err != nil {
			t.Fatal(err)
		}
	}

	fsm := newFSM(src)

	// Take snapshot.
	snap, err := fsm.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	var sink testSnapshotSink
	if err := snap.Persist(&sink); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	// Restore into a fresh store.
	dst, err := store.Open(filepath.Join(t.TempDir(), "kv.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	fsm2 := newFSM(dst)
	if err := fsm2.Restore(io.NopCloser(bytes.NewReader(sink.buf.Bytes()))); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Every key from the source must be readable on the destination.
	for k, want := range entries {
		got, err := dst.Get(k)
		if err != nil {
			t.Fatalf("Get %q after restore: %v", k, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("Get %q: got %q, want %q", k, got, want)
		}
	}
}
