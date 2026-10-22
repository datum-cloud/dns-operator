# Internal DNS metrics and instrumentation proposal

Status: proposal, 2026-10-22. Scope: the signals the internal DNS dashboard strategy (not yet in this repository) needs, and the architecture fixes those signals depend on.

Code references are to dns-operator `fix/internal-dns-name-references` at `1b90c00` (the build staging runs), NSO and Compute `main`, and infra `main`. Live numbers come from staging VictoriaMetrics, queried 2026-10-22.

| Appendix | Area |
|---|---|
| [01-serving.md](01-serving.md) | CoreDNS context plugin, dnsdist, public recursor, serving agent |
| [02-control-plane.md](02-control-plane.md) | Control plane, NATS transport |
| [03-resources-producers.md](03-resources-producers.md) | Resource-state metrics, NSO and Compute producers |
| [04-probes-slos.md](04-probes-slos.md) | Synthetic probes, SLIs, alerting |

> [!NOTE]
> Where an appendix disagrees with this page, this page wins. The main case is the metric prefix: appendix 02 proposes `datum_dns_internal_` for everything. That was based on the fleet namespace mapping to no service, but we verified it maps to `dns`/`delivery`.

## What we found

These findings change the plan. Each one was checked against code or live data.

| # | Finding | Why it matters | Ref |
|---|---|---|---|
| F1 | NATS broker credentials are mounted into the query-serving containers (`serving`, `dnsdist`). Every container runs as UID 65532. The design's UID split (agent 65532, daemons 65533 with no broker credentials) is not in staging. | Opening a metrics port in those containers widens the blast radius of a query-path compromise. This must be fixed first. | infra `edge-staging/members.yaml:27,176`; dns-operator `config/internal-dns/fleet.example.yaml:108` |
| F2 | Fleet readiness accepts any DNS reply, including REFUSED and SERVFAIL. The dnsdist probe queries `. SOA`, which the context classifier always refuses, so it never tests the real path. | A member that fails every private name stays Ready. Dashboards and alerts built on Ready would lie. | `serving/readiness.go:63-99` |
| F3 | No container in the fleet has a liveness or startup probe. `--role=serving` respawns CoreDNS every 50 ms. | CoreDNS crashes never show up as Kubernetes restarts. | `serving/daemons.go:58-124` |
| F4 | CoreDNS is built in Go with one plugin, so it has no `metrics`, `health`, `ready`, `errors` or `log`. dnsdist and the recursor expose no webserver. | The query path has no rate, error or latency signal at all. | `coredns/server.go:36-55` |
| F5 | Control-plane metrics are opt-in (`metricsBindAddress` config key, off in staging). Health probes are disabled. The only readiness probe is a `tcpSocket` probe against the TLS webhook port, which logs about 720 TLS handshake errors per hour per pod. | There's no control-plane signal, and the logs are mostly noise. Infra #7479 couples the scrape to a config key that crash-loops any image without it. | `runtime/run.go:187-199`, `config.go:65,98-110` |
| F6 | About 5.7 msg/s on `DNS_PRIVATE`, of which 99.7% is ACK heartbeats. It isn't a hot loop, but it's wasteful: one ACK per binding per member every 30 s, unique IDs, 24 h retention at R3 (about 300 MB), every member consuming every ACK, and an uncached LIST plus PATCH on the control plane per ACK. That is O(B²) per interval. | It doesn't scale to the 200-binding shard cap. The backlog lags on the **control plane's** ACK consumer, not on the members'. | `serving/agent.go:895-903,1145,1176-1230`; `platform/acks.go:152-197` |
| F7 | Each project's controller is named after its project UID. controller-runtime v0.23.3 rejects duplicate names process-wide. | A project source manager that stops can never restart. The `controller` label also grows by one value per project. | `controlplane/reconciler.go:127` |
| F8 | Both control-plane replicas run the outbox and ACK-refresh loops, so optimistic-lock conflicts show up as errors. | Error ratios are inflated. Lag is hard to read. | `runtime/run.go:215-230,329` |
| F9 | Producers return an error together with `RequeueAfter` (NSO, Compute, dns-operator), treat missing preconditions as errors, poll on timers, never log, and emit no Events. NSO runs about 93k reconciles/h for about 73 network contexts. The Compute publisher logs about 6.4k `requeue_after` against about 20 successes per hour. | Today's incident (infra #7513) was about 200 errors/h with no object key, and nothing surfaced it. The publisher's error ratio is still dominated by us-east-1's missing observation source (compute #517), so it can't be used as an incident signal yet. | appendix 03 §1 |
| F10 | Only 3 of 12 internal DNS kinds have a `Ready` condition. Some kinds track state in free-text `phase`/`state` strings. Outbox and Ownership have no conditions. Condition messages repeat the reason. | A generic "not ready" panel or alert is impossible until there is one. | appendix 03 §1 |
| F11 | Service-scoped kinds (ResolverBinding, Manifest, Outbox) live in the Milo project `internal-dns-service`, not on the infra cluster. NSO's resource-metrics policy (`datum_cloud_network_context_status_condition`) exists but isn't deployed. The NSO cell manager (`internaldns-cell`) isn't scraped. | The strategy doc's plan to route these through kube-state-metrics wouldn't work. Two cheap wins are available. | appendix 03 §1 |
| F12 | No probe exercises a private name through `fd53::53`. The insider probe resolves public names only, and its latency didn't move at the #7475 cutover, so it probably still uses the old resolvers. | That's why #7513 went unseen for about 14 hours. | appendix 04 §1 |
| F13 | Series from lab clusters (`cluster=~".*-lab"`) hit a prepended route to `slack-labs`. | Server-side alerts from the fleet would never page. | infra `alert-routes.yaml` |

