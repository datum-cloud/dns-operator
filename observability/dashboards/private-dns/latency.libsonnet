// dnsdist's latency histogram describes answered UDP frontend queries only.
local group = 'cluster, internal_dns_datum_net_tier';
{
  count(selector, window)::
    'sum by (%s) (increase(dnsdist_latency_count{%s}[%s]))' % [group, selector, window],

  atMost(selector, bound, window)::
    'sum by (%s) (increase(dnsdist_latency_bucket{%s,le="%s"}[%s]))' % [group, selector, bound, window],

  over(selector, bound, window)::
    'clamp_min((%s) - (%s), 0)' % [self.count(selector, window), self.atMost(selector, bound, window)],

  share(selector, bound, window, over=false)::
    '(%s) / (%s > 0)' % [
      if over then self.over(selector, bound, window) else self.atMost(selector, bound, window),
      self.count(selector, window),
    ],

  quantile(selector, quantile, window)::
    'histogram_quantile(%s, sum by (%s, le) (rate(dnsdist_latency_bucket{%s}[%s])))' % [quantile, group, selector, window],
}
