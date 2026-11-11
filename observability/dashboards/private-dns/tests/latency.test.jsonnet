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
local bucket(le, value) = { labels: '{le="%s"}' % le, value: value };
local window = '1m';
{
  evaluation_interval: '15s',
  tests: [
    {
      name: 'Two tail answers among 150 produce a 325ms estimated p99',
      interval: '15s',
      input_series: series('central', 'node', '0 50 100 150', '0 50 100 148', '0 50 100 150'),
      promql_expr_test: [one(q.quantile(selector, '0.99', window), 325)],
    },
    {
      name: 'Two tail answers among 143 produce a 356.5ms estimated p99',
      interval: '15s',
      input_series: series('central', 'node', '0 50 100 143', '0 50 100 141', '0 50 100 143'),
      promql_expr_test: [one('round(%s, 0.000001)' % q.quantile(selector, '0.99', window), 356.5)],
    },
    {
      name: 'Overflow quantile is capped at the last finite bucket',
      interval: '15s',
      input_series: series('central', 'node', '0 30 60 100', '0 30 60 98', '0 30 60 98'),
      promql_expr_test: [one(q.quantile(selector, '0.99', window), 1000)],
    },
    {
      name: 'Sub-millisecond quantiles interpolate',
      interval: '15s',
      input_series: series('central', 'node', '0 30 60 100', '0 30 60 100', '0 30 60 100'),
      promql_expr_test: [
        one(q.quantile(selector, '0.5', window), 0.5),
        one(q.quantile(selector, '0.99', window), 0.99),
      ],
    },
    {
      name: 'Node heatmap adds node and node-b in one cluster and leaves out other clusters and the regional tier',
      interval: '15s',
      input_series: series('central', 'node', '0 50 100 150', '0 50 100 148', '0 50 100 150')
                    + series('central', 'node-b', '0 10 20 30', '0 10 20 29', '0 10 20 29')
                    + series('central', 'regional-dnsdist', '0 500 1000 1500', '0 0 0 0', '0 0 0 0')
                    + series('east', 'node', '0 500 1000 1500', '0 0 0 0', '0 0 0 0'),
      promql_expr_test: [
        check(q.buckets('%s,cluster="central",%s' % [selector, q.nodeTiers], window), [
          bucket('1', 177),
          bucket('10', 177),
          bucket('50', 177),
          bucket('100', 177),
          bucket('1000', 179),
          bucket('+Inf', 180),
        ]),
      ],
    },
    {
      name: 'Regional heatmap reads only the regional tier',
      interval: '15s',
      input_series: series('central', 'node', '0 50 100 150', '0 50 100 150', '0 50 100 150')
                    + series('central', 'regional-dnsdist', '0 5 10 20', '0 5 10 18', '0 5 10 19'),
      promql_expr_test: [
        check(q.buckets('%s,cluster="central",%s' % [selector, q.regionalTier], window), [
          bucket('1', 18),
          bucket('10', 18),
          bucket('50', 18),
          bucket('100', 18),
          bucket('1000', 19),
          bucket('+Inf', 20),
        ]),
      ],
    },
    {
      name: 'Heatmap shares divide each cumulative bucket by the cluster total and ignore other clusters',
      interval: '15s',
      input_series: series('central', 'node', '0 5 10 20', '0 5 10 18', '0 5 10 19')
                    + series('east', 'node', '0 500 1000 1500', '0 0 0 0', '0 0 0 0'),
      promql_expr_test: [
        check(q.shares('%s,cluster="central",%s' % [selector, q.nodeTiers], window), [
          bucket('1', 0.9),
          bucket('10', 0.9),
          bucket('50', 0.9),
          bucket('100', 0.9),
          bucket('1000', 0.95),
          bucket('+Inf', 1),
        ]),
      ],
    },
    {
      name: 'Heatmap shares leave out intervals with no answers',
      interval: '15s',
      input_series: series('central', 'node', '0 0 0 0', '0 0 0 0', '0 0 0 0'),
      promql_expr_test: [check(q.shares('%s,cluster="central",%s' % [selector, q.nodeTiers], window), [])],
    },
    {
      name: 'Counter resets do not become a burst of answers',
      interval: '15s',
      input_series: series('central', 'node', '0 5 0 5', '0 4 0 4', '0 5 0 5'),
      promql_expr_test: [
        check(q.buckets('%s,cluster="central",%s' % [selector, q.nodeTiers], window), [
          bucket('1', 8),
          bucket('10', 8),
          bucket('50', 8),
          bucket('100', 8),
          bucket('1000', 10),
          bucket('+Inf', 10),
        ]),
      ],
    },
    {
      name: 'Absent telemetry remains absent',
      interval: '15s',
      input_series: [],
      promql_expr_test: [check(q.buckets('%s,%s' % [selector, q.nodeTiers], window), [])],
    },
  ],
}
