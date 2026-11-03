local q = import '../latency.libsonnet';
local selector = 'container="dnsdist"';
local labels(cluster, tier) = 'cluster="%s", internal_dns_datum_net_tier="%s"' % [cluster, tier];
local series(cluster, tier, count, fast, finite) = [
  { series: 'dnsdist_latency_count{container="dnsdist",%s}' % labels(cluster, tier), values: count },
] + [
  { series: 'dnsdist_latency_bucket{container="dnsdist",%s,le="%s"}' % [labels(cluster, tier), bound], values: if bound == '+Inf' then count else if bound == '1000' then finite else fast }
  for bound in ['1', '10', '50', '100', '1000', '+Inf']
];
local sample(value, cluster='central', tier='node') = { labels: '{%s}' % labels(cluster, tier), value: value };
local check(expr, samples) = { expr: expr, eval_time: '45s', exp_samples: samples };
local one(expr, value) = check(expr, [sample(value)]);
local window = '1m';
{
  evaluation_interval: '15s',
  tests: [
    {
      name: 'Two tail answers among 150 produce a 325ms estimated p99',
      interval: '15s',
      input_series: series('central', 'node', '0 50 100 150', '0 50 100 148', '0 50 100 150'),
      promql_expr_test: [
        one(q.quantile(selector, '0.99', window), 325),
        one(q.count(selector, window), 150),
        one(q.over(selector, '100', window), 2),
        one(q.over(selector, '1000', window), 0),
        one(q.share(selector, '1', window), 148 / 150),
        one(q.share(selector, '100', window, true), 2 / 150),
      ],
    },
    {
      name: 'Two tail answers among 143 produce a 356.5ms estimated p99',
      interval: '15s',
      input_series: series('central', 'node', '0 50 100 143', '0 50 100 141', '0 50 100 143'),
      promql_expr_test: [one('round(%s, 0.000001)' % q.quantile(selector, '0.99', window), 356.5)],
    },
    {
      name: 'Overflow quantile is capped, but over-one-second count and share remain visible',
      interval: '15s',
      input_series: series('central', 'node', '0 30 60 100', '0 30 60 98', '0 30 60 98'),
      promql_expr_test: [
        one(q.quantile(selector, '0.99', window), 1000),
        one(q.over(selector, '1000', window), 2),
        one(q.share(selector, '1000', window, true), 0.02),
      ],
    },
    {
      name: 'Sub-millisecond quantiles interpolate while the finite bucket share is one',
      interval: '15s',
      input_series: series('central', 'node', '0 30 60 100', '0 30 60 100', '0 30 60 100'),
      promql_expr_test: [
        one(q.quantile(selector, '0.5', window), 0.5),
        one(q.quantile(selector, '0.99', window), 0.99),
        one(q.share(selector, '1', window), 1),
      ],
    },
    {
      name: 'Clusters and primary-secondary tiers retain independent volumes',
      interval: '15s',
      input_series: series('central', 'node', '0 50 100 150', '0 50 100 148', '0 50 100 150')
                    + series('central', 'node-b', '0 100000 200000 384000', '0 100000 200000 384000', '0 100000 200000 384000')
                    + series('east', 'node', '0 10 20 30', '0 10 20 30', '0 10 20 30')
                    + [{ series: 'dnsdist_responses{container="dnsdist",%s}' % labels('central', 'node'), values: '0 100000 200000 384186' }],
      promql_expr_test: [
        check(q.count(selector, window), [sample(150), sample(384000, tier='node-b'), sample(30, cluster='east')]),
        check(q.share(selector, '100', window, true), [sample(2 / 150), sample(0, tier='node-b'), sample(0, cluster='east')]),
      ],
    },
    {
      name: 'Counter resets do not become slow answers',
      interval: '15s',
      input_series: series('central', 'node', '0 5 0 5', '0 4 0 4', '0 5 0 5'),
      promql_expr_test: [
        one(q.count(selector, window), 10),
        one(q.over(selector, '100', window), 2),
        one(q.share(selector, '100', window, true), 0.2),
      ],
    },
    {
      name: 'No answers gives zero count but no misleading zero-percent share',
      interval: '15s',
      input_series: series('central', 'node', '0 0 0 0', '0 0 0 0', '0 0 0 0'),
      promql_expr_test: [
        one(q.count(selector, window), 0),
        check(q.share(selector, '1', window), []),
        check(q.share(selector, '1000', window, true), []),
      ],
    },
    {
      name: 'Absent telemetry remains absent',
      interval: '15s',
      input_series: [],
      promql_expr_test: [check(q.count(selector, window), []), check(q.over(selector, '100', window), [])],
    },
  ],
}
