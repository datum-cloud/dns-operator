# Internal DNS per-zone reconciliation

With `perZoneReconcile` enabled, a private record change queues its zone for
compilation. It no longer waits for the control plane to reconcile every
context,
binding, and zone in the project. The option is off by default.

This document describes the implemented event routing and its limits. See
[Cached Private DNS zone
inputs](../architecture/internal-dns/cached-zone-inputs.md)
for the separate `cachedZoneInputs` option.

## Reconciliation work

The existing project controller processes project keys and zone keys. They
separate project-wide decisions from record compilation; they are not separate
controller deployments.

| Work | Responsibilities | Triggers |
| --- | --- | --- |
| Project reconciliation | Contexts, managed zones, associations, naming policies, reference state, resolver bindings, destination claims, and zone lease renewal | Project-wide resource changes and periodic reconciliation |
| Zone reconciliation | Registration references, complete zone compilation, publication, and input acceptance status | Record set, registration, and contribution changes; project reconciliation queues affected zones |
| Acknowledgment handling and refresh | Publication observations and project-facing serving status | Member acknowledgments and periodic refresh |

Project reconciliation keeps decisions that require the whole project. Examples
include duplicate consumer IDs, conflicting zone apexes, and destination
release.
It queues a zone when the zone's project-wide inputs change: its own version,
writer epoch, associations, or resolver bindings. It also queues an unchanged
zone after five minutes.

Before compiling, zone reconciliation pins each registration to its zone's
lifetime. If an association still refers to an older zone generation, the zone
key waits for project reconciliation to update that reference. The compiler does
not treat a temporarily outdated reference as a zone detachment.

## Event routing

The controller routes events as follows:

| Resource | Relevant changes | Queue key |
| --- | --- | --- |
| `DNSRecordSet` | Create, delete, or generation change | Old and new referenced zones |
| `DNSRegistration` | Create, delete, or generation change | Old and new referenced zones |
| `DNSRecordContribution` | Create, delete, or generation change | Registration key, which resolves to its zone key |
| `DNSZoneAssociation` | Create, delete, generation change, or `Accepted` change | Project |
| `DNSZone` | Create, delete, generation change, or finalizer change | Project and zone |
| `DNSResolverContext`, `DNSResolverAccessBinding` | Create, delete, or generation change | Project |
| `DNSNamingPolicy` | Create, delete, or generation change | Project |

A 75 ms delay combines repeated events for the same zone. Contribution events
resolve to the registration's zone key, so one worker at a time compiles that
zone. A process-wide limit bounds concurrent zone compilations across projects.
Input status changes do not queue another compilation.

Resolver bindings live in the service API and have no source-project watch.
Project reconciliation detects their changes and queues affected zones. The
acknowledgment handler does not directly queue zones; that integration remains
proposed work.

## Reads and consistency

Without `cachedZoneInputs`, each zone compilation lists its namespace's inputs
from the project API. Per-zone queueing alone does not reduce those lists.

With both options enabled, the compiler reads record sets, registrations, and
contributions from the watch cache. Associations, contexts, reference pins, and
resolver bindings remain live reads. The compiler starts each writer epoch from
a live baseline, confirms potential losses of accepted input with a live
compilation, and checks access again before committing. Status writes read the
current object and use optimistic locks. The
[cached-input consistency
checks](../architecture/internal-dns/cached-zone-inputs.md#consistency-checks)
define the full contract.

The compiler evaluates the complete zone in either mode. It still writes and
sends complete publication chunks. Content-addressed chunk delivery remains a
separate [incremental-zone proposal](internal-dns-incremental-zones.md).

## Ownership and recovery

Project reconciliation renews the zone leases held by its controller process.
A zone compilation commits only while that process holds the zone's current
lease. Different queue settings on two replicas do not bypass this ownership
check.

On shutdown, a controller stops acquiring or renewing leases and releases its
zone and shard leases with compare-and-swap writes. A new owner publishes under
a higher writer epoch even if the records did not change. This step lets the
new owner deliver an update that the previous owner committed but did not
export.
A running standby notices a released zone lease on its next project pass.

Region leadership is separate. One elected process per region runs the exporter
and acknowledgment refresh; every replica can consume acknowledgments. That
region lease does not select the owner of each zone.

## Rollout and validation

To enable per-zone reconciliation, set `perZoneReconcile: true` in the
control-plane runtime configuration. To use cached inputs, also set
`cachedZoneInputs: true`. Deploy a runtime that supports each option before
setting it.

Compare publication latency and API request rates before and after the change.
In the test deployment reported in
[the per-zone
implementation](https://github.com/datum-cloud/dns-operator/pull/345),
p50 update latency fell from 1.4 seconds to 0.8–1.2 seconds, and p50 delete
latency fell from 1.1 seconds to 0.7–0.9 seconds. These measurements describe
that
deployment, not a service-level objective.

When cached inputs are enabled, the compiler compares a cached plan with a live
plan at a five-minute interval and uses the live result. Differences increment
`internal_dns_cached_plan_mismatches_total`. To return to live input reads,
disable `cachedZoneInputs`. To return to project-wide compilation, disable both
options.

## Limits and proposed work

Per-zone reconciliation does not make regional publication independent. Zone
publication still uses one owner and the shared publication store. With
JetStream storage, each region needs the home publication broker to receive a
zone update. See the
[regional
dependencies](../architecture/internal-dns/README.md#publication-storage-and-regional-dependencies).

The following changes remain proposals:

- Direct zone queueing from acknowledgment status changes.
- A shared platform cache for reference pins and resolver bindings.
- Content-addressed chunks that avoid sending unchanged publication data.
- Independent writers and input sources for each region.

A missing event mapping can leave a zone unchanged until its periodic resync.
Any new compiler input must also have an event mapping. A burst across many
zones can increase concurrent API work; the process-wide compile limit and
client request budgets bound that work.
