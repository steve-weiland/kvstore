# How Raft Works (and how kvstore uses it)

Raft is a consensus algorithm: it keeps an identical, ordered log of commands on
every node in a cluster, even when nodes crash or the network drops messages.
This doc is a short tour of the algorithm, then a map from each concept onto the
code in `internal/raftnode`.

## The algorithm in one page

### Roles

Every node is a **follower**, a **candidate**, or the **leader**. At most one
leader exists per *term* (a monotonically increasing election counter).

- **Follower** — passive. Answers the leader's RPCs and resets an election timer
  each time it hears from a live leader.
- **Candidate** — a follower whose election timer expired. It increments the
  term and asks the others for votes.
- **Leader** — won a majority of votes. It is the only node that accepts writes,
  and it heartbeats followers to keep them from starting new elections.

### Leader election

Election timeouts are randomised (~150–300 ms), so one follower usually times out
first and wins before the others try. A node grants its vote only if the
candidate's log is at least as up to date as its own — which is what guarantees a
new leader already holds every committed entry. If the vote splits, nobody gets a
majority, the term ends with no leader, and the randomised timers make the next
round very likely to settle.

### Log replication

1. A client sends a write to the leader.
2. The leader appends the command to its own log (not yet committed) and sends
   `AppendEntries` to every follower.
3. Once a **quorum** (majority — 2 of 3) has durably written the entry, the
   leader marks it **committed**.
4. Each node then *applies* the committed entry to its state machine, in log
   order. Same log + same order = same state everywhere.

A minority partition can elect nothing and commit nothing, so a split brain
cannot produce two conflicting histories. The cost is that the cluster needs a
majority alive to accept writes: 3 nodes tolerate 1 failure, 5 tolerate 2.

### Snapshots

The log cannot grow forever. Periodically a node serialises its state machine
into a snapshot and discards the log prefix the snapshot covers. A follower that
has fallen too far behind (or a brand-new node) is caught up by shipping it the
snapshot instead of replaying history from the beginning.

## How kvstore uses it

We do not implement Raft — we use [`hashicorp/raft`](https://github.com/hashicorp/raft)
and supply the two things a library cannot provide: a state machine, and a
command encoding.

```
HTTP PUT ──► internal/server ──► Node.Apply ──► [Raft: replicate to quorum]
                                                          │ on commit
                                                          ▼
                                              FSM.Apply ──► store.Put
```

### The pieces

| Raft concept | Where it lives |
|---|---|
| Node lifecycle, transport, bootstrap | `internal/raftnode/node.go` |
| State machine (apply / snapshot / restore) | `internal/raftnode/fsm.go` |
| Replicated command payload | `internal/raftnode/command.go` |
| Raft log + stable store (term, vote) | BoltDB at `data/nodeN/raft/raft.db` |
| Snapshots | `data/nodeN/raft/snapshots` |
| Applied state | the Bitcask log `data/nodeN/kv.log` (`internal/store`) |

### Writes

`server.applyWrite` calls `Node.Apply`, which gob-encodes a
`Command{Op, Key, Value}` and hands it to `raft.Apply` with a 5 s timeout. When
the entry commits, `FSM.Apply` decodes it and calls `store.Put` or
`store.Delete` on **every** node. The HTTP handler returns `204` only after the
command is committed and applied locally.

Only the leader may call `Apply`. A follower that receives a write fails, and
`server.writeError` answers `307 Temporary Redirect` to the leader. The trick
that makes this cheap: **the Raft `ServerID` *is* the node's HTTP address**
(`http://localhost:9091`), so `raft.LeaderWithID()` already returns a URL a
client can follow — no separate service discovery. If no leader is known yet
(election in progress), the client gets `503`.

### Reads

- **Default (stale)** — `store.Get` reads the local index directly. No network,
  no consensus. A lagging follower may return a slightly old value.
- **`?consistent=true`** — `Node.Barrier` first. A barrier is an empty log entry
  pushed through consensus; when it commits, the FSM has provably applied every
  entry committed before the read began, so the subsequent `Get` is
  linearizable. It costs about as much as a write. `Barrier` is leader-only, so
  a follower redirects with `307` — preserving the query string — rather than
  failing.

### Snapshots and restore

`FSM.Snapshot` calls `store.Snapshot` to grab the live key set and writes it as
JSON to the snapshot sink; `FSM.Restore` decodes that and calls
`store.ReplaceContents`, atomically swapping the node's whole data file. This is
how a rejoining or lagging node gets caught up.

### Durability

`store.Open` runs with `WithSyncWrites(false)` under Raft. That is deliberate,
not a shortcut: BoltDB already fsyncs the Raft log entry *before* it is
acknowledged, and the KV file is derived state that can be rebuilt by replaying
the Raft log. Fsyncing both would pay the same durability cost twice — see
[perf-notes.md](perf-notes.md).

### Cluster formation

On first start (`raft.HasExistingState` is false) a node calls
`BootstrapCluster` with the peer set from `-peers`. Afterwards the configuration
lives in BoltDB and is not re-bootstrapped. `make run` starts a single-node
cluster that elects itself instantly; `make run-cluster` starts three nodes on
`:9091–:9093`.

## Where Raft shows up in the wild

Five real systems, each a different shape of the same problem.

**etcd — the Kubernetes control plane store.** Every Kubernetes object (pods,
deployments, secrets) lives in etcd, replicated by Raft across 3 or 5
control-plane nodes. The scheduler and controllers depend on watches returning a
single consistent ordering of changes, so two schedulers can never see divergent
state and double-book a pod. This is the closest production cousin to what this
repo builds.

**Consul / Nomad — service discovery and cluster membership.** HashiCorp's
servers replicate the service catalog, health state, and KV config through the
same `hashicorp/raft` library used here. The catalog is read-mostly and
write-rare, which is Raft's sweet spot: writes cost a quorum round-trip, but
every server can serve reads locally.

**TiKV / CockroachDB — sharded transactional databases.** The interesting
variant: instead of one Raft group per cluster, the keyspace is split into
thousands of ranges, each its own independent Raft group with its own leader.
That is how Raft scales past the "writes do not scale with node count" ceiling
noted in [perf-notes.md](perf-notes.md) — you do not make one group faster, you
run many groups in parallel.

**Kafka KRaft — metadata without ZooKeeper.** Kafka moved its controller
metadata (topics, partitions, leader assignments, ISR lists) into a self-managed
Raft quorum, retiring the external ZooKeeper dependency in 4.0. Note the split:
metadata gets Raft, but the message partitions themselves use Kafka's own ISR
replication, trading strict consensus for throughput. Consensus belongs on the
control plane, not always the data plane.

**Distributed locks and leader election for other services.** The most common
indirect use. A fleet of workers needs exactly one leader to run a cron job or
own a shard, so they take a lease from a Raft-backed store (etcd, Consul) rather
than each implementing consensus. The lease has a TTL, so a partitioned leader's
grip expires and the work fails over safely. Most teams consume Raft this way
without ever running it themselves.

The thread through all five: Raft is for the small, critical metadata that must
have exactly one agreed history — who is the leader, where is the shard, what is
the config. Bulk data usually gets cheaper replication layered underneath.

## Further reading

- [In Search of an Understandable Consensus Algorithm](https://raft.github.io/raft.pdf) — the Raft paper (extended version); §5 is the core.
- [raft.github.io](https://raft.github.io/) — the visualisation is worth five minutes.
- DDIA ch. 9 — consensus in the wider context of linearizability and ordering.
