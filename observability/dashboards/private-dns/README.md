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
instance. Every dashboard has a `datasource` variable for the Prometheus or
VictoriaMetrics data source that holds the metrics.

## Load test series

The performance dashboards read the `idns_perf_*` series that the load test
harness writes, each labelled with `run`, `scenario` and `commit`, and
`idns_perf_run`, which is 1 every 10 seconds while a run lasts. Control-plane
series carry no run label; the comparison dashboard cuts them to a run's
window with the `during()` helper in `lib.libsonnet`.
