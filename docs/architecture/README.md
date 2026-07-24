# DNS Operator Architecture

The DNS operator is the control plane for a multi-tenant, Kubernetes-native
authoritative DNS service. Platform users declare zones and records as ordinary
Kubernetes resources in their own control plane; the operator carries that
desired state down to an authoritative DNS backend and publishes the resulting
records to a globally distributed serving layer.

## How It Works

Users author three kinds of resources — a `DNSZoneClass` that names the backend
and nameserver policy, a `DNSZone` for each domain, and `DNSRecordSet` resources
for the records within it. These live in the user's own **tenant control plane**,
so they inherit the platform's identity, RBAC, and audit surface for free.

The operator runs in one of two roles that split the work across trust
boundaries:

- A **replicator** watches tenant control planes, mirrors the desired state into
  a shared authoritative cluster, and synthesizes status back up so users see
  whether their DNS is live.
- A **downstream agent** runs next to the DNS backend, translates records into
  backend calls, and owns the authoritative zone data.

The authoritative data is then fanned out to a read-only **serving layer** that
answers live DNS queries close to end users.

This separation means tenants never touch the DNS backend directly, backend
technology and cluster topology stay hidden, and the authoritative data has a
single writer.

## System Context

<p align="center">
  <img src="./diagrams/system-context.png" alt="System Context — DNS Operator" />
</p>

The tenant control plane publishes desired state (and, separately, audit logs
and events). The operator consumes desired state, programs the authoritative
backend, and writes status back. Recursive resolvers query the serving layer
directly over standard DNS. The system is built on proven open-source
technologies.

## Core Concepts

### Zones, Records, and Zone Classes

- **[`DNSZone`](./api-reference.md#dnszone)** models a single domain. Its status
  reports the authoritative nameservers and readiness.
- **[`DNSRecordSet`](./api-reference.md#dnsrecordset)** models the records for one
  owner name and type within a zone (A, AAAA, CNAME, ALIAS, TXT, MX, SRV, CAA,
  NS, SOA, PTR, TLSA, HTTPS, SVCB).
- **[`DNSZoneClass`](./api-reference.md#dnszoneclass)** is a cluster-scoped policy,
  analogous to a `StorageClass`, that selects the **backend** (via
  `controllerName`) and the **nameserver policy** for every zone that references
  it. This is the seam that keeps backend choice out of individual zones.

See the [API Reference](./api-reference.md) for the full resource schema.

### Backends

A `DNSZoneClass` selects a backend by name through `spec.controllerName`. The
downstream agent only acts on zones whose class names a backend it implements,
so a single deployment can host multiple classes and multiple backends side by
side. **PowerDNS** is the backend implemented today; the record-translation and
zone-management logic sit behind a narrow backend interface so additional
authoritative servers can be added without touching the reconcilers.

### Multi-Tenancy via Control Plane Discovery

Tenants are isolated by control plane rather than by namespace convention. The
replicator **discovers** the control planes it should serve and reconciles each
one independently:

- **`single`** — a single upstream cluster (the local one). Used for
  self-contained deployments and development.
- **`milo`** — dynamically discovers per-project control planes from a platform
  control plane and connects to each. Used for the multi-tenant platform.

Because desired state lives in the tenant's own control plane, tenants only ever
see their own zones and records, and the operator's authoritative cluster is
never exposed to them.

### Status Synthesis and Conditions

Every resource carries two conditions the operator drives:

- **`Accepted`** — the resource is valid and its dependencies are satisfied.
- **`Programmed`** — the desired state is realized in the backend.

The replicator mirrors realized status from the authoritative cluster back to
the tenant control plane, so a user watching a `DNSZone` sees `Programmed=True`
only once the zone is actually serving. See
[Replication Model](./replication.md) for how status is synthesized across the
two clusters.

## Technology Stack

| Component | Technology | Purpose |
|-----------|------------|---------|
| **API model** | [Custom Resource Definitions](https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/custom-resources/) | Zones, records, and zone classes as native Kubernetes objects |
| **Controller runtime** | [controller-runtime](https://github.com/kubernetes-sigs/controller-runtime) + [multicluster-runtime](https://github.com/kubernetes-sigs/multicluster-runtime) | Reconciliation across one or many control planes |
| **Authoritative backend** | [PowerDNS Authoritative Server](https://doc.powerdns.com/authoritative/) | Serves authoritative zone data |
| **State replication** | [LightningStream](https://github.com/PowerDNS/lightningstream) + object storage | Replicates the authoritative LMDB store to the serving layer |
| **Activity** | [Activity Service](../enhancements/activity-integration.md) | Human-readable DNS activity timelines |

## API Resources

Exposed under `dns.networking.miloapis.com/v1alpha1`:

| Resource | Scope | Description |
|----------|-------|-------------|
| `DNSZoneClass` | Cluster | Selects backend and nameserver policy for zones |
| `DNSZone` | Namespaced | A single authoritative domain |
| `DNSRecordSet` | Namespaced | Records for one owner name and type within a zone |
| `DNSZoneDiscovery` | Namespaced | One-shot snapshot of a zone's live records |

See the [API Reference](./api-reference.md) for complete field documentation.

## Learn More

- [Deployment Topology](./topology.md) — Roles, control planes, and the serving
  layer
- [Replication Model](./replication.md) — Shadow objects, namespace mapping, and
  status synthesis
- [API Reference](./api-reference.md) — Full resource schema and conditions
- [Activity Integration](../enhancements/activity-integration.md) — Human-readable
  DNS activity timelines

## References

- [PowerDNS Authoritative Server](https://doc.powerdns.com/authoritative/)
- [Kubernetes Custom Resources](https://kubernetes.io/docs/concepts/extend-kubernetes/api-extension/custom-resources/)
- [multicluster-runtime](https://github.com/kubernetes-sigs/multicluster-runtime)
- [LightningStream](https://github.com/PowerDNS/lightningstream)
- [C4 model](https://c4model.com) — notation used for the diagrams in these docs
