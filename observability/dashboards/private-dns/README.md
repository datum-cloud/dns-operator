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
`idns_perf_run`, which is 1 every 10 seconds while a run lasts. Control-plane
series carry no run label; the comparison dashboard cuts them to a run's
window with the `during()` helper in `lib.libsonnet`.

Query reporters can publish `idns_perf_query_offered_qps{run,scenario,commit,kind}`
once per second alongside the actual sent rate.
`idns_perf_query_dropped_iterations{run,scenario,commit}` is one sample containing
the total generator iterations dropped, emitted at the end of the run. Missing
series remain blank for older reports; they must not be interpreted as zero.

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
two in the 100–1000 ms bucket yield an estimated p99 of 325 ms. The companion
threshold-share and count panels show those two tail answers and their volume.
They use the histogram's own count, retain cluster and tier, and leave shares
empty when no UDP answers were observed. Counts use the same rolling rate
interval and may be extrapolated between scrapes. Over 1 s is included within
over 100 ms; the threshold shares do not sum to one.

Query fixtures import the same expressions as the dashboards:

```bash
task -d observability/dashboards/private-dns test
```

They cover low-volume tails, overflow-bucket caps, sub-millisecond estimates,
cluster/tier isolation, counter resets, empty windows and missing telemetry.
The test needs jsonnet and promtool; CI runs it with the alert-rule checks.
See [dnsdist latency statistics](https://www.dnsdist.org/statistics.html#latency-bucket)
for the measured protocol and histogram population.
