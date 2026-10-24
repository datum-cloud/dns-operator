// Private DNS overview dashboard
//
// Is Private DNS for VPCs answering, and is configuration reaching it? Top to
// bottom: headline health, resolver queries, the serving fleet, the control
// plane that compiles contexts, private zone readiness, broker delivery and
// load test results.
//
// Usage:
//   jsonnet -J vendor overview.jsonnet > overview.json
// Or:
//   task build

local lib = import 'lib.libsonnet';
local g = lib.g;

local prometheus = g.query.prometheus;
local timeSeries = g.panel.timeSeries;
local table = g.panel.table;
local var = g.dashboard.variable;

local ds = lib.ds;

local controlPlane = lib.controlPlane;
local loops = 'namespace="internal-dns-system",operation=~"$operation"';
local idleLoops = 'operation!~"ack (read|apply|commit) .*"';
local periodicLoops = 'loop!~"ack (read|apply|commit)"';
local configConsumers = lib.configConsumers;
local controllers = lib.controllers;
local broker = lib.broker;
local fleet = lib.fleet;
local fleetPods = lib.fleetPods;
local tier = lib.tier;
local dnsdist = lib.dnsdist;

local privateZones = 'label_replace(dns_zone_info{visibility="Private"}, "dns_zone_name", "$1", "name", "(.*)")';

local worstErrorRatio(metric, by, selector) =
  'max(((sum by (%(by)s) (increase(%(m)s{%(sel)s,result="error"}[1h])) or 0 * sum by (%(by)s) (increase(%(m)s{%(sel)s}[1h]))) / sum by (%(by)s) (increase(%(m)s{%(sel)s}[1h]))) and on (%(by)s) sum by (%(by)s) (increase(%(m)s{%(sel)s}[1h])) >= 10)'
  % { m: metric, by: by, sel: selector };

local labelled(expr) =
  '(%s) and on () count(dns_zone_info{visibility!=""})' % expr;

local privateZoneCondition(condition) =
  '(dns_zone_status_condition{condition="%s"} and on ("milo.project.name", namespace, name) dns_zone_info{visibility="Private"})' % condition;

local privateRecordSetCondition(condition) =
  '((dns_record_set_status_condition{condition="%s"} * on ("milo.project.name", namespace, name) group_left (dns_zone_name) dns_record_set_info) and on ("milo.project.name", namespace, dns_zone_name) %s)' % [condition, privateZones];

local qInstant = lib.qInstant;
local qRange = lib.qRange;
local grid = lib.grid;
local statPanel = lib.statPanel;
local ts = lib.ts;
local section = lib.section;

local green = lib.green;
local blue = lib.blue;
local anyIsBad = lib.anyIsBad;

local datasourceVar = lib.datasourceVar;

local operationVar =
  var.query.new('operation')
  + var.query.generalOptions.withLabel('Control-plane loop')
  + var.query.generalOptions.withCurrent('All', '$__all')
  + var.query.generalOptions.showOnDashboard.withLabelAndValue()
  + var.query.withDatasource('prometheus', ds)
  + var.query.queryTypes.withLabelValues(label='operation', metric='internal_dns_operations_total{%s}' % controlPlane)
  + var.query.refresh.onTime()
  + var.query.withSort(1)
  + var.query.selectionOptions.withMulti(true)
  + var.query.selectionOptions.withIncludeAll(value=true, customAllValue='.*');

local statFleetUnavailable = statPanel(
  'Serving replicas down',
  'Serving replicas that are not available: node resolvers, regional routers, regional CoreDNS and public recursors. Readiness checks the listener address, the activated configuration and local DNS over UDP and TCP, so an unavailable member is not answering.',
  'sum(kube_deployment_spec_replicas{%s}) - sum(kube_deployment_status_replicas_available{%s})' % [fleet, fleet],
  'short',
  anyIsBad,
  grid(4, 4, 0, 0),
);

