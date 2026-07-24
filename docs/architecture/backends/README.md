# DNS Backends

A **backend** is the authoritative DNS server that the downstream agent programs
and that answers queries. The operator keeps backend choice out of individual
zones: a `DNSZoneClass` names a backend, and every `DNSZone` that references the
class uses it. This design lets the platform offer several backends and lets you
add new ones without changing the DNS API.

## Backend Model

The downstream agent talks to a backend through a narrow Go interface. The
interface covers two record operations — replace the records for one owner name
and type, and delete them — plus zone-level create, read, and delete. Each
backend implements this interface; the reconcilers never call a backend's native
API directly.

Three properties hold for every backend:

- **Selection by name.** A `DNSZoneClass` sets `spec.controllerName` (for
  example, `powerdns`). The agent acts only on zones whose class names a backend
  it implements and ignores every other class. One deployment can therefore host
  several classes and several backends at once.
- **Single writer.** For each (zone, record type, owner name) tuple, the agent
  programs exactly one owner. When several `DNSRecordSet` resources target the
  same tuple, the agent picks one owner and marks the rest `Programmed=False`
  with reason `NotOwner`, so conflicting records never overwrite each other.
- **Authoritative reconciliation.** The agent treats the desired state as
  authoritative. It replaces the owners a zone should have and deletes owners of
  the same type that no longer belong, so the backend converges on the declared
  records.

Because these properties live in the agent rather than in any one backend, every
backend behaves consistently from the user's point of view. A user picks a
`DNSZoneClass`; the choice of authoritative server behind it stays an operator
concern.

## Available Backends

| Backend | `controllerName` | Status | Documentation |
|---------|------------------|--------|---------------|
| [PowerDNS](./powerdns.md) | `powerdns` | Implemented | [PowerDNS backend](./powerdns.md) |

The platform expects to offer more backends over time. Each new backend adds a
row here and a companion page that documents its specifics.

## Adding a Backend

Adding a backend is an operator task, not an API change. At a high level:

1. Implement the backend interface for the target authoritative server.
2. Choose a `controllerName` and register the backend under it.
3. Package the server and its dependencies in a deployment overlay, following the
   pattern in [`config/agent`](../../../config/agent).
4. Add a companion page under this directory and a row to
   [Available Backends](#available-backends).

The API types, replicator, zone-class model, and status conditions stay
unchanged, so existing zones and clients keep working.

## Related

- [Architecture Overview](../README.md) — Zone classes and the backend concept
- [Deployment Topology](../topology.md) — Where a backend and its serving layer
  run
- [API Reference](../api-reference.md#dnszoneclass) — `DNSZoneClass` schema
