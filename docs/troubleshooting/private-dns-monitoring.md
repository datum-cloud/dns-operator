# Monitoring Private DNS

This runbook tells an engineer or an agent how to check each part of a Private
DNS deployment with the telemetry the software exposes today, what healthy and
unhealthy look like, which failure modes we've seen, and which signals are
still missing.

It's written for any deployment. Names in angle brackets, such as
`<fleet-namespace>`, stand for values your deployment chooses. Keep
deployment-specific baselines, cluster names and dashboards with your own
deployment configuration.

Related documents:

- [Architecture](../architecture/internal-dns/README.md) for how the parts fit
  together.
- [Metrics proposal](../internal-dns-metrics-proposal/README.md) (phases 0 to
  5) for the planned signals. Finding numbers such as F2 refer to its "What we
  found" table.

## Contents

- [Before you start](#before-you-start)
- [Components](#components)
  1. [Delivery (Flux)](#1-delivery-flux)
  2. [Control plane](#2-control-plane)
  3. [NATS JetStream broker](#3-nats-jetstream-broker)
  4. [Zone ownership leases](#4-zone-ownership-leases)
  5. [Serving agents](#5-serving-agents)
  6. [Node resolvers](#6-node-resolvers)
  7. [Regional front and regional CoreDNS](#7-regional-front-and-regional-coredns)
  8. [Public recursor](#8-public-recursor)
  9. [Cell controller](#9-cell-controller)
  10. [Compute and network producers](#10-compute-and-network-producers)
  11. [Milo and resource-metrics](#11-milo-and-resource-metrics)
- [Gaps, in priority order](#gaps-in-priority-order)

## Before you start

All checks here are read-only.

You need:

- Access to the cluster that runs the control plane and broker (namespace
  `internal-dns-system` by default) and to each cluster that runs a serving
  fleet (`<fleet-namespace>`).
- A Prometheus-compatible query endpoint that scrapes the control plane, the
  broker exporter and dnsdist.

Label conventions used below:

- Control-plane loop series carry `loop` and `result`, plus `region` and
  `shard` where the loop has them (`internal_dns_loop_duration_seconds` has no
  `shard`).
- dnsdist series carry `internal_dns_datum_net_tier` (from the pod label
  `internal-dns.datum.net/tier`) with values `node`, `node-b` and
  `regional-dnsdist`.
- `result` on loop series is `success`, `conflict` or `error`. Both control-plane
  replicas run the outbox and ack-refresh loops (F8), so `conflict` there is
  normal.

Record your own baseline the first time you run these checks; most numbers
here are only meaningful compared with your deployment's normal.

## Components

Each section answers the same questions: what the part does, how to tell it is
healthy now, what to watch, what bad looks like, and what is missing.

### 1. Delivery (Flux)

**What it does.** The reference deployment applies everything with Flux: the
control plane, the broker HelmRelease, the APIs, admission and runtime on the
control-plane cluster, and the fleet on each serving cluster. Producers
(network services, Compute, resource-metrics) ship separately.

**Healthy now.**

```promql
gotk_resource_info{name=~"internal-dns.*"}
```

Every row should be `ready="True"` with `suspended` not `true`. Run `kubectl get
kustomization -A` against both the control-plane cluster and each fleet
cluster; the Kustomizations should report the same source revision.

**Bad looks like.** A Kustomization `Ready=False` with a health-check failure on
a fleet Deployment, or control-plane and fleet copies on different revisions for
more than one reconcile interval.

**Known failure modes.** A new agent setting applied before the serving image
that understands it reaches the fleet makes older agents reject their config.
Flux reports Ready because the Deployment rolled; the agent then fails closed.

**Gaps.** Nothing ties a deployed revision to the image digest each member
runs, so skew is only visible by reading both.

### 2. Control plane

**What it does.** `internal-dns-control-plane` (normally two replicas, both
active) runs one controller per project plus these loops per region and shard:
`project discovery`, `planner`, `outbox`, `ack read`, `ack apply`, `ack commit`
and `ack refresh`. It plans each project's DNS config, publishes it through the
outbox to NATS, and folds members' acknowledgements back into
DNSResolverBinding status in the DNS service project.

**Healthy now.**

```promql
max by (loop) (internal_dns_loop_consecutive_failures)                      # 0 for every loop
min by (loop) (time() - internal_dns_loop_last_success_timestamp_seconds)   # freshest replica
sum by (loop, result) (increase(internal_dns_loop_runs_total[1h]))          # errors near 0
histogram_quantile(0.99, sum by (loop, le) (rate(internal_dns_loop_duration_seconds_bucket[1h])))
```

Per-project reconciles:

```promql
sum by (result) (increase(controller_runtime_reconcile_total{namespace="internal-dns-system"}[1h]))
topk(3, histogram_quantile(0.99, sum by (controller, le) (rate(controller_runtime_reconcile_time_seconds_bucket{namespace="internal-dns-system"}[1h]))))
```

Logs are JSON; filter on `"level":"error"`. Read every replica in full:

```bash
kubectl -n internal-dns-system logs -l app.kubernetes.io/name=internal-dns-control-plane --since=1h --tail=-1 --prefix
```

**Watch.**

- Consecutive failures. The proposal's `>= 5 for 5m` misses short, sharp
  disruptions: a platform API restart can push `ack apply` past 100
  consecutive failures and recover within five minutes. Pair it with
  `max_over_time(internal_dns_loop_consecutive_failures[5m]) >= 20` and no
  `for`.
- Staleness, using `min by (loop)` so one slow replica doesn't alarm. The
  planner runs every lease/3, capped at 10s, and `ack refresh` every 2s, but
  scrape lag puts a floor of about 20s under every loop. Set thresholds from
  your own baseline, for example planner above 60s and other loops above 60s.
  The planner's consecutive-failure count also climbs every 10s during an
  API outage, so a failure-count alert fires sooner for it than before.

**Bad looks like.** When the platform API server is unavailable or slow, all
loops start failing together: `ack apply` logs `committed serving snapshot
fence mismatch`, `project discovery` and per-project reconciles fail on `Get`
timeouts, and loop staleness climbs. The loop metrics record this clearly, but
only an alert shaped for short bursts (above) catches it.

**Known failure modes.**

- Ack head-of-line block: acks from members the control plane doesn't know
  used to block the ack consumer. Fixed by #291 and #293 (drop them) and #305
  (drain the backlog in batches).
- Platform API pressure turns into loop errors and fence mismatches (above).

**Gaps.**

- No counter for acks dropped from unknown members, so the head-of-line fix
  can't show it is working.
- No publication-lag or outbox-age metric
  (`internal_dns_publication_lag_seconds`,
  `internal_dns_outbox_oldest_pending_age_seconds` in the proposal).
- One controller name per project (F7), so the `controller` label grows with
  projects.
- The legacy `internal_dns_operations_total` and
  `internal_dns_operation_last_success_timestamp_seconds` families are still
  emitted next to `internal_dns_loop_*`; dashboards should move to the loop
  families.
- The control plane serves a `/healthz` loop-liveness check, but the
  reference Deployment has no liveness probe. Its readiness probe doesn't
  prove it can reach the platform API or NATS.

### 3. NATS JetStream broker

**What it does.** A three-replica JetStream cluster carries the `DNS_PRIVATE`
stream (config records, serving snapshots, acks) and the
`DNS_PRIVATE_SNAPSHOTS` key-value bucket (stream `KV_DNS_PRIVATE_SNAPSHOTS`).
The standard `prometheus-nats-exporter` sidecar exposes `nats_*`.

**Healthy now.**

```promql
max by (stream_name, consumer_name) (nats_consumer_num_pending{namespace="internal-dns-system",consumer_name!~".*-acks"})  # 0
max by (stream_name, consumer_name) (avg_over_time(nats_consumer_num_pending{namespace="internal-dns-system",consumer_name=~".*-acks"}[1h]))
max by (stream_name, consumer_name) (nats_consumer_num_ack_pending{namespace="internal-dns-system"})
max by (stream_name) (nats_stream_total_bytes{namespace="internal-dns-system"})
  / max by (stream_name) (nats_stream_limit_bytes{namespace="internal-dns-system"} > 0)
sum (nats_connz_num_connections{namespace="internal-dns-system"})       # members + control-plane replicas
```

**Watch.** Alert on failure to drain, `min_over_time(nats_consumer_num_pending[2m]) > 0`,
not on a fixed pending threshold.

**Bad looks like.** A consumer whose pending count never returns to 0, or a
connection count below members plus control-plane replicas. Every `-acks`
consumer, members' and the control plane's, saw-tooths by design: instant
values above 100 are normal, so judge them by whether they drain, not by a
single reading.

**Known failure modes.** ACK heartbeats are most of the traffic and grow with
bindings times members (F6). This is a scaling limit, not a fault.

**Gaps.** No per-member connection signal: a member that drops off the broker
only shows up as a lower total. No stream-near-limit alert ships with the
deployment.

### 4. Zone ownership leases

**What it does.** Each private zone has a `DNSPublicationOwnership` in the DNS
service project with `holderIdentity`, `writerEpoch` and `leaseUntil`. The
replica holding the lease publishes the zone; the other waits until the lease
could expire (`internal/internaldns/controlplane/publication_reconciler.go`).

**Healthy now.** There is no metric. Use per-project reconcile time as a proxy
(section 2). Today a pass renews a zone's lease only when it reaches that zone
and less than half a lease remains, then requeues half a lease after it ends,
so a lease can lapse on passes well under the lease duration. After #316,
leases hold while passes stay under about two-thirds of a lease. The reconcile
histogram's top bucket is 60s, so the proxy saturates before a pass gets that
long; treat any project at the top bucket as at risk.

Ownership conflicts show in the logs as `Operation cannot be fulfilled on
dnspublicationownerships`.

**Bad looks like.** Two replicas take a zone from each other on every pass:
`writerEpoch` keeps rising, passes conflict on
`dnspublicationownerships`, and each takeover republishes the zone, which makes
the next pass slower.

**Known failure modes.** Lease expiry mid-pass: a long pass renewed each lease
only when it reached that zone. #311 wakes standbys at expiry; #316 renews every
lease before the slow work.

**Gaps.** No metric for lease takeovers, writer epoch, or time left on the
oldest lease.

### 5. Serving agents

**What it does.** The `agent` container in every member pod consumes three
durables, `<member>-records`, `<member>-serving` and `<member>-acks`, writes
daemon config, reloads CoreDNS or dnsdist, and sends acks.

**Healthy now.**

```promql
max by (consumer_name) (nats_consumer_num_pending{namespace="internal-dns-system",consumer_name=~".*-(records|serving)"})   # 0
max by (consumer_name) (increase(nats_consumer_num_redelivered{namespace="internal-dns-system"}[1h]))                 # 0
```

Each reload logs a pair of lines:

```bash
kubectl -n <fleet-namespace> logs deploy/<member-deployment> -c agent --since=1h | grep -c 'daemon update applied'
```

**Bad looks like.** A member's `-serving` or `-records` consumer stuck above 0,
redeliveries climbing, or an agent that stops logging reloads while the control
plane keeps publishing.

**Known failure modes.** A reload that reports errors but exits 0, and a member
serving a restored checkpoint without reaching the control plane. Both are in
the proposal; neither has a signal yet.

**Gaps.** No agent metrics (`internal_dns_agent_*`, phase 2): fail-closed state,
fence, activations by result, checkpoint restore, transport connection. No
liveness or startup probe on any container (F3); the `agent` container has a
readiness probe only on the regional front.
The reference fleet still mounts broker credentials in `serving` (F1), which
must change before CoreDNS metrics are turned on.

### 6. Node resolvers

**What it does.** Node members (`internal-dns-node` on `fd53::53`, and a
second instance on `fd53::54` where deployed) run dnsdist in front of a local
CoreDNS. dnsdist classifies each query to a context; CoreDNS answers private
names and forwards the rest to the public recursor.

**Healthy now.** dnsdist metrics are off by default; turn them on with the
`metricsAddress` and `metricsACLs` render settings (#315).

```promql
sum by (internal_dns_datum_net_tier) (rate(dnsdist_queries[1h]))
sum by (internal_dns_datum_net_tier) (increase(dnsdist_servfail_responses[1h]))
sum by (internal_dns_datum_net_tier) (increase(dnsdist_downstream_timeouts[1h]))          # 0
sum by (internal_dns_datum_net_tier) (increase(dnsdist_server_healthcheckfailures[1h]))   # 0
sum by (internal_dns_datum_net_tier) (changes(dnsdist_server_status[1h]))                 # 0
sum by (internal_dns_datum_net_tier) (rate(dnsdist_latency_sum[1h]))
  / sum by (internal_dns_datum_net_tier) (rate(dnsdist_latency_count[1h]))                # mean, ms
```

`dnsdist_latency_bucket` is in milliseconds with buckets at 1, 10, 50, 100 and
1000, so quantiles jump between bucket edges. Prefer the mean and the share
of answers under 1 ms on low-volume members.

**Bad looks like.** A backend with `dnsdist_server_status` 0, timeouts above 0,
or SERVFAIL rising while queries stay flat.

**Known failure modes.**

- Backend flap: dnsdist marks the local CoreDNS backend down and back up, and
  while it is down the node pool has no server. Watch
  `changes(dnsdist_server_status[1h])` and `dnsdist_server_healthcheckfailures`.
- The readiness check queries `. SOA`, which the classifier always refuses
  (F2), so readiness never tests the real path. A steady refusal rate of about
  one a second per dnsdist, on idle members too, is probably this and other
  health checks (inferred, not traced); real refusals hide under it.

**Also check placement.** If every member in a region lands on one node, one
node failure takes out the region's Private DNS. Nothing alerts on this:

```bash
kubectl -n <fleet-namespace> get pods -o wide
```

**Gaps.** CoreDNS metrics (#300) are opt-in and wait on F1. Until then there is
no per-outcome counter, so a query refused for an unknown context looks the
same as the probe. Readiness is an `exec` every 2s with `failureThreshold: 1`,
so one slow run flips the pod.

### 7. Regional front and regional CoreDNS

**What it does.** `internal-dns-regional-front` runs dnsdist for the region and
balances across the regional CoreDNS members, which hold the regional records.
The front builds one pool per readiness set (`ready_<hash>`) and a
`regional_shared` pool.

**Healthy now.**

```promql
dnsdist_server_status{internal_dns_datum_net_tier="regional-dnsdist"}           # every backend 1
dnsdist_pool_active_servers{internal_dns_datum_net_tier="regional-dnsdist"}
```

Readiness failures only show up as Events:

```bash
kubectl -n <fleet-namespace> get events --field-selector reason=Unhealthy
```

**Bad looks like.** Every regional backend down on the front, or a `ready_<hash>`
pool at 0 active servers while queries for it keep arriving.

**Known failure modes.** Readiness blips during a snapshot swap, with the
message `no approved configuration is active`. They line up with config reloads
and last less than a scrape interval, so `kube_pod_status_ready` never shows
them.

**Gaps.** No metric for reloads or snapshot applies (the CoreDNS
`context_snapshot_*` families arrive with #300), so reload churn is only
visible by counting agent log lines. `dnsdist_pool_*` series grow with
readiness sets; the proposal drops them at scrape.

### 8. Public recursor

**What it does.** `internal-dns-public-recursor` (PowerDNS Recursor) resolves
public names for CoreDNS.

**Healthy now.** The recursor logs a statistics report every 15 minutes:

```bash
kubectl -n <fleet-namespace> logs -l app.kubernetes.io/name=internal-dns-public-recursor --since=1h --tail=-1 --prefix | grep 'Periodic statistics'
```

Check `outgoing-timeouts`, `throttled-queries-perc` and `concurrent-queries`.

**Bad looks like.** Rising `outgoing-timeouts`, or `concurrent-queries` pinned.

**Gaps.** No metrics: the webserver is off and nothing scrapes it
(`pdns_recursor_*` per the proposal). Readiness is a TCP connect only.

### 9. Cell controller

**What it does.** Network services' cell controller runs `internaldns-cell`,
which places the fleet and programs each member's interface, and the
`networkinterfaceclaim` and `galactic-networkinterface` controllers that attach
members and guests.

**Healthy now.**

```promql
sum by (controller, result) (increase(controller_runtime_reconcile_total{controller=~"internaldns-cell|networkinterfaceclaim|galactic-networkinterface"}[1h]))
sum by (name) (workqueue_depth{name=~"internaldns-cell|networkinterfaceclaim"}) > 0
```

Errors that are optimistic-lock conflicts (`Operation cannot be fulfilled`) are
noise; anything else is not.

**Bad looks like.** Non-conflict errors, a work queue that doesn't drain, or
claims for Private DNS members that stay unbound.

**Gaps.** `internaldns-cell` never logs and emits no Events (F9); it polls, so
`requeue_after` counts dwarf real work. Conflicts are counted as errors, which
makes the error ratio useless as an incident signal.

### 10. Compute and network producers

**What it does.** Network services sets the `DNSReady` condition on each
NetworkContext. Compute's `internal-dns-instance-publisher` (feature gate
`InternalDNSPublishing`) publishes instance names.

**Healthy now.**

```promql
datum_cloud_network_context_status_condition{condition="DNSReady"} == 1
sum by (controller, result) (increase(controller_runtime_reconcile_total{controller=~"internal-dns-instance-publisher|internaldns-.*"}[1h]))
```

resource-metrics exports one series per context with the condition's current
`status` and `reason` as labels, and a value of 1 only when the status is
`True`. So `== 1` hides contexts that aren't ready. To see them:

```promql
count by (status, reason) (datum_cloud_network_context_status_condition{condition="DNSReady"} == 0)
```

The `reason` says why: `Pending` (waiting on publication), `NetworkPathNotReady`
(no serving path yet, as in a region with no fleet), or `Terminating`.

**Bad looks like.** A context with `DNSReady` False, or instances created but
never published while the publisher's error count climbs. A context stuck at
`Unknown`/`NetworkPathNotReady` in a region that has a fleet is worth a look.

**Gaps.** No instance-level DNS condition metric
(`datum_cloud_compute_instance_dns_status_condition`), no publisher
`publications_total` or latency, and no age on a non-ready `DNSReady`, so a
context that is slow can't be told from one that is stuck. Producers return
errors together with `RequeueAfter` (F9).

### 11. Milo and resource-metrics

**What it does.** Milo's API server stores every Private DNS object; the
control plane talks to it constantly. resource-metrics turns object conditions
into the `datum_cloud_*` series used above.

**Healthy now.** Check API server memory against its limit, its restarts, and
its etcd client errors next to the control plane's loop errors.

**Bad looks like.** Control-plane loop errors, `ack apply` fence mismatches and
resource-metrics gaps all at the same time point to the platform API, not to
Private DNS.

**Gaps.** No `datum_dns_internal_*` families yet (phase 3), so
DNSResolverBinding, DNSRegistration and DNSResolverContext state, including ack
freshness, can't be read from metrics.

## Gaps, in priority order

These are gaps in what the software and its reference deployment expose.

| # | Gap | Why it matters | Fix |
|---|---|---|---|
| 1 | No alert rules ship for any Private DNS signal | Control-plane disruptions are recorded by the loop metrics but reported by nothing; short ones also slip past the proposal's `for 5m` shapes | Burst-aware rules on `internal_dns_loop_consecutive_failures`, loop staleness, `dnsdist_server_status`, SERVFAIL and timeout rates, consumer drain |
| 2 | No synthetic probe of a private name through `fd53::53` (F12) | Wrong answers for private names are invisible | Phase 4: canary project, `_canary` record, probe |
| 3 | Readiness queries `. SOA`, which is always refused (F2), and flips on one failure | Ready says nothing about answering; refusal counts are polluted; regional members blip on reloads | Phase 2: NOERROR canary readiness, HTTP probes, separate liveness |
| 4 | CoreDNS metrics wait on removing broker credentials from `serving` (F1) | No error, latency or outcome signal behind dnsdist, and no snapshot state | UID split, then set the #300 metrics address |
| 5 | No agent metrics (`internal_dns_agent_*`) | Fail-closed, fence, reload errors and checkpoint restore are only in logs | Phase 2 |
| 6 | No ownership lease signal | Lease takeovers republish zones and slow every pass | #316, then a takeover counter and lease-age gauge |
| 7 | No reload counter; reload churn and readiness blips only show in logs and Events | Churn is invisible to dashboards | Reload and snapshot-apply counters by daemon |
| 8 | Public recursor exposes no metrics | Public resolution failures only show in log stats | Recursor webserver and scrape config |
| 9 | No resource-state families (`datum_dns_internal_*`) | Binding, registration and context state, and ack freshness, need API access to read | Phase 3, three kinds first |
| 10 | No instance DNS condition or publisher counters | Per-instance publication and its latency can't be seen | Compute, phase 3 |
| 11 | Producers and the cell controller poll and count conflicts as errors (F9) | Error ratios can't be used as incident signals | Producer hygiene |
| 12 | Cardinality: one `controller` value per project (F7) and `dnsdist_pool_*` series | Series grow with tenants | Stable controller names (phase 1); drop pool series at scrape |
| 13 | No anti-affinity or placement signal for serving members | Members can all land on one node, a single point of failure for the region | Spread constraints in the reference fleet; a placement check |
| 14 | No counter for acks dropped from unknown members | The head-of-line fix can't show it is working | Control plane |
| 15 | Legacy `internal_dns_operations_total` still emitted | Two families for one thing | Remove once dashboards use `internal_dns_loop_*` |


## Opt-in CoreDNS profiling

CoreDNS can serve CPU and sampled memory profiles on a separate loopback-only
listener. It is disabled by default and does not change Go's normal memory
sampling rate or enable background CPU, mutex, block, or trace profiling.
Set `agent.render.nodeCoreDNS.profileAddress` or
`agent.render.clusterCoreDNS.profileAddress` in the member runtime configuration
to `127.0.0.1:6060`. The serving helper passes it to CoreDNS as
`-profile-address`; a standalone process accepts the same flag. Apply through
the normal reviewed member rollout and restart the daemon supervisor to pick
up this process argument. It is not a hot-reloaded context snapshot setting.

Only literal loopback addresses and nonzero ports are accepted. The listener
is separate from the metrics address, has no Service or public bind, and is
reachable by containers sharing the pod network namespace. Leave it disabled
outside a coordinated diagnostic window. A bind failure is logged and DNS
continues without profiling; an invalid configured address is rejected.

Use an HTTP client in an existing pod container against `127.0.0.1:6060`, or a
pod-scoped tunnel from your workstation:

```sh
kubectl --context "$context" -n "$namespace" port-forward "pod/$pod" 16060:6060
```

In another terminal, collect profiles during a stable load hold:

```sh
curl --fail --max-time 35 http://127.0.0.1:16060/debug/pprof/allocs -o allocs-before.pb.gz
curl --fail --max-time 35 'http://127.0.0.1:16060/debug/pprof/profile?seconds=10' -o cpu.pb.gz
curl --fail --max-time 35 http://127.0.0.1:16060/debug/pprof/allocs -o allocs-after.pb.gz
curl --fail --max-time 35 http://127.0.0.1:16060/debug/pprof/heap -o heap.pb.gz
go tool pprof -top cpu.pb.gz
go tool pprof -top -sample_index=alloc_space -base=allocs-before.pb.gz allocs-after.pb.gz
go tool pprof -top -sample_index=alloc_objects -base=allocs-before.pb.gz allocs-after.pb.gz
```

CPU captures default to 10 seconds and accept only an integer `seconds` from
1 through 30. Disconnecting cancels the capture and releases the CPU profiler.
Heap and allocation captures are instantaneous sampled profiles, with no
parameters; forced GC and text/debug dumps are rejected. Heap statistics can
lag recent allocations until a GC cycle. Compare allocation snapshots from the
same process, and record process start time, image, offered load, CPU quota,
GOMAXPROCS, socket drops, and effective receive buffer. Profiling itself adds
work: use a separate profiled replicate and compare an unprofiled hold before
using results to explain capacity.

Only these three GET routes are exposed. Concurrent captures return 429,
invalid parameters return 400, and unsupported paths return 404. HTTP reads are
limited to five seconds and writes to 35 seconds; CPU sampling stops at its
requested duration even if the client stops reading. No goroutine, command
line, mutex, block, or trace endpoint is mounted. Close the tunnel and remove
the opt-in setting through the normal rollout when collection is complete.
