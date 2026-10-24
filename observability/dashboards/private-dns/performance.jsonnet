// Private DNS performance dashboard
//
// For reading a load test run and what the system did while it ran. The
// table at the top lists every run in the time range; clicking a run narrows
// the dashboard to that run's window. Below it: what the load test measured
// (publishing and queries), then the control plane, the broker and the
// serving fleet over the same window.
//
// Load test series (idns_perf_*) are written by the harness in
// test/internaldns/perf. Every one carries `run`, `scenario` and `commit`
// (the dns-operator commit the environment ran).
//
// Usage:
//   jsonnet -J vendor performance.jsonnet > performance.json
// Or:
//   task build

local lib = import 'lib.libsonnet';
local g = lib.g;

local timeSeries = g.panel.timeSeries;
local table = g.panel.table;

local ds = lib.ds;
local qRange = lib.qRange;
local qTable = lib.qTable;
local grid = lib.grid;
local ts = lib.ts;
local points = lib.points;
local bars = lib.bars;
local section = lib.section;

local runSel = 'run=~"$run"';
local controlPlane = lib.controlPlane;
local controllers = lib.controllers;
local projectQueues = lib.projectQueues;
local tier = lib.tier;

// Serving pods of the chosen members. Pods are named
// <deployment>-<replica set hash>-<suffix>, so anchoring two trailing
// segments keeps internal-dns-node from also matching internal-dns-node-b.
local memberPods = 'namespace=~"ns-.*",cluster=~"$cluster",pod=~"internal-dns-.*",pod=~"(${member:regex})-[a-z0-9]+-[a-z0-9]+"';
local dnsdist = 'container="dnsdist",%s' % memberPods;

// ---------------------------------------------------------------------------
// Variables
// ---------------------------------------------------------------------------

local scenarioVar = lib.queryVar('scenario', 'Scenario', 'idns_perf_run', 'scenario');
local commitVar = lib.queryVar('commit', 'Commit', 'idns_perf_run{scenario=~"$scenario"}', 'commit');
local runVar = lib.queryVar('run', 'Run', 'idns_perf_run{scenario=~"$scenario",commit=~"$commit"}', 'run', sort=2);
local clusterVar = lib.queryVar('cluster', 'Cluster', 'kube_deployment_status_replicas_available{%s}' % lib.fleet, 'cluster');
local memberVar = lib.queryVar('member', 'Serving member', 'kube_deployment_status_replicas_available{%s,cluster=~"$cluster"}' % lib.fleet, 'deployment');

// ---------------------------------------------------------------------------
// Runs
// ---------------------------------------------------------------------------

local byRun = 'run, scenario, commit';
local runs = 'idns_perf_run{scenario=~"$scenario",commit=~"$commit",%s}' % runSel;

local lagQuantile(q, step) =
  'max by (%s) (quantile_over_time(%s, idns_perf_publish_lag_ms{%s,step="%s"}[$__range]))' % [byRun, q, runSel, step];

// The run link opens this dashboard on one run, from a minute before it
// started to two minutes after it ended so the control plane can be seen
// settling.
local openRun = {
  title: 'Show this run',
  url: '/d/internal-dns-performance/private-dns-performance?${datasource:queryparam}&${cluster:queryparam}&${member:queryparam}&var-run=${__data.fields.run}&from=${__data.fields.from}&to=${__data.fields.to}',
};

local compareRun = {
  title: 'Compare this run with another',
  url: '/d/internal-dns-performance-compare/private-dns-run-comparison?${datasource:queryparam}&var-run_a=${__data.fields.run}&from=${__data.fields.compare_from}&to=now',
};

