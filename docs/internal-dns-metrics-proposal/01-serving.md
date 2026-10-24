# 01 — Serving data plane metrics (G-1, G-2, G-3)

Scope: CoreDNS context plugin, node and regional dnsdist, public recursor, serving agent. Code refs are to dns-operator `fix/internal-dns-name-references` (#244). Infra refs are to `origin/main`.

## 1. Current state

Staging VictoriaMetrics has zero `coredns_*`, `dnsdist_*` or `internal_dns_*` series as of 2026-10-22. The only signals for the fleet in namespace `ns-1b86cf09-…` are cAdvisor and kube-state-metrics.

| Area | Finding | Ref |
|---|---|---|
| Metrics server | None in the agent, `serving`, CoreDNS, dnsdist or recursor. The agent role doesn't start one. The control plane has one (`internal_dns_operations_total`), but staging leaves `metricsBindAddress` unset, so it's off. | `internal/internaldns/runtime/run.go:48-58,193,354`; `runtime/metrics.go:10`; infra `apps/dns-operator/internal-dns/staging/runtime/config.json` |
| CoreDNS build | `dnsserver.NewServer` is called with one plugin, so there's no `metrics`, `health`, `ready`, `errors` or `log`. The server is TCP-only, behind a hand-written PROXYv2 listener. Snapshot hot-swap uses `p.Replace` (atomic pointer). | `internal/internaldns/coredns/server.go:36-55,70-84`; `transport.go` |
| Early exits | 7 bare `return Rcode…` paths with no counter. The no-upstream error is returned but nothing logs it. | `coredns/plugin.go:111-154` |
| Readiness (#261) | `probeLocalDaemons` passes on any DNS response (`answer.Response`), including REFUSED and SERVFAIL. The dnsdist probe sends `. SOA` from loopback, which `classifyContext` refuses, so the probe can never test the real path. Staging wraps it in `ready.sh` (net1 address and route checks) with `periodSeconds: 2, failureThreshold: 1`. It's an exec that forks a Go binary every 2 s. | `serving/readiness.go:63-99`; `render.go:267`; infra `edge-staging/ready.sh`, `members.yaml:156,355` |
| Readiness owner | node and regional-coredns: the `serving` container. regional-front: the agent container, which holds NATS credentials, so dnsdist itself has no probe. | `members.yaml:355` |
| Liveness / startup | No liveness or startup probe on any container, including the recursor, which only has a `tcpSocket` readiness probe. | `members.yaml`, `public-recursors.yaml` |
| Daemon supervision | `--role=serving` respawns CoreDNS as a child process every 50 ms. Crashes never show in `kube_pod_container_status_restarts_total`. | `serving/daemons.go:58-124` |
| Logging | The agent logs zap JSON (`Development:false`). CoreDNS uses stdlib `log.Printf` and `fmt.Fprintf(stderr)`. dnsdist logs plain text with `--disable-syslog`. | `cmd/internal-dns/main.go:25-31`; `server.go:78`; `cmd/internal-dns-coredns/main.go:48` |
| Bind addresses | CoreDNS binds only the net1 address (`fd70:…`). dnsdist binds `[::]:53`, which covers both net1 and the pod network (`10.244.0.0/16`, `fd20::/48`). Any `[::]` metrics port would be reachable from the producer network. | infra `edge-staging/node.json`, `regional-front.json` |
| UID separation | **Not in staging.** `fleet.example.yaml:108` runs `serving` as 65533 without NATS. Staging `members.yaml` runs every container as pod UID 65532 and mounts the `nats` secret into `serving`, `agent` and both init containers (`:64,94,125,175`). | as cited |
| NetworkPolicy | None for the fleet in either repo. | — |
| Identity | Pod names carry ReplicaSet hashes. dnsdist backend names are `b_<OpaqueToken(memberID)>`, which nobody can read. `memberID` lives only in config JSON and `MEMBER_ID` env. kube-state-metrics doesn't export pod labels. | `render.go:227`; `members.yaml:144` |
| dnsdist pools | One `ready_<token>` pool per distinct eligibility set, never removed (`internalDNSPools`). The count is bounded by 2^backends−1 but stale pools persist until restart. | `render.go:231-256` |

**Plugin chaining decision: keep the Go-built server and wire upstream pieces in. Don't move to a Corefile.**
- The atomic `Replace` swap is the property that makes the fleet fail closed. Caddy's `reload` restarts listeners instead.
- Both upstream pieces we need are usable from Go in CoreDNS v1.14.7:
  - `metrics.New(addr)`, `OnStartup` and `Next` are exported.
  - `dnsserver.Config.ProxyProtoConnPolicy` is a plain field, used by the new `proxyproto` plugin.
- `health` and `ready` have unexported types, so we reimplement their HTTP contracts. They're about 40 lines each.

## 2. Proposed design

### Prefix: `internal_dns_*`
- `metric-prefixes.yaml` says "Only exporter-produced families belong here". It lists `datum_dns_` for custom-resource-state series only.
- Self-emitted series route by `namespace:datum_service:info`. The fleet namespace already carries `meta.datumapis.com/service: dns` (`edge-staging/namespace.yaml`), so alerts route to `dns`/delivery with no prefix entry.
- The control plane already ships `internal_dns_operations_total`.
- `datum_dns_internal_*` would add a second routing path that the file explicitly discourages.

### Common
- Every port binds `$(POD_IP)` from the downward API, never `[::]`, so nothing listens on net1.
- Each pod gets labels `internal-dns.datum.net/member`, `/role` and `/region`. The PodMonitor copies them with `podTargetLabels` and relabels them to `member`, `serving_role` and `dns_region`. Dashboards key on `member`, not `pod`.
- Scrape objects go in infra `apps/dns-operator/internal-dns/edge-staging/podmonitor.yaml` and are added to that kustomization. Edge vmagent `selectAllByDefault` picks them up. Interval 30 s.
- NetworkPolicy `edge-staging/network-policy.yaml`: ingress to metrics ports only from `telemetry-system` (vmagent).

### Components

| Component | Expose (upstream first) | Custom | Port / path | Series per pod |
|---|---|---|---|---|
| CoreDNS plugin | `metrics.New` in front of `ContextPlugin`: `coredns_dns_requests_total`, `_responses_total{rcode}`, `_request_duration_seconds`, `_request/response_size_bytes`. `coredns_forward_*`, which the embedded forwarders already register (`to` = 1–3 upstreams). Go and process collectors (`process_start_time_seconds` exposes the silent respawns). `/health` and `/ready` with upstream semantics. | See below | `POD_IP:9153/metrics`, `:8080/health`, `:8181/ready` | ~250 |
| dnsdist (node, regional) | Built-in webserver: `dnsdist_queries`, `_responses`, `_servfail_responses`, `_rule_refused`, `_rule_servfail`, `_latency_*`, `_frontend_*`, `_server_state/_queries/_latency{server}`. `dnsdist_pool_*` is dropped at scrape (see the cardinality budget). | `declareMetric` with prometheusName `internal_dns_dnsdist_context_queries_total{outcome}` (`unknown_context`, `context_unready`, `pool_unavailable`, `routed`), incremented in `classifyContext` and the SERVFAIL rules | `webserver(POD_IP..":8083")`, `setWebserverConfig{acl="10.244.0.0/16", statsRequireAuthentication=false, password/apiKey=random}` | ~400 |
| Recursor | `pdns_recursor_*`, the same families as `datum-managed-dns-cache` (183 families and ~256 series per pod observed) | none | `--webserver=yes --webserver-address=$(POD_IP) --webserver-port=8083 --webserver-allow-from=10.244.0.0/16`, port name `recursor` (matches prior art) | ~260 |
| Agent | controller-runtime `metricsserver` plus Go collectors | See below | `POD_IP:9090/metrics`, `/healthz`, `/readyz` | ~60 |

**CoreDNS custom metrics:**

| Metric | Type | Labels |
|---|---|---|
| `internal_dns_context_queries_total` | counter | `outcome` ∈ `served_zone`, `forwarded_next`, `forwarded_public`, `refused_unknown_context`, `refused_bad_destination`, `refused_bad_identity`, `refused_unsupported`, `refused_transfer`, `servfail_no_state`, `servfail_context_pending`, `servfail_zone_pending`, `servfail_no_upstream`, `readiness_canary`, `servfail_canary_bad_identity` |
| `internal_dns_context_snapshot_contexts` | gauge | `state` ∈ `ready`, `pending` |
| `internal_dns_context_snapshot_reloads_total` | counter | `result` ∈ `applied`, `rejected` |
| `internal_dns_context_snapshot_applied_timestamp_seconds` | gauge | — |

**Agent metrics (G-3, revised).** Drop the strategy's `role` label because target labels carry it.

| Metric | Type | Labels / buckets |
|---|---|---|
| `internal_dns_agent_serving_fence` | gauge | `state` ∈ `target`, `applied`; `part` ∈ `epoch`, `revision`. Lag is only valid when the epochs match: `Compare` orders by (epoch, revision). |
| `internal_dns_agent_events_total` | counter | `kind` (envelope kind, ≤6), `result` ∈ `applied`, `stale`, `rejected`, `error` |
| `internal_dns_agent_activations_total` | counter | `daemon` ∈ `coredns`, `dnsdist`, `unnamed`, `all`. Validate and reload runs take it from the command's `daemon` field, or `unnamed` when the field is unset. `verify_failed` is not tied to one command and uses `all`; `result` ∈ `success`, `validate_failed`, `reload_failed`, `reload_reported_errors`, `verify_failed`. `reload_reported_errors` counts a reload that exited 0 but wrote errors, so a silent partial reload is visible per daemon. |
| `internal_dns_agent_activation_duration_seconds` | histogram | `daemon`; .05, .1, .25, .5, 1, 2.5, 5, 10, 30 |
| `internal_dns_agent_fail_closed` | gauge | — |
| `internal_dns_agent_checkpoint_timestamp_seconds` | gauge | — |
| `internal_dns_agent_checkpoint_restored` | gauge | 1 while the agent serves state restored from its checkpoint and has not yet confirmed a fence with the control plane, else 0 |
| `internal_dns_agent_fence_confirmed_timestamp_seconds` | gauge | —. Last time the control plane confirmed the serving fence. `time() -` this shows how long a member has served without control-plane contact. |
| `internal_dns_agent_loop_consecutive_failures` | gauge | `loop` ∈ `bootstrap`, `consume_serving`, `consume_records`, `consume_acks`, `step`, the agent's actual loops: the startup bootstrap retry, one consumer per event source, and the reconcile ticker that drives activation; reset to 0 on success |
| `internal_dns_agent_bindings` | gauge | `state` ∈ `active`, `pending`, `unverified` (≤ `maxBindings` = 200, count only) |
| `internal_dns_agent_publications` | gauge | `state` ∈ `verified`, `pending`, `rejected`, `tombstone` |
| `internal_dns_agent_acks_published_total` | counter | `result` ∈ `success`, `error` |
| `internal_dns_agent_transport_connected` | gauge | — |

**Cardinality budget.**
- At most 1,000 series per member pod. The fleet total, about 4 members and 2 recursors, stays under 6,000.
- Forbidden: context, binding, VPC, zone, qname and client labels.
- dnsdist `server` (≤ backends) is allowed.
- dnsdist `pool` is not. Pools number up to 2^backends−1 and are never pruned, so the PodMonitor drops `dnsdist_pool_.*` with `metricRelabelConfigs` from the start. Pool availability comes from `internal_dns_dnsdist_context_queries_total{outcome="pool_unavailable"}` and `dnsdist_server_state`.

## 3. Architecture fixes the dashboards depend on

1. **Canary readiness (fixes #261).**
   - The plugin answers `CH TXT ready.internal-dns.` before it selects a context. NOERROR means the snapshot is loaded; SERVFAIL means `state == nil`.
   - dnsdist forwards that name from loopback to its pool, and returns SERVFAIL when `NotRule(PoolAvailableRule)` matches.
   - `probeLocalDaemons` must require `Rcode == NOERROR` and the TXT answer.
   - Node and regional-front Ready then means "dnsdist plus at least one healthy backend". It no longer means "a socket replied".
2. **HTTP probes instead of exec.**
   - `--role=serving` serves `/readyz`: `Readiness()` plus the canary, cached for 1 s.
   - regional-front moves its readiness from the agent to a dnsdist `httpGet` on the same canary through a small `serving` sidecar, or onto dnsdist's own webserver through a Lua `registerWebHandler`.
   - Set `failureThreshold: 3`.
3. **Liveness, kept separate.**
   - CoreDNS: `/health`, process-local, with a lameduck period.
   - dnsdist: `tcpSocket` on 8083.
   - Agent: `/healthz` checks only that its `step` loop is ticking, never NATS. Activation must keep working offline.
   - All three: `failureThreshold` of 6 or more, plus a `startupProbe`.
4. **Restore UID separation in staging.** `serving` and `dnsdist` run as 65533 without the `nats` mount, matching `fleet.example.yaml`. This must land **before** any metrics port opens in those containers.
5. **Member identity as pod labels.** Use them in dashboards. Keep `memberID` only as a value inside the protocol.
6. **Structured CoreDNS logs.** Use `slog` JSON. Log returned plugin errors with rate limiting (the equivalent of `errors`). No per-query log.
7. **Optional: replace `proxyListener` with upstream proxyproto.** Set `ProxyProtoConnPolicy` to REQUIRE for peers and REJECT otherwise. This removes about 170 lines of bespoke parsing, but needs a spike to confirm that `LocalAddr()` returns the PROXY destination, which `plugin.go:117` relies on.

## 4. Work breakdown

| # | Repo / files | Change | Depends on |
|---|---|---|---|
| S1 | infra `edge-staging/members.yaml` | UID 65533 for `serving`/`dnsdist`; drop `nats` from them; add member/role/region pod labels; add `POD_IP` env | — |
| S2 | dns-operator `coredns/server.go`, `plugin.go`, `cmd/internal-dns-coredns/main.go`, `serving/daemons.go` | `metrics.New` chain, outcome counter, snapshot gauges, `/health` and `/ready`, `--metrics-address` from `POD_IP`, slog | — |
| S3 | dns-operator `coredns/plugin.go`, `serving/readiness.go`, `render.go` (canary rule) | Canary plus NOERROR-only readiness (#261) | S2 |
| S4 | dns-operator `serving/render.go`, `render_test.go` | dnsdist webserver on `POD_IP`, ACL, random key, `declareMetric`. Put it in the init block, because the topology fingerprint asserts there (`render.go:229`). | — |
| S5 | dns-operator `runtime/run.go` (`runAgent`), `serving/agent.go`, new `serving/metrics.go` | Agent metrics server and G-3 metrics (including the per-daemon activation result, checkpoint-restored, fence-confirmed and consecutive-failure gauges), `/healthz`, `/readyz` | — |
| S6 | dns-operator `serving/daemons.go` | `--role=serving` HTTP `/readyz` | S3 |
| S7 | infra `edge-staging/members.yaml`, `public-recursors.yaml` | Container ports, HTTP readiness, liveness and startup probes, recursor webserver flags | S1, S2, S4–S6 images |
| S8 | infra `edge-staging/podmonitor.yaml`, `network-policy.yaml`, `kustomization.yaml` | Scrape objects with `podTargetLabels` and relabel, a `dnsdist_pool_.*` drop; NetworkPolicy | S1, S7 |
| S9 | dns-operator `config/internal-dns/fleet.example.yaml`, `routers.example.yaml`, `docs/architecture/internal-dns/` | Mirror S1/S7/S8 in the examples | S7 |
| S10 | dns-operator `coredns/transport.go` | Optional: upstream proxyproto | spike |

## 5. Open questions

1. Does dnsdist 2.0.10 `incMetric` accept labels, or do we need one counter per outcome?
2. Is the pod network dual-stack on every edge cluster? If so, `POD_IP` (IPv4) still keeps metrics off net1, but it should be confirmed for IPv6-only cells.
3. Should a node with zero contexts be Ready? This proposal says yes, because the canary is independent of contexts.
4. Deferred (README decision 6): should the serving supervisor be removed, with CoreDNS as its own container and an `allow-coredns` gate as a startupProbe? That would make crashes visible to Kubernetes, but it changes the fail-closed gating.
5. The strategy doc names `dnsdist_rule_servfail` per pod. Confirm we want `_rule_*` totals: they're global, not per-rule, so they're safe.

## Corrections to the strategy doc

- **Readiness command:** staging runs `ready.sh`, not bare `--role=readiness`. On regional-front, readiness sits on the agent container.
- **Recursor:** the prior-art port is 8083, named `recursor`, not `:8082`. The recursor runs from the serving image, not `pdns-recursor-54`.
- **dnsdist bind:** `webserver("[::]:8083")` would also listen on net1. Bind `POD_IP` instead.
- **Member identity:** "Per-member identity comes from `pod`" breaks on every rollout because of the ReplicaSet hash. Use pod labels instead.
- **Per-rule counters:** dnsdist doesn't emit per-rule counters on `/metrics`. The real cardinality risk is `dnsdist_pool_*`, which we drop.
- **UID separation:** the strategy (and the brief) assume it exists. It doesn't in staging.
- **Revision lag:** `target − applied` revision ignores epoch.
