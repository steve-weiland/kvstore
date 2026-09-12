# Performance Notes

## Assumptions

- Commodity hardware: 4-core CPU, 16 GB RAM, SATA SSD (~0.5 ms fsync latency)
- 1 Gbps LAN, ~0.3 ms RTT between nodes
- Small values (< 1 KB) — latency-bound, not bandwidth-bound
- 3-node cluster, quorum of 2

## Write path

```
leader BoltDB fsync (0.5 ms)  ─┐
                                ├─ ~1 ms critical path (parallel)
follower RTT + fsync (1 ms)   ─┘
+ FSM.Apply / KV store write       ~0 ms (sync disabled under Raft)
──────────────────────────────────────
~1–1.5 ms per write round-trip
```

| Scenario | Throughput |
|----------|-----------|
| Single client, no batching | ~700–1 000 writes/sec |
| Concurrent clients (Raft batches AppendEntries) | ~6 000–15 000 writes/sec |

For comparison: etcd v3 on similar hardware benchmarks at ~10 000–30 000 writes/sec.

## Read path

| Mode | Throughput | Notes |
|------|-----------|-------|
| Stale read (default) | ~50 000–100 000/sec | Local map lookup + `ReadAt`, no network |
| Consistent read (`?consistent=true`) | ~1 000–3 000/sec | `raft.Barrier()` — same cost as a write |

## Scaling observations

- **Writes do not scale with node count.** Adding nodes increases quorum size and therefore
  write latency. Stop at 5 nodes; beyond that fault-tolerance gains rarely justify the cost.
- **Reads scale horizontally.** Every follower can serve stale reads; adding nodes increases
  read throughput linearly.
- **Large values (near 64 KB limit)** shift the bottleneck from fsync latency to SSD
  bandwidth (~500 MB/s on SATA). At 64 KB per write, throughput caps at ~1 000–2 000
  writes/sec regardless of concurrency.

## What was already optimised

- **`WithSyncWrites(false)` under Raft** — the KV store no longer calls `file.Sync()` per
  write. BoltDB already fsyncs the committed log entry; the KV file is a derived state
  machine that can be rebuilt from the Raft log on crash. Removing this redundant fsync
  roughly doubled expected write throughput.

## TODO — benchmark

- [ ] Write a benchmark script (or `go test -bench`) that measures:
  - Single-client write throughput (sequential puts, small value)
  - Concurrent-client write throughput (N goroutines, measure at N=1, 10, 50, 100)
  - Stale read throughput (N goroutines)
  - Consistent read throughput
- [ ] Run against a real 3-node cluster (not loopback) to get honest RTT numbers
- [ ] Compare with and without `WithSyncWrites(false)` to validate the 2× estimate
- [ ] Profile under load (`go tool pprof`) to confirm fsync is the dominant cost, not
  encoding, locking, or HTTP overhead
- [ ] Record results here with hardware specs and date for future comparison
