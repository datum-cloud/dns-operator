// Private DNS run comparison dashboard
//
// Puts two load test runs side by side: what each measured, and what the
// control plane and serving fleet did while each ran. Runs happen at
// different times, so every number here is a total or an average over one
// run's window rather than a line over time. The dashboard time range only
// has to include both runs.
//
// Control-plane series carry no run label. They are cut to a run's window by
// lib.during(), which keeps a sample only while that run's idns_perf_run
// series is present.
//
// Usage:
//   jsonnet -J vendor compare.jsonnet > compare.json
// Or:
//   task build

local lib = import 'lib.libsonnet';
local g = lib.g;

local table = g.panel.table;
local barChart = g.panel.barChart;
local var = g.dashboard.variable;
local prometheus = g.query.prometheus;

local ds = lib.ds;
local qTable = lib.qTable;
local grid = lib.grid;
local section = lib.section;

local controlPlane = lib.controlPlane;
local controllers = lib.controllers;
local fleetPods = lib.fleetPods;

// Subquery step for control-plane series cut to a run window. It matches the
// 30s increase windows so each sample is counted once.
local step = '30s';

local overRun(run, inner) =
  'sum_over_time((%s)[$__range:%s])' % [lib.during(inner, run, step), step];

local avgOverRun(run, inner) =
  'avg_over_time((%s)[$__range:%s])' % [lib.during(inner, run, step), step];

// ---------------------------------------------------------------------------
// Measures
// ---------------------------------------------------------------------------

// Each measure turns a run name into a query that returns one series.
local lag(q, s) = function(run)
  'max(quantile_over_time(%s, idns_perf_publish_lag_ms{run="%s",step="%s"}[$__range]))' % [q, run, s];

local missed(s) = function(run)
  'sum(count_over_time(idns_perf_publish_missed{run="%s",step="%s"}[$__range])) or on () (max(max_over_time(idns_perf_run{run="%s"}[$__range])) * 0)' % [run, s, run];

local passQuantile(q) = function(run)
  lib.passQuantile(q, function(m) 'sum by (le) (%s)' % overRun(run, 'sum by (le) (increase(%s[%s]))' % [m, step]));

local phaseAvg(phase) = function(run)
  'sum(%s) / sum(%s)' % [
    overRun(run, 'sum(increase(internal_dns_project_pass_phase_seconds_sum{%s,phase="%s"}[%s]))' % [controlPlane, phase, step]),
    overRun(run, 'sum(increase(internal_dns_project_pass_phase_seconds_count{%s,phase="%s"}[%s]))' % [controlPlane, phase, step]),
  ];

local rateDuring(expr) = function(run)
  avgOverRun(run, expr);

