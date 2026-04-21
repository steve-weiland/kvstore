package raftnode_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/steve-weiland/kvstore/internal/raftnode"
	"github.com/steve-weiland/kvstore/internal/store"
)

// TestSingleNodeCluster verifies that a single-voter Raft cluster elects itself
// leader and correctly applies Put and Delete commands through the FSM.
func TestSingleNodeCluster(t *testing.T) {
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "kv.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	node, err := raftnode.New(st, raftnode.Config{
		NodeID:   "http://localhost:19091",
		RaftAddr: "127.0.0.1:19091",
		DataDir:  dir,
		Peers:    map[string]string{"http://localhost:19091": "127.0.0.1:19091"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { node.Shutdown() })

	// Wait for leader election (single voter elects itself quickly).
	deadline := time.Now().Add(5 * time.Second)
	for !node.IsLeader() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !node.IsLeader() {
		t.Fatal("node did not become leader within 5s")
	}

	// Put via Raft.
	if err := node.Apply(raftnode.CmdPut, "hello", []byte("world")); err != nil {
		t.Fatalf("Apply put: %v", err)
	}
	got, err := st.Get("hello")
	if err != nil || string(got) != "world" {
		t.Fatalf("Get after put: err=%v got=%q", err, got)
	}

	// Overwrite.
	if err := node.Apply(raftnode.CmdPut, "hello", []byte("raft")); err != nil {
		t.Fatalf("Apply overwrite: %v", err)
	}
	got, err = st.Get("hello")
	if err != nil || string(got) != "raft" {
		t.Fatalf("Get after overwrite: err=%v got=%q", err, got)
	}

	// Delete via Raft.
	if err := node.Apply(raftnode.CmdDel, "hello", nil); err != nil {
		t.Fatalf("Apply del: %v", err)
	}
	_, err = st.Get("hello")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got: %v", err)
	}

	// LeaderAddr returns this node's own ID when it's the leader.
	if addr := node.LeaderAddr(); addr != "http://localhost:19091" {
		t.Fatalf("LeaderAddr: got %q, want %q", addr, "http://localhost:19091")
	}
}
