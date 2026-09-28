// SPDX-License-Identifier: AGPL-3.0-only

package claims

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

var base = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

func recordSet(name string, created time.Time, rrType dnsv1alpha1.RRType, zoneRef string, owners ...string) dnsv1alpha1.DNSRecordSet {
	records := make([]dnsv1alpha1.RecordEntry, 0, len(owners))
	for _, owner := range owners {
		records = append(records, dnsv1alpha1.RecordEntry{Name: owner})
	}
	return dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", CreationTimestamp: metav1.NewTime(created)},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zoneRef},
			RecordType: rrType,
			Records:    records,
		},
	}
}

func aRecordSet(name string, created time.Time, owners ...string) dnsv1alpha1.DNSRecordSet {
	return recordSet(name, created, dnsv1alpha1.RRTypeA, "zone-a", owners...)
}

func deleting(rs dnsv1alpha1.DNSRecordSet) dnsv1alpha1.DNSRecordSet {
	now := metav1.NewTime(base)
	rs.DeletionTimestamp = &now
	return rs
}

func TestFirstClaimant(t *testing.T) {
	t.Parallel()

	older := aRecordSet("zeta", base, "www")
	newer := aRecordSet("alpha", base.Add(time.Hour), "www")
	if got := FirstClaimant(&older, &newer); got.Name != "zeta" {
		t.Fatalf("the older record set holds the name, got %q", got.Name)
	}
	if got := FirstClaimant(&newer, &older); got.Name != "zeta" {
		t.Fatalf("the answer must not depend on argument order, got %q", got.Name)
	}

	tieA := aRecordSet("alpha", base, "www")
	tieB := aRecordSet("beta", base, "www")
	if got := FirstClaimant(&tieB, &tieA); got.Name != "alpha" {
		t.Fatalf("at one creation time the lower name holds it, got %q", got.Name)
	}
}

// The holders are what is left when self stops claiming: the claims every other
// live record set of the same type and zone makes, each name to its first
// claimant, keyed the way PowerDNS names an RRset.
func TestHolders(t *testing.T) {
	t.Parallel()

	self := aRecordSet("self", base, "www", "api")

	tests := []struct {
		name   string
		others []dnsv1alpha1.DNSRecordSet
		want   map[string]string
	}{
		{
			name:   "no other record set claims anything",
			others: nil,
			want:   map[string]string{},
		},
		{
			name:   "self does not hold what it gives up",
			others: []dnsv1alpha1.DNSRecordSet{self},
			want:   map[string]string{},
		},
		{
			name:   "another record set of the type and zone holds its names",
			others: []dnsv1alpha1.DNSRecordSet{self, aRecordSet("other", base.Add(time.Hour), "www", "shop")},
			want:   map[string]string{"www.example.com.": "other", "shop.example.com.": "other"},
		},
		{
			name: "a record set being deleted claims nothing",
			others: []dnsv1alpha1.DNSRecordSet{
				deleting(aRecordSet("leaving", base, "www")),
			},
			want: map[string]string{},
		},
		{
			name: "another type or another zone is a different key",
			others: []dnsv1alpha1.DNSRecordSet{
				recordSet("txt", base, dnsv1alpha1.RRTypeTXT, "zone-a", "www"),
				recordSet("elsewhere", base, dnsv1alpha1.RRTypeA, "zone-b", "www"),
			},
			want: map[string]string{},
		},
		{
			name: "a record set in another namespace never contends",
			others: []dnsv1alpha1.DNSRecordSet{
				func() dnsv1alpha1.DNSRecordSet {
					rs := aRecordSet("neighbour", base, "www")
					rs.Namespace = "elsewhere"
					return rs
				}(),
			},
			want: map[string]string{},
		},
		{
			name: "every spelling of a name is one key",
			others: []dnsv1alpha1.DNSRecordSet{
				aRecordSet("upper", base, "WWW"),
				aRecordSet("absolute", base.Add(time.Minute), "api.example.com."),
				aRecordSet("apex", base, "@"),
			},
			want: map[string]string{"www.example.com.": "upper", "api.example.com.": "absolute", "example.com.": "apex"},
		},
		{
			name: "of several claimants the first claimant holds it",
			others: []dnsv1alpha1.DNSRecordSet{
				aRecordSet("newest", base.Add(2*time.Hour), "www"),
				aRecordSet("tie-b", base.Add(time.Hour), "www"),
				aRecordSet("tie-a", base.Add(time.Hour), "www"),
			},
			want: map[string]string{"www.example.com.": "tie-a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Holders(tt.others, &self, "example.com")
			names := make(map[string]string, len(got))
			for key, holder := range got {
				names[key] = holder.Name
			}
			if len(names) != len(tt.want) {
				t.Fatalf("holders = %v, want %v", names, tt.want)
			}
			for key, want := range tt.want {
				if names[key] != want {
					t.Fatalf("holders = %v, want %v", names, tt.want)
				}
			}
		})
	}
}