local statControlPlane = statPanel(
  'Control plane up',
  'Control-plane replicas available to compile contexts and publish configuration.',
  'sum(kube_deployment_status_replicas_available{%s,deployment="internal-dns-control-plane"})' % controlPlane,
  'short',
  [{ color: 'red', value: null }, { color: 'orange', value: 1 }, { color: 'green', value: 2 }],
  grid(4, 4, 4, 0),
);

local statLoopErrors = statPanel(
  'Worst loop error rate',
  'Share of runs that failed in the last hour, for the worst control-plane loop. Each loop retries, so a few percent is noise; a loop near 100% cannot make progress. Loops with fewer than 10 runs in the hour are left out so one failure cannot read as 50%. Broker reads are left out because they wait for messages. Once the control plane reports conflicts on their own, update conflicts between the two replicas no longer count as failures.',
  '(%s) or (%s) or vector(0)' % [
    worstErrorRatio('internal_dns_loop_runs_total', 'loop, region, shard', '%s,loop!="ack read"' % controlPlane),
    worstErrorRatio('internal_dns_operations_total', 'operation', '%s,operation!~"ack read .*"' % controlPlane),
  ],
  'percentunit',
  [green, { color: 'orange', value: 0.05 }, { color: 'red', value: 0.5 }],
  grid(4, 4, 8, 0),
);

local statStalestLoop = statPanel(
  'Stalest loop',
  'Time since the slowest periodic control-plane loop last succeeded on any replica. Acknowledgement steps only run when a member reports, so they are left out.',
  'max(min by (loop, region, shard) (time() - internal_dns_loop_last_success_timestamp_seconds{%s,%s})) or max(min by (operation) (time() - internal_dns_operation_last_success_timestamp_seconds{%s,%s}))' % [controlPlane, periodicLoops, controlPlane, idleLoops],
  's',
  [green, { color: 'orange', value: 120 }, { color: 'red', value: 600 }],
  grid(4, 4, 12, 0),
);

local statZonesNotProgrammed = statPanel(
  'Private zones pending',
  'Private zones whose Programmed condition is not true in their project. A pending zone answers SERVFAIL beneath its apex while the context keeps serving its other zones. A zone whose network has no serving members in its region stays pending; the table under Private zones and records lists each one with its reason.',
  labelled('count(%s != 1) or vector(0)' % privateZoneCondition('Programmed')),
  'short',
  anyIsBad,
  grid(4, 4, 16, 0),
);

local statBacklog = statPanel(
  'Config backlog',
  'Fewest messages over the last 5 minutes that the most backed-up configuration consumer had not yet received: member publication and record consumers, and the control plane acknowledgement reader. Members keep serving their applied configuration while this is above zero, but changes are not reaching them. Member acknowledgement readers are left out: every member reads every heartbeat, and a new member replays the last day of history. They are under Configuration delivery.',
  'max(min_over_time(nats_consumer_num_pending{%s,%s}[5m]))' % [broker, configConsumers],
  'short',
  [green, { color: 'orange', value: 100 }, { color: 'red', value: 1000 }],
  grid(4, 4, 20, 0),
);

local queriesRow = section('Resolver queries', 100, 4);

local queriesPerResolver = ts(
  'Queries per resolver',
  'Queries each dnsdist front received: the node resolvers at fd53::53 and fd53::54 and the regional routers. Only members with dnsdist metrics turned on report.',
  [qRange('sum by (cluster, %(tier)s, pod) (rate(dnsdist_queries{%(sel)s}[$__rate_interval]))' % { tier: tier, sel: dnsdist }, '{{cluster}} {{%s}} {{pod}}' % tier)],
  'reqps',
  grid(8, 8, 0, 5),
);

