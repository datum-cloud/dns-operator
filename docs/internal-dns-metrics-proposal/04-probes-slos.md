# 04 — Synthetic probing, SLIs and alerting (G-6, §4, O10/O11)

Every live number below comes from staging VictoriaMetrics, queried on 2026-10-22 around 17:00Z.

## 1. Current state

### What the existing probes cover

| Check | What it actually does | Private name via `fd53::53`? |
|---|---|---|
| `probe-compute-insider-dns-resolve` (`qa-synthetics:probes/compute-insider/main.go`, README) | `AAAA ipv6.icanhazip.com` and `AAAA ipv4.icanhazip.com` (the second is DNS64-synthesised), queried every 1s against whatever `/etc/resolv.conf` lists. It pushes per-minute stats to `metrics-qa`. In staging it runs only from `us-central-1` (us-east-1 cannot push), on `general-purpose` and `unikernel`. | **No.** It queries public names only and has no NXDOMAIN or isolation case. Its average latency held at 8–9 ms through the 02:04Z cutover of infra #7475. Guests pick up their resolvers when they are created, so these probe instances most likely still use the old public resolvers. |
| `scenario-compute-workload-lifecycle` | `dig +short <public hostname>` from Nomad VMs outside the platform | No. It never reads `Instance.status.dns`. |
| `probe-dns-propagation` | Public authoritative check (`ns1.datum-staging.net`, `just-for-test.online`) every 15 min from IAD | No |
| dns-operator `test/internaldns/kubernetes/qualify/scenarios.go` | The full private matrix in Kind: A/AAAA over UDP and TCP, NXDOMAIN, cross-project isolation, unknown destination answered with REFUSED or SERVFAIL, and DNSSEC public recursion | It is a CI qualifier, not a running probe. **Reuse its case list and `expect()` logic.** |

So nothing exercises "a private name resolves through `fd53::53`" or "a new instance gets a resolvable name". That gap is why the #7513 incident went unseen for about 14 hours.

### House patterns

- **QA alerts** (`infra:infrastructure/monitoring/victoria-metrics/base/qa-synthetics-alerts.yaml`, `qa-insider-dns-alerts.yaml`):
  - They are `warning` only, with `service: qa-synthetics` and `team: observability`, and route to `slack-qa`.
  - They come in three shapes, keyed per check: Failing (`max_over_time(...)==0`), Flaky (`<0.8` over 6h) and NotReporting (staleness).
- **The only burn-rate SLO in the house** is the kube-mixin `KubeAPIErrorBudgetBurn` (`kubernetes-system-alerts.yaml:1495`):
  - It reads `*:burnrate{5m,30m,1h,2h,6h,1d,3d}` recording rules.
  - Its factors are 14.4/6 (`critical`) and 3/1 (`warning`).
  - It carries `long` and `short` labels.
- **DNS alerts** (`dns-system/base/dns-zone-alerts.yaml`) carry `service: dns`, `team: delivery` (per `ownership.yaml`) and `runbook_url`.
- **Routing** (`alert-routes.yaml`): `severity=critical` goes to `grafana-oncall`. A `lab-clusters` route on `cluster=~".*-lab"` is **prepended** and sends to `slack-labs`.

### Standards gaps found