local runsTable =
  table.new('Runs')
  + table.panelOptions.withGridPos(9, 24, 0, 0)
  + table.panelOptions.withDescription('One row per run in the time range. Click a run to narrow the dashboard to its window, or to compare it with another run. Publish columns are the time from the write request to the first answer from the VPC resolver; query columns cover the whole run.')
  + table.queryOptions.withDatasource('prometheus', ds)
  + table.queryOptions.withTargets([
    qTable('max by (%s) (tfirst_over_time(%s[$__range]) * 1000)' % [byRun, runs], 'A'),
    qTable('max by (%s) (tlast_over_time(%s[$__range]) - tfirst_over_time(%s[$__range]))' % [byRun, runs, runs], 'B'),
    qTable('max by (%s) ((tfirst_over_time(%s[$__range]) - 60) * 1000)' % [byRun, runs], 'FROM'),
    qTable('max by (%s) ((tlast_over_time(%s[$__range]) + 120) * 1000)' % [byRun, runs], 'TO'),
    // The comparison opens on the span from this run to now, so run B can be
    // any later run.
    qTable('max by (%s) ((tfirst_over_time(%s[$__range]) - 60) * 1000)' % [byRun, runs], 'CMP'),
    qTable(lagQuantile('0.5', 'create'), 'C'),
    qTable(lagQuantile('0.99', 'create'), 'D'),
    qTable(lagQuantile('0.5', 'update'), 'E'),
    qTable(lagQuantile('0.99', 'update'), 'F'),
    qTable(lagQuantile('0.5', 'delete'), 'G'),
    qTable(lagQuantile('0.99', 'delete'), 'H'),
    // Zero rather than blank for a publish run that missed nothing.
    qTable('sum by (%(by)s) (count_over_time(idns_perf_publish_missed{%(sel)s}[$__range])) or (max by (%(by)s) (max_over_time(idns_perf_publish_lag_ms{%(sel)s}[$__range])) * 0)' % { by: byRun, sel: runSel }, 'I'),
    qTable('sum by (%s) (avg_over_time(idns_perf_query_qps{%s}[$__range]))' % [byRun, runSel], 'J'),
    qTable('max by (%s) (max_over_time(idns_perf_query_p99_ms{%s}[$__range]))' % [byRun, runSel], 'K'),
    qTable('sum by (%s) (sum_over_time(idns_perf_query_failed{%s}[$__range]))' % [byRun, runSel], 'L'),
  ])
  + table.queryOptions.withTransformations([
    { id: 'merge', options: {} },
    // Runs come from idns_perf_run, so a run whose other series fell outside
    // the time range still gets a row, and nothing else adds one.
    { id: 'filterByValue', options: { type: 'exclude', match: 'any', filters: [{ fieldName: 'Value #A', config: { id: 'isNull', options: {} } }] } },
    {
      id: 'organize',
      options: {
        excludeByName: { Time: true },
        indexByName: {
          run: 0, scenario: 1, commit: 2, 'Value #A': 3, 'Value #B': 4,
          'Value #C': 5, 'Value #D': 6, 'Value #E': 7, 'Value #F': 8, 'Value #G': 9, 'Value #H': 10,
          'Value #I': 11, 'Value #J': 12, 'Value #K': 13, 'Value #L': 14, 'Value #FROM': 15, 'Value #TO': 16, 'Value #CMP': 17,
        },
        renameByName: {
          'Value #A': 'started', 'Value #B': 'duration',
          'Value #C': 'create p50', 'Value #D': 'create p99',
          'Value #E': 'update p50', 'Value #F': 'update p99',
          'Value #G': 'delete p50', 'Value #H': 'delete p99',
          'Value #I': 'never answered', 'Value #J': 'query rate',
          'Value #K': 'query worst p99', 'Value #L': 'failed queries',
          'Value #FROM': 'from', 'Value #TO': 'to', 'Value #CMP': 'compare_from',
        },
      },
    },
    { id: 'sortBy', options: { sort: [{ field: 'started', desc: true }] } },
    // A short commit such as 44162e7 is guessed to be a number in
    // scientific notation; keep it as text.
    { id: 'convertFieldType', options: { conversions: [{ targetField: 'commit', destinationType: 'string' }], fields: {} } },
  ])
  + table.standardOptions.withUnit('ms')
  + table.standardOptions.withDecimals(0)
  + table.standardOptions.withOverrides([
    table.fieldOverride.byName.new('run')
    + table.fieldOverride.byName.withProperty('links', [openRun, compareRun]),
    // The table unit is ms; without this a commit such as 44162e7 is read as
    // a number in scientific notation and shown as a duration.
    table.fieldOverride.byRegexp.new('run|scenario|commit')
    + table.fieldOverride.byRegexp.withProperty('unit', 'string'),
    table.fieldOverride.byName.new('started')
    + table.fieldOverride.byName.withProperty('unit', 'dateTimeAsLocal')
    + table.fieldOverride.byName.withProperty('custom.width', 200),
    table.fieldOverride.byName.new('duration')
    + table.fieldOverride.byName.withProperty('unit', 's'),
    table.fieldOverride.byRegexp.new('never answered|failed queries')
    + table.fieldOverride.byRegexp.withProperty('unit', 'short')
    + table.fieldOverride.byRegexp.withProperty('custom.cellOptions', { type: 'color-text' })
    + table.fieldOverride.byRegexp.withProperty('thresholds', { mode: 'absolute', steps: [lib.green, { color: 'red', value: 1 }] }),
    table.fieldOverride.byName.new('query rate')
    + table.fieldOverride.byName.withProperty('unit', 'reqps')
    + table.fieldOverride.byName.withProperty('decimals', 1),
    // Only the links read these; they hold the run window in epoch ms.
    table.fieldOverride.byRegexp.new('^(from|to|compare_from)$')
    + table.fieldOverride.byRegexp.withProperty('unit', 'none')
    + table.fieldOverride.byRegexp.withProperty('custom.hidden', true),
  ]);