local measures = [
  { name: 'publish create p50 (ms)', expr: lag('0.5', 'create') },
  { name: 'publish create p90 (ms)', expr: lag('0.9', 'create') },
  { name: 'publish create p99 (ms)', expr: lag('0.99', 'create') },
  { name: 'publish update p50 (ms)', expr: lag('0.5', 'update') },
  { name: 'publish update p90 (ms)', expr: lag('0.9', 'update') },
  { name: 'publish update p99 (ms)', expr: lag('0.99', 'update') },
  { name: 'publish delete p50 (ms)', expr: lag('0.5', 'delete') },
  { name: 'publish delete p90 (ms)', expr: lag('0.9', 'delete') },
  { name: 'publish delete p99 (ms)', expr: lag('0.99', 'delete') },
  { name: 'changes never answered, create', expr: missed('create') },
  { name: 'changes never answered, update', expr: missed('update') },
  { name: 'changes never answered, delete', expr: missed('delete') },
  { name: 'API write p99, worst step (ms)', expr: function(run) 'max(quantile_over_time(0.99, idns_perf_publish_api_ms{run="%s"}[$__range]))' % run },
  { name: 'query rate (per second)', expr: function(run) 'sum(avg_over_time(idns_perf_query_qps{run="%s"}[$__range]))' % run },
  { name: 'query worst p99 (ms)', expr: function(run) 'max(max_over_time(idns_perf_query_p99_ms{run="%s"}[$__range]))' % run },
  { name: 'queries failed', expr: function(run) 'sum(sum_over_time(idns_perf_query_failed{run="%s"}[$__range]))' % run },
  { name: 'project pass p50 (s)', expr: passQuantile('0.5') },
  { name: 'project pass p90 (s)', expr: passQuantile('0.9') },
  { name: 'project pass p99 (s)', expr: passQuantile('0.99') },
  { name: 'compile phase, average (s)', expr: phaseAvg('compile') },
  { name: 'zone confirm step, average (s)', expr: phaseAvg('zone confirm') },
  { name: 'zone persist step, average (s)', expr: phaseAvg('zone persist') },
  { name: 'project queue depth, average', expr: rateDuring('sum(workqueue_depth{%s})' % lib.projectQueues) },
  { name: 'API requests (per second)', expr: rateDuring('sum(rate(rest_client_requests_total{%s}[%s]))' % [controlPlane, step]) },
  { name: 'API conflicts, 409 (per second)', expr: rateDuring('sum(rate(rest_client_requests_total{%s,code="409"}[%s]))' % [controlPlane, step]) },
  { name: 'API throttled, 429 (per second)', expr: rateDuring('sum(rate(rest_client_requests_total{%s,code="429"}[%s]))' % [controlPlane, step]) },
  { name: 'client rate limit wait (s per s)', expr: rateDuring('sum(rate(internal_dns_kube_client_throttle_seconds_sum{%s}[%s]))' % [controlPlane, step]) },
  { name: 'control plane CPU (cores)', expr: rateDuring('sum(max by (cluster, namespace, pod, container) (rate(container_cpu_usage_seconds_total{%s,pod=~"internal-dns-control-plane-.*",container!="",container!="POD"}[%s])))' % [controlPlane, step]) },
  { name: 'broker CPU (cores)', expr: rateDuring('sum(max by (cluster, namespace, pod, container) (rate(container_cpu_usage_seconds_total{%s,pod=~"internal-dns-nats-.*",container!="",container!="POD"}[%s])))' % [controlPlane, step]) },
  { name: 'serving fleet CPU (cores)', expr: rateDuring('sum(max by (cluster, namespace, pod, container) (rate(container_cpu_usage_seconds_total{%s,container!="",container!="POD"}[%s])))' % [fleetPods, step]) },
  { name: 'serving fleet agent CPU (cores)', expr: rateDuring('sum(max by (cluster, namespace, pod, container) (rate(container_cpu_usage_seconds_total{%s,container="agent"}[%s])))' % [fleetPods, step]) },
];

// One query per side. Every measure is labelled with its name and position,
// so the two sides join on the name and the table keeps this order.
local side(runVar) =
  std.join(' or ', [
    'label_set(%s, "measure", "%s", "order", "%02d")' % [measures[i].expr(runVar), measures[i].name, i]
    for i in std.range(0, std.length(measures) - 1)
  ]);

local comparisonTable =
  table.new('Run A against run B')
  + table.panelOptions.withGridPos(33, 12, 0, 0)
  + table.panelOptions.withDescription('Each measure over the whole of run A and run B, and how B differs from A. Publish times are the worse of the two resolvers. Control-plane and fleet measures cover only the time each run was going, plus up to 30 seconds after it ended. They are worked out over the whole time range before being cut to each run, so keep the range to the hours around the two runs. A blank cell means the run reported no such series, for example on a build without that metric.')
  + table.queryOptions.withDatasource('prometheus', ds)
  + table.queryOptions.withTargets([
    qTable(side('$run_a'), 'A'),
    qTable(side('$run_b'), 'B'),
  ])
  + table.queryOptions.withTransformations([
    // Both sides are instant queries at the same time, so merge pairs up
    // the rows that share a measure.
    { id: 'merge', options: {} },
    { id: 'calculateField', options: { mode: 'binary', alias: 'ratio', binary: { left: 'Value #B', operator: '/', right: 'Value #A' }, replaceFields: false } },
    { id: 'calculateField', options: { mode: 'binary', alias: 'B against A', binary: { left: 'ratio', operator: '-', right: '1' }, replaceFields: false } },
    { id: 'sortBy', options: { sort: [{ field: 'order', desc: false }] } },
    {
      id: 'organize',
      options: {
        excludeByName: { Time: true, order: true, ratio: true },
        indexByName: { measure: 0, 'Value #A': 1, 'Value #B': 2, 'B against A': 3 },
        renameByName: { measure: 'Measure', 'Value #A': 'Run A', 'Value #B': 'Run B' },
      },
    },
  ])
  + table.standardOptions.withUnit('none')
  // B against A is not a number when A is zero.
  + table.standardOptions.withMappings([{ type: 'special', options: { match: 'nan', result: { text: '-' } } }])
  + table.standardOptions.withOverrides([
    table.fieldOverride.byName.new('B against A')
    + table.fieldOverride.byName.withProperty('unit', 'percentunit')
    + table.fieldOverride.byName.withProperty('decimals', 0),
    table.fieldOverride.byName.new('Measure')
    + table.fieldOverride.byName.withProperty('custom.width', 260),
  ]);

