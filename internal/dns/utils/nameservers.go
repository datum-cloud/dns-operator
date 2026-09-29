// SPDX-License-Identifier: AGPL-3.0-only

package utils

import dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"

// ClassNameservers returns the nameservers a DNSZoneClass assigns to its zones,
// normalized. It returns nil when the class assigns none.
func ClassNameservers(class dnsv1alpha1.DNSZoneClass) []string {
	policy := class.Spec.NameServerPolicy
	if policy == nil || policy.Mode != dnsv1alpha1.NameServerPolicyModeStatic || policy.Static == nil {
		return nil
	}
	return NormalizeStringSlice(policy.Static.Servers)
}
