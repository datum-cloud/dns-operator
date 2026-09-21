// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

func TestIsPlatformShape(t *testing.T) {
	tests := []struct {
		name       string
		recordType dnsv1alpha1.RRType
		ownerName  string
		zoneDomain string
		want       bool
	}{
		{"SOA is always platform shape", dnsv1alpha1.RRTypeSOA, "www", "example.com", true},
		{"SOA at any owner name", dnsv1alpha1.RRTypeSOA, "@", "example.com", true},
		{"apex NS is platform shape", dnsv1alpha1.RRTypeNS, "@", "example.com", true},
		{"apex NS spelled as the zone's FQDN", dnsv1alpha1.RRTypeNS, "example.com.", "example.com", true},
		{"apex NS spelled uppercase", dnsv1alpha1.RRTypeNS, "EXAMPLE.COM.", "example.com", true},
		{"non-apex NS is not platform shape", dnsv1alpha1.RRTypeNS, "ns1", "example.com", false},
		{"A record is never platform shape", dnsv1alpha1.RRTypeA, "@", "example.com", false},
		{"CNAME is never platform shape", dnsv1alpha1.RRTypeCNAME, "www", "example.com", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPlatformShape(tc.recordType, tc.ownerName, tc.zoneDomain); got != tc.want {
				t.Errorf("IsPlatformShape(%s, %q, %q) = %v, want %v", tc.recordType, tc.ownerName, tc.zoneDomain, got, tc.want)
			}
		})
	}
}

func TestClassifyOwnership(t *testing.T) {
	tests := []struct {
		name           string
		labels         map[string]string
		recordType     dnsv1alpha1.RRType
		ownerName      string
		zoneDomain     string
		wantProvenance Provenance
		wantSource     string
	}{
		{
			name:           "an ordinary user record",
			labels:         nil,
			recordType:     dnsv1alpha1.RRTypeA,
			ownerName:      "www",
			zoneDomain:     "example.com",
			wantProvenance: ProvenanceUser,
		},
		{
			name:           "the operator's own SOA",
			labels:         nil,
			recordType:     dnsv1alpha1.RRTypeSOA,
			ownerName:      "@",
			zoneDomain:     "example.com",
			wantProvenance: ProvenancePlatform,
		},
		{
			name:           "the operator's own apex NS",
			labels:         nil,
			recordType:     dnsv1alpha1.RRTypeNS,
			ownerName:      "@",
			zoneDomain:     "example.com",
			wantProvenance: ProvenancePlatform,
		},
		{
			name: "Gateway-owned, AI Edge",
			labels: map[string]string{
				LabelManagedBy:       ValueManagedByNetworking,
				LabelDNSManaged:      ValueDNSManaged,
				LabelSourceKind:      ValueSourceKindGateway,
				LabelSourceName:      "web",
				LabelSourceNamespace: "shop",
			},
			recordType:     dnsv1alpha1.RRTypeA,
			ownerName:      "www",
			zoneDomain:     "example.com",
			wantProvenance: ProvenanceGateway,
			wantSource:     "shop/web",
		},
		{
			// iroh sets the same app.kubernetes.io/managed-by value the Gateway
			// controller does, so this pins that the iroh-specific labels win
			// before the generic three-label Gateway rule is ever consulted.
			name: "iroh-owned despite sharing Gateway's managed-by value",
			labels: map[string]string{
				LabelManagedBy:              ValueManagedByNetworking,
				LabelIrohConnectorName:      "conn-1",
				LabelIrohConnectorNamespace: "shop",
			},
			recordType:     dnsv1alpha1.RRTypeTXT,
			ownerName:      "_iroh",
			zoneDomain:     "example.com",
			wantProvenance: ProvenanceIroh,
			wantSource:     "shop/conn-1",
		},
		{
			name:           "iroh-owned with no namespace label",
			labels:         map[string]string{LabelIrohConnectorName: "conn-1"},
			recordType:     dnsv1alpha1.RRTypeTXT,
			ownerName:      "_iroh",
			zoneDomain:     "example.com",
			wantProvenance: ProvenanceIroh,
			wantSource:     "conn-1",
		},
		{
			name: "ExternalDNS-owned",
			labels: map[string]string{
				LabelExternalDNSManagedBy: ValueExternalDNSManagedBy,
				LabelExternalDNSOwner:     "my-cluster",
			},
			recordType:     dnsv1alpha1.RRTypeA,
			ownerName:      "app",
			zoneDomain:     "example.com",
			wantProvenance: ProvenanceExternalDNS,
			wantSource:     "my-cluster",
		},
		{
			// A shape that would otherwise read as platform (SOA) still belongs
			// to whichever producer's label says so — labels are the stronger
			// signal.
			name: "a labelled producer outranks platform shape",
			labels: map[string]string{
				LabelManagedBy:  ValueManagedByNetworking,
				LabelDNSManaged: ValueDNSManaged,
				LabelSourceKind: ValueSourceKindGateway,
			},
			recordType:     dnsv1alpha1.RRTypeSOA,
			ownerName:      "@",
			zoneDomain:     "example.com",
			wantProvenance: ProvenanceGateway,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyOwnership(tc.labels, tc.recordType, tc.ownerName, tc.zoneDomain)
			if got.Provenance != tc.wantProvenance {
				t.Errorf("Provenance = %s, want %s", got.Provenance, tc.wantProvenance)
			}
			if got.Source != tc.wantSource {
				t.Errorf("Source = %q, want %q", got.Source, tc.wantSource)
			}
		})
	}
}

func TestProvenanceManaged(t *testing.T) {
	if ProvenanceUser.Managed() {
		t.Error("ProvenanceUser.Managed() = true, want false")
	}
	for _, p := range []Provenance{ProvenancePlatform, ProvenanceGateway, ProvenanceIroh, ProvenanceExternalDNS} {
		if !p.Managed() {
			t.Errorf("%s.Managed() = false, want true", p)
		}
	}
}