local answersByRcode = ts(
  'Answers by response code',
  'What the resolvers sent back to clients. NXDOMAIN is expected for names outside a project. SERVFAIL means a backend failed or did not answer in time. A query can pass through more than one tier, so the tiers do not add up to a client total.',
  [
    qRange('sum by (%(tier)s) (rate(dnsdist_frontend_noerror{%(sel)s}[$__rate_interval]))' % { tier: tier, sel: dnsdist }, '{{%s}} NOERROR' % tier),
    qRange('sum by (%(tier)s) (rate(dnsdist_frontend_nxdomain{%(sel)s}[$__rate_interval]))' % { tier: tier, sel: dnsdist }, '{{%s}} NXDOMAIN' % tier),
    qRange('sum by (%(tier)s) (rate(dnsdist_frontend_servfail{%(sel)s}[$__rate_interval]))' % { tier: tier, sel: dnsdist }, '{{%s}} SERVFAIL' % tier),
  ],
  'reqps',
  grid(8, 8, 8, 5),
);

local answerTime = ts(
  'Answer time, p50 and p99',
  'Time from query to answer at each tier. dnsdist counts answers in 1, 10, 50, 100 and 1000 ms buckets, so a value is an estimate within its bucket: anything under 1 ms is under 1 ms, not exactly that, and 1000 ms means 1 s or more.',
  [
    qRange('histogram_quantile(0.5, sum by (%(tier)s, le) (rate(dnsdist_latency_bucket{%(sel)s}[$__rate_interval])))' % { tier: tier, sel: dnsdist }, '{{%s}} p50' % tier),
    qRange('histogram_quantile(0.99, sum by (%(tier)s, le) (rate(dnsdist_latency_bucket{%(sel)s}[$__rate_interval])))' % { tier: tier, sel: dnsdist }, '{{%s}} p99' % tier),
  ],
  'ms',
  grid(8, 8, 16, 5),
);

local failedQueries = ts(
  'Unanswered and dropped queries',
  'Queries a backend did not answer in time, could not be sent to a backend, or were dropped by an access rule or for lack of a backend. Clients retry, so a short burst usually shows up as slow answers rather than failures.',
  [
    qRange('sum by (%(tier)s) (rate(dnsdist_downstream_timeouts{%(sel)s}[$__rate_interval]))' % { tier: tier, sel: dnsdist }, '{{%s}} backend timeout' % tier),
    qRange('sum by (%(tier)s) (rate(dnsdist_downstream_send_errors{%(sel)s}[$__rate_interval]))' % { tier: tier, sel: dnsdist }, '{{%s}} backend send error' % tier),
    qRange('sum by (%(tier)s) (rate(dnsdist_acl_drops{%(sel)s}[$__rate_interval]) + rate(dnsdist_rule_drop{%(sel)s}[$__rate_interval]) + rate(dnsdist_no_policy{%(sel)s}[$__rate_interval]))' % { tier: tier, sel: dnsdist }, '{{%s}} dropped' % tier),
  ],
  'reqps',
  grid(8, 8, 0, 13),
);

local backendReuse = ts(
  'Backend connection reuse',
  'Share of queries each dnsdist front sent to CoreDNS over an already open TCP connection. Near 100% means connections are reused; a drop means each query is paying for a new connection.',
  [qRange('sum by (cluster, %(tier)s, pod) (rate(dnsdist_server_tcpreusedconnections{%(sel)s}[$__rate_interval])) / (sum by (cluster, %(tier)s, pod) (rate(dnsdist_server_tcpreusedconnections{%(sel)s}[$__rate_interval])) + sum by (cluster, %(tier)s, pod) (rate(dnsdist_server_tcpnewconnections{%(sel)s}[$__rate_interval])))' % { tier: tier, sel: dnsdist }, '{{cluster}} {{%s}} {{pod}}' % tier)],
  'percentunit',
  grid(8, 8, 8, 13),
);