## Decisions

| Topic | Decision | Reason |
|---|---|---|
| Metric prefix | Self-emitted series use `internal_dns_*` (control plane, agent, CoreDNS plugin, dnsdist custom). Exporter-produced resource-state families use `datum_dns_internal_*`. Producer metrics use their owner's prefix (`datum_cloud_compute_*`, `datum_cloud_network_*`). | House rule: `metric-prefixes.yaml` is for exporter families. Self-emitted series route by `namespace:datum_service:info`, which already maps `internal-dns-system` and the fleet namespace to `dns`/`delivery` (verified live). `datum_dns_zone_*` is taken. `datum_cloud_network_*` → alb → delivery is already the right owner. |
| Endpoints | Every process serves `/metrics`, `/healthz` and `/readyz`, always on. Ports come from **flags with defaults**, not the config file. | Matches the kubebuilder convention. Removes the #7479 rollout coupling. |
| Bind address | Metrics and probe ports bind `$(POD_IP)`, never `[::]`, plus a NetworkPolicy that admits only vmagent. | `[::]` would listen on `net1`, the producer network. |
| Member identity | Pod labels `internal-dns.datum.net/{member,role,region}`, copied with PodMonitor `podTargetLabels`. Dashboards key on `member`. | `pod` changes on every rollout. `memberID` is an opaque token. |
| CoreDNS | Keep the Go-built server, which keeps the atomic snapshot swap that makes serving fail closed. Wire in upstream `metrics.New`, and reimplement `/health` and `/ready`. | We get the standard `coredns_*` families without giving up the fail-closed design. |
| Readiness | Ready means "can answer". A fixed `CH TXT ready.internal-dns.` canary must return NOERROR through dnsdist and the backend pool. Liveness is a separate, lenient, process-local check. | Fixes F2 and F3. The canary doesn't depend on any context, so a node with zero contexts is still Ready. |
| Freshness canary | The planner adds a `_canary.<suffix>` TXT record (`rev=…`) to every context. Probes and the member self-test use it. | One positive name per context, and end-to-end freshness without churn. |
| Cardinality | No context, binding, VPC, zone, qname or client labels on server metrics. At most about 1,000 series per member pod. Per-member ACK series are exported only while not acknowledged, or as a per-binding max. | Context count grows with tenants. |
| Server-side alerts | Aggregate `without (cluster)`, or add an explicit route ahead of `lab-clusters`. | F13 |
| SLOs | Use burn rates in the house `KubeAPIErrorBudgetBurn` shape. Set targets only after 2–4 weeks of baseline. Isolation leaks are a zero-tolerance alert, not an SLO. | The insider probe's own floor is 98.8%, so a fixed "99% over 10 m" rule would page constantly. |

## Signals, by layer

The full catalogue (names, types, labels, buckets) is in the appendices. This table maps each layer to what the dashboards need.