1. **The QA metrics are per-minute gauges, not counters.** The strategy's O11 query `increase(qa_compute_dns_failed_lookups[15m])` is therefore wrong. Over 24h it returns 28 failures for `general-purpose`, while `sum_over_time` gives the true 32. Use `sum_over_time`.
2. **The strategy's Phase 3 "probe success <99% over 10m (paging)" would page all the time.** The insider check's 24h pass rate is 98.8% (general-purpose) and 98.6% (unikernel), and one failed minute out of 10 already drops below 99%. Use burn rates instead.
3. **The QA alerts use a `dashboard` annotation.** The standard is `dashboard_url`.
4. **Fleet series are labelled `cluster="us-central-1-staging-lab"` and `environment="edge"`.** The standard says `lab`. Because of the prepended lab route, any server-side alert that keeps `cluster` goes to `slack-labs` and never pages. That is the standard working as intended, so server-side lab alerts stay at `warning` (README decision 8) rather than dropping `cluster`. Probe series pushed via `metrics-qa` carry no `cluster` at all.
5. **Fleet readiness does not check that a member can answer.** `serving.Readiness` sends a DNS query for `.` to each local daemon and accepts any response, whatever its rcode (`dns-operator-244:internal/internaldns/serving/readiness.go`). A member that SERVFAILs every private name stays Ready. The per-binding SOA proof (`verify.go: DNSVerifier.Verify`) runs only at activation.
6. **The publisher error signal is not clean.** `internal-dns-instance-publisher` still logs 234 errors, 20 successes and 6,446 `requeue_after` per hour after #7513 (merged 16:03Z). An error-ratio alert alone cannot detect an incident like #7513, so a canary is required.
7. **The insider probe is deployed from a personal account into `project-8b278`.** That is not owned infrastructure.

## 2. Probe design

**Where it runs.** On in-VPC Compute workloads, one per (location, runtime class), wherever `instanceDNS` points at `fd53::53` (today only `us-central-1-staging-lab`). An external harness cannot reach `fd53::53`. The external Nomad harness only drives the lifecycle canary.

**Who owns it.**
- Code: a new `qa-synthetics:probes/internal-dns/` directory, to reuse the harness conventions and ingest key.
- Platform: a dedicated project, `internal-dns-canary`, owned by a DNS-team service account. It has two VPCs, `canary-a` (the probes) and `canary-b` (the isolation target).
- Alerts: `service: dns`, `team: delivery`.

### Checks

Each check runs every 1s, over UDP, with TCP every 10th query, using the search domain where a short name is noted.

| `case` | Query | Pass |
|---|---|---|
| `static_a`, `static_aaaa` | `canary.<suffix>` A/AAAA (also queried as short name `canary` via `datum.internal`) | NOERROR, exact expected RRset |
| `nxdomain` | `does-not-exist.<suffix>` | NXDOMAIN with SOA in authority |
| `public` | `cloudflare.com` A | NOERROR, ≥1 answer, AD bit set |
| `nat64` | `ipv4.icanhazip.com` AAAA | NOERROR, every answer in `64:ff9b::/96` |
| `isolation` | `canary.<canary-b suffix>` and `canary-b`'s own unique name, queried from `canary-a` | NXDOMAIN. **Any answer containing a `canary-b` address is `reason="leak"`.** |
| `freshness` | `_canary.<suffix>` TXT (see §4.2) | `rev` ≥ the control plane's revision within 60s |
| `churn` (every 5 min) | The probe updates its own `DNSRecordContribution` (`churn` AAAA = `fd00::<nonce>`), polls until the nonce is seen, then deletes it and polls until NXDOMAIN | Publish ≤ 60s, withdraw ≤ 120s |

**Lifecycle canary** (Nomad, every 15 min per runtime class, extending `scenario-compute-workload-lifecycle`):
1. Create an instance in `canary-a`.
2. Assert that `status.dns[].conditions[type=Ready]=True` and that `hostnames` is non-empty within 120s.
3. The resident probe watches its project for instances labelled `canary=lifecycle` and resolves each `status.dns` hostname. It measures from `creationTimestamp` to the first answer, then from the delete event to NXDOMAIN.
4. Today this run would have failed at stage `status_dns`, with reason `PublicationFailed`, within 15 min.

### Metrics

The labels on every series are `service="internal_dns"`, `check="probe-internal-dns-resolve"` (or `scenario-internal-dns-lifecycle`), `environment`, `region` and `runtime_class`.

