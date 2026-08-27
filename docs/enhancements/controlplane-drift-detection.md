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
Customer projects  ──replicate──►  Serving control plane  ──►  Live DNS
  (upstream)                          (downstream)
        │                                   │
        └──────── resource-metrics ─────────┘   emits one metric per DNS record, per side
                          │
                          ▼
              Victoria Metrics + alerts   (compare the two sides; a mismatch is drift)
```

- [`milo-os/resource-metrics`](https://github.com/datum-cloud/resource-metrics) emits one metric per `DNSRecordSet`/`DNSZone` from every customer project and from the serving control plane — no new code in the DNS operator.
- Recording rules compare the two sides. A serving-side record with no matching project record is an **orphan**; a project record with no matching serving-side record is **missing**.
- Rules ship as a standard Prometheus `PrometheusRule` and evaluate in Victoria Metrics.

## What's included

- `config/observability/` — the `dns-metrics` metrics policy and the `dns-controlplane-drift` alert/recording rules.
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

The metrics and rules are additive, but the serving-side metrics require one platform change: set `discovery.collectRootControlPlane: true` in the `resource-metrics` config (it collects only customer projects today). Validate the new series in staging for one metrics-retention window before wiring the alerts to paging.
