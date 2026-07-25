# resource-metrics dependency (DNS control-plane drift detection)

Deploys the [milo-os/resource-metrics](https://github.com/milo-os/resource-metrics)
controller for the DNS control-plane drift-detection e2e, plus the pieces that
must live on the Milo core control plane.

`resource-metrics` watches every project (UPSTREAM) control plane served by
`milo-apiserver` and — with `discovery.collectRootControlPlane: true` — the
Milo core/root (DOWNSTREAM) control plane as well. It evaluates the
`dns-metrics` `ResourceMetricsPolicy` and pushes one gauge series per matching
object over OTLP to the test-infra OTel collector, which forwards to Victoria
Metrics. Recording rules then diff the upstream (source of truth) and
downstream (replicated copy) series to detect drift. See
`docs/enhancements/controlplane-drift-detection.md`.

## Topology (validated on the dns-control kind cluster)

- **dns-control** hosts the Milo core control plane (the DOWNSTREAM / "root"),
  the PowerDNS agent, RustFS, the observability stack, AND this
  resource-metrics controller.
- Project control planes **alpha/beta** (served by `milo-apiserver`) are the
  UPSTREAM.
- **dns-upstream** hosts the replicator; **dns-edge** hosts PowerDNS.

Because resource-metrics runs on dns-control alongside `milo-apiserver`, it
reaches the core CP over the in-cluster Service
(`https://milo-apiserver.milo-system.svc.cluster.local:6443`, self-signed →
`insecure-skip-tls-verify`) using the static `test-admin-token`
(`system:masters`).

## Two deployment slices, two different API servers

This directory is split by **which control plane / kubeconfig each slice
targets**. They are applied separately and never combined into a single
`kubectl apply`.

### `controller/` — applied to the KIND (dns-control) context

Rather than vendoring the controller manifests, this **references the operator's
published kustomize bundle** (`oci://ghcr.io/milo-os/resource-metrics-kustomize`,
`overlays/test-infra`) via Flux — the same pattern `config/dependencies/milo`
uses. That overlay already ships the Deployment, RBAC, namespace, the
`milo-kubeconfig` Secret, and a `mode: milo` + `collectRootControlPlane: true`
server-config, so we only override two things.

| File | Purpose |
| --- | --- |
| `ocirepository.yaml` | Flux `OCIRepository` on `resource-metrics-kustomize`, pinned to a bundle tag that ships a multi-arch image. |
| `flux-install.yaml` | Flux `Kustomization` on `overlays/test-infra` with two overrides: `images:` (the controller image tag) and a patch pointing the OTLP endpoint at our collector (`telemetry-system`, not the overlay default `otel-collector-system`). |
| `kustomization.yaml` | Applies the two Flux resources into `flux-system`. |

```sh
kubectl --context kind-dns-control apply -k config/dependencies/resource-metrics/controller
kubectl --context kind-dns-control -n flux-system wait kustomization/resource-metrics --for=condition=Ready --timeout=300s
```

> [!NOTE]
> The OTLP-endpoint override is a full-ConfigMap patch because the pinned bundle
> hardcodes the endpoint. Once [milo-os/resource-metrics#14](https://github.com/milo-os/resource-metrics/pull/14)
> (configurable endpoint) and [#13](https://github.com/milo-os/resource-metrics/pull/13)
> (multi-arch image) land in `main`, bump `ocirepository.yaml` to a `v0.0.0-main`
> tag and replace the patch with a Deployment env patch:
> `OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector-collector.telemetry-system:4317`.

### `core-control-plane/` — installed onto the Milo core control plane

These land on the **Milo core control plane** (`milo-apiserver`), NOT the kind
apiserver, because that is where the controller reads its policy from.

| Path | Purpose |
| --- | --- |
| `core-control-plane/crd/` | Flux `Kustomization` (applied to the local cluster) that installs the `ResourceMetricsPolicy` CRD onto the core CP **from the published bundle** (`path: crd`) via `kubeConfig.secretRef`. No vendored CRD. Mirrors infra `apps/resource-metrics-system/base/milo-control-plane.yaml`. Includes the milo-kubeconfig Secret Flux targets the core CP with (test-only `test-admin-token`). |
| `core-control-plane/policy/` | Applies the `dns-metrics` `ResourceMetricsPolicy`. This one is dns-operator-owned (not in the bundle), so it **references** `config/observability/dns-metrics-policy.yaml` (single source of truth) and is applied directly to the core CP after the CRD is Established. |

```sh
# 1) Install the CRD onto the core CP from the bundle (Flux objects go on the
#    LOCAL cluster; the Kustomization targets the core CP via its kubeConfig).
kubectl --context kind-dns-control apply -k config/dependencies/resource-metrics/core-control-plane/crd
kubectl --context kind-dns-control -n flux-system wait kustomization/resource-metrics-crd --for=condition=Ready --timeout=180s

# 2) Apply the dns-metrics policy to the core CP (point kubectl at milo-apiserver).
kustomize build --load-restrictor LoadRestrictionsNone \
  config/dependencies/resource-metrics/core-control-plane/policy \
  | kubectl --kubeconfig <milo-kubeconfig> apply -f -
```

## OTel endpoint assumption (confirm live)

`controller/server-config.yaml` sets:

```
otel.endpoint: otel-collector-collector.telemetry-system.svc.cluster.local:4317
```

Rationale: test-infra's `install-observability` task applies an
`OpenTelemetryCollector` CR named `otel-collector` in namespace
`telemetry-system`
(`.test-infra/components/observability/otel-collector/opentelemetry-collector.yaml`).
The OpenTelemetry Operator renders a Service named `<cr-name>-collector`
(`otel-collector-collector`), and the CR's `receivers.otlp.protocols.grpc`
listens on `:4317`. The task even waits on
`daemonset/otel-collector-collector` in `telemetry-system`, confirming the
name.

> [!NOTE]
> This differs from milo-os/infra, whose resource-metrics ships its own
> `metrics-collector` CR and points at
> `metrics-collector-collector.resource-metrics-system...:4317`. We reuse the
> shared test-infra collector in `telemetry-system` instead of deploying a
> second collector. If the collector CR name or namespace changes, update the
> endpoint. The collector CR is a **daemonset**; the OTel Operator still
> renders the `-collector` Service used above.

## Assumptions needing live confirmation

- **OTel endpoint** — as above; confirm `otel-collector-collector` exists in
  `telemetry-system` and serves gRPC on 4317 after `install-observability`.
- **Controller image tag** — defaults to `:latest`; pin to whatever tag is
  published/loaded for the e2e run.
- **`test-admin-token`** — must match milo-apiserver's `tokens.csv`
  (`milo-apiserver-auth-tokens` in `milo-system`).