// ---------------------------------------------------------------------------
// Publishing
// ---------------------------------------------------------------------------

local publishRow = section('Publishing', 100, 9);

local byStep = 'run, step';
local lagStat(fn) = 'max by (%s) (%s(idns_perf_publish_lag_ms{%s}[$__range]))' % [byStep, fn, runSel];
local lagQ(q) = 'max by (%s) (quantile_over_time(%s, idns_perf_publish_lag_ms{%s}[$__range]))' % [byStep, q, runSel];
local apiQ(q) = 'max by (%s) (quantile_over_time(%s, idns_perf_publish_api_ms{%s}[$__range]))' % [byStep, q, runSel];

local publishTable =
  table.new('Publish time by step')
  + table.panelOptions.withGridPos(7, 24, 0, 10)
  + table.panelOptions.withDescription('For each run and step: how many answers were seen, the spread of time from write request to first answer, changes never answered, and how long the project API took to accept the write. Answers are counted per resolver, so one change watched at two resolvers counts twice.')
  + table.queryOptions.withDatasource('prometheus', ds)
  + table.queryOptions.withTargets([
    qTable('sum by (%s) (count_over_time(idns_perf_publish_lag_ms{%s}[$__range]))' % [byStep, runSel], 'A'),
    qTable(lagQ('0.5'), 'B'),
    qTable(lagQ('0.9'), 'C'),
    qTable(lagQ('0.99'), 'D'),
    qTable(lagStat('max_over_time'), 'E'),
    qTable('sum by (%(by)s) (count_over_time(idns_perf_publish_missed{%(sel)s}[$__range])) or (max by (%(by)s) (max_over_time(idns_perf_publish_lag_ms{%(sel)s}[$__range])) * 0)' % { by: byStep, sel: runSel }, 'F'),
    qTable(apiQ('0.5'), 'G'),
    qTable(apiQ('0.99'), 'H'),
  ])
  + table.queryOptions.withTransformations([
    { id: 'merge', options: {} },
    {
      id: 'organize',
      options: {
        excludeByName: { Time: true },
        renameByName: {
          'Value #A': 'answers', 'Value #B': 'p50', 'Value #C': 'p90', 'Value #D': 'p99', 'Value #E': 'max',
          'Value #F': 'never answered', 'Value #G': 'API p50', 'Value #H': 'API p99',
        },
      },
    },
    { id: 'sortBy', options: { sort: [{ field: 'run', desc: true }] } },
  ])
  + table.standardOptions.withUnit('ms')
  + table.standardOptions.withDecimals(0)
  + table.standardOptions.withOverrides([
    table.fieldOverride.byRegexp.new('answers|never answered')
    + table.fieldOverride.byRegexp.withProperty('unit', 'short'),
    table.fieldOverride.byName.new('never answered')
    + table.fieldOverride.byName.withProperty('custom.cellOptions', { type: 'color-text' })
    + table.fieldOverride.byName.withProperty('thresholds', { mode: 'absolute', steps: [lib.green, { color: 'red', value: 1 }] }),
  ]);

