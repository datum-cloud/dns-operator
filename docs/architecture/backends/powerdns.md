# PowerDNS Backend

[PowerDNS Authoritative Server](https://doc.powerdns.com/authoritative/) is the
backend the operator implements today. A zone opts in by referencing a
`DNSZoneClass` with `controllerName: powerdns`. This page documents how the
PowerDNS backend translates records, programs zones, stores data, and serves
queries. For the properties every backend shares, see the
[backend model](./README.md#backend-model).

The deployment view below shows where each component runs: the downstream agent
**as a sidecar in the PowerDNS writer pod** in the authoritative cluster (fed
shadow resources by the replicator through the cluster's Kubernetes API), and the
read-only serving nodes at the edge, replicating through shared object storage.

<p align="center">
  <img src="./powerdns-backend.png" alt="Deployment View — PowerDNS Backend" />
</p>

## Writer Tier

The authoritative writer is a single pod that bundles the agent with the
PowerDNS instance it programs:

| Container | Role |
|-----------|------|
| Downstream agent | Reconciles shadow resources and programs PowerDNS |
| `pdns-auth` | Authoritative server; owns this pod's LMDB store |
| `pdns-recursor` | Loopback-only resolver used for `ALIAS` expansion |
| LightningStream (`sync`) | Snapshots this pod's LMDB and merges peers' snapshots |

The agent also listens on `127.0.0.1:4242` for PowerDNS protobuf query logs and
emits billed `zone/queries` events from every pod that answers queries. The
writer stamps compact billing identity as PowerDNS domain metadata
(`DATUM-USAGE`); LightningStream copies it to edge pods so they can attribute
without `DNSZone` CRs. See [Usage Metering](../usage.md).

Three properties of this arrangement matter when reasoning about writes:

- **The agent programs PowerDNS over loopback.** It connects to the PowerDNS
  container in its own pod (`PDNS_API_URL`, `http://localhost:8082` in the
  shipped base). It never addresses another replica, and there is no
  load-balanced API path between agent and backend.
- **Each pod mints its own API key at startup.** An init container generates a
  random key into a pod-local volume shared only with that pod's PowerDNS. Keys
  are therefore per-pod, which makes the loopback path the only usable one by
  construction.
- **Exactly one agent writes at a time.** The writer may run more than one
  replica for availability, but the agent is leader-elected, so a single replica
  reconciles. On failover the write path moves to the new leader's pod and its
  local LMDB.

Every replica — not just the leader — runs LightningStream in `sync` mode, so
the leader's writes reach its peers through object storage rather than through
the cluster network. Peer replicas therefore converge on the same schedule as
the serving layer, described in [Consistency Model](#consistency-model).

> [!NOTE]
>
> A common misreading of this topology is that the agent talks to PowerDNS
> through a Kubernetes `Service` and may land on any replica. It does not. That
> distinction matters: a read-modify-write spread across replicas would be
> subject to the convergence window below, whereas the loopback path always
> reads and writes one consistent local store.

## Record Translation

The PowerDNS client translates each typed `DNSRecordSet` entry into the
presentation format PowerDNS expects. Translation handles the details that DNS
record types require:

- Synthesizes a date-based `SOA` serial (`YYYYMMDD01`) when a record omits it.
- Encodes `SVCB` and `HTTPS` service parameters.
- Quotes and escapes `TXT` content.
- Qualifies owner names against the zone and removes duplicate values.

## Zone and Record Programming

The client drives PowerDNS through its HTTP API, authenticating with an API key
in the `X-API-Key` header. For a zone, the agent ensures the zone exists and
applies the nameserver policy from its `DNSZoneClass`. For a record set, the
agent replaces the desired owners and deletes extraneous owners of the same
type. When PowerDNS rejects a change, the agent reports the failure on the
resource with reason `PDNSError` and a human-readable message.

## Storage and Serving

PowerDNS stores authoritative zone data in an embedded **LMDB** database. To
serve queries close to end users, the deployment replicates that database rather
than the query path:

1. A [LightningStream](https://github.com/PowerDNS/lightningstream) sidecar
   snapshots the LMDB store to shared, S3-compatible object storage.
2. Each serving node runs a read-only PowerDNS with a LightningStream sidecar
   that pulls the snapshots and answers queries.

Serving nodes hold only their local replica, so the deployment can add nodes
anywhere object storage is reachable. A serving node can also run a local
recursor to expand `ALIAS` records at query time. See
[Deployment Topology](../topology.md#authoritative-serving-and-state-replication)
for how the serving layer fits the wider system.

The write path is enforced at two layers rather than by convention. Writers run
LightningStream in `sync` mode and hold read-write credentials for the bucket;
serving nodes run it in `receive` mode and hold read-only credentials. Serving
nodes additionally disable the PowerDNS API and zone transfers, so nothing but a
merge from object storage can alter a serving node's data.

## Consistency Model

Every PowerDNS instance keeps its **own** LMDB store. LMDB is a single-process
embedded database and is never shared between pods, which is why replication
goes through object storage rather than a shared volume. The stores converge;
they are not a single copy.

LightningStream **merges** snapshots rather than overwriting them, resolving
per-record conflicts by last-writer-wins on a timestamp. Three PowerDNS settings
make that merge well-defined, and all of them are required on every instance:

| Setting | Why the merge needs it |
|---------|------------------------|
| `lmdb-lightning-stream=yes` | Stamps each record with the timestamp and originating instance the merge compares |
| `lmdb-flag-deleted=yes` | Records deletions as tombstones, so a delete propagates instead of being undone by a peer that still holds the record |
| `lmdb-random-ids=yes` | Assigns random zone IDs, so two instances creating zones independently do not collide on the same sequential ID |

Three consequences follow for anyone reasoning about the system:

- **Instance names must be globally unique within a bucket.** Each
  LightningStream instance publishes snapshots under its own name. If two
  instances share a name, each treats the other's snapshots as its own and
  neither merges — replication stops silently, with no error surfaced. A
  deployment spanning multiple clusters must therefore qualify the instance name
  beyond the pod name, which alone repeats across clusters.
- **Merge ordering depends on wall-clock time.** Last-writer-wins compares
  timestamps across hosts, so clock skew between instances can let a stale write
  win. Instances need synchronized clocks for the merge to reflect real ordering.
- **A write is immediately visible only on the instance that accepted it.** Every
  other instance sees it after a snapshot round-trip. An operation that is
  *rejected* because the accepting instance has not yet converged — a delete
  against a zone that instance has not seen, for example — writes no tombstone
  and therefore leaves nothing for the merge to resolve. Such an operation must
  be retried until it succeeds, never recorded as complete. The agent's
  reconcile loop provides that guarantee: desired state is always re-derived
  from Kubernetes, so a rejected write stays queued rather than being lost.

## Propagation and Timing

A record change reaches end users through four stages. Each stage adds delay, and
a different setting governs each one, so it helps to reason about them separately.

| Stage | What happens | What governs the delay |
|-------|--------------|------------------------|
| 1. Reconcile | The agent programs the change into the writer's PowerDNS through the HTTP API. | Controller queue latency, normally seconds. On a backend error the agent retries with exponential backoff, from `rateLimiterBaseDelay` (1s) up to `rateLimiterMaxDelay` (30s). |
| 2. Authoritative write | The writer's PowerDNS serves the change. It reads LMDB directly (`zone-cache-refresh-interval=0`), so no cache stands between the write and the answer. | Effectively immediate on the writer. |
| 3. Replicate to serving nodes | A LightningStream sidecar snapshots the LMDB to object storage; each serving node lists object storage, downloads new snapshots, and merges them. | The writer's `lmdb_poll_interval` plus each node's `storage_poll_interval` (see [LightningStream intervals](#lightningstream-intervals)), plus object-storage upload and download time — typically a few seconds. |
| 4. Resolver caching | Recursive resolvers and clients cache the answer until its TTL expires. They also cache a *missing* name for the zone's negative-cache TTL (the `SOA` minimum). | The record's TTL, and — for a newly added name — the negative-cache TTL of any earlier lookup. |

Stages 1–3 move a change from the API to every authoritative serving node,
typically within a few seconds. Stage 4 dominates what end users observe, because
a resolver keeps serving a cached answer until the TTL expires regardless of how
fast the authoritative servers update.

### LightningStream intervals

LightningStream drives stage 3. The deployment runs it with default intervals, so
a change reaches every serving node within a few seconds of becoming
authoritative on the writer, bounded mainly by object-storage latency:

| Setting | Default | Effect on propagation |
|---------|---------|-----------------------|
| `lmdb_poll_interval` | `1s` | How often the writer's sidecar checks LMDB for changes to snapshot. |
| `storage_poll_interval` | `1s` | How often a serving node lists object storage for new snapshots. |
| `storage_force_snapshot_interval` | `4h` | Writes a snapshot even with no changes, so an idle instance does not look stale. |
| `storage_retry_interval` | `5s` | Retry delay after a failed snapshot upload or download. |

Lowering the poll intervals shortens propagation at the cost of more frequent
storage listings; raising them does the reverse. These intervals govern only how
fast a change reaches the serving nodes — end users still see it no sooner than
their cached TTL allows.

Two practical consequences follow:

- **To make a change take effect quickly, lower the record's TTL before you make
  it.** Set a short TTL, wait for the old TTL to expire everywhere, change the
  record, then raise the TTL again. The serving pipeline itself adds only seconds
  to minutes; the TTL sets the ceiling.
- **A brand-new name can appear slowly if something looked it up first**, because
  resolvers cached the negative answer for the zone's negative-cache TTL. The
  operator's default `SOA` sets this value (see
  [Replication Model](../replication.md#default-soa-and-ns-records)).

## Configuration

The agent connects to PowerDNS through environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `PDNS_API_URL` | `http://127.0.0.1:8081` | PowerDNS HTTP API endpoint. |
| `PDNS_API_KEY` | — | API key (or use `PDNS_API_KEY_FILE`). |
| `PDNS_API_KEY_FILE` | — | Path to a file that contains the API key. |

The PowerDNS record-set controller adds its own tuning under
`controllers.dnsRecordSetPowerDNS` in the [`DNSOperator` server
config](../topology.md#operator-configuration):

| Field | Default | Description |
|-------|---------|-------------|
| `controllers.dnsRecordSetPowerDNS.maxConcurrentReconciles` | `4` | Concurrent reconciles for the PowerDNS record-set controller. |
| `controllers.dnsRecordSetPowerDNS.rateLimiterBaseDelay` | `1s` | Exponential backoff base delay. |
| `controllers.dnsRecordSetPowerDNS.rateLimiterMaxDelay` | `30s` | Exponential backoff max delay. |

The [`config/agent`](../../../config/agent) base bundles PowerDNS, the recursor,
and LightningStream alongside the agent and wires the API key through a shared
volume; the runnable [`config/overlays/agent-powerdns`](../../../config/overlays/agent-powerdns)
overlay adds the namespace and a storage backend. See
[Deployment Topology](../topology.md) for the deployment shape.

## Related

- [DNS Backends](./README.md) — The backend model that every backend shares
- [API Reference](../api-reference.md#dnszoneclass) — `DNSZoneClass` schema
- [Deployment Topology](../topology.md#operator-configuration) — The generic
  `DNSOperator` server config
