# 03: Resource-state metrics and producers (G-5, `internal-dns-producers`)

Paths: dns-operator = PR #244 worktree; NSO and Compute = `origin/main`. Live data comes from staging VictoriaMetrics, 2026-10-22.

## 1. Current state

### Where each kind lives, and which mechanism can see it

| Kinds | Stored in | Fitting mechanism |
|---|---|---|
| ResolverContext, AccessBinding, ZoneAssociation, NamingPolicy, Registration, RecordContribution, private DNSZone | Each Milo project control plane | `ResourceMetricsPolicy` (milo discovery) |
| ResolverBinding, PublicationManifest/Chunk, TransportOutbox, PublicationOwnership | The Milo project `internal-dns-service` (infra `apps/dns-operator/internal-dns/staging/installers.yaml:34-35`, `projects/projects.yaml`), not the infra cluster | `ResourceMetricsPolicy`. The project is discovered like any other. |
| NetworkContext | Project control plane | `ResourceMetricsPolicy` |
| Instance | Project control plane | `ResourceMetricsPolicy` |

Live check: `kubectl get dnsresolverbindings -A` on the infra cluster returns "No resources found". **The strategy doc is wrong to route service-scoped kinds through the infra-cluster kube-state-metrics.** KSM only reads its own apiserver.

### Existing families

| Family | Producer | Notes |
|---|---|---|
| `dns_zone_status_condition` (208 series) | dns-operator `config/milo/resource-metrics/policies/dns-metrics.yaml` | Hard-codes `Accepted` and `Programmed`, so `Published` is never exported. The name has no routing prefix. |
| `datum_dns_zone_*` / `datum_dns_recordset_*` | infra `infrastructure/monitoring/dns-system/base/custom-resource-state.yaml:8,39` (KSM, infra cluster) | Use `type=` where Milo families use `condition=`. **The `datum_dns_zone_` name is taken, so new families must not reuse it.** |
| `datum_cloud_compute_instance_status_condition` (1301 series) | Compute `config/components/resource-metrics/compute-metrics.yaml:423` | Uses `forEach: object.status.conditions`. `status.dns[]` is not exported. |
| `datum_cloud_network_context_status_condition` | NSO `config/resource-metrics-policies/networking-metrics.yaml:235` (`forEach` over all conditions, so it would include `DNSReady`) | **Defined but not deployed.** Staging has 0 `datum_cloud_network*` series, and infra references no NSO policy. The strategy doc says "NSO has no policy", which is wrong, and it uses the wrong name (`datum_cloud_networking_network_context_…`). |

### API conventions in the Private DNS CRDs

| Kind | Conditions set | `status.observedGeneration` | Ready rollup |
|---|---|---|---|
| ResolverContext | Ready (`controlplane/context_reconciler.go:191`) | no (`internaldns_project_types.go:70`) | yes |
| AccessBinding | Accepted, Ready (`:331-402`) | yes (`:120`) | yes |
| ZoneAssociation | Accepted (`:245`) | no (`:38`) | **no** |
| NamingPolicy | Accepted (`:106`) | no (`:305`) | **no** |
| Registration | Accepted, Published (`publication_reconciler.go:171,572`) | yes | **no** |
| RecordContribution | Accepted, Published (`:207,589`) | yes | **no** |
| private DNSZone | Published, Programmed (`platform/acks.go:251-252`) | n/a | **no** |
| PublicationChunk | Published, Available (`acks.go:318-319`) | — | **no** |
| PublicationManifest | Published (`acks.go:139`) plus the free-string `exportState` | — | **no** |
| ResolverBinding | FullyReplicated, ObservationHealthy, Serving (`acks.go:606-610`) plus the string `phase` | uses `observedConfigurationRevision` | **no** |
| TransportOutbox | none. Only the string `state` and `lastError` (`internaldns_service_types.go:225`) | — | — |
| PublicationOwnership | no status at all | — | — |

- Per-condition `observedGeneration` is set everywhere, which is good. The top-level `status.observedGeneration` is set on only 3 of 6 project kinds.
- `acks.go:400` sets `Message: reason`, so conditions carry no human-readable message.
- There is no `Ready` summary on 9 of 12 kinds. A generic "not-Ready objects" panel is impossible until there is one.