| Metric | Extra labels | Type |
|---|---|---|
| `qa_check_run_success` | — | gauge per minute (house contract) |
| `qa_internal_dns_queries_total` | `case`, `transport` | counter |
| `qa_internal_dns_query_failures_total` | `case`, `transport`, `reason` (`timeout`, `rcode`, `answer`, `leak`) | counter |
| `qa_internal_dns_query_duration_seconds` | `case` | histogram, buckets 0.005–5 |
| `qa_internal_dns_churn_seconds` | `phase` (`publish`, `withdraw`) | histogram, completed runs only; buckets 1 2 5 10 20 30 60 120 300 |
| `qa_internal_dns_churn_result_total` | `phase`, `result` (`ok`, `timeout`, `api_error`) | counter. A run that never sees its change within 300 s is `timeout` and never reaches the histogram. |
| `qa_internal_dns_lifecycle_stage_seconds` / `_stage_result` | `stage` (`ready`, `status_dns`, `resolves`, `withdrawn`), `result` | house lifecycle shape |

These diverge from the insider probe on purpose:
- They use **cumulative `_total` counters and histograms**, so `rate`/`increase` and burn-rate math work.
- The probe initialises every `reason` series of `_query_failures_total` (and every `result` of `_churn_result_total`) at 0 on start. Without that, the first `reason="leak"` creates the series, and `increase()` over a series that didn't exist before returns nothing, so the first leak would be missed.
- Labels are bounded: there is no instance, qname or address label.
- `qa-synthetics-overview` must add an `internal_dns` row. It renders only `compute`, `compute_dns`, `dns` and `cloud_console` automatically.

## 3. SLIs and SLOs

The windows below follow the PromQL range-vector syntax. `$P` is `service="internal_dns",check="probe-internal-dns-resolve"`.

| SLI | Probe PromQL (primary) | Server-side cross-check | Initial SLO (28d) |
|---|---|---|---|
| Private-name availability | `1 - sum(rate(qa_internal_dns_query_failures_total{$P,case=~"static_.*\|nxdomain"}[w])) / sum(rate(qa_internal_dns_queries_total{$P,case=~"static_.*\|nxdomain"}[w]))` | `1 - sum(rate(coredns_dns_responses_total{rcode="SERVFAIL"}[w])) / sum(rate(coredns_dns_responses_total[w]))` | 99.5% |
| Latency | `sum(rate(qa_internal_dns_query_duration_seconds_bucket{$P,le="0.1"}[w])) / sum(rate(..._count[w]))` | `coredns_dns_request_duration_seconds_bucket{le="0.128"}` (CoreDNS buckets are 0.00025×2ⁿ, so there is no 0.1) | 99% under 100 ms |
| Publication lag | `sum(rate(qa_internal_dns_churn_seconds_bucket{$P,phase="publish",le="60"}[w])) / sum(rate(qa_internal_dns_churn_result_total{$P,phase="publish",result=~"ok\|timeout"}[w]))`: runs that published within 60 s over all runs, so timeouts count as failures. `api_error` is the probe's own fault and is alerted on separately. | `internal_dns_publication_lag_seconds` p99 | 99% within 60s |
| Withdrawal delay | the same ratio with `phase="withdraw"` and `le="120"` | `internal_dns_withdrawal_seconds`; `max(internal_dns_resolver_binding_ack_lag_seconds)` must stay under the lease | 99% within 120s |
| Isolation | `sum(increase(qa_internal_dns_query_failures_total{reason="leak"}[5m])) > 0` (the series is initialised at 0, §2) | `internal_dns_context_queries_total{outcome="refused_unknown_context"}` (expected nonzero is fine) | **Zero.** No budget; this is not an SLO. |
| Public and NAT64 | availability with `case=~"public\|nat64"` | `dnsdist_servfail_responses` | 99.5% |

**Churn sample counts.** One churn every 5 minutes gives 12 runs an hour per probe, so a 1 h window holds about 12 samples and one failure is an 8% error. A p99 over that is meaningless, which is why the SLI is a good-over-total ratio. Even so, churn SLOs alert only on the 6h, 1d and 3d windows (at least 72 runs), never 5m/30m/1h. To get faster detection, run churn every minute or add probe instances per region, and let the lifecycle canary and `InternalDNSPrivateNameDown` cover fast failures.

