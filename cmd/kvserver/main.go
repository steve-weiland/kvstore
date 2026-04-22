package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/steve-weiland/kvstore/internal/raftnode"
	"github.com/steve-weiland/kvstore/internal/server"
	"github.com/steve-weiland/kvstore/internal/store"
)

func main() {
	nodeID   := flag.String("node-id", "http://localhost:9090", "this node's HTTP address (used as Raft ServerID for leader redirects)")
	httpAddr := flag.String("http-addr", ":9090", "HTTP listen address")
	raftAddr := flag.String("raft-addr", "localhost:7000", "Raft TCP bind address")
	dataDir  := flag.String("data-dir", "data", "directory for Raft state and KV log")
	peersStr := flag.String("peers", "", "cluster peers: nodeID1=raftAddr1,nodeID2=raftAddr2,... (defaults to single-node)")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0755); err != nil {
		log.Fatalf("mkdir %s: %v", *dataDir, err)
	}

	st, err := store.Open(filepath.Join(*dataDir, "kv.log"), store.WithSyncWrites(false))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}

	peers := parsePeers(*peersStr)
	if len(peers) == 0 {
		peers[*nodeID] = *raftAddr
	}

	node, err := raftnode.New(st, raftnode.Config{
		NodeID:   *nodeID,
		RaftAddr: *raftAddr,
		DataDir:  *dataDir,
		Peers:    peers,
	})
	if err != nil {
		log.Fatalf("raft node: %v", err)
	}

	srv := server.New(st, node)
	log.Printf("node %s listening on %s (raft: %s, data: %s)", *nodeID, *httpAddr, *raftAddr, *dataDir)
	log.Fatal(http.ListenAndServe(*httpAddr, srv))
}

func parsePeers(s string) map[string]string {
	peers := make(map[string]string)
	if s == "" {
		return peers
	}
	for _, part := range strings.Split(s, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 {
			peers[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return peers
}