### Producer controller issues

| Issue | NSO | Compute publisher | dns-operator |
|---|---|---|---|
| Returns an error together with `RequeueAfter` (controller-runtime drops the RequeueAfter and warns) | `internaldns_controller.go:539,546`; `internaldns_cell_controller.go:74,83` | `internaldns_publisher.go:202,204` | `publication_reconciler.go:116` |
| Missing precondition treated as an error | readiness `:523` puts `err.Error()` into a Pending condition | `:133-159`: project/identity/projection UID lookups return errors. `internaldns_observation.go:59-60`: a missing `source-instance-uid` label becomes an error with no object key. This was today's incident. | — |
| Pending without any status | project reconciler `:165,186,198` silently requeues (no region, context not ready, access not ready), so the user sees the readiness reconciler's `NetworkPathNotReady`, which is misleading | — | — |
| Timer polling instead of watches | every path requeues every 10s (`:211`, cell `:130,146`). Only `For(NetworkContext)` is registered (`:474,577`, cell `:376`), with no `Owns`/`Watches` on DNSResolverContext/AccessBinding | 10–30s polling (`:202-204`). `For(Instance)` only (`:577`). Every read is an uncached `GetAPIReader` (`:127`) | — |
| Logging and Events | 0 log calls and 0 recorders | 0 log calls and 0 recorders | 0 recorders in `internal/internaldns` |
| Controller metrics | `internaldns-cell` (`:377`) is named correctly, but **the cell manager is not scraped**: `kube_pod_info` shows the pods in `us-*-staging-lab`, and `controller_runtime_*` has none (`:8443` secure, no scrape config) | scraped (`job=compute-metrics`) | — |

Live, 1h window:
- Publisher: 248 errors/h, 23 success/h, 6,442 `requeue_after`/h, so errors are 92% of non-requeue reconciles. **It is still failing after infra #7513.**
- NSO polling volume: about 45k `internaldns-project` reconciles/h and about 48k `internaldns-readiness` reconciles/h for about 73 contexts.

### Routing

- The strategy doc's concern is wrong. NSO is in Backstage system `alb`, owner `delivery` (catalog `network-services-operator`, `spec.system: alb`), and `ownership.yaml` maps `alb` and `dns` to `delivery`.
- So `datum_cloud_network_` → alb is already NSO's owner. Keep the existing name.

## 2. Proposed design

All families are `ResourceMetricsPolicy` generators. Labels `milo.project.name` and `milo.control_plane.type` arrive automatically.

| Kind(s) | Families | Labels | Owner repo |
|---|---|---|---|
| Each Private DNS kind `<k>` ∈ {`resolver_context`, `access_binding`, `zone_association`, `naming_policy`, `registration`, `contribution`, `resolver_binding`, `publication_manifest`}. Phase 3 ships `resolver_binding`, `registration` and `resolver_context` first; the rest wait for a project selector. | `datum_dns_internal_<k>_info`, `_owner`, `_status_condition` (forEach), `_status_condition_last_transition_time` (forEach), `_condition_generation_lag` (forEach, value `metadata.generation - item.observedGeneration`) | `name, namespace, condition, status, reason`. Info adds `region` (binding/access) and `owner_kind`. No `zone` label. **Per-object series exist only while the object is not Ready, or its `observedGeneration` is behind** (see below). | dns-operator, new `config/milo/resource-metrics/policies/internal-dns-metrics.yaml` |
| ResolverBinding ACKs | Not exported per member (README decision 4). ACK freshness comes from the control plane's `internal_dns_resolver_binding_ack_lag_seconds`, a per-binding max (appendix 02), and an unhealthy binding shows up in its `_status_condition` series. | — | dns-operator |
| ResolverBinding revision | `datum_dns_internal_resolver_binding_configuration_revision` (spec) and `_observed_configuration_revision`, emitted only while the two differ | `name, region, shard` | dns-operator |
| TransportOutbox | `datum_dns_internal_transport_outbox_state` (value 1) | `state` only, aggregated: no `name` label | dns-operator |
| public DNSZone/RecordSet | Switch to `forEach` so `Published` is exported. Leave the names unchanged so existing alerts keep working. | — | dns-operator |
| NetworkContext | Ship the existing `datum_cloud_network_context_status_condition`; `DNSReady` comes free. Add `_last_transition_time`. | existing | NSO bundle path and infra Flux Kustomization |
| Instance DNS | `datum_cloud_compute_instance_dns_status_condition`, forEach `has(object.status.dns) ? object.status.dns : []`, value `Ready=='True'` | `name, namespace, network` (= `item.network.name`), `reason, status`. No FQDN. | Compute `compute-metrics.yaml` |