local fleetCpu = ts(
  'Serving CPU by container, cores',
  'CPU each serving pod used, in cores, split by container: dnsdist (the resolver front), serving (CoreDNS), agent (receives and activates configuration) and recursor (public recursion). The pod name carries the tier: node, node-b, regional-front, regional-coredns or public-recursor.',
  [qRange('sum by (cluster, pod, container) (rate(container_cpu_usage_seconds_total{%s,container!="",container!="POD"}[$__rate_interval]))' % fleetPods, '{{cluster}} {{pod}} {{container}}')],
  'short',
  grid(8, 8, 16, 13),
);

local recursorRow = section('Public recursor', 150, 21);

// The public recursor answers names outside every project. Its pods are
// labelled with the public-recursor tier, and it reports PowerDNS Recursor
// counters as pdns_recursor_*.
local recursor = 'internal_dns_datum_net_tier="public-recursor"';

local recursorQueries = ts(
  'Recursor queries',
  'Questions each public recursor pod received per second.',
  [qRange('sum by (cluster, pod) (rate(pdns_recursor_questions{%s}[$__rate_interval]))' % recursor, '{{cluster}} {{pod}}')],
  'reqps',
  grid(8, 8, 0, 22),
);

local recursorAnswerShare = ts(
  'Recursor answer time, share of answers',
  'Share of answers in each time band, across the recursor pods. Answers from cache land under 1 ms; anything over 100 ms waited on an authoritative server.',
  [
    qRange('sum(rate(pdns_recursor_answers0_1{%(s)s}[$__rate_interval])) / sum(rate(pdns_recursor_questions{%(s)s}[$__rate_interval]))' % { s: recursor }, 'under 1 ms'),
    qRange('sum(rate(pdns_recursor_answers1_10{%(s)s}[$__rate_interval])) / sum(rate(pdns_recursor_questions{%(s)s}[$__rate_interval]))' % { s: recursor }, '1 to 10 ms'),
    qRange('sum(rate(pdns_recursor_answers10_100{%(s)s}[$__rate_interval])) / sum(rate(pdns_recursor_questions{%(s)s}[$__rate_interval]))' % { s: recursor }, '10 to 100 ms'),
    qRange('sum(rate(pdns_recursor_answers100_1000{%(s)s}[$__rate_interval])) / sum(rate(pdns_recursor_questions{%(s)s}[$__rate_interval]))' % { s: recursor }, '100 ms to 1 s'),
    qRange('sum(rate(pdns_recursor_answers_slow{%(s)s}[$__rate_interval])) / sum(rate(pdns_recursor_questions{%(s)s}[$__rate_interval]))' % { s: recursor }, 'over 1 s'),
  ],
  'percentunit',
  grid(8, 8, 8, 22),
  stack=true,
);

local recursorLatency = ts(
  'Recursor average answer time',
  'Moving average of the time each recursor pod took to answer, as the recursor reports it.',
  [qRange('max by (cluster, pod) (pdns_recursor_qa_latency{%s}) / 1000' % recursor, '{{cluster}} {{pod}}')],
  'ms',
  grid(8, 8, 16, 22),
);

local recursorRcodes = ts(
  'Recursor answers by response code',
  'What the public recursors sent back. NXDOMAIN is normal for names that do not exist; SERVFAIL means the recursor could not get an answer.',
  [
    qRange('sum(rate(pdns_recursor_noerror_answers{%s}[$__rate_interval]))' % recursor, 'NOERROR'),
    qRange('sum(rate(pdns_recursor_nxdomain_answers{%s}[$__rate_interval]))' % recursor, 'NXDOMAIN'),
    qRange('sum(rate(pdns_recursor_servfail_answers{%s}[$__rate_interval]))' % recursor, 'SERVFAIL'),
  ],
  'reqps',
  grid(8, 8, 0, 30),
);

local recursorServfail = ts(
  'Recursor SERVFAIL share',
  'Share of questions each recursor pod answered with SERVFAIL. A rise usually means an upstream or authoritative server is failing.',
  [qRange('sum by (cluster, pod) (rate(pdns_recursor_servfail_answers{%(s)s}[$__rate_interval])) / sum by (cluster, pod) (rate(pdns_recursor_questions{%(s)s}[$__rate_interval]))' % { s: recursor }, '{{cluster}} {{pod}}')],
  'percentunit',
  grid(8, 8, 8, 30),
);