local publishLag = points(ts(
  'Time to answer a published change',
  'One dot per answered change: time from the write request to the first answer, by run and step, plotted when the answer arrived.',
  [qRange('max by (run, step) (max_over_time(idns_perf_publish_lag_ms{%s}[$__interval]))' % runSel, '{{run}} {{step}}')],
  'ms',
  grid(9, 12, 0, 17),
  interval='1s',
));

local publishLagByResolver = points(ts(
  'Time to answer by resolver',
  'The same time split by the resolver that answered, to tell the two node resolvers apart.',
  [qRange('max by (run, resolver) (max_over_time(idns_perf_publish_lag_ms{%s}[$__interval]))' % runSel, '{{run}} {{resolver}}')],
  'ms',
  grid(9, 12, 12, 17),
  interval='1s',
));

local publishApi = points(ts(
  'API write time',
  'How long the project API took to accept each create, update and delete, plotted when the write was sent.',
  [qRange('max by (run, step) (max_over_time(idns_perf_publish_api_ms{%s}[$__interval]))' % runSel, '{{run}} {{step}}')],
  'ms',
  grid(8, 12, 0, 26),
  interval='1s',
));

local publishMissed = bars(ts(
  'Changes never answered',
  'Changes the prober gave up waiting for, plotted when the write was sent. Any bar here is a failed publish.',
  [qRange('sum by (run, step) (count_over_time(idns_perf_publish_missed{%s}[$__interval]))' % runSel, '{{run}} {{step}}')],
  'short',
  grid(8, 12, 12, 26),
  interval='1s',
));

// ---------------------------------------------------------------------------
// Queries
// ---------------------------------------------------------------------------

local queryRow = section('Queries', 200, 34);

local queryLatency = ts(
  'Load test query time',
  'Per-second p50, p99 and max of load test queries, from the generator with the slowest answers, by run and query kind. Timeouts count as 5000 ms.',
  [
    qRange('max by (run, kind) (max_over_time(idns_perf_query_max_ms{%s}[$__interval]))' % runSel, '{{run}} {{kind}} max'),
    qRange('max by (run, kind) (max_over_time(idns_perf_query_p99_ms{%s}[$__interval]))' % runSel, '{{run}} {{kind}} p99'),
    qRange('max by (run, kind) (max_over_time(idns_perf_query_p50_ms{%s}[$__interval]))' % runSel, '{{run}} {{kind}} p50'),
  ],
  'ms',
  grid(8, 8, 0, 35),
  interval='1s',
);

local queryRate = ts(
  'Load test queries sent',
  'Queries per second across all generators, by run and query kind.',
  [qRange('sum by (run, kind) (avg_over_time(idns_perf_query_qps{%s}[$__interval]))' % runSel, '{{run}} {{kind}}')],
  'reqps',
  grid(8, 8, 8, 35),
  interval='1s',
);

local queryFailed = bars(ts(
  'Load test queries failed',
  'Queries per second that timed out or got a wrong answer, across all generators, by run and query kind.',
  [qRange('sum by (run, kind) (avg_over_time(idns_perf_query_failed{%s}[$__interval])) > 0' % runSel, '{{run}} {{kind}}')],
  'reqps',
  grid(8, 8, 16, 35),
  interval='1s',
));

// ---------------------------------------------------------------------------
// Control plane: project passes
// ---------------------------------------------------------------------------

local passRow = section('Control plane: project passes', 300, 43);

