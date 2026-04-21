package raftnode

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"github.com/steve-weiland/kvstore/internal/store"
)

// Config holds parameters for creating a Raft node.
type Config struct {
	// NodeID is this node's unique identifier and doubles as its HTTP address
	// (e.g. "http://localhost:9091"). Raft stores it as the ServerID so that
	// LeaderAddr() can return the leader's HTTP address for client redirects.
	NodeID string
	// RaftAddr is the TCP address Raft listens on (e.g. "localhost:7001").
	RaftAddr string
	// DataDir is the directory for Raft state (BoltDB, snapshots) and the KV log.
	DataDir string
	// Peers maps nodeID → raftAddr for every cluster member including this node.
	Peers map[string]string
}

// Node wraps a Raft instance and exposes the Applier interface consumed by the HTTP server.
type Node struct {
	raft *raft.Raft
}

// New creates and starts a Raft node.
// On first run (no existing Raft state) it bootstraps the cluster from cfg.Peers.
func New(s *store.Store, cfg Config) (*Node, error) {
	raftCfg := raft.DefaultConfig()
	raftCfg.LocalID = raft.ServerID(cfg.NodeID)

	addr, err := net.ResolveTCPAddr("tcp", cfg.RaftAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve raft addr: %w", err)
	}
	transport, err := raft.NewTCPTransport(cfg.RaftAddr, addr, 3, 10*time.Second, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("raft transport: %w", err)
	}

	raftDir := filepath.Join(cfg.DataDir, "raft")
	if err := os.MkdirAll(raftDir, 0755); err != nil {
		return nil, err
	}

	boltDB, err := raftboltdb.New(raftboltdb.Options{
		Path: filepath.Join(raftDir, "raft.db"),
	})
	if err != nil {
		return nil, fmt.Errorf("raft boltdb: %w", err)
	}

	snapshots, err := raft.NewFileSnapshotStore(raftDir, 3, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("raft snapshots: %w", err)
	}

	r, err := raft.NewRaft(raftCfg, newFSM(s), boltDB, boltDB, snapshots, transport)
	if err != nil {
		return nil, fmt.Errorf("raft new: %w", err)
	}

	hasState, err := raft.HasExistingState(boltDB, boltDB, snapshots)
	if err != nil {
		return nil, fmt.Errorf("check raft state: %w", err)
	}
	if !hasState {
		servers := make([]raft.Server, 0, len(cfg.Peers))
		for id, addr := range cfg.Peers {
			servers = append(servers, raft.Server{
				ID:      raft.ServerID(id),
				Address: raft.ServerAddress(addr),
			})
		}
		if err := r.BootstrapCluster(raft.Configuration{Servers: servers}).Error(); err != nil {
			return nil, fmt.Errorf("bootstrap: %w", err)
		}
	}

	return &Node{raft: r}, nil
}

// IsLeader reports whether this node is the current Raft leader.
func (n *Node) IsLeader() bool {
	return n.raft.State() == raft.Leader
}

// LeaderAddr returns the current leader's HTTP address (stored as the Raft ServerID).
// Returns "" if the leader is not yet known.
func (n *Node) LeaderAddr() string {
	_, leaderID := n.raft.LeaderWithID()
	return string(leaderID)
}

// Apply proposes a command through Raft consensus.
// Should only be called on the leader (check IsLeader first).
// Returns an error if the command is rejected or the node loses leadership mid-apply.
func (n *Node) Apply(op, key string, value []byte) error {
	cmd := Command{Op: op, Key: key, Value: value}
	data, err := encodeCommand(cmd)
	if err != nil {
		return err
	}
	f := n.raft.Apply(data, 5*time.Second)
	if err := f.Error(); err != nil {
		return err
	}
	if resp := f.Response(); resp != nil {
		if err, ok := resp.(error); ok {
			return err
		}
	}
	return nil
}

// Shutdown gracefully stops the Raft node.
func (n *Node) Shutdown() error {
	return n.raft.Shutdown().Error()
}