**Name only on unhealthy objects.** These families would otherwise carry `name` and `namespace` for every object in every project, so cardinality would grow per tenant and per record. Each per-object family therefore filters in CEL: it emits nothing for an object that is Ready with `observedGeneration` current. Healthy objects cost zero series, and the alertable set (not Ready, or stale) keeps its `name`. Totals by state come from the control plane's own inventory gauges (`internal_dns_contexts`, `_private_zones`, `_bindings`, appendix 02), not from per-object series. Kinds without a `Ready` rollup can't be filtered this way, which is one reason phase 3 lands the rollup (§3) before this policy.

Sketch, with each family following the same shape:

```yaml
- name: internal-dns-registration-condition
  resource: {group: dns.networking.miloapis.com, version: v1alpha1, resource: dnsregistrations}
  families:
  - name: datum_dns_internal_registration_status_condition
    type: gauge
    metrics:
    - forEach: >-
        has(object.status) && has(object.status.conditions) &&
        !(object.status.conditions.exists(c, c.type == 'Ready' && c.status == 'True') &&
          object.status.conditions.all(c, c.observedGeneration == object.metadata.generation))
        ? object.status.conditions : []
      value: "item.status == 'True' ? 1.0 : 0.0"
      labels: [{name: name, value: object.metadata.name}, {name: namespace, value: object.metadata.namespace},
               {name: condition, value: item.type}, {name: reason, value: item.reason}, {name: status, value: item.status}]
```

### Producer-side custom metrics

All registered on the controller-runtime registry.

| Repo | Metric | Labels |
|---|---|---|
| Compute | `datum_cloud_compute_internal_dns_publications_total` (counter, incremented at each Ready transition) | `result` (published/pending/failed), `reason` |
| Compute | `datum_cloud_compute_internal_dns_publication_latency_seconds` (histogram: Instance Ready → DNS Ready) | — |
| NSO | `datum_cloud_network_internal_dns_ready_latency_seconds` (histogram: NetworkContext creation → `DNSReady=True`) | `region` |
| NSO | `datum_cloud_network_internal_dns_contexts` (gauge) | `reason` |

These are self-emitted, so they route by namespace. The prefix is a safeguard.

### Cardinality

| Source | Staging | Production estimate |
|---|---|---|
| Project kinds: 5 series per object, only while not Ready or stale | 5 × unhealthy objects | linear in unhealthy objects, about 0 in steady state |
| Instance DNS | ~90 (87 instances) | 1 per instance × network |
| NetworkContext | ~73 per condition | 1 per network × location |
| ACKs | 1 series per binding, up to 200 per shard | Decided (README decision 4): per-binding max through the control plane's `internal_dns_resolver_binding_ack_lag_seconds` (G-4). No per-member series. |

`resource-metrics` memory scales with projects × resource types. Every kind opens an informer on every project control plane, including projects without DNS. The staging manager already peaks at about 2.7 of 4 GiB after the NSO policy added its generators. So phase 3 ships three kinds first (DNSResolverBinding, DNSRegistration, DNSResolverContext; README decision 7), and the rest wait for a project selector in milo-os/resource-metrics.

## 3. Architecture fixes

- **Pending is status, not error.** Compute should return `Ready=Unknown`, reason `NetworkObservationUnavailable`/`PendingPublication`, and `nil` for a missing label, projection, project identity or observation; Compute PR #517 starts this. NSO's project reconciler should write `DNSReady=False` with `Pending`, `RegionUnavailable` or `AccessPending` instead of a silent requeue.
- **Never return error + RequeueAfter.** Return the error alone for real API failures (backoff) and `RequeueAfter` alone with a nil error for pending.
  - NSO: `internaldns_controller.go:539,546`, `internaldns_cell_controller.go:74,83`
  - Compute: `internaldns_publisher.go:202,204`
  - dns-operator: `publication_reconciler.go:116`
