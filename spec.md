# Key-Value Store — V1 (Single-Node)

| Field   | Value              |
|---------|--------------------|
| Version | 0.2 (draft)        |
| Author  | Steve Weiland       |
| Date    | 2026-04-21         |
| Status  | In review          |

---

## 1. Overview

A single-node, persistent key-value store exposed over HTTP. Every mutation (PUT, DELETE) is appended to an on-disk log file; an in-memory hash table maps each key to the byte offset of its most recent log entry, giving O(1) reads without scanning the log. On process startup, the index is rebuilt by replaying the log from the beginning.

V1 is deliberately minimal: no fsync, no checksums, no compaction. These omissions are intentional failure modes to explore in the break-it phase (kill the process mid-write; corrupt the log file) before fixing them in V2 with a WAL and crash-recovery path.

---

## 2. Definitions

| Term | Definition |
|------|------------|
| Entry | One log record representing a single mutation (PUT or DELETE) |
| Tombstone | A DELETE entry; signals that a key no longer has a live value |
| Index | In-memory `map[string]int64` from key to the byte offset of its latest entry |
| Segment | The single append-only log file used in V1 (multi-segment compaction is out of scope) |
| Replay | Sequential scan of the log on startup to reconstruct the index |

---

## 3. Requirements

Requirements use [RFC 2119](https://www.rfc-editor.org/rfc/rfc2119) keywords: **MUST**, **MUST NOT**, **SHOULD**, **SHOULD NOT**, **MAY**.

| ID    | Requirement |
|-------|-------------|
| KV‑01 | The server **MUST** accept PUT, GET, and DELETE operations via HTTP. |
| KV‑02 | The server **MUST** append a log entry for every mutation before sending the HTTP response. |
| KV‑03 | The server **MUST** maintain an in-memory index for O(1) key lookups (no log scan per read). |
| KV‑04 | On startup, the server **MUST** rebuild the index by replaying the full log from byte 0. |
| KV‑05 | A DELETE operation **MUST** append a tombstone entry rather than removing prior entries. |
| KV‑06 | Keys **MUST** be non-empty UTF-8 strings of at most 1 KB. |
| KV‑07 | Values **MUST** be arbitrary bytes of at most 64 KB. |
| KV‑08 | The server **MUST NOT** call `fsync`/`fdatasync` after appending log entries. OS-buffered writes are acceptable in V1. |
| KV‑09 | The server **SHOULD** return `404 Not Found` for GET or DELETE on a key that is absent or tombstoned. |
| KV‑10 | The server **SHOULD** return `400 Bad Request` for an empty key or an oversized payload. |
| KV‑11 | The log file path and HTTP listen address **SHOULD** be configurable via CLI flags or environment variables. |
| KV‑12 | Concurrent HTTP requests **SHOULD** be serialized through a single write lock so log entries are never interleaved. |
| KV‑13 | Log entries **MUST** use newline-delimited JSON. Each entry is one UTF-8 line terminated by `\n`. PUT entries encode the value as standard base64: `{"op":"put","key":"<key>","value":"<base64>"}`. DELETE tombstones omit the value field: `{"op":"del","key":"<key>"}`. |
| KV‑14 | The server **MAY** expose `GET /health` returning `200 OK` with process uptime. |
| KV‑15 | The server **MAY** expose `GET /keys` returning a JSON array of all live keys. |

---

## 4. Inputs / Outputs

```
// Store or overwrite a value
PUT /keys/{key}
  Content-Type: application/octet-stream
  Body: raw bytes (≤ 64 KB); key in URL must be ≤ 1 KB

  204 No Content

  400 Bad Request  { "error": "key must not be empty" }
  400 Bad Request  { "error": "value exceeds maximum size" }
  413 Payload Too Large


// Retrieve a value
GET /keys/{key}

  200 OK
  Content-Type: application/octet-stream
  Body: raw bytes (decoded from base64 in log)

  404 Not Found    { "error": "key not found" }


// Delete a key
DELETE /keys/{key}

  204 No Content

  404 Not Found    { "error": "key not found" }


// Health check (optional)
GET /health

  200 OK           { "status": "ok", "uptime_seconds": 42 }
```

### Log entry format

Each line in the log file is a complete JSON object followed by `\n`. The index maps each key to the **byte offset of the start of its most recent entry line**; on GET the server seeks to that offset, reads until `\n`, parses JSON, and base64-decodes the value.

```
// PUT entry
{"op":"put","key":"session:abc","value":"aGVsbG8gd29ybGQ="}

// DELETE tombstone
{"op":"del","key":"session:abc"}
```

On index rebuild (startup), entries are processed left-to-right: each PUT overwrites the index for that key; each tombstone removes the key from the index.

---

## 5. Out of Scope

The following are explicitly excluded from V1:

- `fsync` or any durability guarantee (added in V2)
- Log checksums or entry-level CRC (added in V2)
- Crash recovery from a partially-written entry (added in V2)
- Raft replication or any multi-node coordination (added in V2)
- Log compaction or segment merging (stretch goal in V2)
- Authentication, TLS, or access control
- TTL / key expiration
- Multi-key transactions or atomic batches
- Log format versioning or schema migration

---

## 6. Open Questions

None.

---

## 7. Revision History

| Version | Date | Author | Notes |
|---------|------|--------|-------|
| 0.1 | 2026-04-21 | Steve Weiland | Initial V1 draft |
| 0.2 | 2026-04-21 | Steve Weiland | Resolved Q1–Q3: JSON-lines log format, octet-stream HTTP, full log replay |