local phaseAvg(selector) =
  'sum by (phase) (rate(internal_dns_project_pass_phase_seconds_sum{%(s)s}[$__rate_interval])) / sum by (phase) (rate(internal_dns_project_pass_phase_seconds_count{%(s)s}[$__rate_interval]))' % { s: selector };

local phaseTotal(selector) =
  'sum by (phase) (rate(internal_dns_project_pass_phase_seconds_sum{%s}[$__rate_interval]))' % selector;

local passPhases = ts(
  'Pass phases, average',
  'Average time one project pass spends in each phase, across every project. A record change waits for at most two passes before it is published.',
  [qRange(phaseAvg('%s,phase!~"zone .*"' % controlPlane), '{{phase}}')],
  's',
  grid(8, 8, 0, 44),
  calcs=['mean', 'max'],
);

local zoneSteps = ts(
  'Zone compile steps, average',
  'Average time of each step of compiling one changed zone.',
  [qRange(phaseAvg('%s,phase=~"zone .*"' % controlPlane), '{{phase}}')],
  's',
  grid(8, 8, 8, 44),
  calcs=['mean', 'max'],
);

local phaseShare = ts(
  'Where pass time goes',
  'Seconds of pass time spent in each phase per second, summed over every project and both replicas. This weighs a phase by how often it runs, so it shows which phase is worth making faster. Zone steps are part of compile and are left out.',
  [qRange(phaseTotal('%s,phase!~"zone .*"' % controlPlane), '{{phase}}')],
  's',
  grid(8, 8, 16, 44),
  stack=true,
  calcs=['mean', 'max'],
);

local phaseP90 = ts(
  'Pass phases and zone steps, p90',
  'Slowest 10% of each phase and zone step.',
  [qRange('histogram_quantile(0.9, sum by (phase, le) (rate(internal_dns_project_pass_phase_seconds_bucket{%s}[$__rate_interval])))' % controlPlane, '{{phase}}')],
  's',
  grid(8, 8, 0, 52),
  calcs=['mean', 'max'],
);

local passDuration = ts(
  'Project pass time',
  'Whole project pass time across every project.',
  [
    qRange('histogram_quantile(0.5, sum by (le) (rate(controller_runtime_reconcile_time_seconds_bucket{%s}[$__rate_interval])))' % controllers, 'p50'),
    qRange('histogram_quantile(0.9, sum by (le) (rate(controller_runtime_reconcile_time_seconds_bucket{%s}[$__rate_interval])))' % controllers, 'p90'),
    qRange('histogram_quantile(0.99, sum by (le) (rate(controller_runtime_reconcile_time_seconds_bucket{%s}[$__rate_interval])))' % controllers, 'p99'),
  ],
  's',
  grid(8, 8, 8, 52),
);

local queueWait = ts(
  'Project queue wait and depth',
  'How long a project waits in the queue before its pass starts (left axis), and how many projects are waiting (right axis).',
  [
    qRange('histogram_quantile(0.9, sum by (le) (rate(workqueue_queue_duration_seconds_bucket{%s}[$__rate_interval])))' % projectQueues, 'wait p90'),
    qRange('sum(workqueue_depth{%s})' % projectQueues, 'waiting'),
  ],
  's',
  grid(8, 8, 16, 52),
) + timeSeries.standardOptions.withOverrides([
  timeSeries.fieldOverride.byName.new('waiting')
  + timeSeries.fieldOverride.byName.withProperty('unit', 'short')
  + timeSeries.fieldOverride.byName.withProperty('custom.axisPlacement', 'right'),
]);

// ---------------------------------------------------------------------------
// Control plane: API, loops and resources
// ---------------------------------------------------------------------------

local apiRow = section('Control plane: API, loops and resources', 400, 60);

local apiRequests = ts(
  'API requests from the control plane',
  'Requests per second the control plane sends to the project API, by method and response code. 409 is a write that lost a race with the other replica; 429 is the server asking it to slow down.',
  [qRange('sum by (method, code) (rate(rest_client_requests_total{%s}[$__rate_interval]))' % controlPlane, '{{method}} {{code}}')],
  'reqps',
  grid(8, 8, 0, 61),
  stack=true,
);

