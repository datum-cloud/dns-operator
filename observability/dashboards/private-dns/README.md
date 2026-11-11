# Private DNS dashboards

Grafana dashboards for Private DNS, written in jsonnet with grafonnet and
rendered into `config/components/dashboards`, which ships in the kustomize
bundle.

| Dashboard | Source | What it answers |
| --- | --- | --- |
| Private DNS (`internal-dns`) | `overview.jsonnet` | Is Private DNS answering, and is configuration reaching it? |
| Private DNS Performance (`internal-dns-performance`) | `performance.jsonnet` | What did a load test run measure, and what did the system do while it ran? |
| Private DNS Run Comparison (`internal-dns-performance-compare`) | `compare.jsonnet` | How do two load test runs differ? |

Shared selectors and panel builders are in `lib.libsonnet`.

## Building

Needs jsonnet v0.20.0 and jsonnet-bundler v0.6.0.

```bash
task -d observability/dashboards/private-dns install build
```

Never edit the rendered JSON by hand. CI renders the dashboards again and
fails when the committed JSON differs.

## Installing

Include `config/components/dashboards` from the bundle with a namespace, and
patch the GrafanaDashboards' `instanceSelector` to match your Grafana
instance. Every dashboard has a `datasource` variable for the data source that holds the
metrics. The queries use MetricsQL functions such as `label_set` and
`tfirst_over_time`, so the data source must be VictoriaMetrics.

## Load test series

The performance dashboards read the `idns_perf_*` series that the load test
harness writes, each labelled with `run`, `scenario` and `commit`, and
`idns_perf_run`, which is 1 every 10 seconds between the first and last observed
query sends, including both endpoints. The query reporter writes 0 one millisecond
after the final send to stop query lookback from extending the annotation.
Annotations and comparison windows explicitly select value 1. Control-plane
series carry no run label; the comparison dashboard cuts them to a run's
window with the `during()` helper in `lib.libsonnet`.

Query reporters can publish `idns_perf_query_offered_qps{run,scenario,commit,kind}`
once per second alongside the actual sent rate.
`idns_perf_query_dropped_iterations{run,scenario,commit}` is one sample containing
the total generator iterations dropped, emitted at the end of the run. Missing
series remain blank for older reports; they must not be interpreted as zero.

## Control-plane phase timings

`internal_dns_project_pass_seconds` measures a whole project pass, including
failed attempts, but excludes queue/slot waits and identity verification before
the pass starts. Independent zone keys use `internal_dns_zone_reconcile_seconds`
instead. The controller-runtime histogram remains a compatibility fallback for
older versions; its population includes other reconcile keys and is not an
exact substitute for a project-pass duration.

`internal_dns_project_pass_phase_seconds{phase}` records elapsed time, including
API waits, for each reached phase or zone step. The labels are a fixed set with
no project or zone identity. Failed attempts are included; steps skipped by
caching, unchanged content or early errors do not add observations.

The legacy `compile` phase covers the project pass's zone loop, including
publication status updates in whole-project mode. With per-zone reconciliation,
it only dispatches zone keys. The `zone ...` steps describe compilation in both
modes and must not be added to project-pass phases: they overlap in whole-project
mode and run independently in per-zone mode. `zone persist` starts only when a
new publication is attempted, including revision reservation and ownership
updates. Its average excludes unchanged publications. Per-phase averages have
different populations and need not sum to the whole-pass average.

## Reading UDP latency

The resolver percentile charts estimate p50 and p99 from dnsdist's coarse
UDP frontend histogram (1, 10, 50, 100, 1000 ms and +Inf). TCP frontend answers
are excluded, even when they were forwarded to a backend over UDP. UDP error
responses and readiness/background queries are included; the histogram has no
response-code dimension. The response-code panels show a separate population
and cannot turn this histogram into success-only latency.

A sub-millisecond estimate is interpolation within the first bucket. A p99 at
1000 ms can mean the quantile fell in +Inf, not that an answer took exactly one
second. Low volume also matters: 150 UDP answers with 148 at or below 100 ms and
two in the 100–1000 ms bucket yield an estimated p99 of 325 ms.

The answer time heatmaps show the measured buckets instead, one tile per
cluster: node resolvers (node and node-b together) and regional routers
separately, since a query can pass through both. Each cell is the share of
one interval's answers that landed in one bucket, so those two tail answers
appear as 1.3% of that interval rather than as a quantile. Every tile uses the
same colour scale, so clusters with different traffic compare directly. Shares
of 5% or more render at full colour, so the busy bands are solid and the tail
shows as paler cells; a band holding at least 1 in 2,500 answers is shaded.
Grafana bins values linearly before shading them, which is why the scale stops
at 5% rather than 100%. Hover a cell for its exact share. Columns are back-to-back `increase()` windows, which count
each answer once only because VictoriaMetrics does not extrapolate them.

Query fixtures import the same expressions as the dashboards:

```bash
task -d observability/dashboards/private-dns test
```

They cover low-volume tails, overflow-bucket caps, sub-millisecond estimates,
which tiers each heatmap adds together, counter resets and missing telemetry.
The test needs jsonnet and promtool; CI runs it with the alert-rule checks.
See [dnsdist latency statistics](https://www.dnsdist.org/statistics.html#latency-bucket)
for the measured protocol and histogram population.
