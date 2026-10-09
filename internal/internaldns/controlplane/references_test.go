package controlplane

import (
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"testing"
)

func TestReferenceRequiresExactLifetime(t *testing.T) {
	for _, tt := range []struct {
		ref     dnsv1alpha1.DNSObjectReference
		matches bool
	}{
		{dnsv1alpha1.DNSObjectReference{Name: "zone"}, false},
		{dnsv1alpha1.DNSObjectReference{Name: "zone", UID: "old"}, false},
		{dnsv1alpha1.DNSObjectReference{Name: "zone", UID: "current"}, true},
		{dnsv1alpha1.DNSObjectReference{Name: "zone", UID: "current", Generation: 1}, false},
		{dnsv1alpha1.DNSObjectReference{Name: "zone", UID: "current", Generation: 2}, true},
	} {
		if got := refMatches(tt.ref, "zone", "current", 2); got != tt.matches {
			t.Errorf("%#v matches=%v", tt.ref, got)
		}
	}
}
