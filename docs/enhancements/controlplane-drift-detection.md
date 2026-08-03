# DNS Control-Plane Drift Detection

Detects when a customer's DNS records fall out of sync between the control plane they manage (their project) and the control plane that actually serves DNS — and alerts on-call before it turns into a customer-visible outage.

## Why this exists

DNS records a customer creates in their project are replicated to a shared control plane that programs the live DNS servers. When that replication misbehaves, a record can be left behind on the serving side with nothing owning it anymore. A leftover like this keeps "reserving" a hostname, so the customer can no longer point that name anywhere — every new record is rejected as a duplicate.

That is exactly what happened in [engineering#346](https://github.com/datum-cloud/engineering/issues/346): a customer's `www` record was unresolvable for ~24 hours, and it was only found by hand. There was no signal for the underlying condition. This feature makes that condition an alert.

## What it detects

Three alerts, each pointing at the specific record and customer project involved:

| Alert | What it means | What to do |
|---|---|---|
| **DNSDownstreamOrphanRecordSet** | A record exists on the serving control plane with **no owner** in any customer project — a leftover that can block the customer from reusing that hostname (the #346 case). | Remove the orphaned record on the serving control plane. The alert labels name the project, namespace, and record. |
| **DNSDownstreamMissingRecordSet** | A record exists in a **customer project but was never replicated** to the serving side — the customer's change isn't taking effect. | Check replicator health for that project; the record isn't live until it replicates. |
| **DNSRecordSetNotAccepted** | A customer's record has been sitting **un-accepted** (e.g. a misconfigured zone). | Inspect the record's status conditions in the customer project. |

Alerts only fire after the condition **persists** (`for: 10m`), so normal replication lag never pages anyone.

## How it works

```
Customer projects  ──replicate──►  DNS infrastructure cluster  ──►  Live DNS
  (upstream)                            (downstream)
        │                                     │
  resource-metrics                     resource-metrics
   (mode: milo)                        (mode: single)
        │                                     │
        └──────────────┬──────────────────────┘   one metric per DNS record, per side
                       ▼
           Victoria Metrics + alerts   (compare the two sides; a mismatch is drift)
```

- The two sides are two different API servers, so they need two [`milo-os/resource-metrics`](https://github.com/milo-os/resource-metrics) collectors. The upstream one runs `discovery.mode: milo` and engages every customer project; the downstream one runs `discovery.mode: single` on the infrastructure cluster that stores the replicated copies. No new code in the DNS operator.
- Recording rules compare the two sides. A downstream record with no matching project record is an **orphan**; a project record with no matching downstream record is **missing**.
- Rules ship as a standard Prometheus `PrometheusRule` and evaluate in Victoria Metrics.

## What's included

- `config/milo/resource-metrics/policies/dns-metrics.yaml` — the upstream side. This already ships to the Milo core control plane today; the rules join against the series it already emits.
- `config/observability/cluster-policy/` — `dns-downstream-metrics`, the downstream side, for the infrastructure cluster.
- `config/observability/rules/` — the `dns-controlplane-drift` recording rules and alerts.
- `config/dependencies/` and `config/overlays/` — the environment used to validate it end-to-end.
- `test/e2e/controlplane-drift/` — automated tests for all three cases (healthy, orphan, missing).

## Try it

Against a local Kubernetes (kind) setup:

```sh
export TASK_X_REMOTE_TASKFILES=1
task env:milo-all-up      # bring up the full DNS platform + metrics + alerting
task env:chainsaw-milo    # run the drift-detection tests (healthy / orphan / missing)
```

The orphan test reproduces #346 end-to-end: replicate a record, break replication, delete the customer's record, and watch the orphan alert fire on the leftover — then clear once it's removed.

## Enabling in staging / production

The upstream series already exist — `dns-metrics` runs in production today. What's new is the downstream half, and it needs a second `resource-metrics` deployment on the DNS infrastructure cluster in `discovery.mode: single`, with the `dns-downstream-metrics` policy and read access to `DNSZone`/`DNSRecordSet` there.

> [!NOTE]
> `discovery.collectRootControlPlane: true` is **not** the way to get these series. It collects Milo's root control plane, and the replicated copies do not live there — they live in the infrastructure cluster's own apiserver.

Validate the new series in staging for one metrics-retention window before wiring the alerts to paging.
