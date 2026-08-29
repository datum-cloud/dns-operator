# Observability

Detects DNS records that fall out of sync between the control plane a customer
manages and the cluster that actually serves DNS, and alerts on it. See
[the feature doc](../../docs/enhancements/controlplane-drift-detection.md).

The two directories here land on **different clusters**, which is why they are
not one kustomization:

| Path | Goes to | What it is |
|---|---|---|
| `cluster-policy/` | the downstream cluster | `dns-downstream-metrics` — emits one series per replicated shadow object |
| `rules/` | the cluster running the metrics stack | `dns-controlplane-drift` — recording rules that diff the two sides, plus the alerts |

The upstream side needs nothing here. `dns-metrics`
([`config/milo/resource-metrics/policies`](../milo/resource-metrics/policies))
already emits `dns_record_set_info` from every project control plane, and it
already ships to the Milo core control plane with the rest of the milo
component.

## Deploying

Deployment wiring lives in `datum-cloud/infra`; `task env:metrics-up` applies
the same objects to the local e2e environment. Each side needs a
`resource-metrics` reading its policy from the cluster it runs against:
`discovery.mode: milo` for the upstream side, `discovery.mode: single` on the
downstream cluster.
