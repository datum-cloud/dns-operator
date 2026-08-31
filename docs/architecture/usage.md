# Usage Metering

DNS bills three signals declared by the published `ServiceConfiguration`
`dns-networking-miloapis-com`. This page describes how the operator **produces**
those events. Meter names, kinds, and units are immutable once published; see
[`config/components/service-catalog/services_v1alpha1_serviceconfiguration_dns.yaml`](../../config/components/service-catalog/services_v1alpha1_serviceconfiguration_dns.yaml).

The operator does not author `MeterDefinition` objects. The services-operator
fans the `ServiceConfiguration` out into billing CRDs. Events are emitted with
the [billing emission SDK](https://github.com/datum-cloud/billing/blob/main/docs/emitting-usage.md)
to the node-local Vector Agent (`http://localhost:9880/cloudevents` by default).
The pipeline attributes a project to a billing account; the operator never
self-declares who pays.

## Meters

| Meter | Kind | Unit | Emitter |
|-------|------|------|---------|
| `dns.networking.miloapis.com/zone/queries` | Delta | `{query}` | Downstream agent on every pod that answers queries (writer and edge) |
| `dns.networking.miloapis.com/zones` | Gauge | `{zone}` | Replicator leader |
| `dns.networking.miloapis.com/records/active` | Gauge | `{record}` | Replicator leader |

All three bill against `dns.networking.miloapis.com/DNSZone`. Dimensions:

- `zone/queries`: `rcode`, `record_type`, `location`
- `zones`: `location`
- `records/active`: `record_type`, `location`

`location` is the Datum point-of-presence configured on the operator
(`spec.usage.location`). Empty values are omitted so single-cluster deployments
need not invent a region.

Inventory gauges are emitted only from the replicator so hosted-zone count is
not multiplied by replica or edge count. Query deltas are emitted from each
replica that answered the query, with that pod's `location`.

## Query volume

PowerDNS Authoritative 5.1 exports questions and responses on
`protobuf-servers` (2-byte length-prefixed `PBDNSMessage` over TCP). The agent
listens on `127.0.0.1:4242` in the same pod.

The collector counts **responses only** so `rcode` is present and questions are
not double-counted. Each response's qname is mapped to a hosted zone by
longest-suffix match against the identity index.

The writer agent stamps a compact billing identity (`project`, `name`,
`namespace`, `uid`) as PowerDNS domain metadata kind `DATUM-USAGE` when it
ensures a zone. LightningStream replicates that metadata with the LMDB, so
edge pods — which have no `DNSZone` CRs — can attribute the same way the
writer does. The collector prefers Kubernetes shadows when they exist and
fills remaining domains from metadata.

Names that match no hosted zone are dropped. Counters are aggregated in memory
keyed by `(zone, rcode, record_type)` and flushed on `spec.usage.flushInterval`
(default 60s) as a single delta event per key. Quantity `0` is never emitted
(the SDK rejects it). If `Record` fails after the SDK retry budget with a
transient error, the collector restores that key and retries on the next flush.
`ValidationError` is a producer bug: those events are dropped, not retried.
The pipeline deduplicates on ULID, so at-least-once delivery does not
double-bill.

The collector implements `NeedLeaderElection() bool { return false }`. Every
replica that answered a query must count it; only the leader-elected agent
writes zone data.

## Attribution

`UsageEvent.Project.Name` is the Milo **project** (a plain name with no slash),
not the downstream `ns-<uid>` namespace and not the in-project namespace.

Downstream shadows already carry `meta.datumapis.com/upstream-*` annotations.
The collector and inventory reporter decode:

- project from `upstream-cluster-name` (`cluster-{name}`, `/` encoded as `_`).
  Milo cluster keys are `/p-abc`; billing strips the leading slash so the SDK
  accepts `p-abc`.
- resource name / namespace / UID from `upstream-name`, `upstream-namespace`,
  `upstream-uid`

`ResourceRef` always names the upstream `DNSZone`. On the writer this comes
from shadow annotations; on the edge it comes from `DATUM-USAGE` metadata.
Objects that cannot be attributed to a project, or that lack a UID, are not
billed.

## Configuration

Usage is **off** by default so environments without a Vector Agent stay quiet.

```yaml
usage:
  enabled: true
  endpoint: http://localhost:9880/cloudevents
  location: us-east-1
  flushInterval: 60s
  protobufListenAddress: 127.0.0.1:4242
```

When `enabled` is false or `endpoint` is empty, emitters use a no-op recorder.
The downstream collector still binds the protobuf port so PowerDNS can connect.

Inventory gauges are registered only when leader election is on. Overlays that
run multiple replicator replicas with `LEADER_ELECT=false` (for example
`config/overlays/replicator-milo`) skip the inventory reporter so gauges are
not multiplied.

The Vector Agent itself is platform infrastructure. This operator only needs the
endpoint to be reachable on localhost.

## Reliability

| Step | Guarantee |
|------|-----------|
| SDK `Record` returns nil | Event is on the Vector Agent disk buffer; the pipeline owns delivery |
| SDK `Record` returns a transient error | Collector keeps the delta and retries next flush |
| SDK `Record` returns `ValidationError` | Event is dropped; it will never become valid |
| Unknown meter / missing billing binding | Quarantined centrally; not dropped by the operator |

Pricing rates live in the billing/pricing engine, not in this repository.