// ---------------------------------------------------------------------------
// Distributions
// ---------------------------------------------------------------------------

local quantiles = [
  { label: 'p10', q: '0.1' },
  { label: 'p25', q: '0.25' },
  { label: 'p50', q: '0.5' },
  { label: 'p75', q: '0.75' },
  { label: 'p90', q: '0.9' },
  { label: 'p95', q: '0.95' },
  { label: 'p99', q: '0.99' },
  { label: 'max', q: '1' },
];

local lagSide(run) =
  std.join(' or ', [
    'label_set(max(quantile_over_time(%s, idns_perf_publish_lag_ms{run="%s",step="$step"}[$__range])), "quantile", "%s", "order", "%02d")' % [quantiles[i].q, run, quantiles[i].label, i]
    for i in std.range(0, std.length(quantiles) - 1)
  ]);

local compareBars(title, description, targets, joinField, orderField, unit, pos, horizontal=false) =
  barChart.new(title)
  + barChart.panelOptions.withGridPos(pos.h, pos.w, pos.x, pos.y)
  + barChart.panelOptions.withDescription(description)
  + barChart.queryOptions.withDatasource('prometheus', ds)
  + barChart.queryOptions.withTargets(targets)
  + barChart.queryOptions.withTransformations([
    { id: 'merge', options: {} },
  ] + (if orderField != '' then [{ id: 'sortBy', options: { sort: [{ field: orderField, desc: false }] } }] else []) + [
    {
      id: 'organize',
      options: {
        excludeByName: { Time: true, order: true },
        renameByName: { 'Value #A': 'Run A', 'Value #B': 'Run B' },
      },
    },
  ])
  + barChart.options.withXField(joinField)
  + barChart.options.withOrientation(if horizontal then 'horizontal' else 'vertical')
  + barChart.options.withShowValue('never')
  + barChart.options.withGroupWidth(0.7)
  + barChart.options.withBarWidth(0.9)
  + barChart.options.legend.withShowLegend(true)
  + barChart.options.legend.withPlacement('bottom')
  + barChart.options.tooltip.withMode('multi')
  + barChart.standardOptions.withUnit(unit)
  + barChart.standardOptions.withMin(0)
  + barChart.standardOptions.color.withMode('palette-classic');

local lagDistribution =
  compareBars(
    'Time to answer, $step',
    'Spread of the time from write request to first answer for $step changes in each run, the worse of the two resolvers at each point.',
    [qTable(lagSide('$run_a'), 'A'), qTable(lagSide('$run_b'), 'B')],
    'quantile',
    'order',
    'ms',
    grid(11, 12, 12, 0),
  )
  + { repeat: 'step', repeatDirection: 'v', maxPerRow: 1 };

local phaseSide(run, selector) =
  'sum by (phase) (%s) / sum by (phase) (%s)' % [
    overRun(run, 'sum by (phase) (increase(internal_dns_project_pass_phase_seconds_sum{%s,%s}[%s]))' % [controlPlane, selector, step]),
    overRun(run, 'sum by (phase) (increase(internal_dns_project_pass_phase_seconds_count{%s,%s}[%s]))' % [controlPlane, selector, step]),
  ];

local passPhases = compareBars(
  'Pass phases, average',
  'Average time one project pass spent in each phase while each run was going.',
  [qTable(phaseSide('$run_a', 'phase!~"zone .*"'), 'A'), qTable(phaseSide('$run_b', 'phase!~"zone .*"'), 'B')],
  'phase',
  '',
  's',
  grid(10, 12, 0, 34),
  horizontal=true,
);

local zoneSteps = compareBars(
  'Zone compile steps, average',
  'Average time of each step of compiling one changed zone while each run was going.',
  [qTable(phaseSide('$run_a', 'phase=~"zone .*"'), 'A'), qTable(phaseSide('$run_b', 'phase=~"zone .*"'), 'B')],
  'phase',
  '',
  's',
  grid(10, 12, 12, 34),
  horizontal=true,
);

local apiSide(run) =
  avgOverRun(run, 'sum by (method, code) (rate(rest_client_requests_total{%s}[%s]))' % [controlPlane, step]);

