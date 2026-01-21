// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"reflect"
	"testing"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

func TestNormalizeStringSlice(t *testing.T) {
	if got := normalizeStringSlice(nil); got != nil {
		t.Fatalf("expected nil for nil input, got %v", got)
	}
	if got := normalizeStringSlice([]string{}); got != nil {
		t.Fatalf("expected nil for empty input, got %v", got)
	}

	in := []string{"b", "a", "c"}
	want := []string{"a", "b", "c"}
	got := normalizeStringSlice(in)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected normalize result: got=%v want=%v", got, want)
	}
	// Ensure input not mutated
	if reflect.DeepEqual(in, want) {
		t.Fatalf("expected input slice to remain unsorted")
	}
}

func TestNormalizeDomainNameservers(t *testing.T) {
	in := []networkingv1alpha.Nameserver{
		{
			Hostname: "ns2.example.com",
			IPs: []networkingv1alpha.NameserverIP{
				{Address: "192.0.2.20"},
				{Address: "192.0.2.10"},
			},
		},
		{
			Hostname: "ns1.example.com",
			IPs: []networkingv1alpha.NameserverIP{
				{Address: "192.0.2.5"},
			},
		},
	}

	got := normalizeDomainNameservers(in)
	want := []networkingv1alpha.Nameserver{
		{
			Hostname: "ns1.example.com",
			IPs: []networkingv1alpha.NameserverIP{
				{Address: "192.0.2.5"},
			},
		},
		{
			Hostname: "ns2.example.com",
			IPs: []networkingv1alpha.NameserverIP{
				{Address: "192.0.2.10"},
				{Address: "192.0.2.20"},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected normalize result: got=%v want=%v", got, want)
	}
	// Ensure input not mutated
	if reflect.DeepEqual(in, want) {
		t.Fatalf("expected input slice to remain unsorted")
	}
}

func TestNormalizeDomainRef(t *testing.T) {
	in := &dnsv1alpha1.DomainRef{
		Name: "example.com",
		Status: dnsv1alpha1.DomainRefStatus{
			Nameservers: []networkingv1alpha.Nameserver{
				{
					Hostname: "ns2.example.com",
					IPs: []networkingv1alpha.NameserverIP{
						{Address: "192.0.2.20"},
						{Address: "192.0.2.10"},
					},
				},
				{
					Hostname: "ns1.example.com",
					IPs: []networkingv1alpha.NameserverIP{
						{Address: "192.0.2.5"},
					},
				},
			},
		},
	}

	got := normalizeDomainRef(in)
	if got == nil {
		t.Fatalf("expected non-nil normalized ref")
	}
	if got.Name != in.Name {
		t.Fatalf("expected name to be preserved")
	}
	if got == in {
		t.Fatalf("expected a copy, got same pointer")
	}

	wantNameservers := []networkingv1alpha.Nameserver{
		{
			Hostname: "ns1.example.com",
			IPs: []networkingv1alpha.NameserverIP{
				{Address: "192.0.2.5"},
			},
		},
		{
			Hostname: "ns2.example.com",
			IPs: []networkingv1alpha.NameserverIP{
				{Address: "192.0.2.10"},
				{Address: "192.0.2.20"},
			},
		},
	}
	if !reflect.DeepEqual(got.Status.Nameservers, wantNameservers) {
		t.Fatalf("unexpected normalized nameservers: got=%v want=%v", got.Status.Nameservers, wantNameservers)
	}
	// Ensure input not mutated
	if reflect.DeepEqual(in.Status.Nameservers, wantNameservers) {
		t.Fatalf("expected input nameservers to remain unsorted")
	}
}
