// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"reflect"
	"testing"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// zoneWith builds a DNSZone with the given assigned and registrar nameservers.
// A nil observed slice means the zone has no linked Domain at all.
func zoneWith(assigned []string, observed []string, linked bool) *dnsv1alpha1.DNSZone {
	z := &dnsv1alpha1.DNSZone{Status: dnsv1alpha1.DNSZoneStatus{Nameservers: assigned}}
	if linked {
		ref := &dnsv1alpha1.DomainRef{Name: "example-com"}
		for _, h := range observed {
			ref.Status.Nameservers = append(ref.Status.Nameservers, networkingv1alpha.Nameserver{Hostname: h})
		}
		z.Status.DomainRef = ref
	}
	return z
}

func TestDelegationState(t *testing.T) {
	tests := []struct {
		name         string
		zone         *dnsv1alpha1.DNSZone
		wantState    string
		wantSetCount int
		wantTotal    int
	}{
		{
			name:      "nil zone is unknown",
			zone:      nil,
			wantState: DelegationUnknown,
		},
		{
			name:      "no assigned nameservers is unknown",
			zone:      zoneWith(nil, []string{"ns1.datum.net."}, true),
			wantState: DelegationUnknown,
		},
		{
			name:         "no linked domain is unknown even with assigned nameservers",
			zone:         zoneWith([]string{"ns1.datum.net.", "ns2.datum.net."}, nil, false),
			wantState:    DelegationUnknown,
			wantSetCount: 0,
			wantTotal:    2,
		},
		{
			name: "all nameservers set is complete",
			zone: zoneWith(
				[]string{"ns1.datum.net.", "ns2.datum.net."},
				[]string{"ns1.datum.net.", "ns2.datum.net."}, true),
			wantState:    DelegationComplete,
			wantSetCount: 2,
			wantTotal:    2,
		},
		{
			name: "one of two set is partial",
			zone: zoneWith(
				[]string{"ns1.datum.net.", "ns2.datum.net."},
				[]string{"ns1.datum.net.", "ns-cloud-a2.googledomains.com."}, true),
			wantState:    DelegationPartial,
			wantSetCount: 1,
			wantTotal:    2,
		},
		{
			name: "none set is incomplete",
			zone: zoneWith(
				[]string{"ns1.datum.net.", "ns2.datum.net."},
				[]string{"ns-cloud-a1.googledomains.com.", "ns-cloud-a2.googledomains.com."}, true),
			wantState:    DelegationIncomplete,
			wantSetCount: 0,
			wantTotal:    2,
		},
		{
			// The registrar has not been observed yet, which is not the same
			// as a registrar pointing elsewhere. Reporting Incomplete here
			// would send the user to fix what may not be broken.
			name:         "a linked domain with no observed nameservers is unknown",
			zone:         zoneWith([]string{"ns1.datum.net.", "ns2.datum.net."}, nil, true),
			wantState:    DelegationUnknown,
			wantSetCount: 0,
			wantTotal:    2,
		},
		{
			// Observed and genuinely wrong: this one really is Incomplete, and
			// the distinction from the case above is the whole point.
			name: "a linked domain observing only other nameservers is incomplete",
			zone: zoneWith(
				[]string{"ns1.datum.net.", "ns2.datum.net."},
				[]string{"ns-cloud-a1.googledomains.com."}, true),
			wantState:    DelegationIncomplete,
			wantSetCount: 0,
			wantTotal:    2,
		},
		{
			name: "trailing dots are normalized on both sides",
			zone: zoneWith(
				[]string{"ns1.datum.net.", "ns2.datum.net"},
				[]string{"ns1.datum.net", "ns2.datum.net."}, true),
			wantState:    DelegationComplete,
			wantSetCount: 2,
			wantTotal:    2,
		},
		{
			name: "case is normalized",
			zone: zoneWith(
				[]string{"NS1.Datum.NET."},
				[]string{"ns1.datum.net."}, true),
			wantState:    DelegationComplete,
			wantSetCount: 1,
			wantTotal:    1,
		},
		{
			name: "surrounding whitespace is normalized",
			zone: zoneWith(
				[]string{" ns1.datum.net. "},
				[]string{"ns1.datum.net"}, true),
			wantState:    DelegationComplete,
			wantSetCount: 1,
			wantTotal:    1,
		},
		{
			name: "extra registrar nameservers do not break completeness",
			zone: zoneWith(
				[]string{"ns1.datum.net."},
				[]string{"ns1.datum.net.", "ns9.elsewhere.example."}, true),
			wantState:    DelegationComplete,
			wantSetCount: 1,
			wantTotal:    1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DelegationState(tc.zone)
			if got.State != tc.wantState {
				t.Errorf("State = %q, want %q", got.State, tc.wantState)
			}
			if got.SetCount != tc.wantSetCount {
				t.Errorf("SetCount = %d, want %d", got.SetCount, tc.wantSetCount)
			}
			if got.Total != tc.wantTotal {
				t.Errorf("Total = %d, want %d", got.Total, tc.wantTotal)
			}
		})
	}
}

func TestDelegationPreservesAPISpelling(t *testing.T) {
	// The comparison normalizes, but the rendered lists keep the trailing dots
	// users must paste into a registrar.
	z := zoneWith([]string{"NS1.Datum.net."}, []string{"ns1.datum.net"}, true)
	got := DelegationState(z)

	if !reflect.DeepEqual(got.Expected, []string{"NS1.Datum.net."}) {
		t.Errorf("Expected = %#v, want the API's own spelling", got.Expected)
	}
	if !reflect.DeepEqual(got.Observed, []string{"ns1.datum.net"}) {
		t.Errorf("Observed = %#v, want the API's own spelling", got.Observed)
	}
}

func TestDelegationIsSet(t *testing.T) {
	d := DelegationState(zoneWith(
		[]string{"ns1.datum.net.", "ns2.datum.net."},
		[]string{"NS1.DATUM.NET"}, true))

	if !d.IsSet("ns1.datum.net.") {
		t.Errorf("IsSet(ns1.datum.net.) = false, want true")
	}
	if d.IsSet("ns2.datum.net.") {
		t.Errorf("IsSet(ns2.datum.net.) = true, want false")
	}
}

func TestDelegationLinked(t *testing.T) {
	tests := []struct {
		name string
		zone *dnsv1alpha1.DNSZone
		want bool
	}{
		{
			name: "no domain ref",
			zone: zoneWith([]string{"ns1.datum.net."}, nil, false),
			want: false,
		},
		{
			name: "a domain ref with nothing observed yet",
			zone: zoneWith([]string{"ns1.datum.net."}, nil, true),
			want: true,
		},
		{
			name: "a domain ref with observations",
			zone: zoneWith([]string{"ns1.datum.net."}, []string{"ns1.datum.net."}, true),
			want: true,
		},
		{
			name: "nil zone",
			zone: nil,
			want: false,
		},
	}

	// Linked is what lets a caller word the two Unknown cases differently: "no
	// linked domain to check against" versus "not checked yet".
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := DelegationState(tc.zone).Linked; got != tc.want {
				t.Errorf("Linked = %v, want %v", got, tc.want)
			}
		})
	}
}