**How to baseline the targets.**
- Run the probe for 2–4 weeks in `warning` mode.
- Record `internal_dns_probe:sli_error:ratio_rate{5m,30m,1h,2h,6h,1d,3d}`.
- Set the target to the observed 28d attainment minus headroom, and never above the insider probe's public-path floor (98.8% today). The two probes share the guest network, so that floor is a ceiling for the private SLO.
- The insider probe's 10s `max` latency points to timeouts on the guest path. Fix those before committing to 99.5%.

### Burn-rate sketch

This mirrors the house `KubeAPIErrorBudgetBurn` shape, for a 99.5% target where the budget is 0.005.

```yaml
- record: internal_dns_probe:sli_error:ratio_rate1h
  expr: |
    sum by (environment, region) (rate(qa_internal_dns_query_failures_total{service="internal_dns",case=~"static_.*|nxdomain"}[1h]))
    / sum by (environment, region) (rate(qa_internal_dns_queries_total{service="internal_dns",case=~"static_.*|nxdomain"}[1h]))
- alert: InternalDNSAvailabilityBudgetBurn
  expr: |
    internal_dns_probe:sli_error:ratio_rate1h > (14.4 * 0.005)
    and internal_dns_probe:sli_error:ratio_rate5m > (14.4 * 0.005)
  for: 2m
  labels: {severity: critical, service: dns, team: delivery, plane: data, slo: private-name-availability, long: 1h, short: 5m}
  annotations:
    summary: "Private names in {{ $labels.region }} burn availability budget at 14x"
    description: "The private-name probe error ratio in {{ $labels.region }} is {{ $value | humanizePercentage }} over 1h and 5m, against a 0.5% budget. At this rate the 28-day budget is gone in about 2 days."
    impact: "Guests cannot resolve private names"
    runbook_url: https://github.com/datum-cloud/infra/blob/main/docs/runbooks/dns/internal-dns-availability-burn.md
    dashboard_url: "https://grafana.{{ if eq $labels.environment \"production\" }}prod{{ else }}staging{{ end }}.env.datum.net/d/internal-dns-overview"
# plus 6h/30m ×6 (critical), 1d/2h ×3 and 3d/6h ×1 (warning)
```

Server-side rules keep the `cluster` label. The alerting standard keeps lab clusters out of paging on purpose, so these rules must not aggregate it away to dodge the `lab-clusters` route. Server-side alerts from lab clusters stay at `warning` (README decision 8). Probe series carry no `cluster`, so probe alerts still page.

### Staged alerts

All of these route on `service: dns` to the deliver channel. `critical` also pages through `grafana-oncall`. In staging, probe alerts page as listed (README decision 3). Burn-rate alerts (3c-c) stay `warning` until the baseline exists. Server-side alerts from lab clusters stay `warning` (README decision 8).

| Phase | Alert | Condition | Severity | Panel |
|---|---|---|---|---|
| 3c-a | `InternalDNSProbeNotReporting` | no `qa_check_run_success{$P}` for 5m | warning | O10 |
| 3c-a | `InternalDNSIsolationLeak` | any `reason="leak"` | critical, `for: 0m` | O10 |
| 3c-a | `InternalDNSPrivateNameDown` | all `static_*` cases failing for 5m in a region | critical | O10 |
| 3c-a | `InternalDNSNAT64Failing`, `InternalDNSPublicRecursionFailing` | case failure ratio > 10% for 10m | warning | O10, serving recursor row |
| 3c-b | `InternalDNSLifecycleCanaryFailing` | stage `status_dns` or `resolves` failed in 2 consecutive runs | critical | O10, producers |
| 3c-b | `InternalDNSPublicationSlow` / `InternalDNSWithdrawalSlow` | good-over-total churn ratio below its SLO over 6h, or ≥2 `timeout` results per hour | warning | O8, publication-lag panel |
| 3c-c | `InternalDNSAvailabilityBudgetBurn`, `InternalDNSLatencyBudgetBurn` | 4-window burn | critical/warning | O9, O10 |
| 3c-c | `InternalDNSMemberSelfTestFailing` | §4.2 self-test failing for 2m on any member | warning | O4 |

## 4. Architecture improvements

