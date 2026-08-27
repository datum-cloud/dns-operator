# resource-metrics (drift-detection dependency)

Deploys the [resource-metrics](https://github.com/milo-os/resource-metrics)
controller that powers DNS control-plane drift detection: it emits a metric per
`DNSRecordSet`/`DNSZone` from every control plane so the alert rules can spot
records that have fallen out of sync between a customer's project and the
serving control plane. See `docs/enhancements/controlplane-drift-detection.md`
for the feature.

You don't normally apply this by hand — `task env:metrics-up` does it while
bringing up the e2e. The rest of this file is for changing or debugging it.

## What gets deployed, and where

resource-metrics comes from its own published release (a Flux-managed kustomize
bundle), not copied into this repo, so it tracks upstream. It's split in two
because the pieces live on different clusters:

- **`controller/`** — the controller, on the local kind cluster. This is the
  upstream `overlays/test-infra` bundle with our image tag and OTel endpoint
  patched in.
- **`core-control-plane/`** — its CRD and the `dns-metrics` policy, on the Milo
  core control plane (where the controller reads them). The CRD comes from the
  same bundle; the policy is ours and points back to
  `config/observability/dns-metrics-policy.yaml`.

## Changing it

- **Image tag / OTel endpoint** — `controller/flux-install.yaml`.
- **Bundle version** — pinned in `controller/ocirepository.yaml` (and reused by
  `core-control-plane/crd/`).

> [!NOTE]
> The OTel endpoint is overridden with a full-ConfigMap patch because the pinned
> bundle hardcodes it. After resource-metrics
> [#14](https://github.com/milo-os/resource-metrics/pull/14) (configurable
> endpoint) and [#13](https://github.com/milo-os/resource-metrics/pull/13)
> (multi-arch image) merge, bump the pin to a `v0.0.0-main` tag and swap the
> patch for an `OTEL_EXPORTER_OTLP_ENDPOINT` env patch.
