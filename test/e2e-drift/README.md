# control-plane drift e2e

Chainsaw scenarios that prove upstream/downstream DNS drift detection end to
end: two resource-metrics collectors emit per-object series from the two
replication seams, the `dns-controlplane-drift` rules diff them, and these tests
assert the results in Victoria Metrics. See
`docs/enhancements/controlplane-drift-detection.md`.

Run with `task env:chainsaw-milo` against a running environment
(`task env:milo-all-up`).

## Scenarios

| Dir | Proves |
|---|---|
| `happy-path/` | A record on project CP `alpha` replicates downstream; both `dns_record_set_info` and `dns_record_set_downstream_info` land in VM and `dns:recordset_downstream_orphan == 0`. |
| `orphan/` | engineering#346: replicate a record, scale the replicator to 0, delete the upstream → the downstream copy is orphaned → `dns:recordset_downstream_orphan > 0` and `DNSDownstreamOrphanRecordSet` becomes active. Removing the leftover clears it. |
| `missing/` | Scale the replicator to 0, create an upstream record → never replicated → `dns:recordset_downstream_missing > 0`. |

## Clusters

Each `chainsaw-test.yaml` names four clusters, with kubeconfigs in this directory
(referenced as `../kubeconfig-*`). `env:chainsaw-milo` generates them; they are
gitignored.

| Name | Kubeconfig | Cluster |
|---|---|---|
| `alpha` | `kubeconfig-alpha` | Project CP alpha (upstream), milo aggregation path `.../projects/alpha/control-plane`. |
| `downstream` | `kubeconfig-downstream` | The cluster storing the replicated shadow objects — dns-control's own apiserver, not milo. |
| `infra` | `kubeconfig-infra` | dns-control kind cluster — hosts VM + OTel; runs the in-cluster `curl` VM queries. |
| `replicator` | `kubeconfig-replicator` | dns-upstream kind cluster — runs the replicator; the scenarios scale `deployment/dns-operator-controller-manager` in `dns-replicator-system` here. |

`downstream` and `infra` are the same kind cluster, exactly as in production:
one DNS infrastructure cluster both stores the shadow objects and runs the
metrics stack. They are separate entries so each step reads as what it is doing.

`alpha` reaches milo-apiserver on dns-control through a host port-forward.

## Victoria Metrics

Scenarios query VM from a one-shot `curl` pod on the `infra` cluster. Default
endpoint (override with `VM_QUERY_URL`):

```
http://vmsingle-telemetry-system-vm.telemetry-system.svc.cluster.local:8428/api/v1/query
```

VM must serve the raw `dns_record_set_*` series from both collectors and the
vmalert recording-rule / `ALERTS` series.

## Timing

- Recording rules (`dns:recordset_downstream_{orphan,missing}`, no `for:`, 30s
  interval) are the primary gate — the tests assert those `> 0`.
- The alerts carry `for: 10m`, so `orphan/` accepts
  `alertstate="pending"|"firing"` rather than waiting for `firing`.
- The orphan only materializes after the deleted upstream series ages out of
  VM's ~5m staleness window, so `orphan/` polls for up to ~9m.
- The suite runs sequentially (`.chainsaw.yaml` `parallel: 1`): the scenarios
  share clusters and scale the single replicator.
