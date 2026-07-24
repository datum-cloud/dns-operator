# DNS

Manage authoritative DNS the Kubernetes way. Declare a domain and its records as
ordinary Kubernetes resources, and the DNS service programs them into an
authoritative DNS backend and publishes them to a globally distributed serving
layer — no zone files, no backend API calls, no manual nameserver wiring.

You manage DNS through native Kubernetes resources, so it works with `kubectl`
and any Kubernetes client, and inherits your platform's identity, RBAC, and audit
controls.

This repository provides the **DNS operator**, the control-plane component that
reconciles those resources and programs the backend. See the
[Architecture Overview](docs/architecture/README.md) for how the operator fits
into the wider service.

## What it does

- **Zones and records as resources** — Model domains with `DNSZone` and records
  with `DNSRecordSet`, covering `A`, `AAAA`, `CNAME`, `ALIAS`, `TXT`, `MX`,
  `SRV`, `CAA`, `NS`, `SOA`, `PTR`, `TLSA`, `HTTPS`, and `SVCB` types.
- **Pluggable backends** — A cluster-scoped `DNSZoneClass` selects the backend
  and nameserver policy, keeping backend choice out of individual zones.
  [PowerDNS](https://doc.powerdns.com/authoritative/) is supported today.
- **Multi-tenant by design** — Each tenant authors DNS in their own control
  plane; the operator discovers and serves many control planes from one shared
  authoritative backend, with per-domain ownership accounting.
- **Automatic zone bootstrap** — Default `SOA` and `NS` records are created for
  every zone from its nameserver policy, without clobbering user-authored apex
  records.
- **Clear status** — `Accepted` and `Programmed` conditions report whether a
  zone or record is valid and actually serving, mirrored back from the
  authoritative backend.

## How it works

Users declare `DNSZone` and `DNSRecordSet` resources in their own control plane.
A **replicator** mirrors that desired state into a shared authoritative cluster,
where a **downstream agent** programs it into the DNS backend. The authoritative
data is then replicated to a read-only serving layer that answers live queries.

For the full picture — components, control planes, and the serving layer — see
the [Architecture Overview](docs/architecture/README.md).

## Documentation

**Architecture**
- [Architecture Overview](docs/architecture/README.md) — System design and core
  concepts
- [Deployment Topology](docs/architecture/topology.md) — Roles, control planes,
  and the serving layer
- [Replication Model](docs/architecture/replication.md) — How desired state and
  status move between clusters
- [API Reference](docs/architecture/api-reference.md) — Full resource schema and
  conditions

**Guides**
- [Service Catalog](config/components/service-catalog/README.md) — DNS as a
  billable platform service

## Deploying

The operator runs in one of two roles, deployed with the Kustomize overlays in
[`config/`](config). See [Deployment Topology](docs/architecture/topology.md) for
how the roles fit together.

### Agent with embedded PowerDNS

Runs the operator as a downstream agent alongside PowerDNS and a storage backend
— the quickest way to a working DNS service:

```sh
kubectl apply -k config/overlays/agent-powerdns
```

Then create a `DNSZoneClass`, `DNSZone`, and `DNSRecordSet` (see
[`config/samples`](config/samples) and the
[API Reference](docs/architecture/api-reference.md)).

### Replicator (upstream → downstream)

Runs the operator as a replicator that mirrors DNS resources from tenant control
planes into a downstream authoritative cluster:

```sh
# Provide the downstream cluster kubeconfig
kubectl -n dns-replicator-system create secret generic downstream-kubeconfig \
  --from-file=kubeconfig=/path/to/downstream/kubeconfig

kubectl apply -k config/overlays/replicator
```

The replicator mirrors upstream `DNSZone` / `DNSRecordSet` resources downstream
and synthesizes their status back upstream. See
[Replication Model](docs/architecture/replication.md).

## Development

- **Build:** `make docker-build` (see the [`Makefile`](Makefile))
- **Generate code / manifests:** `make generate` and `make manifests`
- **End-to-end tests:** see `test/e2e/` and the samples under `config/samples/`
