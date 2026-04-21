# V2 Implementation Roadmap

Branch: `v1-raft` → merge to `main` → tag `v2.0.0`

Each step is a discrete commit on `v1-raft`. Steps 0–3 are local durability/disk fixes.
Step 4 is the distributed consensus layer. Steps 5–6 are stretch goals.

---

## Prerequisite — Binary log format with CRC32

**Fixes:** `TestCorruptEntryDroppedSilently`  
**Why first:** Every subsequent step builds on it. fsync without checksums can't detect partial
writes. Hint files need fixed-width record headers. Raft snapshot/restore needs compact binary.

Replace JSON-lines with a length-prefixed binary record:

```
┌──────────┬─────────┬─────────┬─────┬─────────────┬─────────────┐
│  crc32   │ key_sz  │ val_sz  │ op  │    key      │    value    │
│  4 bytes │ 2 bytes │ 4 bytes │ 1 B │ key_sz bytes│ val_sz bytes│
└──────────┴─────────┴─────────┴─────┴─────────────┴─────────────┘
  header = 11 bytes
  op: 0x01 = PUT, 0x02 = DELETE (val_sz = 0, no value bytes)
  crc32 covers bytes [4..end]: key_sz, val_sz, op, key, value
```

On replay: read 11-byte header, verify CRC, read key+value. CRC mismatch on last record →
truncate to last good record (crash recovery). CRC mismatch mid-log → return error from Open().

Files: `internal/store/store.go`

---

## Step 1 — WAL + fsync + crash recovery

**Fixes:** `TestDataLossOnTruncatedWrite`

1. Call `file.Sync()` after every `WriteAt`.
2. In `replay()`: detect torn tail write via CRC mismatch → `file.Truncate(lastGoodOffset)`.
   Store opens cleanly; partial entry is excised, not silently skipped.

Files: `internal/store/store.go`

---

## Step 2 — Log compaction

**Fixes:** `TestLogGrowsUnbounded`, `TestDeleteDoesNotReclaimDisk`  
**Required** (not stretch) — prerequisite for bounded Raft snapshot cost.

Atomic single-file compaction:
1. Write all live key-value pairs to `data.log.compact` (binary format, fsync).
2. `os.Rename("data.log.compact", "data.log")` — atomic on POSIX.
3. Reopen file handle; rebuild writePos.

Triggered when `file.Size() > compactionThreshold` (default 32 MB, configurable).
Add `Compact()` as a public method; call automatically from `Put`/`Delete` when threshold crossed.

Files: `internal/store/store.go`

---

## Step 3 — Hint file

**Fixes:** `TestReplayScansFullLogHistory`

Written immediately after every successful compaction:

```
┌─────────┬─────────┬────────────┬──────────────┐
│ key_sz  │ val_sz  │  val_pos   │     key      │
│ 2 bytes │ 4 bytes │  8 bytes   │ key_sz bytes │
└─────────┴─────────┴────────────┴──────────────┘
  val_pos = byte offset of record start in the data file
```

On `Open()`: if `data.log.hint` exists and is readable, load it to build the index in
O(live keys). Missing or corrupt hint file → fall back to full log replay.

Files: `internal/store/store.go`

---

## Step 4 — Raft replication (3-node cluster)

**New capability:** fault-tolerant replication; linearizable writes survive leader failure.

```
HTTP client
    │
    ▼
HTTP server  ──(write)──→  raft.Apply(cmd)  ──(on commit)──→  FSM.Apply()  →  store.Put/Delete
             ──(read)───→  store.Get                      (stale, default)
             ──(read?consistent=true)──→  raft.Barrier()  →  store.Get    (linearizable)
             ──(not leader)──→  307 redirect to leader
```

**New package `internal/raftnode`:**
- `FSM`: implements `raft.FSM` — `Apply`, `Snapshot`, `Restore`
- `Command`: serialized PUT/DELETE (`encoding/gob`)
- Node wiring: `raft.New(config, fsm, logStore, stableStore, snapshotStore, transport)`

**New dependencies:**
- `github.com/hashicorp/raft`
- `github.com/hashicorp/raft-boltdb/v2` — LogStore + StableStore

**`cmd/kvserver` new flags:**
- `--node-id` — unique node name (e.g. `node1`)
- `--raft-addr` — Raft TCP bind address (e.g. `127.0.0.1:7000`)
- `--peers` — comma-separated `id=raft-addr` pairs for initial cluster
- `--data-dir` — directory for BoltDB, snapshots, KV log (replaces `--log-file`)

**`internal/server` changes:**
- Writes: call `raft.Apply(cmd, timeout)`; return `503` if not leader with `X-Raft-Leader` header
- Reads: `store.Get` by default; `?consistent=true` calls `raft.Barrier()` first
- `GET /health`: add Raft state (leader/follower/candidate) and leader address

Files: `internal/raftnode/fsm.go` (new), `internal/raftnode/command.go` (new),
`internal/server/server.go`, `cmd/kvserver/main.go`, `go.mod`

---

## Step 5 (stretch) — Snapshot transfer

Needed when a new node joins after old log entries have been compacted away.

- `FSM.Snapshot()`: iterate live index, write all key-value pairs to snapshot sink (binary format)
- `FSM.Restore(r)`: read snapshot, replace live data file, rebuild index

Files: `internal/raftnode/fsm.go`

---

## Step 6 (stretch) — Read-index for linearizable reads

Already wired in Step 4 (`?consistent=true` → `raft.Barrier()`). This step benchmarks
the latency cost and documents the trade-off in the README.

---

## Failure test disposition after V2

| Test | Expected outcome |
|------|-----------------|
| `TestDataLossOnTruncatedWrite` | Now **fails** — torn write is recovered |
| `TestLogGrowsUnbounded` | Now **fails** — compaction keeps file bounded |
| `TestCorruptEntryDroppedSilently` | Now **fails** — `Open()` returns an error |
| `TestDeleteDoesNotReclaimDisk` | Now **fails** — compaction reclaims space |
| `TestReplayScansFullLogHistory` | Still **passes** but logged time drops sharply |
| `TestReplayTombstoneWithoutPut` | Still **passes** — unchanged behaviour |
