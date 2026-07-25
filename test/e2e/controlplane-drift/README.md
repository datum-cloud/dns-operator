# control-plane drift e2e

Chainsaw scenarios that prove upstream/downstream control-plane DNS drift
detection: the resource-metrics collectors emit per-object series from every
control plane, the `dns-controlplane-drift` recording/alerting rules diff the
two sides, and these tests assert the resulting series land in Victoria Metrics.

See `docs/enhancements/controlplane-drift-detection.md` for the full design and
`config/observability/{dns-metrics-policy,dns-drift-rules}.yaml` for the policy
and rules under test.

## Scenarios

| Dir | Proves |
|---|---|
| `happy-path/` | Record created on project CP `alpha` replicates to the core CP; both `dns_recordset_upstream_info{milo_project_name="alpha"}` and `dns_recordset_downstream_info{milo_control_plane_type="root"}` exist in VM and `dns:recordset_downstream_orphan == 0`. |
| `orphan/` | engineering#346 regression. Replicate a record, scale the replicator to 0, delete the upstream object → downstream copy is orphaned → `dns:recordset_downstream_orphan > 0` and `DNSDownstreamOrphanRecordSet` becomes active. Restore the replicator → orphan clears. |
| `missing/` | Scale the replicator to 0, create an upstream record → never replicated → `dns:recordset_downstream_missing > 0`. |

## Named clusters the Taskfile must provide

Each `chainsaw-test.yaml` declares its clusters inline (matching the
`federation/` and `zones-and-records/` suites) and expects these kubeconfig
files, one directory up (`test/e2e/`), exactly as the existing suites consume
`kubeconfig-{control,upstream,edge,downstream}`:

| Chainsaw cluster name | Kubeconfig file | Role / server URL |
|---|---|---|
| `alpha` | `test/e2e/kubeconfig-alpha` | Project CP **alpha** (UPSTREAM). Aggregation path: `<milo-base>/apis/resourcemanager.miloapis.com/v1alpha1/projects/alpha/control-plane`. |
| `core` | `test/e2e/kubeconfig-core` | Milo **core** control plane (DOWNSTREAM), `<milo-base>` root. Replicated copies land here. |
| `infra` | `test/e2e/kubeconfig-infra` | The `dns-control` kind cluster that hosts Victoria Metrics + the OTel collector **and** the replicator `Deployment`. Used to run in-cluster `curl` against VM and to `kubectl scale` the replicator. |

Notes for wiring:

- `fixtures/milo-projects.yaml` (already present, do not edit) creates the Org +
  projects `alpha`/`beta` on the core CP; the Taskfile should apply it and mint
  the `alpha` aggregation-path kubeconfig the same way `resource-metrics` does
  (clone the milo kubeconfig, rewrite the `server:` URL to the project's
  `.../projects/alpha/control-plane` path).
- `kubeconfig-beta` (project CP `beta`) is created by the fixture but is **not**
  consumed by the current scenarios; provide it only if/when a multi-project
  sharding scenario is added.
- `alpha`, `core`, and `infra` may all be served by the same `dns-control` kind
  cluster (milo-apiserver serves the aggregation views); the three kubeconfigs
  differ only in their `server:` URL and token. Admin token: `test-admin-token`.
- The replicator is scaled via `kubectl -n dns-replicator-system scale
  deployment --all --replicas=0|1` — namespace `dns-replicator-system`, matched
  by `--all` rather than a hard-coded deployment name. Confirm that namespace is
  correct for the drift topology (the design doc co-locates the replicator on
  the core cluster; adjust the `infra` kubeconfig / namespace if it differs).

## Victoria Metrics query endpoint

Scenarios query VM over its HTTP API from a one-shot `curl` pod running inside
the `infra` cluster (the `kubectl run --rm ... curlimages/curl` pattern from the
resource-metrics suite). Default endpoint:

```
http://vmsingle-telemetry-system-vm.telemetry-system.svc.cluster.local:8428/api/v1/query
```

Override per run by exporting `VM_QUERY_URL` (the scripts honour it). If the
observability stack uses a `vmselect`/cluster VM instead of `vmsingle`, point
`VM_QUERY_URL` at that `.../api/v1/query`. The endpoint must serve BOTH raw
series (`dns_recordset_*_info`) and the recording-rule / `ALERTS` series that
`vmalert` remote-writes back — i.e. `vmalert` must be configured against this
VM as its remote-write target (the `telemetry.miloapis.com/resource-metrics-aggregator`
`vmalert` in datum-cloud/infra).

## Timing / `for:` considerations

- **Recording rules are the primary gate.** `dns:recordset_downstream_orphan`
  and `dns:recordset_downstream_missing` have no `for:` and are evaluated every
  30s, so the tests assert on those series (`> 0`) rather than on the alert's
  full delay. This is fast and deterministic.
- **The alerts carry `for: 10m`.** After the recording rule fires, the alert
  sits in `pending` for 10m before `firing`. Waiting the full period would blow
  the e2e budget, so `orphan/` accepts `ALERTS{...alertstate="pending"|"firing"}`
  (the alert is active on the correct series). To assert `firing` specifically,
  deploy the drift rules with a shortened `for:` in a test-only overlay (e.g.
  `for: 30s`); that is an infra/Taskfile change outside this suite.
- **Orphan detection lags by VM's staleness window.** The orphan only
  materializes once the deleted upstream series ages out of VM's staleness
  window (default ~5m) so the recording rule's `unless` no longer cancels the
  downstream vector. `orphan/` therefore polls for up to ~9m (`timeout: 540s`).
  Shortening VM's staleness window in the test-infra observability stack would
  let this budget drop.
- **Pipeline latency.** watch → collect (OTel interval ~5s) → OTLP → remote-write
  → VM ingest adds seconds; the happy-path VM polls allow a few minutes.
- **Run sequentially.** All scenarios share the `alpha`/`core`/`infra` clusters
  and the `orphan`/`missing` scenarios scale the replicator globally, so run the
  suite with `parallel: 1` (chainsaw's default when unset, or set it in the
  Taskfile's chainsaw invocation).

## Assumptions needing live confirmation

- **Promoted label names/values** — `milo_project_name` (value `alpha`, and
  `root` for the core CP) and `milo_control_plane_type` (`project` / `root`) are
  taken from the resource-metrics OTLP attributes (`milo.project.name`,
  `milo.control_plane.type`) after prometheus-remote-write dot→underscore
  normalization. Confirm against a real series and adjust the queries + rules if
  the aggregator promotes different keys.
- **Downstream join labels** — the downstream series lifts
  `meta.datumapis.com/upstream-{cluster-name,namespace,name}` onto
  `upstream_{cluster,namespace,name}`. The tests key on `upstream_name`
  (= the upstream DNSRecordSet's `metadata.name`); verify the replicator stamps
  the object name (not a derived value) into the `upstream-name` annotation.
- **Replicator namespace / deployment** — assumed `dns-replicator-system` on the
  `infra` cluster. Confirm and adjust if the drift topology runs the replicator
  elsewhere.
- **`vmalert` wiring** — assumes the drift `PrometheusRule`/`VMRule` is evaluated
  and its recording results + `ALERTS` are queryable at `VM_QUERY_URL`. If the
  observability stack doesn't remote-write vmalert results back to the same VM,
  point the queries at the vmalert datasource instead.
- **Whether the upstream DNSRecordSet reaches `Accepted` on the project CP** is
  not asserted (the condition-setting controller for that topology is
  unconfirmed); the tests assert object existence + the VM series instead.
