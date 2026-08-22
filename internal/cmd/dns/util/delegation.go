// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"strings"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// Delegation state words. The first word is the filter token, as elsewhere.
const (
	DelegationComplete   = "Complete"
	DelegationPartial    = "Partial"
	DelegationIncomplete = "Incomplete"
	DelegationUnknown    = "Unknown"
)

// Delegation is the client-side comparison of the nameservers Datum assigned to
// a zone against the ones the registrar actually publishes.
//
// Verification lives on the Domain object rather than the DNSZone, so the
// observed side comes from status.domainRef.status.nameservers[].hostname while
// the expected side comes from status.nameservers.
type Delegation struct {
	// State is one of Complete, Partial, Incomplete, or Unknown.
	State string
	// Expected is the assigned nameserver list, in the API's own spelling.
	Expected []string
	// Observed is the registrar's nameserver list, in the API's own spelling.
	Observed []string
	// SetCount is how many expected nameservers appear in Observed.
	SetCount int
	// Total is len(Expected).
	Total int
	// Linked reports whether the zone has a Domain object at all, which
	// separates the two reasons State can be Unknown: no Domain to check
	// against, or a Domain that has not been checked yet.
	Linked bool
}

// IsSet reports whether the given expected nameserver appears in the observed
// list, so a describe view can annotate each line.
func (d Delegation) IsSet(nameserver string) bool {
	want := normalizeNameserver(nameserver)
	for _, o := range d.Observed {
		if normalizeNameserver(o) == want {
			return true
		}
	}
	return false
}

// DelegationState compares a zone's assigned nameservers against the ones
// observed at the registrar. Comparison is lowercase with trailing dots
// stripped — the same normalization the portal makes — while the returned
// slices keep the API's spelling so callers can render the trailing dots users
// expect to paste.
//
// State is Unknown whenever there is nothing to compare against: no assigned
// nameservers, no linked Domain, or a linked Domain whose nameservers have not
// been observed yet.
//
// That last case is the important one. An empty observed list means "we have
// not looked yet", not "the registrar points elsewhere", and the two are
// indistinguishable from the data. Reporting Incomplete for it would tell the
// user — confidently, in writing, and during the ordinary window right after a
// zone is created — something false about a third party's configuration, and
// send them to their registrar to fix what is not broken. An unobserved
// registrar is unknown.
func DelegationState(z *dnsv1alpha1.DNSZone) Delegation {
	d := Delegation{State: DelegationUnknown}
	if z == nil {
		return d
	}

	d.Expected = append(d.Expected, z.Status.Nameservers...)
	d.Total = len(d.Expected)

	if z.Status.DomainRef != nil {
		d.Linked = true
		for _, ns := range z.Status.DomainRef.Status.Nameservers {
			d.Observed = append(d.Observed, ns.Hostname)
		}
	}

	// Nothing assigned, nothing linked, or nothing observed yet: all three are
	// "cannot tell", not "not delegated".
	if d.Total == 0 || !d.Linked || len(d.Observed) == 0 {
		return d
	}

	observed := make(map[string]bool, len(d.Observed))
	for _, o := range d.Observed {
		if n := normalizeNameserver(o); n != "" {
			observed[n] = true
		}
	}
	for _, e := range d.Expected {
		if observed[normalizeNameserver(e)] {
			d.SetCount++
		}
	}

	switch d.SetCount {
	case d.Total:
		d.State = DelegationComplete
	case 0:
		d.State = DelegationIncomplete
	default:
		d.State = DelegationPartial
	}
	return d
}

// normalizeNameserver lowercases a hostname and strips the trailing root dot so
// "NS1.Datum.net." and "ns1.datum.net" compare equal.
func normalizeNameserver(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}