local clientThrottle = ts(
  'Waiting on the client rate limit',
  'Seconds per second that API requests spent waiting for the control plane\'s own client-side rate limiter, by client, and the p99 wait of one request. Time spent here is time a pass is not doing work. Empty on builds that do not report it.',
  [
    qRange('sum by (client) (rate(internal_dns_kube_client_throttle_seconds_sum{%s}[$__rate_interval]))' % controlPlane, '{{client}} total'),
    qRange('histogram_quantile(0.99, sum by (client, le) (rate(internal_dns_kube_client_throttle_seconds_bucket{%s}[$__rate_interval])))' % controlPlane, '{{client}} p99'),
  ],
  's',
  grid(8, 8, 8, 61),
);

local loopResults = ts(
  'Loop runs that did not succeed',
  'Control-plane loop runs per second that ended in anything but success. Conflicts mean the two replicas raced for the same object.',
  [qRange('sum by (loop, result) (rate(internal_dns_loop_runs_total{%s,result!="success"}[$__rate_interval]))' % controlPlane, '{{loop}} {{result}}')],
  'ops',
  grid(8, 8, 16, 61),
);

local loopTime = ts(
  'Loop run time',
  'Average run time of the loops that move publications to the broker and acknowledgements back.',
  [qRange('sum by (loop) (rate(internal_dns_loop_duration_seconds_sum{%s}[$__rate_interval])) / sum by (loop) (rate(internal_dns_loop_duration_seconds_count{%s}[$__rate_interval]))' % [controlPlane, controlPlane], '{{loop}}')],
  's',
  grid(8, 8, 0, 69),
);

local controlPlaneCpu = ts(
  'Control plane and broker CPU, cores',
  'CPU used by each control-plane and broker pod, by container.',
  [qRange('sum by (pod, container) (rate(container_cpu_usage_seconds_total{%s,container!="",container!="POD"}[$__rate_interval]))' % controlPlane, '{{pod}} {{container}}')],
  'short',
  grid(8, 8, 8, 69),
);

local controlPlaneMemory = ts(
  'Control plane and broker memory',
  'Working set memory of each control-plane and broker pod, by container.',
  [qRange('sum by (pod, container) (container_memory_working_set_bytes{%s,container!="",container!="POD"})' % controlPlane, '{{pod}} {{container}}')],
  'bytes',
  grid(8, 8, 16, 69),
);

// ---------------------------------------------------------------------------
// Delivery and serving fleet
// ---------------------------------------------------------------------------

local servingRow = section('Delivery and serving fleet ($cluster, $member)', 500, 77);

local pending = ts(
  'Broker messages waiting',
  'Messages waiting for each configuration consumer, and messages delivered but not yet acknowledged. A consumer that climbs during a run is a member falling behind.',
  [
    qRange('max by (consumer_name) (nats_consumer_num_pending{%s,%s})' % [lib.broker, lib.configConsumers], '{{consumer_name}} waiting'),
    qRange('max by (consumer_name) (nats_consumer_num_ack_pending{%s,%s})' % [lib.broker, lib.configConsumers], '{{consumer_name}} not acknowledged'),
  ],
  'short',
  grid(8, 8, 0, 78),
);

local resolverQueries = ts(
  'Resolver queries',
  'Queries per second each dnsdist front received, by tier and pod. Only members with dnsdist metrics turned on report.',
  [qRange('sum by (%(tier)s, pod) (rate(dnsdist_queries{%(sel)s}[$__rate_interval]))' % { tier: tier, sel: dnsdist }, '{{%s}} {{pod}}' % tier)],
  'reqps',
  grid(8, 8, 8, 78),
);