local recursorUpstream = ts(
  'Recursor upstream problems',
  'Queries the recursor sent out that timed out or could not be sent, queries it dropped because it was at capacity or a resource limit, and answers that failed DNSSEC validation.',
  [
    qRange('sum(rate(pdns_recursor_outgoing_timeouts{%s}[$__rate_interval]))' % recursor, 'outgoing timeouts'),
    qRange('sum(rate(pdns_recursor_unreachables{%s}[$__rate_interval]))' % recursor, 'server unreachable'),
    qRange('sum(rate(pdns_recursor_resource_limits{%s}[$__rate_interval]))' % recursor, 'resource limit'),
    qRange('sum(rate(pdns_recursor_over_capacity_drops{%s}[$__rate_interval]))' % recursor, 'dropped over capacity'),
    qRange('sum(rate(pdns_recursor_dnssec_result_bogus{%s}[$__rate_interval]))' % recursor, 'DNSSEC bogus'),
  ],
  'ops',
  grid(8, 8, 16, 30),
);

local recursorCacheHits = ts(
  'Recursor cache hit ratio',
  'Share of lookups each recursor pod answered from its cache.',
  [qRange('sum by (cluster, pod) (rate(pdns_recursor_cache_hits{%(s)s}[$__rate_interval])) / (sum by (cluster, pod) (rate(pdns_recursor_cache_hits{%(s)s}[$__rate_interval])) + sum by (cluster, pod) (rate(pdns_recursor_cache_misses{%(s)s}[$__rate_interval])))' % { s: recursor }, '{{cluster}} {{pod}}')],
  'percentunit',
  grid(8, 8, 0, 38),
);

local recursorLoad = ts(
  'Recursor cache size and queries in flight',
  'Entries in each pod\'s record cache (left axis) and queries it is resolving right now (right axis). Queries in flight that keep climbing mean the recursor is waiting on slow upstreams.',
  [
    qRange('max by (cluster, pod) (pdns_recursor_cache_entries{%s})' % recursor, '{{pod}} cache entries'),
    qRange('max by (cluster, pod) (pdns_recursor_concurrent_queries{%s})' % recursor, '{{pod}} in flight'),
  ],
  'short',
  grid(8, 8, 8, 38),
) + timeSeries.standardOptions.withOverrides([
  timeSeries.fieldOverride.byRegexp.new('.* in flight')
  + timeSeries.fieldOverride.byRegexp.withProperty('custom.axisPlacement', 'right'),
]);

local recursorResources = ts(
  'Recursor CPU and memory',
  'CPU in cores (left axis) and working set memory (right axis) of each recursor container. A pod near its CPU limit answers late.',
  [
    qRange('sum by (cluster, pod) (rate(container_cpu_usage_seconds_total{namespace=~"ns-.*",pod=~"internal-dns-public-recursor-.*",container="recursor"}[$__rate_interval]))', '{{pod}} CPU'),
    qRange('sum by (cluster, pod) (container_memory_working_set_bytes{namespace=~"ns-.*",pod=~"internal-dns-public-recursor-.*",container="recursor"})', '{{pod}} memory'),
  ],
  'short',
  grid(8, 8, 16, 38),
) + timeSeries.standardOptions.withOverrides([
  timeSeries.fieldOverride.byRegexp.new('.* memory')
  + timeSeries.fieldOverride.byRegexp.withProperty('unit', 'bytes')
  + timeSeries.fieldOverride.byRegexp.withProperty('custom.axisPlacement', 'right'),
]);

local fleetRow = section('Serving fleet', 200, 46);

local availablePerMember = ts(
  'Available replicas per member',
  'Available replicas of each serving deployment at each location. A member that drops to zero is out of the regional pool or, for the node resolver, out of service for its node.',
  [qRange('max by (cluster, deployment) (kube_deployment_status_replicas_available{%s})' % fleet, '{{cluster}} {{deployment}}')],
  'short',
  grid(8, 6, 0, 47),
);