| Layer | Upstream families | Custom families (prefix `internal_dns_`) | Appendix |
|---|---|---|---|
| CoreDNS plugin | `coredns_dns_requests_total`, `_responses_total{rcode}`, `_request_duration_seconds`, `coredns_forward_*` | `context_queries_total{outcome}`, `context_snapshot_contexts{state}`, `context_snapshot_reloads_total`, `context_snapshot_applied_timestamp_seconds` | 01 |
| dnsdist | `dnsdist_queries`, `_responses`, `_servfail_responses`, `_latency_*`, `_server_state{server}`, `_pool_*` | `dnsdist_context_queries_total{outcome}` | 01 |
| Recursor | `pdns_recursor_*`, as `datum-managed-dns-cache` does | none | 01 |
| Agent | Go, process | `agent_serving_fence{state,part}`, `agent_events_total`, `agent_activations_total`, `agent_activation_duration_seconds`, `agent_fail_closed`, `agent_checkpoint_timestamp_seconds`, `agent_bindings{state}`, `agent_transport_connected`, `selftest_*` | 01, 04 |
| Control plane | controller-runtime, workqueue, webhook | `loop_runs_total{loop,region,shard,result}` (replaces `operations_total`), `loop_duration_seconds`, `publication_lag_seconds`, `withdrawal_seconds`, `outbox_pending`, `outbox_oldest_pending_age_seconds`, `nats_publish_duration_seconds`, `nats_connected`, `ack_projection_seconds`, `shard_bindings` / `_limit`, `shard_writer`, `build_info` | 02 |
| NATS | existing exporter (`nats_stream_*`, `nats_consumer_*`) | none. Alert on failure to drain (`min_over_time(num_pending[2m]) > 0`), not on a pending threshold. | 02 |
| Resource state | — | `datum_dns_internal_<kind>_{info,owner,status_condition,status_condition_last_transition_time,condition_generation_lag}`, `datum_dns_internal_resolver_binding_ack_*`, `datum_cloud_network_context_status_condition` (deploy the existing policy), `datum_cloud_compute_instance_dns_status_condition` | 03 |
| Producers | controller-runtime | `datum_cloud_compute_internal_dns_publications_total{result,reason}`, `_publication_latency_seconds`, `datum_cloud_network_internal_dns_ready_latency_seconds` | 03 |
| Probes | — | `qa_internal_dns_queries_total{case,transport}`, `_query_failures_total{reason}`, `_query_duration_seconds`, `_churn_seconds{phase}`, `qa_internal_dns_lifecycle_*` (counters, not per-minute gauges) | 04 |

## Architecture fixes

