// Shared selectors, queries and panel builders for the Private DNS
// dashboards. Every dashboard reads from the `datasource` variable, so the
// rendered JSON names no particular Prometheus or VictoriaMetrics install.

local g = import 'github.com/grafana/grafonnet/gen/grafonnet-v11.4.0/main.libsonnet';

local prometheus = g.query.prometheus;
local stat = g.panel.stat;
local timeSeries = g.panel.timeSeries;
local row = g.panel.row;
local var = g.dashboard.variable;

local ds = '${datasource}';
local latency = import 'latency.libsonnet';
local latencyLegend = '{{cluster}} {{internal_dns_datum_net_tier}}';

{
  g: g,
  ds: ds,

  // -------------------------------------------------------------------------
  // Selectors
  // -------------------------------------------------------------------------

  controlPlane: 'namespace="internal-dns-system"',
  controllers: 'namespace="internal-dns-system",controller=~"internal-dns-control-plane-.*"',
  projectQueues: 'namespace="internal-dns-system",name=~"internal-dns-control-plane-.*"',
  broker: 'namespace="internal-dns-system"',
  configConsumers: 'consumer_name=~".*-(serving|records)|control-plane-.*-acks"',
  fleet: 'namespace=~"ns-.*",deployment=~"internal-dns-.*"',
  fleetPods: 'namespace=~"ns-.*",pod=~"internal-dns-.*"',
  tier: 'internal_dns_datum_net_tier',
  dnsdist: 'container="dnsdist",internal_dns_datum_net_tier=~"node|node-b|regional-dnsdist"',

  // -------------------------------------------------------------------------
  // Queries
  // -------------------------------------------------------------------------

  qRange(expr, legend)::
    prometheus.new(ds, expr)
    + prometheus.withLegendFormat(legend)
    + { editorMode: 'code' },

  qInstant(expr)::
    prometheus.new(ds, expr)
    + prometheus.withInstant(true)
    + { editorMode: 'code' },

  qTable(expr, ref)::
    prometheus.new(ds, expr)
    + prometheus.withInstant(true)
    + prometheus.withFormat('table')
    + prometheus.withRefId(ref)
    + { editorMode: 'code' },

  // Load test runs report idns_perf_run = 1 every 10 seconds while they last.
  // during() keeps `expr` only at instants inside the run, so a subquery over
  // it covers that run's window and nothing else. `expr` should look back one
  // `step` (a rate or increase over `step`): the run series is read `step`
  // earlier, so a point is kept only when the interval it covers starts after
  // the run began. The window ends up to one step after the last run sample,
  // which keeps the tail of a run's publishing in.
  // Whole project pass time at quantile q, from rate or increase buckets
  // built by `buckets(metric)`. internal_dns_project_pass_seconds reaches
  // 600 s; builds without it fall back to the controller-runtime reconcile
  // histogram, whose top bucket is 60 s, so anything slower reads as 60 s.
  passQuantile(q, buckets)::
    'histogram_quantile(%s, %s) or histogram_quantile(%s, %s)' % [
      q,
      buckets('internal_dns_project_pass_seconds_bucket{%s}' % self.controlPlane),
      q,
      buckets('controller_runtime_reconcile_time_seconds_bucket{%s}' % self.controllers),
    ],

  during(expr, run, step)::
    '(%s) and on () max(idns_perf_run{run="%s"} offset %s)' % [expr, run, step],

  // -------------------------------------------------------------------------
  // Layout and panels
  // -------------------------------------------------------------------------

  grid(h, w, x, y):: { h: h, w: w, x: x, y: y },

  section(title, id, y)::
    row.new(title)
    + row.withId(id)
    + row.withCollapsed(false)
    + { gridPos: { h: 1, w: 24, x: 0, y: y } },

  statPanel(title, description, expr, unit, steps, pos)::
    stat.new(title)
    + stat.panelOptions.withGridPos(pos.h, pos.w, pos.x, pos.y)
    + stat.panelOptions.withDescription(description)
    + stat.queryOptions.withDatasource('prometheus', ds)
    + stat.queryOptions.withTargets([self.qInstant(expr)])
    + stat.options.withColorMode('background')
    + stat.options.withGraphMode('none')
    + stat.options.withTextMode('value')
    + stat.options.reduceOptions.withCalcs(['lastNotNull'])
    + stat.options.reduceOptions.withFields('')
    + stat.options.reduceOptions.withValues(false)
    + stat.standardOptions.withUnit(unit)
    + stat.standardOptions.withMin(0)
    + stat.standardOptions.color.withMode('thresholds')
    + stat.standardOptions.thresholds.withMode('absolute')
    + stat.standardOptions.thresholds.withSteps(steps),

  ts(title, description, targets, unit, pos, stack=false, interval='', calcs=['max', 'lastNotNull'])::
    timeSeries.new(title)
    + timeSeries.panelOptions.withGridPos(pos.h, pos.w, pos.x, pos.y)
    + timeSeries.panelOptions.withDescription(description)
    + timeSeries.queryOptions.withDatasource('prometheus', ds)
    + timeSeries.queryOptions.withTargets(targets)
    + (if interval != '' then timeSeries.queryOptions.withInterval(interval) else {})
    + timeSeries.standardOptions.withUnit(unit)
    + timeSeries.standardOptions.withMin(0)
    + timeSeries.standardOptions.color.withMode('palette-classic')
    + timeSeries.fieldConfig.defaults.custom.withDrawStyle('line')
    + timeSeries.fieldConfig.defaults.custom.withFillOpacity(if stack then 60 else 10)
    + timeSeries.fieldConfig.defaults.custom.withLineWidth(1)
    + timeSeries.fieldConfig.defaults.custom.withShowPoints('never')
    + timeSeries.fieldConfig.defaults.custom.withSpanNulls(false)
    + (if stack then timeSeries.fieldConfig.defaults.custom.stacking.withMode('normal') else {})
    + timeSeries.options.legend.withDisplayMode('table')
    + timeSeries.options.legend.withPlacement('bottom')
    + timeSeries.options.legend.withShowLegend(true)
    + timeSeries.options.legend.withCalcs(calcs)
    + timeSeries.options.legend.withSortBy('Max')
    + timeSeries.options.legend.withSortDesc(true)
    + timeSeries.options.tooltip.withMode('multi')
    + timeSeries.options.tooltip.withSort('desc'),

  udpLatencyEstimate(selector, pos)::
    self.ts(
      'UDP answer time (estimated p50 and p99)',
      'Histogram estimates for answered UDP frontend queries, by cluster and tier. Buckets end at 1, 10, 50, 100 and 1000 ms, then +Inf. Interpolation can show 0.5 ms without measuring that exact latency; a quantile in +Inf is capped at 1000 ms, not a measured one-second answer. Includes UDP error responses, readiness and background traffic; excludes TCP frontend answers and unanswered queries. The histogram has no response-code label: use Answers by response code separately. At low volume, one or two tail answers can move p99 sharply; compare the UDP band shares and counts below.',
      [
        self.qRange(latency.quantile(selector, '0.5', '$__rate_interval'), latencyLegend + ' estimated p50'),
        self.qRange(latency.quantile(selector, '0.99', '$__rate_interval'), latencyLegend + ' estimated p99'),
      ],
      'ms', pos,
    ),

  udpLatencyShares(selector, pos)::
    self.ts(
      'UDP answers by latency threshold, share',
      'Shares from measured histogram bucket counts, without quantile interpolation: under 1 ms, over 100 ms and over 1 s. Thresholds overlap: over 1 s is a subset of over 100 ms; the lines do not sum to 100%. The denominator is the same UDP histogram population, including error and background answers, not all-protocol queries. Empty when no UDP answers were observed; missing telemetry stays empty.',
      [
        self.qRange(latency.share(selector, '1', '$__rate_interval'), latencyLegend + ' under 1 ms'),
        self.qRange(latency.share(selector, '100', '$__rate_interval', true), latencyLegend + ' over 100 ms'),
        self.qRange(latency.share(selector, '1000', '$__rate_interval', true), latencyLegend + ' over 1 s'),
      ],
      'percentunit', pos,
    ) + timeSeries.standardOptions.withMax(1),

  udpLatencyCounts(selector, pos)::
    self.ts(
      'UDP answer counts in the lookback window',
      'Rolling counts over the same Grafana rate interval as the percentile chart: all UDP histogram answers on the right axis, over 100 ms and over 1 s on the left. Slow counts subtract finite buckets from that same population. A large p99 alongside only one or two slow answers is different from sustained high-volume latency. Counts may be fractional because increase() can extrapolate between scrapes; they are not packet-level event logs. Zero means a sampled counter did not increase, while missing telemetry stays empty. Includes error responses; see Answers by response code for their separate breakdown.',
      [
        self.qRange(latency.count(selector, '$__rate_interval'), latencyLegend + ' all UDP answers'),
        self.qRange(latency.over(selector, '100', '$__rate_interval'), latencyLegend + ' over 100 ms'),
        self.qRange(latency.over(selector, '1000', '$__rate_interval'), latencyLegend + ' over 1 s'),
      ],
      'short', pos,
    ) + { fieldConfig+: { overrides+: [{
      matcher: { id: 'byRegexp', options: '.* all UDP answers$' },
      properties: [{ id: 'custom.axisPlacement', value: 'right' }],
    }] } },

  // Draws one dot per sample. Load test results are sparse events, not a
  // continuous signal, so lines between them would suggest values that were
  // never measured.
  points(panel)::
    panel
    + timeSeries.fieldConfig.defaults.custom.withDrawStyle('points')
    + timeSeries.fieldConfig.defaults.custom.withShowPoints('always')
    + timeSeries.fieldConfig.defaults.custom.withPointSize(4)
    + timeSeries.fieldConfig.defaults.custom.withFillOpacity(0),

  bars(panel)::
    panel + timeSeries.fieldConfig.defaults.custom.withDrawStyle('bars'),

  green: { color: 'green', value: null },
  blue: [{ color: 'blue', value: null }],
  anyIsBad: [self.green, { color: 'red', value: 1 }],

  // -------------------------------------------------------------------------
  // Variables, annotations and links
  // -------------------------------------------------------------------------

  datasourceVar:
    var.datasource.new('datasource', 'prometheus')
    + var.datasource.generalOptions.withLabel('Data Source')
    + var.datasource.generalOptions.withCurrent('Service Metrics', 'SERVICE_METRICS')
    + var.datasource.generalOptions.showOnDashboard.withLabelAndValue()
    + var.datasource.withRegex(''),

  queryVar(name, label, metric, labelName, multi=true, includeAll=true, sort=1)::
    var.query.new(name)
    + var.query.generalOptions.withLabel(label)
    + var.query.generalOptions.showOnDashboard.withLabelAndValue()
    + var.query.withDatasource('prometheus', ds)
    + var.query.queryTypes.withLabelValues(labelName, metric)
    + var.query.refresh.onTime()
    + var.query.withSort(sort)
    + var.query.selectionOptions.withMulti(multi)
    + (if includeAll then var.query.selectionOptions.withIncludeAll(value=true, customAllValue='.*') else {}),

  loadTestAnnotation: {
    name: 'Load tests',
    datasource: { type: 'prometheus', uid: ds },
    enable: true,
    iconColor: 'purple',
    expr: 'max by (run, scenario, commit) (idns_perf_run)',
    step: '10s',
    titleFormat: 'Load test {{scenario}}',
    textFormat: 'run {{run}}, commit {{commit}}',
    tagKeys: 'scenario,commit',
    useValueForTime: false,
  },

  dnsLinks: [
    { type: 'dashboards', tags: ['dns'], asDropdown: true, title: 'DNS dashboards', includeVars: false, keepTime: true },
  ],

  dashboard(title, uid, description, tags)::
    g.dashboard.new(title)
    + g.dashboard.withUid(uid)
    + g.dashboard.withDescription(description)
    + g.dashboard.withTags(tags)
    + g.dashboard.withEditable(true)
    + g.dashboard.withSchemaVersion(41)
    + g.dashboard.time.withTo('now')
    + g.dashboard.withLinks(self.dnsLinks),
}