local notReadyPods = ts(
  'Serving pods not ready',
  'Serving pods failing readiness. Readiness stays local to the pod, so this reflects the member and not the consumer path through Galactic.',
  [qRange('sum by (cluster, pod) (kube_pod_status_ready{%s,condition="false"})' % fleetPods, '{{cluster}} {{pod}}')],
  'short',
  grid(8, 6, 6, 47),
);

local fleetRestarts = ts(
  'Serving container restarts',
  'Container restarts in serving pods. CoreDNS restarts during activation are expected; repeated restarts of one container are not.',
  [qRange('sum by (cluster, pod, container) (increase(kube_pod_container_status_restarts_total{%s}[$__rate_interval]))' % fleetPods, '{{pod}} {{container}}')],
  'short',
  grid(8, 6, 12, 47),
);

local fleetMemory = ts(
  'Serving memory by container',
  'Working set memory of each serving pod, split by container. A container that climbs toward its limit is killed and restarted.',
  [qRange('sum by (cluster, pod, container) (container_memory_working_set_bytes{%s,container!="",container!="POD"})' % fleetPods, '{{cluster}} {{pod}} {{container}}')],
  'bytes',
  grid(8, 6, 18, 47),
);

local controlPlaneRow = section('Control plane', 300, 55);

local operationsByResult = ts(
  'Loop runs by result',
  'Completed runs of each control-plane loop. A successful run can be idle; it does not prove that an endpoint is serving.',
  [qRange('sum by (operation, result) (rate(internal_dns_operations_total{%s}[$__rate_interval]))' % loops, '{{operation}} {{result}}')],
  'ops',
  grid(8, 8, 0, 56),
);

local operationErrors = ts(
  'Loop errors',
  'Failed runs per loop. Planner and outbox errors stop configuration reaching the fleet; acknowledgement errors stop status reaching projects.',
  [qRange('sum by (operation) (rate(internal_dns_operations_total{%s,result="error"}[$__rate_interval]))' % loops, '{{operation}}')],
  'ops',
  grid(8, 8, 8, 56),
);

local sinceLastSuccess = ts(
  'Time since last success',
  'Age of the latest successful run of each loop, across control-plane replicas. Acknowledgement reads advance only when a member reports.',
  [qRange('min by (operation) (time() - internal_dns_operation_last_success_timestamp_seconds{%s})' % loops, '{{operation}}')],
  's',
  grid(8, 8, 16, 56),
);

local reconcilesByResult = ts(
  'Project reconciles by result',
  'Reconciles of project resources across every source project. Each project runs its own controller, so they are summed here.',
  [qRange('sum by (result) (rate(controller_runtime_reconcile_total{%s}[$__rate_interval]))' % controllers, '{{result}}')],
  'ops',
  grid(8, 8, 0, 64),
  stack=true,
);

local reconcileLatency = ts(
  'Reconcile time, p99',
  'Slowest 1% of project reconciles. Every reconcile reads the source project API directly, so this follows project API latency.',
  [qRange('histogram_quantile(0.99, sum by (le) (rate(controller_runtime_reconcile_time_seconds_bucket{%s}[$__rate_interval])))' % controllers, 'p99')],
  's',
  grid(8, 8, 8, 64),
);

local workqueueDepth = ts(
  'Reconcile queue depth',
  'Project reconciles waiting, summed across projects. A queue that keeps growing means changes are not being compiled.',
  [qRange('sum(workqueue_depth{%s,name=~"internal-dns-control-plane-.*"})' % controlPlane, 'waiting')],
  'short',
  grid(8, 8, 16, 64),
);

local zonesRow = section('Private zones and records', 400, 72);

