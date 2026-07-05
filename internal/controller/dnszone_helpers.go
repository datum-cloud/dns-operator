// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"sort"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

func normalizeStringSlice(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func normalizeDomainRef(in *dnsv1alpha1.DomainRef) *dnsv1alpha1.DomainRef {
	if in == nil {
		return nil
	}
	out := *in
	out.Status.Nameservers = normalizeDomainNameservers(out.Status.Nameservers)
	return &out
}

func normalizeDomainNameservers(in []networkingv1alpha.Nameserver) []networkingv1alpha.Nameserver {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]networkingv1alpha.Nameserver, 0, len(in))
	for i := range in {
		if _, ok := seen[in[i].Hostname]; ok {
			continue
		}
		seen[in[i].Hostname] = struct{}{}
		ns := in[i]
		if len(ns.IPs) > 0 {
			ips := append([]networkingv1alpha.NameserverIP(nil), ns.IPs...)
			sort.Slice(ips, func(a, b int) bool {
				return ips[a].Address < ips[b].Address
			})
			ns.IPs = ips
		}
		out = append(out, ns)
	}
	sort.Slice(out, func(a, b int) bool {
		return out[a].Hostname < out[b].Hostname
	})
	return out
}