- **Watches over timers.**
  - NSO: `Watches` DNSResolverContext/AccessBinding in the project cluster, mapped through the owner annotation, and the project-side NetworkContext in readiness. Drop the steady-state 10s requeue, keeping only `validUntil`-driven requeues.
  - Compute: watch DNSRegistration/Contribution (owner Instance) and NetworkInterface. Use cached readers.
- **Structured logs.** Use `log.FromContext(ctx).WithValues("project", ..., "instance", ns/name, "network", ..., "reason", ...)` at every pending and failure branch. Errors wrap the object key with `%w`.
- **Events** on condition transitions only, e.g. `DNSPublished`, `DNSNameConflict`, `DNSAccessPending`, using `events.EventRecorder` on the project-side object the user sees.
- **Consistent conditions** (dns-operator API):
  - Add `Ready` to every kind as the rollup of its other conditions.
  - Replace the free-string `phase`/`exportState`/`state` with conditions, keeping the strings as printer columns.
  - Add `status.observedGeneration` to ZoneAssociation, NamingPolicy and ResolverContext.
  - Give conditions a real `Message`.
  - Add Outbox conditions `Delivered` and `Ready`.
- **Scrape the NSO cell manager**: add a VMServiceScrape/PodMonitor in the cell bundle and infra `apps/network-services-operator/cell`.

## 4. Work breakdown

| # | Repo / files | Change | Depends on |
|---|---|---|---|
| 1 | compute `internal/controller/internaldns_publisher.go`, `_observation.go`, `_status.go` | Land #517. Remove error+RequeueAfter. Keyed logs and Events. | — |
| 2 | compute `config/components/resource-metrics/compute-metrics.yaml` | `instance_dns_status_condition` | — |
| 3 | infra `apps/network-services-operator/control-plane` plus an NSO bundle path | Deploy NSO's `networking-metrics` policy (fills the prod gap in infra #4463 too) | — |
| 4 | infra `apps/network-services-operator/cell`, NSO `config/cell` | Scrape the cell controller manager | — |
| 5 | NSO `internal/controller/internaldns_controller.go`, `internaldns_cell_controller.go` | Pending as status, no error+RequeueAfter, watches, logs, Events | — |
| 6 | dns-operator `api/v1alpha1/internaldns_*_types.go`, `internal/internaldns/{controlplane,platform}` | Ready rollup, `observedGeneration`, conditions replacing phases, messages | — |
| 7 | dns-operator `config/milo/resource-metrics/policies/internal-dns-metrics.yaml` | Families from §2 for DNSResolverBinding, DNSRegistration and DNSResolverContext only (README decision 7) | 6 (the non-Ready filter needs the rollup) |
| 8 | compute and NSO `internal/metrics` | Publication counters and latency histograms | 1, 5 |
| 9 | infra dashboards `internal-dns-producers` | Panels with corrected names | 2, 3, 7 |
| 10 | milo-os/resource-metrics, then dns-operator policy | Project selector on `ResourceMetricsPolicy`, then add the remaining kinds | 7 |

## 5. Open questions

1. (Decided as a follow-up, README decision 7.) Can `ResourceMetricsPolicy` be scoped to projects, so the service-scoped generators read only `internal-dns-service`? The schema (`milo-os/resource-metrics api/v1alpha1/custom_resource_metrics_types.go`) has no selector. What does the operator do when a CRD is absent from a project?
2. ACK cardinality: decided, per-binding max (README decision 4).
3. Should NSO's `DNSReady` be split, so that project-side pending and data-plane reasons are not reported under the same type?
4. Does #7513 fully explain the error rate? Errors persist at 248/h now.
5. Should the public `dns_zone_*` families move to `datum_dns_` naming to get routing? That needs an alert migration.

### Strategy doc corrections

- Service-scoped kinds are not on the infra cluster.
- NSO already has a policy, and the family is `datum_cloud_network_context_status_condition`.
- `datum_cloud_network_*` → alb is the correct owner (delivery).
- The cell controller is named `internaldns-cell` but is not scraped.
- The "97% error" figure is now 92%.