local statPrivateZones = statPanel(
  'Private zones',
  'Private zones across every project.',
  labelled('count(dns_zone_info{visibility="Private"}) or vector(0)'),
  'short',
  blue,
  grid(4, 8, 0, 73),
);

local statRecordSets = statPanel(
  'Private record sets',
  'Record sets in private zones across every project.',
  labelled('count(%s) or vector(0)' % privateRecordSetCondition('Programmed')),
  'short',
  blue,
  grid(4, 8, 8, 73),
);

local statRecordSetsNotProgrammed = statPanel(
  'Private record sets not programmed',
  'Record sets in private zones whose Programmed condition is not true.',
  labelled('count(%s != 1) or vector(0)' % privateRecordSetCondition('Programmed')),
  'short',
  anyIsBad,
  grid(4, 8, 16, 73),
);

local zonesTable =
  table.new('Private zones not programmed')
  + table.panelOptions.withGridPos(8, 24, 0, 77)
  + table.panelOptions.withDescription('Every private zone whose Programmed condition is not true, with its reason.')
  + table.queryOptions.withDatasource('prometheus', ds)
  + table.queryOptions.withTargets([qInstant(privateZoneCondition('Programmed') + ' != 1') + prometheus.withFormat('table')])
  + table.queryOptions.withTransformations([{
    id: 'organize',
    options: {
      excludeByName: { Time: true, Value: true },
      includeByName: { 'milo.project.name': true, namespace: true, name: true, status: true, reason: true },
      renameByName: { 'milo.project.name': 'Project', namespace: 'Namespace', name: 'Zone', status: 'Programmed', reason: 'Reason' },
      indexByName: { 'milo.project.name': 0, namespace: 1, name: 2, status: 3, reason: 4 },
    },
  }])
  + table.options.withShowHeader(true)
  + table.options.withSortBy([{ displayName: 'Project', desc: false }]);

local deliveryRow = section('Configuration delivery', 500, 85);

local pendingPerConsumer = ts(
  'Undelivered messages per consumer',
  'Publications and acknowledgements waiting for each broker consumer. Each serving member has its own consumers; one that climbs alone is a member that stopped reading.',
  [qRange('max by (stream_name, consumer_name) (nats_consumer_num_pending{%s})' % broker, '{{stream_name}} {{consumer_name}}')],
  'short',
  grid(8, 8, 0, 86),
);

local ackPendingPerConsumer = ts(
  'Delivered but not acknowledged',
  'Messages a consumer received and has not acknowledged. A member that cannot validate or activate a snapshot holds its messages here.',
  [qRange('max by (stream_name, consumer_name) (nats_consumer_num_ack_pending{%s})' % broker, '{{stream_name}} {{consumer_name}}')],
  'short',
  grid(8, 8, 8, 86),
);

local brokerConnections = ts(
  'Broker connections',
  'Client connections on each broker server. The control-plane replicas and every serving member hold one each.',
  [qRange('max by (pod) (nats_varz_connections{%s})' % broker, '{{pod}}')],
  'short',
  grid(8, 8, 16, 86),
);

local loadTestRow = section('Load tests', 600, 94);

local publishLag = ts(
  'Time to answer a published change',
  'Worst time in each interval from a record write to the first answer from the VPC resolver, by test scenario and step. Bars on the right axis count changes that were never answered.',
  [
    qRange('max by (scenario, step, resolver) (max_over_time(idns_perf_publish_lag_ms[$__interval]))', '{{scenario}} {{step}} {{resolver}}'),
    qRange('sum by (scenario, step) (count_over_time(idns_perf_publish_missed[$__interval]))', '{{scenario}} {{step}} never answered'),
  ],
  'ms',
  grid(8, 8, 0, 95),
) + timeSeries.queryOptions.withInterval('1s')
  + timeSeries.fieldConfig.defaults.custom.withDrawStyle('points')
  + timeSeries.fieldConfig.defaults.custom.withShowPoints('always')
  + timeSeries.fieldConfig.defaults.custom.withPointSize(4)
  + timeSeries.standardOptions.withOverrides([
    timeSeries.fieldOverride.byRegexp.new('.*never answered')
    + timeSeries.fieldOverride.byRegexp.withProperty('custom.axisPlacement', 'right')
    + timeSeries.fieldOverride.byRegexp.withProperty('custom.drawStyle', 'bars')
    + timeSeries.fieldOverride.byRegexp.withProperty('unit', 'short')
    + timeSeries.fieldOverride.byRegexp.withProperty('color', { mode: 'fixed', fixedColor: 'red' }),
  ]);

