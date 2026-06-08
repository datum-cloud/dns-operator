# Service catalog

DNS's catalog identifiers — everything the services-operator and
downstream Milo controllers need in order to recognise DNS as a
billable service.

Concrete `Service` + `ServiceConfiguration` registrations are
deployable artefacts that the producing service publishes as part of
its own deployment process, so they live here in `dns-operator` rather
than in the [`datum-cloud/services`](https://github.com/datum-cloud/services)
API-surface repo.

## Contents

| Kind                   | Name                          | What it declares                                                            |
| ---------------------- | ----------------------------- | --------------------------------------------------------------------------- |
| `Service`              | `dns-networking-miloapis-com` | `serviceName`, display metadata, producer owner.                            |
| `ServiceConfiguration` | `dns-networking-miloapis-com` | DNS MonitoredResourceTypes, metrics, and billing routing. _(Phase 2 — TBD)_ |

Ships in `phase: Published`.

## Identity

- **`serviceName`: `dns.networking.miloapis.com`** — the canonical
  reverse-DNS identifier, matching the DNS API group. It is the
  cross-system join key used by `MeterDefinition`,
  `MonitoredResourceType`, billing exports, and the portal. Immutable
  once Published.
- **`metadata.name`: `dns-networking-miloapis-com`** — the Kubernetes
  slug (serviceName with dots replaced by dashes).

## Why a `ServiceConfiguration` instead of raw `MeterDefinition`s

The canonical producer-facing document for billing is
`services.miloapis.com/v1alpha1.ServiceConfiguration` — see
[`billing/docs/emitting-usage.md`](https://github.com/datum-cloud/billing/blob/main/docs/emitting-usage.md).
A producer authors _one_ document; the services-operator fans it out
into `billing.miloapis.com/MeterDefinition` and `MonitoredResourceType`
objects stamped `app.kubernetes.io/managed-by: services-operator`.
Producers must not author those downstream CRDs directly — edit the
`ServiceConfiguration` and let the fan-out catch up.

The DNS `ServiceConfiguration` is tracked as Phase 2 of the catalog
registration (metering): query volume, hosted zones, and record-set
inventory. See the
[catalog registration issue](https://github.com/datum-cloud/dns-operator/issues/44).

## Immutability after `Published`

- On `Service`: `spec.serviceName` is immutable. Display name and
  description may still evolve.
- On `ServiceConfiguration` (once added): `spec.metrics[].name`,
  `.kind`, `.unit` and `spec.monitoredResourceTypes[].type`, `.gvk` are
  immutable. Renames or unit changes ship as a new metric name (e.g.
  `.../v2`).

## Deployment

This bundle is **not** applied by the operator deployment overlays
(`config/agent`, `config/overlays/replicator`). It is deployed into the
services-operator's namespace by Flux from the infra repo, the same way
other services publish their catalog entries.
