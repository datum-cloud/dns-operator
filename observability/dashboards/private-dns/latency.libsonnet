// dnsdist's latency histogram describes answered UDP frontend queries only.
local group = 'cluster, internal_dns_datum_net_tier';
{
  nodeTiers: 'internal_dns_datum_net_tier=~"node|node-b"',
  regionalTier: 'internal_dns_datum_net_tier="regional-dnsdist"',

  buckets(selector, window)::
    'sum by (le) (increase(dnsdist_latency_bucket{%s}[%s]))' % [selector, window],

  // Cumulative buckets over the interval's total; Grafana's heatmap format
  // then turns them into each band's share. Intervals with no answers drop out.
  shares(selector, window)::
    '%s / ignoring (le) group_left () (sum(increase(dnsdist_latency_bucket{%s,le="+Inf"}[%s])) > 0)' % [self.buckets(selector, window), selector, window],

  quantile(selector, quantile, window)::
    'histogram_quantile(%s, sum by (%s, le) (rate(dnsdist_latency_bucket{%s}[%s])))' % [quantile, group, selector, window],
}
