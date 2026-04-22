package raftnode_test

import (
	"errors"
	"fmt"
	"net"
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

	st, err := store.Open(filepath.Join(dir, "kv.log"), store.WithSyncWrites(false))
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

// TestThreeNodeCluster spins up a 3-node Raft cluster, waits for leader election,
// applies a single Put, and verifies all three stores converge to the same value.
func TestThreeNodeCluster(t *testing.T) {
	// Allocate 3 free TCP ports dynamically to avoid conflicts with other tests.
	raftAddrs := make([]string, 3)
	for i := range raftAddrs {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		raftAddrs[i] = ln.Addr().String()
		ln.Close()
	}

	peers := make(map[string]string, 3)
	for i, addr := range raftAddrs {
		peers[fmt.Sprintf("node%d", i+1)] = addr
	}

	stores := make([]*store.Store, 3)
	nodes := make([]*raftnode.Node, 3)

	for i := range 3 {
		dir := t.TempDir()
		st, err := store.Open(filepath.Join(dir, "kv.log"), store.WithSyncWrites(false))
		if err != nil {
			t.Fatal(err)
		}
		stores[i] = st
		t.Cleanup(func() { st.Close() })

		node, err := raftnode.New(st, raftnode.Config{
			NodeID:   fmt.Sprintf("node%d", i+1),
			RaftAddr: raftAddrs[i],
			DataDir:  dir,
			Peers:    peers,
		})
		if err != nil {
			t.Fatalf("node %d: %v", i+1, err)
		}
		nodes[i] = node
		t.Cleanup(func() { node.Shutdown() })
	}

	// Wait for one leader to emerge.
	var leader *raftnode.Node
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, n := range nodes {
			if n.IsLeader() {
				leader = n
				break
			}
		}
		if leader != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if leader == nil {
		t.Fatal("no leader elected within 10s")
	}

	if err := leader.Apply(raftnode.CmdPut, "foo", []byte("bar")); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// All three stores must converge to "foo" → "bar".
	for i, st := range stores {
		convergeDeadline := time.Now().Add(3 * time.Second)
		var got []byte
		var err error
		for time.Now().Before(convergeDeadline) {
			got, err = st.Get("foo")
			if err == nil {
				break
			}
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("store %d unexpected error: %v", i+1, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil || string(got) != "bar" {
			t.Fatalf("store %d: err=%v got=%q want=%q", i+1, err, got, "bar")
		}
	}
}