local apiByCode =
  compareBars(
    'API requests by method and code',
    'Average requests per second the control plane sent to the project API while each run was going.',
    [
      qTable('label_join(%s, "request", " ", "method", "code")' % apiSide('$run_a'), 'A'),
      qTable('label_join(%s, "request", " ", "method", "code")' % apiSide('$run_b'), 'B'),
    ],
    'request',
    '',
    'reqps',
    grid(10, 12, 0, 44),
    horizontal=true,
  )
  + barChart.queryOptions.withTransformations([
    { id: 'merge', options: {} },
    {
      id: 'organize',
      options: {
        excludeByName: { Time: true, method: true, code: true },
        renameByName: { 'Value #A': 'Run A', 'Value #B': 'Run B' },
      },
    },
  ]);

// Deduplicate scrape jobs before summing containers or pods.
// Pods are renamed when they restart, so CPU is summed per member type
// (<deployment> without the pod suffix) inside the window, not per pod.
local fleetSide(run) =
  avgOverRun(run, 'sum by (member, container) (label_replace(max by (cluster, namespace, pod, container) (rate(container_cpu_usage_seconds_total{%s,container!="",container!="POD"}[%s])), "member", "$1", "pod", "internal-dns-(.+)-[a-z0-9]+-[a-z0-9]+"))' % [fleetPods, step]);

local fleetCpu =
  compareBars(
    'Serving CPU by member and container, cores',
    'Average CPU of each serving member type and container while each run was going, summed over its pods.',
    [
      qTable('label_join(%s, "workload", " ", "member", "container")' % fleetSide('$run_a'), 'A'),
      qTable('label_join(%s, "workload", " ", "member", "container")' % fleetSide('$run_b'), 'B'),
    ],
    'workload',
    '',
    'short',
    grid(10, 12, 12, 44),
    horizontal=true,
  )
  + barChart.queryOptions.withTransformations([
    { id: 'merge', options: {} },
    {
      id: 'organize',
      options: {
        excludeByName: { Time: true, member: true, container: true },
        renameByName: { 'Value #A': 'Run A', 'Value #B': 'Run B' },
      },
    },
  ]);

// ---------------------------------------------------------------------------
// Variables
// ---------------------------------------------------------------------------

// Runs listed by start time. Run A defaults to the oldest run in the time
// range and run B to the newest, so the dashboard opens on a comparison.
local runChoice(name, label, order) =
  var.query.new(name)
  + var.query.generalOptions.withLabel(label)
  + var.query.generalOptions.showOnDashboard.withLabelAndValue()
  + var.query.withDatasource('prometheus', ds)
  + var.query.queryTypes.withQueryResult('%s(max by (run) (tfirst_over_time(idns_perf_run[$__range])))' % order)
  + var.query.withRegex('/run="([^"]+)"/')
  + var.query.refresh.onTime()
  // Keep the query's order. grafonnet's withSort(0) falls back to
  // alphabetical, so set it directly.
  + { sort: 0 };

local stepVar =
  var.custom.new('step', ['create', 'update', 'delete'])
  + var.custom.generalOptions.withLabel('Publish step')
  + var.custom.generalOptions.showOnDashboard.withLabelAndValue()
  + var.custom.selectionOptions.withMulti(true)
  + var.custom.selectionOptions.withIncludeAll(true)
  + var.custom.generalOptions.withCurrent(['All'], ['$__all']);

lib.dashboard(
  'Private DNS Run Comparison',
  'internal-dns-performance-compare',
  'Two Private DNS load test runs side by side: publish and query results, and what the control plane and serving fleet did during each. The time range only needs to include both runs.',
  ['dns', 'internal-dns', 'private-dns', 'performance'],
)
+ g.dashboard.withRefresh('')
// Every control-plane measure is worked out across the whole range, so a
// long default range is slow and can run out of memory in VictoriaMetrics.
+ g.dashboard.time.withFrom('now-6h')
+ g.dashboard.withLinks(lib.dnsLinks + [
  { type: 'link', title: 'All runs', url: '/d/internal-dns-performance/private-dns-performance', keepTime: true, includeVars: false, icon: 'dashboard' },
])
+ g.dashboard.withAnnotations([lib.loadTestAnnotation])
+ g.dashboard.withVariables([
  lib.datasourceVar,
  runChoice('run_a', 'Run A', 'sort'),
  runChoice('run_b', 'Run B', 'sort_desc'),
  stepVar,
])
+ g.dashboard.withPanels([
  comparisonTable,
  lagDistribution,
  section('During each run', 100, 33),
  passPhases,
  zoneSteps,
  apiByCode,
  fleetCpu,
])