1. **Readiness that matches the ability to answer.** Make `serving.Readiness` require NOERROR plus the expected RRset for the member's `_canary` record through dnsdist over UDP and TCP, and require that the `step` loop has completed a run within three reconcile intervals. The agent has no watchdog of its own. Keep the `.` check for liveness only.
2. **A canary record baked into every context.** It is hidden from tenant-facing APIs and listings (README decision 5). The planner synthesises `_canary.<context suffix>` TXT `"rev=<snapshot revision> ts=<compile unix>"` into each compiled snapshot. It adds one record per context, inside the shard caps. This gives:
   - a universal positive name for probes and the self-test;
   - end-to-end freshness, by comparing `rev` with the control plane's revision, with no churn needed.
3. **A continuous member self-test.** Run the activation-time `DNSVerifier` every 10s against a fixed sample of contexts and export:
   - `internal_dns_selftest_queries_total{role,case,result}`;
   - `internal_dns_selftest_last_success_timestamp_seconds{role}`.

   There are no binding or context labels.
4. **Counters, not per-minute gauges, for new QA probes.** Document the gauge semantics on the insider probe, and fix O11 to use `sum_over_time`.
5. **Owned vantage points.** Move the probe projects off personal accounts. Recreate the insider instances, or add a `resolver` label, so it is known which path they test.
6. **Producer signal hygiene.** The publisher's 6.4k `requeue_after` per hour against 20 successes looks like a hot loop. Fix it before alerting on its error ratio, and add G-5's `datum_cloud_compute_instance_dns_status_condition`.

## 5. Work breakdown

| # | Repo | Change | Files | Depends on |
|---|---|---|---|---|
| 1 | infra | Fix O11 to use `sum_over_time`; change `dashboard` to `dashboard_url` in the QA alerts | strategy doc, `qa-*-alerts.yaml` | — |
| 2 | infra / datumctl | Create the `internal-dns-canary` project, the `canary-a`/`canary-b` VPCs, static contributions, the ingest Secret and a scoped service account | runbook `docs/runbooks/dns/internal-dns-canary.md` | — |
| 3 | qa-synthetics | `probes/internal-dns`: the resolve loop for all cases, with counters and histograms | `probes/internal-dns/{main.go,README.md,Dockerfile}` and the README consumer table | 2 |
| 4 | infra | Probe dashboard `.jsonnet/probe-internal-dns-resolve`, O10 wiring, and an overview row | `.jsonnet/…`, `dashboards/base/…` | 3 |
| 5 | infra | `internal-dns-probe-alerts.yaml` (phase 3c-a) and runbooks | `monitoring/dns-system/base/` | 3 |
| 6 | qa-synthetics | Churn loop and the resident lifecycle watcher | `probes/internal-dns/churn.go` | 3 |
| 7 | qa-synthetics + infra | The `scenario-internal-dns-lifecycle` stage and its Nomad job, plus phase 3c-b alerts | `scenarios/compute-workload-lifecycle/`, `nomad/jobs/` | 6 |
| 8 | dns-operator | `_canary` synthesis in the planner, a self-test loop with metrics, and the stricter readiness | `internal/internaldns/{controlplane,serving/readiness.go,serving/verify.go,serving/agent.go}` | G-3 metrics server |
| 9 | infra | SLI recording rules, burn alerts (3c-c) and SLO targets | `dns-system/base/internal-dns-slo.yaml` | 2–4 weeks of 3–8, G-1, G-4 |

## 6. Open questions

1. May a guest hold a scoped write credential for churn? If not, the Nomad harness writes the change and the TXT `ts` carries the timestamp the probe measures against.
2. `us-east-1-staging-lab` unikraft still uses public resolvers. Should probes run only where `fd53::53` is configured?
3. Should probe series get `cluster` and `environment` labels that align with platform labels, for example `environment=lab`?
4. Staging paging policy: decided (README decisions 3 and 8). Probe alerts page; server-side lab alerts and burn-rate alerts stay `warning` until the baseline.
5. `_canary` record per context: decided (README decision 5), hidden from tenants.
6. Probe ownership: decided (README decision 3). DNS owns the probe and its alerts, and QA owns the harness.