local queryP99 = ts(
  'Load test query time (p99)',
  'Worst per-second p99 of load test queries in each interval, by scenario and query kind, taken from the generator with the slowest answers. Timeouts count as 5000 ms.',
  [qRange('max by (scenario, kind) (max_over_time(idns_perf_query_p99_ms[$__interval]))', '{{scenario}} {{kind}}')],
  'ms',
  grid(8, 8, 8, 95),
) + timeSeries.queryOptions.withInterval('1s');

local queryRate = ts(
  'Load test queries and failures',
  'Average queries per second the load test sent, and failed queries per second, across all generators, by scenario and query kind.',
  [
    qRange('sum by (scenario, kind) (avg_over_time(idns_perf_query_qps[$__interval]))', '{{scenario}} {{kind}} sent'),
    qRange('sum by (scenario, kind) (avg_over_time(idns_perf_query_failed[$__interval])) > 0', '{{scenario}} {{kind}} failed'),
  ],
  'short',
  grid(8, 8, 16, 95),
) + timeSeries.queryOptions.withInterval('1s');

local loadTestAnnotation = lib.loadTestAnnotation;

g.dashboard.new('Private DNS')
+ g.dashboard.withUid('internal-dns')
+ g.dashboard.withDescription('Is Private DNS for VPCs answering, and is configuration reaching it? Covers the resolvers at fd53::53 and fd53::54, the serving fleet, the control plane that compiles contexts, private zone readiness and broker delivery.')
+ g.dashboard.withTags(['dns', 'internal-dns', 'private-dns'])
+ g.dashboard.withEditable(true)
+ g.dashboard.withSchemaVersion(41)
+ g.dashboard.withRefresh('1m')
+ g.dashboard.time.withFrom('now-6h')
+ g.dashboard.time.withTo('now')
+ g.dashboard.withLinks(lib.dnsLinks + [
  { type: 'link', title: 'Load test runs', url: '/d/internal-dns-performance/private-dns-performance', keepTime: true, includeVars: false, icon: 'dashboard' },
])
+ g.dashboard.withAnnotations([loadTestAnnotation])
+ g.dashboard.withVariables([
  datasourceVar,
  operationVar,
])
+ g.dashboard.withPanels([
  statFleetUnavailable,
  statControlPlane,
  statLoopErrors,
  statStalestLoop,
  statZonesNotProgrammed,
  statBacklog,

  queriesRow,
  queriesPerResolver,
  answersByRcode,
  answerTime,
  failedQueries,
  backendReuse,
  fleetCpu,

  recursorRow,
  recursorQueries,
  recursorAnswerShare,
  recursorLatency,
  recursorRcodes,
  recursorServfail,
  recursorUpstream,
  recursorCacheHits,
  recursorLoad,
  recursorResources,

  fleetRow,
  availablePerMember,
  notReadyPods,
  fleetRestarts,
  fleetMemory,

  controlPlaneRow,
  operationsByResult,
  operationErrors,
  sinceLastSuccess,
  reconcilesByResult,
  reconcileLatency,
  workqueueDepth,

  zonesRow,
  statPrivateZones,
  statRecordSets,
  statRecordSetsNotProgrammed,
  zonesTable,

  deliveryRow,
  pendingPerConsumer,
  ackPendingPerConsumer,
  brokerConnections,

  loadTestRow,
  publishLag,
  queryP99,
  queryRate,
])
