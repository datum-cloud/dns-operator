// Shared selectors, queries and panel builders for the Private DNS
// dashboards. Every dashboard reads from the `datasource` variable, so the
// rendered JSON names no particular Prometheus or VictoriaMetrics install.

local g = import 'github.com/grafana/grafonnet/gen/grafonnet-v11.4.0/main.libsonnet';

local prometheus = g.query.prometheus;
local stat = g.panel.stat;
local timeSeries = g.panel.timeSeries;
local heatmap = g.panel.heatmap;
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
    '(%s) and on () max(idns_perf_run{run="%s"} offset %s == 1)' % [expr, run, step],

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
      'Histogram estimates for answered UDP frontend queries, by cluster and tier. Buckets end at 1, 10, 50, 100 and 1000 ms, then +Inf. Interpolation can show 0.5 ms without measuring that exact latency; a quantile in +Inf is capped at 1000 ms, not a measured one-second answer. Includes UDP error responses, readiness and background traffic; excludes TCP frontend answers and unanswered queries. The histogram has no response-code label: use Answers by response code separately. At low volume, one or two tail answers can move p99 sharply; the answer time heatmaps show what share of answers landed in each band.',
      [
        self.qRange(latency.quantile(selector, '0.5', '$__rate_interval'), latencyLegend + ' estimated p50'),
        self.qRange(latency.quantile(selector, '0.99', '$__rate_interval'), latencyLegend + ' estimated p99'),
      ],
      'ms', pos,
    ),

  heatmap(title, description, expr, unit, pos, repeat='', share=false)::
    heatmap.new(title)
    + heatmap.panelOptions.withGridPos(pos.h, pos.w, pos.x, pos.y)
    + heatmap.panelOptions.withDescription(description)
    + (if repeat != '' then
         heatmap.panelOptions.withRepeat(repeat)
         + heatmap.panelOptions.withRepeatDirection('h')
         + heatmap.panelOptions.withMaxPerRow(4)
       else {})
    + heatmap.queryOptions.withDatasource('prometheus', ds)
    + heatmap.queryOptions.withTargets([
      prometheus.new(ds, expr)
      + prometheus.withFormat('heatmap')
      + prometheus.withLegendFormat('{{le}}')
      + { editorMode: 'code' },
    ])
    + heatmap.queryOptions.withInterval('1m')
    + heatmap.options.withCalculate(false)
    + heatmap.options.withCellGap(1)
    + heatmap.options.rowsFrame.withLayout('le')
    + heatmap.options.yAxis.withUnit(unit)
    + heatmap.options.yAxis.withAxisPlacement('left')
    + heatmap.options.color.withMode('opacity')
    + heatmap.options.color.withFill('orange')
    + heatmap.options.color.withScale('exponential')
    + heatmap.options.color.withMin(0)
    + heatmap.options.filterValues.withLe(1e-9)
    + (if share then
         // Grafana bins values linearly into the steps and applies the
         // exponent only to each step's opacity, so anything under
         // max/steps is transparent. A 5% cap keeps 1 answer in 2,500 visible;
         // larger shares all render at full colour.
         heatmap.options.color.withSteps(128)
         + heatmap.options.color.withExponent(0.2)
         + heatmap.options.color.withMax(0.05)
         + heatmap.options.cellValues.withUnit('percentunit')
         + heatmap.options.cellValues.withDecimals(2)
       else
         heatmap.options.color.withSteps(64)
         + heatmap.options.color.withExponent(0.1)
         + heatmap.options.cellValues.withUnit('short'))
    + heatmap.options.tooltip.withMode('single')
    + heatmap.options.tooltip.withYHistogram(true)
    + heatmap.options.legend.withShow(true),

  udpLatencyHeatmaps(selector, y, repeat='')::
    local description(who) =
      "Answered UDP queries at %s, by latency band. Each column is one interval of at least a minute; each row is one of dnsdist's fixed buckets (under 1 ms, 1-10, 10-50, 50-100 and 100-1000 ms, then over 1 s). The colour is the share of that interval's answers that landed in the band, on the same scale in every cluster: darker means a larger share, and 5%% or more shows as full colour, so the busy fast bands are solid and slow bands stand out as paler cells. A band holding at least 1 in 2,500 answers is shaded. Hover a cell for its exact share. Includes UDP error responses; excludes TCP and unanswered queries." % who;
    [
      self.heatmap(
        'UDP answer time, node resolvers%s' % (if repeat != '' then ' in $' + repeat else ''),
        description('the node resolvers that clients query, node and node-b together'),
        latency.shares('%s,%s' % [selector, latency.nodeTiers], '$__interval'),
        'ms',
        self.grid(8, 24, 0, y),
        repeat,
        share=true,
      ),
      self.heatmap(
        'UDP answer time, regional routers%s' % (if repeat != '' then ' in $' + repeat else ''),
        description('the regional routers, which only see queries the node resolvers forward'),
        latency.shares('%s,%s' % [selector, latency.regionalTier], '$__interval'),
        'ms',
        self.grid(8, 24, 0, y + 8),
        repeat,
        share=true,
      ),
    ],

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
    expr: 'max by (run, scenario, commit) (idns_perf_run == 1)',
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
