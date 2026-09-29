// SPDX-License-Identifier: AGPL-3.0-only

// Package claims decides which DNSRecordSet holds an owner name that several
// claim, by the rule in docs/architecture/record-ownership.md.
package claims

import (
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// Precedes reports whether a's claim comes before b's: the older record set
// first, and at one creation time the lower name.
func Precedes(a, b *dnsv1alpha1.DNSRecordSet) bool {
	if a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.Name < b.Name
	}
	return a.CreationTimestamp.Before(&b.CreationTimestamp)
}
