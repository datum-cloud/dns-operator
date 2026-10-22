# 02. Control plane and transport metrics

Scope: G-4, the NATS row, and the `internal-dns-control-plane` dashboard. Code refs are to dns-operator `fix/internal-dns-name-references` @ `1b90c00`, which is the deployed `source_revision`. Staging data comes from VictoriaMetrics, queried 2026-10-22.

## 1. Current state

| # | Area | Finding | Ref |
|---|---|---|---|
| 1 | Metrics opt-in | The `/metrics` server starts only if `metricsBindAddress` is set. Staging leaves it unset, so no `internal_dns_*` series exist in VM. | `runtime/run.go:193-199`, `config.go:65` |
| 2 | Registry | All managers set `Metrics.BindAddress: "0"`, but controller-runtime still registers controller, workqueue and webhook metrics in the global `metrics.Registry`. Once #1 is on they appear for free. | `run.go:114,175` |
| 3 | Controller naming | Each project's controller is named `internal-dns-control-plane-<12 hex of project UID>`, so the `controller` label grows with the number of projects. A rebuilt source manager reuses the name, and controller-runtime v0.23.3 rejects duplicate names process-wide (`pkg/controller/name.go:37`). So a source manager that stops can never start again: `runProjectSource` keeps retrying a setup that always fails. | `controlplane/reconciler.go:127-128`, `run.go:249-267` |
| 4 | `operation` label | Free text with region and shard folded in: `planner us-central-1/shared-0`, `ack refresh …`, `outbox us-central-1`, `ack read/apply/commit …`, `project discovery`. There is no duration metric and no backoff. | `run.go:222,226,329,512-526,482-501` |
| 5 | Health | `HealthProbeBindAddress: "0"` everywhere. A `/readyz` handler that always returns 200 is already registered on the TLS webhook server, but staging probes the port with `tcpSocket`. Each probe produces `http: TLS handshake error … EOF`. Measured: 120 lines per 10 minutes per pod, so about 720 per hour per pod and 1,440 per hour in total. There is no liveness probe. | `run.go:187-189`, Deployment |
| 6 | Config decoding | `DisallowUnknownFields` plus a single-object check. The reviewer's #7479 warning is correct, but the risk is limited in practice: `5e0148c`, which adds the key, is in the deployed `1b90c00` and exists only on this branch. Strict decoding does not reject duplicate keys. Two older ConfigMaps (`…-d2mtg24dh4` and `…-h5m66b9bb7`) contain `ownershipLeaseSeconds` twice (120, then 30), and the last value silently wins. Strict decoding is good. The problem is that operational knobs (ports) live in a versionless file that ships separately from the image. | `config.go:98-110` |
| 7 | Logging | zap JSON, but the TLS errors come from Go's stdlib `log` as plain text. Every failure on every tick logs one error at ERROR, and there is no backoff. A sample error has an empty project field (`"project  update zone …"`). | `cmd/internal-dns/main.go:25-31` |
| 8 | Events | No `EventRecorder` anywhere under `internal/internaldns`. Capacity rejections, writer takeovers and activation failures leave no trace on the object. | grep |
| 9 | Active-active | Both replicas run the outbox, ACK refresh and every planner loop. Only the planner is fenced, by the shard-owner ConfigMap lease. The outbox and refresh loops collide on optimistic locks. The logs show steady `Operation cannot be fulfilled` errors on outbox, manifest, zone and registration objects, which inflate `result="error"`. | `run.go:215-230,329` |
| 10 | Polling | The outbox does an uncached LIST every 500 ms per replica. ACK refresh does a LIST of bindings and owners plus a GET per manifest every 2 s. Resolver bindings live in the platform namespace and nothing watches them (#261). Project reconcile requeues at lease/2, which is 60 s in staging (120 s lease), not 15 s. | `platform/outbox.go:44-53`, `acks.go:414`, `controlplane/publication_reconciler.go:116,123` |
| 11 | Requeue misuse | The publication reconciler returns `RequeueAfter` together with an error, and controller-runtime logs a warning and ignores the requeue. | `publication_reconciler.go:116` |

### NATS findings, verified in code and data

- **About 5.76 msg/s, of which 99.7% is ACK heartbeat traffic.** It is not a hot loop. Delivered rate per consumer over 30m: `*-acks` 5.74/s, `*-records` 0.014/s, `*-serving` 0.0006/s. KV gets 0.015/s.
  - Each member sends one serving ACK per binding every `AckInterval`, 30 s by default (`serving/agent.go:895-903,1176-1190`).
  - Each regional (`combined`) member also re-ACKs every verified publication every 30 s (`agent.go:1193-1230`).
  - That works out to roughly 173 ACKs per 30 s, which fits 4 members × about 40 bindings plus 2 × a few zones.
  - The planner does not cause it: it dedups on content hash (`planner.go:233-251`).
- **Why the volume is inefficient:**
  - Every per-binding serving ACK carries the same `ServingFence` (`agent.go:1186`).
  - Every ACK has a unique event ID (`UnixNano`, `agent.go:1145`), so nothing dedups.
  - ACKs are kept for 24 h at R3: 349k messages and 298 MB in `DNS_PRIVATE`, although no ACK lease is valid for more than 2 min.
  - On the control plane, each serving ACK does an uncached LIST of all bindings, then a status PATCH (`acks.go:152-197`). That is O(B) reads per ACK and O(B²) per interval. At the 200-binding cap it would be about 27 ACK/s per shard.
- **Member `*-acks` consumers are live, not vestigial.**
  - `runAgentTransport` creates them (`run.go:414-418`) and `RunTransport` consumes all of them (`agent.go:344-360`).
  - Members only act on regional *publication* ACKs and drop serving ACKs after validating them (`agent.go:683`). So about 95% of what they consume is waste, with N² fan-out.
  - The backlog is a sawtooth, not a leak. The heartbeats are synchronized, so each 30 s burst lands at once on a serial handler (ack_pending ≤ 2). Publication ACKs take the agent mutex and run `reconcile` plus `finishEvent` (`agent.go:714-728`).
  - Evidence: the 24 h max of `min_over_time(num_pending[2m])` is **0** for every member consumer, and the 24 h average is 25–32.
- **The control-plane consumer is the one that actually lags.** Its 24 h peak is 443 (the strategy doc wrongly gives this to members), it sat at ≥104 for a full 2 minutes, and it had 144 redeliveries in 24 h.

### Corrections to the strategy doc

- The 443 peak belongs to `control-plane-…-acks`, not to a member.
- TLS noise is about 720 lines/h per pod, not 900/h.
- The resolver-binding requeue is 60 s in staging, not 15 s.
- `datum_dns_operations_total{operation,status}` is not emitted by dns-operator. It comes from the external-dns publisher used in `dns-change-pipeline`. The only metrics on main are `dns_operator_*`.
- Member ACK backlog should alert on drain failure, not on a pending threshold.

## 2. Proposed design

### Prefix: `datum_dns_internal_`

- In-process metrics in `internal-dns-system` already route through `namespace:datum_service:info`.
- The agent runs in the edge namespace `ns-<uuid>`, which maps to no service.
- `datum_dns_` routes to `dns` through `metric-prefixes.yaml`. One prefix for control plane, agent (G-3) and exporter families (G-5) routes correctly wherever the series lands.
- Rename now. Nothing consumes `internal_dns_*` yet.

### Endpoints, always on

- **Metrics:** `--metrics-bind-address`, default `:8080`.
- **Probes:** `--health-probe-bind-address`, default `:8081`. These are flags, not config keys, which matches the kubebuilder `cmd/main.go` convention used by dns-operator itself.

| Path | Checks | Used by |
|---|---|---|
| `/healthz` | `loops`: every `retryLoop` ticked within 3× its interval (a deadlock check, not a dependency check) | livenessProbe |
| `/readyz` | `webhook` (`StartedChecker`, which loads the cert), `projects` (the first discovery pass finished, which admission needs) | readinessProbe, `?exclude=nats&exclude=writer` |
| `/readyz/nats` | every region's connection is `CONNECTED` | humans, metrics only |
| `/readyz/writer` | this replica holds at least one shard lease, or a peer's lease is fresh | humans only |

NATS and the writer stay out of the kubelet probe. Admission does not depend on NATS, and a NATS outage must not take the webhook (which is `failurePolicy`-bound) out with it.

### Catalogue

Labels are bounded: `region`, `shard`, `loop`, `kind ∈ {serving,chunk,manifest,ack}`, `result ∈ {success,error,conflict}`. Never project, binding or zone. Inventory gauges come only from the shard writer. Dashboards use `max by` for gauges and `sum` for counters.

| Metric | Type | Labels | Buckets / notes |
|---|---|---|---|
| `…_loop_runs_total` | counter | loop, region, shard, result | replaces `internal_dns_operations_total`; `conflict` separates optimistic-lock losses |
| `…_loop_duration_seconds` | histogram | loop, region | .01 .05 .1 .25 .5 1 2.5 5 10 30 |
| `…_loop_last_success_timestamp_seconds` | gauge | loop, region, shard | |
| `…_publication_lag_seconds` | histogram | region, shard, kind | outbox `creationTimestamp` → `AckSink` flips Serving/Published; 0.5 1 2 5 10 20 30 60 120 300 600 |
| `…_outbox_pending` | gauge | region, kind | non-acknowledged, non-superseded |
| `…_outbox_oldest_pending_age_seconds` | gauge | region | |
| `…_outbox_publish_total` | counter | region, kind, result | |
| `…_nats_publish_duration_seconds` | histogram | region, kind | wraps `JetStream.Publish` (`jetstream.go:189`); .001 .005 .01 .025 .05 .1 .25 .5 1 2 |
| `…_nats_connected` / `…_nats_reconnects_total` | gauge / counter | region | from `nats.ConnectHandler`/`ReconnectHandler` (`jetstream.go:96`) |
| `…_kv_put_total` | counter | result | snapshot bucket |
| `…_acks_total` | counter | region, shard, kind, phase, result (`applied`,`stale`,`expired`,`rejected`,`error`) | |
| `…_ack_projection_seconds` | histogram | region, shard, kind | ACK `ObservedAt` → status patch committed; .05 .1 .25 .5 1 2 5 10 30 60 |
| `…_shard_bindings`, `…_shard_binding_limit`, `…_shard_snapshot_bytes`, `…_shard_snapshot_byte_limit`, `…_shard_rejected_bindings` | gauge | region, shard | `planner.go` fitSnapshot |
| `…_bindings` | gauge | region, shard, phase | |
| `…_contexts`, `…_private_zones` | gauge | phase | from project sources |
| `…_projects` | gauge | source (`static`,`discovered`), state (`running`,`failed`) | |
| `…_withdrawal_seconds` | histogram | region, kind (`binding`,`zone`) | tombstone outbox create → last required ACK; 1 5 15 30 60 120 300 900 |
| `…_activation_failures_total` | counter | region, shard, reason | |
| `…_shard_writer` | gauge 0/1 | region, shard | |
| `…_shard_writer_epoch`, `…_shard_writer_lease_remaining_seconds` | gauge | region, shard | |
| `…_shard_writer_transitions_total` | counter | region, shard | |
| `…_build_info`, `…_config_info{hash}` | gauge | version, revision | |

### NATS: server vs client

- **Server side.** Rely on the existing exporter for stream bytes and limits, per-consumer `num_pending`, `num_ack_pending`, redelivered, the expected consumer set, and server or JetStream API health.
- **Client side.** Emit only what the server cannot attribute: publish latency and errors per kind, connection state, ACK projection latency, and handler outcomes.
- **Alerting.** For member `*-acks`, alert on `min_over_time(num_pending[2m]) > 0` for 10 m (failure to drain), not on a raw threshold.
- **Dashboard.** No shared NATS dashboard yet. Build the NATS row as a reusable jsonnet helper, and promote it to `Infrastructure / Messaging` when activity-system's NATS joins.

## 3. Architecture fixes

| Fix | Change |
|---|---|
| Config/rollout coupling | Move bind addresses to flags with defaults. Keep strict decoding, but add `apiVersion`/`kind` (`RuntimeConfig/v1alpha1`) so that a newer schema fails with "unsupported version" rather than "unknown field". Add duplicate-key detection, and expose `config_info{hash}`. |
| TLS probe noise | Now, infra only: `readinessProbe.httpGet {scheme: HTTPS, path: /readyz, port: admission}`, since kubelet skips cert verification. Later: probes on `:8081`. Also route stdlib `log` through zap (`http.Server.ErrorLog`). |
| Resolver-binding polling | Run one cached platform-namespace manager with informers for `DNSResolverBinding`, `DNSTransportOutbox` and `DNSPublicationOwnership`. Drive outbox and ACK refresh from watch events, with a slow resync. In `AckSink.serving`, do a GET by name instead of a LIST. |
| ACK traffic (confirmed) | (a) One serving ACK per member per interval, carrying the fence and a binding-set hash. (b) Split subjects into `acks.<r>.<s>.{serving,publication}.<member>`, so member consumers filter `publication` only. (c) Move ACKs to a `DNS_PRIVATE_ACKS` stream with `MaxAge` 5 m. (d) Jitter the heartbeat by ±20%. (e) Patch status only when phase or revision changes, or when the lease crosses half its life. |
| Active-active | Use leader election (or the existing shard lease) for outbox and refresh, or shard them by lease, so that conflicts stop showing up as errors. |
| Controller names | Use one stable name per controller with `SkipNameValidation` scoped to the rebuild path. Alternatively, keep one manager per project for the process lifetime and restart only its sources. |
| Events | Emit Events for capacity rejection, writer takeover, activation failure and supersession. |

## 4. Work breakdown

| # | Repo | Change | Files | Depends on |
|---|---|---|---|---|
| 1 | infra | Amend **#7479**: drop the `metricsBindAddress` config key, keep the `metrics` port and the VMPodScrape, and switch readiness to HTTPS `/readyz`. Merge only after PR 2's image lands. The probe-only part can merge immediately as its own small PR. | `apps/dns-operator/internal-dns/staging/runtime/{controller,config,metrics}.yaml/json` | 2, probe part none |
| 2 | dns-operator | Add flags `--metrics-bind-address :8080` and `--health-probe-bind-address :8081`, keeping a deprecated `metricsBindAddress` key. Add healthz/readyz checks. Rename to `datum_dns_internal_loop_*` with `{loop,region,shard,result}` and a duration histogram. Route stdlib log through zap. | `cmd/internal-dns/main.go`, `runtime/{run,config,metrics}.go` | — |
| 3 | dns-operator | Versioned config and duplicate-key rejection | `runtime/config.go` | 2 |
| 4 | dns-operator | Controller-name fix | `controlplane/reconciler.go`, `runtime/run.go` | — |
| 5 | dns-operator | Pipeline metrics: outbox, publish latency, ACK projection, shard fill, writer, inventory, lag and withdrawal | `platform/{outbox,planner,acks}.go`, `transport/jetstream.go` | 2 |
| 6 | dns-operator | Cached platform manager and watches. Outbox and refresh become event-driven. Leader election. | `runtime/run.go`, `platform/*` | 5 |
| 7 | dns-operator | ACK redesign (a)–(e). Wire compatible: the CP accepts both shapes for one release. | `serving/agent.go`, `platform/acks.go`, `model/`, `runtime/run.go` | 5 |
| 8 | infra | NATS ACL and stream updates for the split subjects and `DNS_PRIVATE_ACKS` | `apps/dns-operator/internal-dns/staging/*nats*` | 7 |
| 9 | infra | Control-plane dashboard plus NATS jsonnet helper and alerts | `.jsonnet/internal-dns-*`, `infrastructure/monitoring/dns-system/` | 2, 5 |

## 5. Open questions

1. Is `RuntimeConfig` versioning worth it, or is "flags for ops knobs, strict file for topology" enough?
2. Should ACK freshness move entirely out of the broker, for example into a KV per member with TTL, instead of a stream?
3. Leader election vs. lease-sharded active-active for outbox and refresh: which failover time do we need?
4. Should publication lag start at the producer's `metadata.generation` observation (more honest, but it needs a timestamp carried through the planner) or at outbox creation (simple)?
5. What is the per-project controller-metric cardinality budget once project discovery reaches hundreds of projects?