local answersByRcode = ts(
  'Answers by response code',
  'What the resolvers sent back, by tier. NXDOMAIN is expected for names outside a project. SERVFAIL means a backend failed or did not answer in time. A query can pass through more than one tier.',
  [
    qRange('sum by (%(tier)s) (rate(dnsdist_frontend_noerror{%(sel)s}[$__rate_interval]))' % { tier: tier, sel: dnsdist }, '{{%s}} NOERROR' % tier),
    qRange('sum by (%(tier)s) (rate(dnsdist_frontend_nxdomain{%(sel)s}[$__rate_interval]))' % { tier: tier, sel: dnsdist }, '{{%s}} NXDOMAIN' % tier),
    qRange('sum by (%(tier)s) (rate(dnsdist_frontend_servfail{%(sel)s}[$__rate_interval]))' % { tier: tier, sel: dnsdist }, '{{%s}} SERVFAIL' % tier),
  ],
  'reqps',
  grid(8, 8, 16, 78),
);

local resolverLatency = ts(
  'Resolver answer time',
  'p50 and p99 answer time measured by dnsdist, by tier. dnsdist counts answers in 1, 10, 50, 100 and 1000 ms buckets, so a value is an estimate within its bucket.',
  [
    qRange('histogram_quantile(0.5, sum by (%(tier)s, le) (rate(dnsdist_latency_bucket{%(sel)s}[$__rate_interval])))' % { tier: tier, sel: dnsdist }, '{{%s}} p50' % tier),
    qRange('histogram_quantile(0.99, sum by (%(tier)s, le) (rate(dnsdist_latency_bucket{%(sel)s}[$__rate_interval])))' % { tier: tier, sel: dnsdist }, '{{%s}} p99' % tier),
  ],
  'ms',
  grid(8, 8, 0, 86),
);

local fleetCpu = ts(
  'Serving CPU by container, cores',
  'CPU each serving pod used, by container: dnsdist (the resolver front), serving (CoreDNS), agent (receives and activates configuration) and recursor (public recursion).',
  [qRange('sum by (pod, container) (rate(container_cpu_usage_seconds_total{%s,container!="",container!="POD"}[$__rate_interval]))' % memberPods, '{{pod}} {{container}}')],
  'short',
  grid(8, 8, 8, 86),
);

local fleetMemory = ts(
  'Serving memory by container',
  'Working set memory of each serving pod, by container.',
  [qRange('sum by (pod, container) (container_memory_working_set_bytes{%s,container!="",container!="POD"})' % memberPods, '{{pod}} {{container}}')],
  'bytes',
  grid(8, 8, 16, 86),
);

lib.dashboard(
  'Private DNS Performance',
  'internal-dns-performance',
  'Private DNS load test runs next to what the control plane, broker and serving fleet did while they ran. Click a run in the table to narrow the dashboard to it, or open the run comparison.',
  ['dns', 'internal-dns', 'private-dns', 'performance'],
)
+ g.dashboard.withRefresh('')
+ g.dashboard.time.withFrom('now-24h')
+ g.dashboard.withLinks(lib.dnsLinks + [
  { type: 'link', title: 'Compare two runs', url: '/d/internal-dns-performance-compare/private-dns-run-comparison', keepTime: true, includeVars: false, icon: 'dashboard' },
])
+ g.dashboard.withAnnotations([lib.loadTestAnnotation])
+ g.dashboard.withVariables([
  lib.datasourceVar,
  scenarioVar,
  commitVar,
  runVar,
  clusterVar,
  memberVar,
])
+ g.dashboard.withPanels([
  runsTable,

  publishRow,
  publishTable,
  publishLag,
  publishLagByResolver,
  publishApi,
  publishMissed,

  queryRow,
  queryLatency,
  queryRate,
  queryFailed,

  passRow,
  passPhases,
  zoneSteps,
  phaseShare,
  phaseP90,
  passDuration,
  queueWait,

  apiRow,
  apiRequests,
  clientThrottle,
  loopResults,
  loopTime,
  controlPlaneCpu,
  controlPlaneMemory,

  servingRow,
  pending,
  resolverQueries,
  answersByRcode,
  resolverLatency,
  fleetCpu,
  fleetMemory,
])