| Standard we aren't meeting | Fix | Repo | Appendix |
|---|---|---|---|
| Least privilege between agent and query daemons | Restore the UID split. Remove the `nats` mount from `serving` and `dnsdist`. | infra | 01 S1 |
| Readiness reflects the ability to serve | NOERROR-only canary readiness. HTTP probes instead of a 2 s exec. Separate liveness and startup probes. | dns-operator, infra | 01 S3, S6, S7 |
| Crashes are visible to Kubernetes | Liveness plus `process_start_time_seconds`. Consider running CoreDNS as its own container. | dns-operator | 01 Q4 |
| Operational knobs are flags, config is versioned | Flags for ports. `RuntimeConfig` gets `apiVersion`/`kind`. Reject duplicate keys. | dns-operator | 02 #2, #3 |
| Probes don't spam logs | HTTPS `/readyz` on the admission port now, `:8081` later. Route stdlib `log` through zap. | infra, dns-operator | 02 #1 |
| Controllers can restart | Stable controller names. | dns-operator | 02 #4 |
| Watches over polling, single writer | A cached platform manager watching ResolverBinding, Outbox and Ownership. Leader election or lease-sharding for outbox and refresh. A GET by name instead of a LIST per ACK. Watches in NSO and Compute in place of 10–30 s timers. | dns-operator, NSO, Compute | 02 #6; 03 #1, #5 |
| Efficient transport | One serving ACK per member per interval. Split ACK subjects. A separate `DNS_PRIVATE_ACKS` stream with a 5 m MaxAge. Jitter. Patch status only on change. | dns-operator, infra | 02 #7, #8 |
| Pending is status, not error | Never return error together with `RequeueAfter`. Missing preconditions → `Unknown`/`Pending` conditions. Keyed structured logs. Events on transitions. | Compute (#517 first), NSO, dns-operator | 03 #1, #5 |
| Consistent API conditions | A `Ready` rollup on every kind. Conditions replace `phase`/`state` strings. Top-level `observedGeneration`. Real messages. | dns-operator | 03 #6 |

## Rollout

Every phase is useful on its own, and later phases don't block earlier dashboard panels.

| Phase | PRs | Unlocks | Risk |
|---|---|---|---|
| **0 — now** | infra: switch the control-plane readiness probe to HTTPS `/readyz` (no code change). infra: deploy NSO's `networking-metrics` policy and scrape the NSO cell manager. compute: land #517. infra: rework #7479 (drop the config key, keep the scrape) and hold it for phase 1. Strategy doc: fix O11 (`sum_over_time`) and the corrections below. | `DNSReady` panel, NSO cell controller panels, quiet logs, a clean publisher error ratio | Low |
| **1 — control plane** | dns-operator: endpoint flags, health checks, reshaped `loop_*` metrics, stable controller names, versioned config. infra: #7479 as reworked. | Control-plane dashboard, overview O6 | Low |
| **2 — security and serving** | infra: UID split and pod labels (**gate for the rest of this phase**). dns-operator: CoreDNS metrics and canary readiness, dnsdist webserver, agent metrics. infra: ports, probes, PodMonitors, NetworkPolicy, recursor webserver. | Serving dashboard, overview O3/O4/O9 that tell the truth | Medium: readiness semantics change |
| **3 — state and producers** | dns-operator: condition consistency, then the `internal-dns-metrics.yaml` policy. compute: `instance_dns_status_condition` plus publisher hygiene. NSO: pending-as-status, watches, Events. | Producers dashboard, overview O8 | Medium: API status changes |
| **4 — probes and SLOs** | Canary project with two VPCs. qa-synthetics `probes/internal-dns` (resolve, isolation, NAT64, churn). Lifecycle canary. dns-operator `_canary` record and member self-test. infra: probe alerts, then burn-rate rules after the baseline. | Overview O10, the SLOs, and detection of a #7513-class incident within about 15 min | Low |
| **5 — efficiency** | dns-operator: cached platform manager, leader election, ACK redesign (wire-compatible for one release). infra: NATS stream and ACL changes. | Scales to the shard cap; less noise in transport panels | Medium: protocol change |

## Decisions needed

1. **Prefix rule.** Approve `internal_dns_*` for self-emitted metrics and `datum_dns_internal_*` for exporter families. This overrides appendix 02.
2. **#7479.** Rework as described, or merge it as-is and accept the coupling until phase 1?
3. **Probe and alert ownership.** Proposed: DNS (delivery) owns the probe and its alerts, and QA (observability) owns the harness. Also needed: a staging paging policy for internal DNS critical alerts.
4. **ACK series cardinality.** Per-binding max (recommended) or per-member series only while unacknowledged?
5. **A `_canary` TXT in every context.** Acceptable in the API? Should tenants be able to see it?
6. **Supervision.** Should CoreDNS move out of the `serving` supervisor into its own container? Crashes would become visible, but the fail-closed gating would need rework.
7. **ResourceMetricsPolicy scope.** It has no project selector, so adding nine kinds opens informers on every project control plane. Is that acceptable, or should resource-metrics gain a selector first?

## Corrections to the dashboard strategy

- The 443 ACK backlog peak belongs to the control plane's consumer. Member backlogs are a sawtooth that drains every 2 minutes.
- O11: the QA DNS metrics are per-minute gauges. Use `sum_over_time`, not `increase`.
- Phase 3's "probe success < 99% over 10 m (paging)" would page constantly. Use burn rates.
- Service-scoped kinds go through `ResourceMetricsPolicy` against `internal-dns-service`, not kube-state-metrics.
- NSO already defines `datum_cloud_network_context_status_condition`, and that prefix already routes to the right team.
- Member identity comes from pod labels, not `pod`. Revision lag must compare epochs as well as revisions.
- The recursor's webserver port is 8083, named `recursor`. dnsdist must bind `POD_IP`, not `[::]`.
- TLS probe noise is about 720 lines/h per pod. The resolver-binding requeue in staging is 60 s.
- `datum_dns_operations_total` comes from the external-dns publisher, not dns-operator.
