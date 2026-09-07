# Key-Value Store — V2 (Durable + Distributed)

| Field   | Value              |
|---------|--------------------|
| Version | 0.6                |
| Author  | Steve Weiland       |
| Date    | 2026-04-21         |
| Status  | Accepted           |

---

## 1. Overview

A three-node, Raft-replicated key-value store exposed over HTTP. Every mutation is written to
a binary append-only log with a CRC32 checksum and fsynced before acknowledgement. An in-memory
index maps each key to its latest log offset for O(1) reads. On startup the index is rebuilt
from a hint file (O(live keys)) if present, otherwise from a full log replay. The log is
compacted when it exceeds a configurable size threshold.

V2 fixes the six deliberate weaknesses documented in V1:

| V1 failure | V2 fix |
|-----------|--------|
| No fsync → torn write = data loss | WAL + fsync on every write |
| No checksums → corruption is silent | CRC32 per record; error on mismatch |
| No compaction → log grows unbounded | Atomic single-file compaction |
| Tombstones never reclaimed | Compaction drops tombstones and dead values |
| Replay is O(write history) | Hint file enables O(live keys) startup |
| Single node → no fault tolerance | Raft replication across 3 nodes |

---

## 2. Definitions

| Term | Definition |
|------|------------|
| Entry | One binary log record representing a single mutation (PUT or DELETE) |
| Tombstone | A DELETE entry; op byte = `0x02`; val_sz = 0 |
| Index | In-memory `map[string]int64` from key to byte offset of its latest entry |
| WAL | Write-ahead log; mutations are durable on disk before the client is acknowledged |
| CRC32 | IEEE CRC32 checksum covering key_sz, val_sz, op, key, and value bytes |
| Compaction | Rewriting only live entries to a new data file, atomically replacing the old one |
| Hint file | Companion file written after compaction; maps each live key to its data-file offset for fast index rebuild |
| Replay | Sequential scan of the data file to reconstruct the index when no hint file is present |
| FSM | Finite state machine; the Store implements `raft.FSM` so Raft can apply committed commands |
| Leader | The Raft node that accepts writes; other nodes redirect writes to the leader |
| Quorum | (N/2)+1 nodes; a write is committed once the leader replicates it to a quorum |

---

## 3. Requirements

