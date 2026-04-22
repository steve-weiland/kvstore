# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test

```bash
make build                                        # bin/kvserver, bin/kvdump
make test                                         # all packages with -race
go test -race -count=1 ./internal/store/...       # storage engine only
go test -race -count=1 ./internal/raftnode/...    # Raft integration (~5s for 3-node test)
go test -race -count=1 ./internal/server/...      # HTTP handlers
go test -race -count=1 -run TestName ./pkg/...    # single test
```

## Architecture

```
HTTP → internal/server → internal/raftnode → internal/store → kv.log (binary)
```

**Write path:** `server` calls `raftnode.Node.Apply` → Raft commits to quorum → `FSM.Apply` calls `store.Put/Delete`.

**Read path:** `store.Get` directly (stale) or `raftnode.Node.Barrier` + `store.Get` (`?consistent=true`).

**Non-leader writes:** server returns `307 Temporary Redirect` to `Node.LeaderAddr()` — which is the Raft ServerID, set to the node's HTTP address so no separate discovery is needed.

## Key packages

| Package | Role |
|---------|------|
| `internal/store` | Bitcask engine: binary append-only log, CRC32 per record, in-memory index, compaction, hint file |
| `internal/raftnode` | `Node` wraps `hashicorp/raft`; `FSM` implements `raft.FSM` on top of the store |
| `internal/server` | HTTP handlers; `Applier` interface decouples server from Raft for testing |
| `cmd/kvserver` | Binary: parses flags, wires store + raftnode + server |
| `cmd/kvdump` | Diagnostic: decodes and prints every record in a log file |

## Storage engine details

- Record format: `[crc32:4][key_sz:2][val_sz:4][op:1][key][value]` — 11-byte header
- `store.Open` loads hint file (O(live keys)) if present, then replays log tail from `compact_size`
- `WithSyncWrites(false)` — used under Raft; BoltDB already fsyncs committed entries
- `WithCompactionThreshold(bytes)` — auto-compact triggers inside `Put`/`Delete` when `writePos >= threshold`
- `store.Snapshot` / `store.ReplaceContents` — used by `FSM.Snapshot` / `FSM.Restore` for Raft snapshot transfer

## Run locally

```bash
make run          # single node :9090
make run-cluster  # 3 nodes :9091–:9093
make stop-cluster
```
