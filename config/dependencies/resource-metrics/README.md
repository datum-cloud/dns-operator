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

The controller Deployment, its namespace, RBAC, the `milo-kubeconfig` Secret,
and the `mode: milo` server-config. Apply with the **kind kubeconfig/context**
for dns-control. The controller itself then talks to the Milo core CP through
the mounted `milo-kubeconfig` Secret.

| File | Purpose |
| --- | --- |
| `namespace.yaml` | `resource-metrics-system` namespace. |
| `rbac.yaml` | ServiceAccount + ClusterRole + ClusterRoleBinding (vendored from the operator's `controller_rbac` component). |
| `milo-kubeconfig-secret.yaml` | Kubeconfig Secret → in-cluster `milo-apiserver` + `test-admin-token`, `insecure-skip-tls-verify`. Modeled on the operator's `overlays/test-infra/milo-kubeconfig-secret.yaml`. **Test-only** credential. |
| `server-config.yaml` | `ResourceMetricsOperator` config: `discovery.mode: milo`, `discoveryKubeconfigPath`/`projectKubeconfigPath` → `/etc/milo/kubeconfig`, and `discovery.collectRootControlPlane: true` (so the root CP is collected as cluster `root`). Also the OTLP endpoint. Rendered into the `resource-metrics-service-config` ConfigMap. |
| `deployment.yaml` | Controller Deployment (base manager + test-infra patch folded in: `KUBECONFIG` env, milo-kubeconfig mount at `/etc/milo`, `--server-config=/etc/resource-metrics/server.yaml`, `imagePullPolicy: IfNotPresent`, no `--leader-elect`). |
| `kustomization.yaml` | Ties the above together; `images:` override for the controller image; `configMapGenerator` for the server-config. |

```sh
# Uses the dns-control kind context.
kustomize build config/dependencies/resource-metrics/controller \
  | kubectl --context kind-dns-control apply -f -
```

> [!NOTE]
> Override the controller image before applying if you are not using
> `ghcr.io/milo-os/resource-metrics:latest`, e.g.
> `kustomize edit set image ghcr.io/milo-os/resource-metrics=ghcr.io/milo-os/resource-metrics:<tag>`
> or load a `dev` image into kind and set `newTag: dev` in `kustomization.yaml`.

### `core-control-plane/` — applied with a MILO kubeconfig

These target the **Milo core control plane** (`milo-apiserver`), NOT the kind
apiserver. Apply them with a milo kubeconfig — the same in-cluster
endpoint + `test-admin-token` used by the controller, or an equivalent
admin kubeconfig. This mirrors milo-os/infra
`apps/resource-metrics-system/base/milo-control-plane.yaml`, which applies the
operator's `crd` path onto the Milo CP.

| Path | Purpose |
| --- | --- |
| `core-control-plane/crd/` | The `ResourceMetricsPolicy` CRD (`resourcemetrics.miloapis.com`), vendored from the operator's `config/crd/bases`. Must be **Established first**. |
| `core-control-plane/policy/` | Applies the `dns-metrics` `ResourceMetricsPolicy`. It **references** `config/observability/dns-metrics-policy.yaml` (single source of truth) rather than duplicating it — so the build needs `--load-restrictor LoadRestrictionsNone`. |

```sh
# Point kubectl at the Milo core control plane. For example, extract the
# controller's kubeconfig from the Secret, or use any admin kubeconfig for
# milo-apiserver. Example using the same Secret the controller mounts:
kubectl --context kind-dns-control -n resource-metrics-system \
  get secret milo-kubeconfig -o jsonpath='{.data.kubeconfig}' \
  | base64 -d > /tmp/milo.kubeconfig

# 1) Install the CRD and wait for it to be Established.
kustomize build config/dependencies/resource-metrics/core-control-plane/crd \
  | kubectl --kubeconfig /tmp/milo.kubeconfig apply -f -
kubectl --kubeconfig /tmp/milo.kubeconfig wait --for=condition=Established \
  crd/resourcemetricspolicies.resourcemetrics.miloapis.com --timeout=60s

# 2) Apply the dns-metrics policy.
kustomize build --load-restrictor LoadRestrictionsNone \
  config/dependencies/resource-metrics/core-control-plane/policy \
  | kubectl --kubeconfig /tmp/milo.kubeconfig apply -f -
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