Requirements use [RFC 2119](https://www.rfc-editor.org/rfc/rfc2119) keywords: **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, **MAY**.

### 3.1 Storage

| ID | Requirement |
|----|-------------|
| KV‑01 | The server **MUST** accept PUT, GET, and DELETE operations via HTTP. |
| KV‑02 | The server **MUST** append a log entry and receive Raft commit confirmation before sending the HTTP response. |
| KV‑03 | The server **MUST** maintain an in-memory index for O(1) key lookups. |
| KV‑04 | On startup, the server **MUST** load the hint file if present to rebuild the index in O(live keys); if absent or corrupt it **MUST** fall back to full log replay. |
| KV‑05 | A DELETE operation **MUST** append a tombstone entry (op = `0x02`, val_sz = 0). |
| KV‑06 | Keys **MUST** be non-empty UTF-8 strings of at most 1 KB. |
| KV‑07 | Values **MUST** be arbitrary bytes of at most 64 KB. |
| KV‑08 | The server **MUST** call `fsync`/`fdatasync` after every log write before updating the in-memory index. |
| KV‑09 | Each log entry **MUST** include a CRC32 checksum covering key_sz, val_sz, op, key, and value. |
| KV‑10 | On replay, a CRC mismatch on the final record (torn write) **MUST** cause the file to be truncated to the last good record; the store **MUST** open successfully. |
| KV‑11 | A CRC mismatch on any non-final record **MUST** cause `Open()` to return an error. |
| KV‑12 | The server **MUST** compact the log when the data file exceeds the compaction threshold. |
| KV‑13 | After compaction, the server **MUST** write a hint file alongside the data file. |
| KV‑14 | The compaction threshold **SHOULD** default to 32 MB and be configurable via CLI flag. |

### 3.2 HTTP API

| ID | Requirement |
|----|-------------|
| KV‑15 | The server **SHOULD** return `404 Not Found` for GET or DELETE on a key that is absent or tombstoned. |
| KV‑16 | The server **SHOULD** return `400 Bad Request` for an empty key or oversized payload. |
| KV‑17 | A non-leader node receiving a write **MUST** respond `307 Temporary Redirect` with a `Location` header pointing to the current leader's HTTP address. |
| KV‑18 | `GET /keys/{key}` **SHOULD** serve a potentially stale local read by default. |
| KV‑19 | `GET /keys/{key}?consistent=true` **MUST** call `raft.Barrier()` before reading, guaranteeing linearizability. |
| KV‑20 | The server **MAY** expose `GET /health` returning Raft state (role, leader address) and process uptime. |
| KV‑21 | The server **MAY** expose `GET /keys` returning a JSON array of all live keys. |

### 3.3 Cluster

| ID | Requirement |
|----|-------------|
| KV‑22 | A write **MUST** be replicated to a quorum of nodes before being committed. |
| KV‑23 | Node identity, Raft bind address, HTTP bind address, initial peer list, and data directory **MUST** be configurable via CLI flags. |
| KV‑24 | The cluster **SHOULD** consist of exactly 3 nodes for a quorum of 2. |
| KV‑25 | Each node **MUST** use BoltDB-backed LogStore and StableStore (`hashicorp/raft-boltdb`). |
| KV‑26 | A read **MUST NOT** return a record whose key differs from the requested key: `Get` verifies the decoded record's key against the request and returns `ErrCorrupt` on mismatch. The CRC proves a well-formed record, not the RIGHT record — a wrong index (e.g. a stale hint surviving a crash) must surface as an error, never as another key's value. |
| KV‑27 | Every rename/remove in compaction, hint writing, and snapshot restore **MUST** be followed by a directory fsync: rename durability is not ordered without it, and the surviving old-hint + new-data pair is exactly the wrong-index case KV‑26 guards. |
| KV‑28 | A `?consistent=true` read on a non-leader **MUST** redirect to the leader (307, query preserved) exactly as writes do — `Barrier` is leader-only, and a dead-end 503 on followers made consistent reads leader-only in practice while the docs implied any node. 503 only when no leader is known. |

---

## 4. Inputs / Outputs

### HTTP API (unchanged from V1)

```
PUT /keys/{key}
  Content-Type: application/octet-stream
  Body: raw bytes (≤ 64 KB); key in URL must be ≤ 1 KB

  204 No Content
  307 Temporary Redirect   Location: http://<leader-addr>/keys/{key}
  400 Bad Request          { "error": "key must not be empty" }
  413 Payload Too Large
  503 Service Unavailable  { "error": "no leader elected" }


GET /keys/{key}
GET /keys/{key}?consistent=true

  200 OK
  Content-Type: application/octet-stream
  Body: raw bytes

  404 Not Found    { "error": "key not found" }


DELETE /keys/{key}

  204 No Content
  307 Temporary Redirect   Location: http://<leader-addr>/keys/{key}
  404 Not Found            { "error": "key not found" }


GET /health

  200 OK
  {
    "status": "ok",
    "uptime_seconds": 42,
    "raft_leader": true | false,
    "leader_addr": "http://localhost:9091"   // omitted when no Raft (single-node mode)
  }
```

### Binary log record format

```
┌──────────┬─────────┬─────────┬─────┬─────────────┬─────────────┐
│  crc32   │ key_sz  │ val_sz  │ op  │    key      │    value    │
│  4 bytes │ 2 bytes │ 4 bytes │ 1 B │ key_sz bytes│ val_sz bytes│
└──────────┴─────────┴─────────┴─────┴─────────────┴─────────────┘

op:    0x01 = PUT   0x02 = DELETE (val_sz = 0, no value bytes)
crc32: IEEE CRC32 over bytes [4..end] (key_sz through end of value)
byte order: big-endian
```

### Hint file record format

Written atomically (temp file + rename) after every compaction. No CRC per entry — a corrupt
or missing hint file falls back to full log replay.

```
┌──────────────────┐
│ compact_size: 8  │   ← total byte length of the compacted data file
├──────────────────┤
│  per-key entries │
└──────────────────┘

Per-key entry:
┌─────────┬─────────┬────────────┬──────────────┐
│ key_sz  │ val_sz  │  val_pos   │     key      │
│ 2 bytes │ 4 bytes │  8 bytes   │ key_sz bytes │
└─────────┴─────────┴────────────┴──────────────┘

val_pos: byte offset of the record's start in the compacted data file
byte order: big-endian
```

On `Open()`: load hint → build index for the compacted prefix → replay data file from
`compact_size` to EOF to pick up any writes appended after the last compaction.

### CLI flags (`kvserver`)

```
--node-id     string   this node's HTTP address, used as Raft ServerID for leader redirects
                       e.g. "http://localhost:9091" (default "http://localhost:9090")
--raft-addr   string   Raft TCP bind address, e.g. "localhost:7001" (default "localhost:7000")
--http-addr   string   HTTP listen address (default ":9090")
--peers       string   comma-separated nodeID=raftAddr pairs for all cluster members
                       e.g. "http://localhost:9091=localhost:7001,http://localhost:9092=localhost:7002"
                       omit for single-node mode (node registers itself as sole voter)
--data-dir    string   directory for kv.log, kv.log.hint, raft/raft.db, raft/snapshots
                       (default "data")
--compact-mb  int      compaction threshold in MB (default 32)
```

---

## 5. Out of Scope

- Authentication, TLS, or access control
- TTL / key expiration
- Multi-key transactions or atomic batches
- Multi-segment log files (single-file compaction is sufficient for V2)
- Log format versioning or schema migration
- Dynamic cluster membership changes (static peer list only in V2)

---

## 6. Resolved Decisions

| # | Question | Decision |
|---|----------|----------|
| Q1 | **Compaction trigger**: size threshold only, or also time-based? | Size-only (32 MB default). Time-based adds complexity for minimal gain at V2 scale; revisit if delete-heavy workloads appear in benchmarks. |
| Q2 | **Leader redirect**: proxy writes transparently, or return 307? | 307 redirect. Simpler to implement, makes cluster topology visible to operators, and standard HTTP clients follow redirects automatically with `-L`. |

---

## 7. Revision History

| Version | Date | Author | Notes |
|---------|------|--------|-------|
| 0.1 | 2026-04-21 | Steve Weiland | Initial V1 draft |
| 0.2 | 2026-04-21 | Steve Weiland | Resolved Q1–Q3: JSON-lines log format, octet-stream HTTP, full log replay |
| 0.3 | 2026-04-21 | Steve Weiland | V2 draft: binary log + CRC, WAL/fsync, compaction, hint file, Raft replication |
| 0.4 | 2026-04-21 | Steve Weiland | Final: resolved Q1/Q2, corrected CLI flags, hint file format, health response shape |
| 0.5 | 2026-09-06 | Steve Weiland | Review fixes: KV‑26 (Get refuses a valid record for a DIFFERENT key — red test returned "beta-value" for alpha pre-fix, silently) and KV‑27 (directory fsyncs after every rename/remove in compact/hint/restore — the crash-ordering the hint-removal comment assumed but nothing enforced). |
| 0.6 | 2026-09-06 | Steve Weiland | KV‑28: consistent reads on followers redirect to the leader like writes (red: got 503, want 307; sabotage-verified). |
