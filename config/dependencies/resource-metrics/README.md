# resource-metrics (drift-detection dependency)

Deploys the [resource-metrics](https://github.com/milo-os/resource-metrics)
controllers that power DNS control-plane drift detection: they emit a metric per
`DNSRecordSet`/`DNSZone` so the alert rules can spot records that have fallen out
of sync between a customer's project and the cluster that serves DNS. See
`docs/enhancements/controlplane-drift-detection.md` for the feature.

You don't normally apply this by hand — `task env:metrics-up` does it while
bringing up the e2e. The rest of this file is for changing or debugging it.

## What gets deployed, and where

resource-metrics comes from its own published release (a Flux-managed kustomize
bundle), not copied into this repo, so it tracks upstream.

There are **two** deployments, because the two replication seams are two
different API servers and each controller reads its policy from the cluster its
local manager targets:

- **`controller/`** — the upstream collector, `discovery.mode: milo`. It engages
  every project control plane. This is the upstream `overlays/test-infra` bundle
  with our image tag and OTel endpoint patched in, and `collectRootControlPlane`
  turned back off.
- **`core-control-plane/`** — the upstream collector's CRD and policy, on the
  Milo core control plane where it reads them. The CRD comes from the bundle; the
  policy is `config/milo/resource-metrics/policies/dns-metrics.yaml`, the same
  one infra ships to production.
- **`downstream/`** — the downstream collector, `discovery.mode: single`, on the
  cluster that stores the replicated shadow objects, plus its CRD and the RBAC
  letting it read DNS resources there. Its policy is
  `config/observability/cluster-policy`.

## Changing it

- **Image tag / OTel endpoint** — `controller/flux-install.yaml` for upstream,
  `downstream/flux-install.yaml` for downstream.
- **Bundle version** — pinned in `controller/ocirepository.yaml`, reused by
  `core-control-plane/crd/` and `downstream/`.

> [!NOTE]
> The OTel endpoint is overridden with a full-ConfigMap patch because the pinned
> bundle hardcodes it. After resource-metrics
> [#14](https://github.com/milo-os/resource-metrics/pull/14) (configurable
> endpoint) and [#13](https://github.com/milo-os/resource-metrics/pull/13)
> (multi-arch image) merge, bump the pin to a `v0.0.0-main` tag and swap the
> patch for an `OTEL_EXPORTER_OTLP_ENDPOINT` env patch.

> [!NOTE]
> The downstream deployment drops `--leader-elect`. resource-metrics hardcodes
> its lease namespace to `milo-system`, so two deployments on one cluster would
> contend for a single lease and only one would ever collect.
