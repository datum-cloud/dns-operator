// SPDX-License-Identifier: AGPL-3.0-only

// Package claims decides which DNSRecordSet holds an owner name that several
// record sets claim, by the rule docs/architecture/record-ownership.md states.
// The admission webhook and the downstream reconciler both decide through it,
// so the two cannot disagree about who was first.
package claims

import (
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	pdnsclient "go.miloapis.com/dns-operator/internal/dns/pdns"
)

// FirstClaimant returns whichever of two record sets claiming one owner name
// holds it: the older by creation time, and at one creation time the lower
// name, so the answer never depends on the order they are compared in.
func FirstClaimant(a, b *dnsv1alpha1.DNSRecordSet) *dnsv1alpha1.DNSRecordSet {
	if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		if a.Name <= b.Name {
			return a
		}
		return b
	}
	if a.CreationTimestamp.Before(&b.CreationTimestamp) {
		return a
	}
	return b
}

// Holders returns, for every owner name that another record set in self's
// namespace, zone and record type claims, the one of them that holds it. It is
// who holds what once self no longer claims anything, so self is not a
// claimant, and neither is a record set being deleted.
//
// Keys are owner names qualified to zoneDomainName as PowerDNS names an RRset,
// so every spelling of one name — "www", "WWW", "www.example.com." — is one
// key.
func Holders(
	candidates []dnsv1alpha1.DNSRecordSet,
	self *dnsv1alpha1.DNSRecordSet,
	zoneDomainName string,
) map[string]*dnsv1alpha1.DNSRecordSet {
	holders := map[string]*dnsv1alpha1.DNSRecordSet{}
	for i := range candidates {
		other := &candidates[i]
		if !contends(other, self) {
			continue
		}
		for _, rec := range other.Spec.Records {
			name := pdnsclient.QualifyOwner(rec.Name, zoneDomainName)
			if held, ok := holders[name]; !ok || FirstClaimant(held, other) == other {
				holders[name] = other
			}
		}
	}
	return holders
}

// contends reports whether other claims owner names in the key space self
// claims in: a live record set other than self, in the same namespace, zone
// and record type.
func contends(other, self *dnsv1alpha1.DNSRecordSet) bool {
	return other.Namespace == self.Namespace &&
		other.Name != self.Name &&
		other.DeletionTimestamp.IsZero() &&
		other.Spec.DNSZoneRef.Name == self.Spec.DNSZoneRef.Name &&
		other.Spec.RecordType == self.Spec.RecordType
}
