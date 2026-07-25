# Enhancement: Control-Plane Drift Detection via Resource Metrics

**Status**: Proposed
**Author**: Engineering
**Created**: 2026-07-24

## Summary

Detect when the upstream (project) and downstream (root) control planes get out of sync — the failure class behind [engineering#346](https://github.com/datum-cloud/engineering/issues/346) — using the [`milo-os/resource-metrics`](https://github.com/datum-cloud/resource-metrics) controller to emit per-object state metrics from every control plane, plus Prometheus recording/alerting rules that diff the two sides. No new instrumentation in the DNS operator is required for the primary signal.

## Motivation

In #346 a `multicluster-runtime` sharding bug caused every replicator replica to reconcile events for clusters it did not own. Multiple replicas programmed DNS downstream, and on AI-Edge deletion a leftover internal record kept "reserving" `www.ab.dk`, so every later record for that name was rejected as a duplicate (PowerDNS 422). The customer domain was unresolvable for ~24 hours and it was found by hand.

We had no signal for the underlying condition: **a resource exists at one replication seam with no owner at the seam above it.** That condition is directly observable as a discrepancy between per-object metrics collected from each control plane.

## Goals

- Alert when a downstream `DNSRecordSet`/`DNSZone` has no surviving upstream owner (orphan — the #346 leftover).
- Alert when an upstream object is not replicated downstream (missing — stalled replication / shard-ownership gaps).
- Alert when an upstream object is stuck not-`Accepted`.
- Prove the whole path (emit → diff → alert) in e2e, building on the three-cluster harness from [PR #60](https://github.com/datum-cloud/dns-operator/pull/60).

## Non-Goals

- Downstream ↔ PowerDNS drift (PowerDNS is not a control plane; see Limitations).
- Detecting the shard-ownership root cause directly (runtime behavior, not resource state). The rules catch the *effect* (orphan), which is what pages.

---

## Background: the replication topology

```
Project CP (upstream)          Root CP (downstream)            PowerDNS
  DNSRecordSet / DNSZone  --->   DNSRecordSet / DNSZone   --->   RRsets
        ^  replicator (mode: milo, per-project shards)   ^  PowerDNS controller
```

- The **replicator** (`internal/controller/dnsrecordset_replicator_controller.go`) uses `multicluster-runtime` with the Milo provider (`cmd/main.go` `initializeClusterDiscovery`, `discovery.mode: milo`) to watch every project control plane and copy DNS resources to the root control plane.
- Each downstream object is stamped with `meta.datumapis.com/upstream-{cluster-name,group,kind,name,namespace}` (`internal/downstreamclient/mappednamespace.go:117-121`). The downstream **namespace is remapped**, so the real upstream namespace lives only in the annotation. The cluster-name annotation value is prefixed `cluster-`.
- The **PowerDNS controller** (`internal/controller/dnsrecordset_powerdns_controller.go`, `internal/pdns/client.go`) programs RRsets and enforces name ownership; a conflicting owner is the 422 the customer hit.

## Design

### Collection: resource-metrics

`resource-metrics` runs one controller centrally, discovers project control planes via the Milo provider (`discovery.mode: milo`), and — with `discovery.collectRootControlPlane: true` — also watches the root control plane. It evaluates CEL-defined gauge families per object and pushes OTLP → OTel collector → Victoria Metrics. Metric/label definitions live in a cluster-scoped `ResourceMetricsPolicy`.

**Confirmed feasibility:** the root/downstream control plane is reachable via `collectRootControlPlane: true` (`resource-metrics internal/config/config.go`). The only open confirmation is that the DNS "downstream" CP *is* the Milo root CP; if it is a separate infra CP the Milo provider does not engage, discovery must be extended to include it.

The policy (`config/observability/dns-metrics-policy.yaml`) defines two series:

| Series | Emitted on | Join labels |
|---|---|---|
| `dns_recordset_upstream_info` | project CPs | `upstream_namespace`, `upstream_name` + Milo project label |
| `dns_recordset_downstream_info` | root CP | `upstream_cluster`, `upstream_namespace`, `upstream_name` (from annotations) |

The downstream generator lifts the `upstream-*` annotations onto labels — this is the join key and the reason no operator code is needed.

### Detection: recording + alerting rules

`config/observability/dns-drift-prometheusrule.yaml`:

- `dns:recordset_downstream_orphan` = `downstream unless on(join keys) upstream` → orphan (the #346 case).
- `dns:recordset_downstream_missing` = `upstream unless on(join keys) downstream` → not replicated.
- Alerts fire with `for: 10m` to ride out normal replication + OTLP push/staleness lag (avoids flapping during healthy eventual-consistency windows).

**Label normalization (verified live):** the two sides label the source project differently, so the rules normalize both to a bare `proj` join key:
- Downstream `upstream_cluster` = `cluster-` + `replace(<milo cluster key>, "/", "_")`. The Milo provider keys a project cluster as `/alpha`, so the annotation is **`cluster-_alpha`** (note the underscore).
- Upstream is tagged by `resource-metrics` as `milo_project_name` = the bare name (`alpha`), and `milo_control_plane_type` = `project` vs `root`.

The recording rules strip `cluster-_?` from the downstream label and use `milo_project_name` directly for the upstream, joining on `(proj, upstream_namespace, upstream_name)`. (An earlier assumption that both sides read `cluster-<name>` was wrong — caught by live e2e, where the matched pair was falsely flagged as both orphan and missing until the normalization was fixed.)

### Limitations / complementary work

- **PowerDNS seam is invisible.** A leak purely downstream→PDNS (downstream object deleted, RRset leaked) won't show here. Keep a `dns_pdns_apply_errors_total{reason="conflict"}` counter in `internal/pdns` for the direct 422 symptom, and consider a PDNS RRset exporter that diffs against downstream objects.
- **Root cause (shard ownership) not directly observed.** Optionally add a `dns_replicator_cluster_reconcile_total{pod,cluster}` metric to alert when >1 pod reconciles one cluster — an early warning ahead of any drift.

## E2E plan: Milo-based, build on PR #60

[PR #60](https://github.com/datum-cloud/dns-operator/pull/60) (`feat/dns-federation-test-env`) already stands up the topology and harness we need. It brings up **three** `datum-cloud/test-infra` kind clusters via a `Taskfile.yaml` (remote test-infra include, the same pattern `milo-os/activity` and `resource-metrics` use):

- **`dns-upstream`** — runs the **replicator** (`config/overlays/replicator`), pushes DNSZone/DNSRecordSet to control.
- **`dns-control`** — dns-operator agent + PowerDNS + RustFS; plays the **downstream** role for the replicator and the Lightningstream source for edge.
- **`dns-edge`** — PowerDNS only; receives via Lightningstream.

The `dns-upstream → dns-control` hop **is the #346 seam.** #60 also already wires multi-cluster Chainsaw: `env:chainsaw-prepare-kubeconfigs` exports `kubeconfig-{upstream,control,edge}`, and `env:chainsaw` runs every suite under `test/e2e/`. So drift detection is almost purely additive on top of #60.

### What #60 gives us for free

- Task + remote `test-infra` harness, three-cluster bring-up/tear-down (`env:up` / `env:stack-up` / `env:down`).
- Cross-cluster addressing pattern (NodePort on the control-plane container) already used for RustFS and the replicator's downstream kubeconfig — reuse it to point OTLP at the collector.
- The pinned test-infra ref already ships `install-observability` — **Victoria Metrics + OTel Collector + the Prometheus-operator CRDs including `PrometheusRule`** (`prometheusrules.monitoring.coreos.com`). It's marked optional and #60 doesn't invoke it yet; we just call it.
- Multi-cluster Chainsaw suites addressing clusters by name, plus a `test/e2e/federation` suite to model the new one on.

### New work (additive to #60)

1. **`config/dependencies/resource-metrics/`** — kustomize/Flux to deploy the `resource-metrics` controller in `discovery.mode: single` (default mode; watches its own cluster's API). Instantiate it on **both** `dns-upstream` and `dns-control`, each pushing OTLP to the OTel collector running on `dns-control` (cross-cluster via NodePort, same as RustFS). In single mode there is no auto project label, so set a static `upstream_cluster` resource attribute per instance so the join keys line up.
2. **Taskfile additions:** `env:observability-up` (call `test-infra:install-observability` on `dns-control`), `env:metrics-up` (deploy the two resource-metrics instances + `kubectl apply -k config/observability`). Fold both into `env:stack-up` behind a flag so the default federation flow stays lean.
3. **New Chainsaw suite `test/e2e/controlplane-drift/`** (added to `env:chainsaw`, addressing `upstream` + `control` by the already-exported kubeconfigs, querying VM's HTTP API):
   - **Happy path:** create a DNSRecordSet on `dns-upstream` → replicator copies to `dns-control` → assert both `*_upstream_info` and `*_downstream_info` series exist in VM and `dns:recordset_downstream_orphan == 0`.
   - **Orphan / #346 regression:** create the record, let it replicate, then delete the upstream object while preventing GC cascade (scale the replicator to 0, or drop the downstream owner/finalizer) → poll VM until `dns:recordset_downstream_orphan > 0` and assert `DNSDownstreamOrphanRecordSet` enters `firing`; restore and assert it clears.
   - **Missing:** scale the replicator to 0, create an upstream object → assert `dns:recordset_downstream_missing > 0` / `DNSDownstreamMissingRecordSet` fires.
4. **CI:** extend #60's `e2e.yml` to run `env:observability-up` + `env:metrics-up` before the drift suite; keep it a separate job/flag so the base federation e2e stays fast.

### Milo topology (production-accurate)

We run the **real production path** so the e2e reproduces the #346 root cause, not just an injected orphan. `milo-apiserver` runs on the control cluster; the downstream is the Milo **core control plane**; a single `resource-metrics` in `mode: milo` + `collectRootControlPlane: true` collects both sides — exactly as production would.

| Cluster (#60) | Role in Phase B |
|---|---|
| `dns-control` | Hosts `milo-apiserver` + `milo-controller-manager` (`--control-plane-scope=core`). Serves the **Milo core CP (downstream)** and **≥2 project CPs (upstream)** via aggregation. Also runs the PowerDNS agent (reading DNSRecordSets from the **core CP**), RustFS, the observability stack (OTel + VM), and the single `resource-metrics` controller. |
| `dns-upstream` | Hosts the **replicator** process only, `discovery.mode: milo`, **2 replicas, leader election off**, discovery + downstream both pointed at `milo-apiserver` on control. (The name is now a slight misnomer — upstream *data* lives in Milo project CPs, not this kind cluster.) |
| `dns-edge` | Unchanged from #60: PowerDNS + Lightningstream from control. |

This rewires #60's control cluster in two ways: the **replicator's downstream target** and the **PowerDNS agent's read source** both move from the plain `dns-control` kind API to the Milo core CP (served by `milo-apiserver`).

### Milo deployment — reuse resource-metrics' proven setup

Lifted from `resource-metrics` (verified in its e2e):
- `config/dependencies/milo/` — Flux `OCIRepository` on `oci://ghcr.io/datum-cloud/milo-kustomize` (pin a tag) → `overlays/test-infra`, plus the separate `milo-infra-crds` Kustomization that installs the `ProjectControlPlane` CRD (the core controller crash-loops without it). Added to this repo as drafts.
- A `milo-kubeconfig` Secret targeting `https://milo-apiserver.milo-system.svc.cluster.local:6443` with the static `test-admin-token` from milo's test-infra overlay (`system:masters` in test only).
- `resource-metrics` server-config: `discovery.mode: milo`, `discoveryKubeconfigPath`/`projectKubeconfigPath` → the milo kubeconfig, `collectRootControlPlane: true`.
- `dns-operator` replicator server-config: `discovery.mode: milo`, same discovery/project kubeconfig, downstream client pointed at the Milo core CP.

### Generator scoping — one policy, two control-plane roles

The single `dns-metrics` policy is applied to all CPs, so **both** the upstream and downstream generators run on every control plane. They are separated by the `milo.project.name` label the Milo provider adds — `"root"` on the core CP, the project name on project CPs. The recording rules therefore filter:
- upstream view: `dns_recordset_upstream_info{milo_project_name!="root"}`
- downstream view: `dns_recordset_downstream_info{milo_project_name="root"}`

Without this filter, the downstream generator's series on project CPs (empty `upstream_*` labels, annotations absent) and the upstream generator's series on the core CP would pollute the diff. **Verify** the exact promoted label name (`milo_project_name` vs `milo.project.name`) against a real series and adjust both the rules and the filter.

### Chainsaw suite `test/e2e/controlplane-drift/`

Added to `env:chainsaw`, addressing Milo project CPs (via aggregation-path kubeconfigs, the pattern `resource-metrics` uses) and querying VM's HTTP API:
- **Happy path:** create a DNSRecordSet on project CP `alpha` → replicator copies it to the core CP → assert both `*_upstream_info{milo_project_name="alpha"}` and `*_downstream_info{milo_project_name="root"}` exist and `dns:recordset_downstream_orphan == 0`.
- **Orphan / #346 regression (injected):** delete the upstream object while preventing GC cascade (scale replicator to 0, or drop the downstream owner/finalizer) → poll VM until `dns:recordset_downstream_orphan > 0` and assert `DNSDownstreamOrphanRecordSet` fires; restore and assert it clears.
- **Missing:** scale replicator to 0, create an upstream object → assert `dns:recordset_downstream_missing > 0`.
- **Genuine sharding repro (the reason for Phase B):** pin the **pre-fix** `multicluster-runtime` (before [datum-cloud/network-services-operator#320](https://github.com/datum-cloud/network-services-operator/pull/320) / [kubernetes-sigs/multicluster-runtime#173](https://github.com/kubernetes-sigs/multicluster-runtime/pull/173)), run **2 replicas across ≥2 project CPs**, exercise a create/delete cycle, and assert the drift alert fires from the real bug — every replica reconciling clusters it doesn't own — not an injected orphan. Flip to the fixed fork and assert it stays clean.

### Taskfile additions (on #60)

- `env:milo-up` — `kubectl apply -k config/dependencies/milo` on control, wait for the Flux Kustomizations, mint the `milo-kubeconfig`, create Org + ≥2 Projects (project CPs).
- `env:observability-up` — `test-infra:install-observability` on control (VM + OTel + PrometheusRule CRD).
- `env:metrics-up` — deploy the single `resource-metrics` (mode:milo, collectRootControlPlane) + `kubectl apply -k config/observability`.
- Rework `env:upstream-up` → deploy the replicator with the `mode: milo` overlay, 2 replicas, pointed at milo on control.
- Rework `env:control-up` → agent reads from the Milo core CP.
- Fold into `env:stack-up`; extend #60's `e2e.yml` to run the drift suite as a separate job.

> Phase B is environment-heavy (Flux reconcile timing, milo token auth, cross-cluster addressing, 2-replica sharding). Expect live-cluster iteration to shake out the wiring; the config drafts here are the starting point, not turnkey.

## Sequencing

1. Land `config/observability/*` (policy + rules) — done as drafts in this change.
2. Add `config/dependencies/milo/`, the replicator `mode: milo` overlay + server-config, the `resource-metrics` deploy overlay, and the Taskfile `env:milo-up`/`env:observability-up`/`env:metrics-up` additions on top of PR #60.
3. Add the `test/e2e/controlplane-drift/` suite (happy / orphan / missing / genuine-sharding-repro) and wire it into `env:chainsaw` + CI.
4. Confirm the production downstream CP == Milo core CP; enable `collectRootControlPlane` in the prod `resource-metrics` config; validate series parity in staging for one retention window before wiring alerts to paging.
5. Complementary operator-side signals: PDNS conflict counter, replica-ownership metric.

## Verified against datum-cloud/infra

Reading the real staging/production deployment (`datum-cloud/infra`) resolved the assumptions that code alone couldn't:

- **The Milo core control plane serves `DNSRecordSet`s.** `apps/dns-operator/control-plane/base/core-control-plane-resources.yaml` installs the DNS CRDs *into the core control plane* (via the `milo-configuration` cert, `system:control@services.miloapis.com`). So replicated records land on the core CP and `resource-metrics`' root collection can see them — the previously live-only caveat, now confirmed.
- **The replicator already runs `mode: milo` in prod** (`apps/dns-operator/control-plane/staging/config.yaml`) with discovery + project kubeconfigs, and `downstreamResourceManagement` **commented out** so it writes downstream via the pod's in-cluster SA — i.e. to the core control plane it runs on. Replicator (`control-plane/`) and agent (`downstream/`) both deploy to the same infra cluster (`datum-dns-system`).
- **`resource-metrics` is deployed but `collectRootControlPlane` is NOT set** (`apps/resource-metrics-system/{staging,production}/config.yaml` have only `mode: milo`). So the downstream/core-CP series this design needs are **not currently emitted** — enabling `collectRootControlPlane: true` is a required, net-new infra change, not just new rules.
- **Policies target the Milo core CP** — `resource-metrics` installs its CRDs into the core control plane (`apps/resource-metrics-system/base/milo-control-plane.yaml`), so the `dns-metrics` `ResourceMetricsPolicy` is applied there, not to a plain cluster.
- **Alert/recording rules stay a standard `PrometheusRule`.** The victoria-metrics-operator auto-converts `PrometheusRule` → `VMRule` (carrying labels through), so no VM-specific CRD is needed. What matters is the label `telemetry.miloapis.com/resource-metrics-aggregator`, which routes the rules to the dedicated `vmalert-datum-resource-metrics-aggregator` (the general vmalert selects the complement — that label `DoesNotExist`). `config/observability/dns-drift-rules.yaml` reflects this.

Implication for the e2e topology: prod co-locates the replicator on the core/Milo cluster (no separate upstream cluster; "upstream" = Milo project CPs). The e2e should therefore run the replicator on `dns-control` alongside Milo (downstream via in-cluster SA), rather than on a separate `dns-upstream` kind cluster — simpler and more faithful than the #60-derived split.

## Open questions

- Enabling `collectRootControlPlane: true` in the infra `resource-metrics` config: confirm it doesn't balloon cardinality (it adds the whole core CP's objects) and that the root series carry `milo_control_plane_type="root"` as expected.
- Victoria Metrics staleness window vs. the `for: 10m` alert delay — tune together so orphans are neither missed nor flapped.
- Prod auth for the policy/agent paths uses cert-based `milo-configuration-kubeconfig` (not the e2e's static `test-admin-token`); keep the e2e's test-only creds out of any shared overlay.
