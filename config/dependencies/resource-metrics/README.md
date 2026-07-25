# resource-metrics dependency (DNS control-plane drift detection)

Deploys [milo-os/resource-metrics](https://github.com/milo-os/resource-metrics)
for the drift-detection e2e by referencing its published kustomize bundle via
Flux — no vendored manifests. resource-metrics emits one gauge per
DNSRecordSet/DNSZone from each control plane, and recording rules diff them. See
`docs/enhancements/controlplane-drift-detection.md`.

The directory has two slices, applied separately because they target different
API servers. `env:metrics-up` in the Taskfile runs both.

## `controller/` — the controller, on the local (dns-control) cluster

Flux pulls the operator's `overlays/test-infra` bundle, which already ships the
Deployment, RBAC, namespace, the `milo-kubeconfig` Secret, and a `mode: milo` +
`collectRootControlPlane: true` server-config. This slice overrides two things.

| File | Purpose |
| --- | --- |
| `ocirepository.yaml` | `OCIRepository` on `resource-metrics-kustomize`, pinned to a bundle tag with a multi-arch image. |
| `flux-install.yaml` | `Kustomization` on `overlays/test-infra`; overrides the image tag and patches the OTLP endpoint to our collector (`telemetry-system`, not the overlay default `otel-collector-system`). |
| `kustomization.yaml` | Applies both Flux resources into `flux-system`. |

```sh
kubectl --context kind-dns-control apply -k config/dependencies/resource-metrics/controller
kubectl --context kind-dns-control -n flux-system wait kustomization/resource-metrics --for=condition=Ready --timeout=300s
```

The controller reaches the core CP over the in-cluster Service
(`milo-apiserver.milo-system.svc.cluster.local:6443`, self-signed →
`insecure-skip-tls-verify`) with the test-only `test-admin-token`.

> [!NOTE]
> The endpoint override is a full-ConfigMap patch because the pinned bundle
> hardcodes the endpoint. After resource-metrics
> [#14](https://github.com/milo-os/resource-metrics/pull/14) (configurable
> endpoint) and [#13](https://github.com/milo-os/resource-metrics/pull/13)
> (multi-arch image) merge, bump `ocirepository.yaml` to a `v0.0.0-main` tag and
> replace the patch with an env patch:
> `OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector-collector.telemetry-system:4317`.

## `core-control-plane/` — the CRD and policy, on the Milo core CP

These target `milo-apiserver` — where the controller reads its policy — not the
kind apiserver.

| Path | Purpose |
| --- | --- |
| `crd/` | Flux `Kustomization` (on the local cluster) that installs the `ResourceMetricsPolicy` CRD onto the core CP from the bundle (`path: crd`) via `kubeConfig.secretRef`. Mirrors infra's `milo-control-plane.yaml`. Includes the test-only milo-kubeconfig Secret. |
| `policy/` | Applies the `dns-metrics` `ResourceMetricsPolicy`, which references `config/observability/dns-metrics-policy.yaml` (the single source), after the CRD is Established. |

```sh
kubectl --context kind-dns-control apply -k config/dependencies/resource-metrics/core-control-plane/crd
kubectl --context kind-dns-control -n flux-system wait kustomization/resource-metrics-crd --for=condition=Ready --timeout=180s
kustomize build --load-restrictor LoadRestrictionsNone \
  config/dependencies/resource-metrics/core-control-plane/policy \
  | kubectl --kubeconfig <milo-kubeconfig> apply -f -
```
