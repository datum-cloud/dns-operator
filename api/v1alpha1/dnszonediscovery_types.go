// SPDX-License-Identifier: AGPL-3.0-only
//
// One-shot discovery/snapshot of existing DNS records for a DNSZone.
// On creation, a controller queries common RR types for the zone and stores
// them in .status for easy extraction/translation into DNSRecordSet objects.
// This object is write-once (status) and has no lifecycle beyond initial discovery.
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DNSZoneDiscoverySpec defines the desired discovery target.
type DNSZoneDiscoverySpec struct {
	// DNSZoneRef references the DNSZone (same namespace) this discovery targets.
	// +kubebuilder:validation:Required
	DNSZoneRef corev1.LocalObjectReference `json:"dnsZoneRef"`
}

// DiscoveredRecordSet groups discovered records by type.
type DiscoveredRecordSet struct {
	// RecordType is the DNS RR type for this recordset.
	// +kubebuilder:validation:Required
	RecordType RRType `json:"recordType"`

	// Records contains one or more owner names with values appropriate for the RecordType.
	// The RecordEntry schema is shared with DNSRecordSet for easy translation.
	Records []RecordEntry `json:"records"`
}

// DNSZoneDiscoveryStatus defines the observed snapshot of a DNS zone.
type DNSZoneDiscoveryStatus struct {
	// Conditions includes Accepted and Discovered.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// RecordSets is the set of discovered RRsets grouped by RecordType.
	// +optional
	RecordSets []DiscoveredRecordSet `json:"recordSets,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=.status.conditions[?(@.type=="Accepted")].status
// +kubebuilder:printcolumn:name="Discovered",type=string,JSONPath=.status.conditions[?(@.type=="Discovered")].status
// +kubebuilder:selectablefield:JSONPath=".spec.dnsZoneRef.name"
// +kubebuilder:resource:path=dnszonediscoveries,shortName=dnszd

// DNSZoneDiscovery is the Schema for the DNSZone discovery API.
type DNSZoneDiscovery struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired target for discovery.
	// +required
	Spec DNSZoneDiscoverySpec `json:"spec"`

	// status contains the discovered data (write-once).
	// +optional
	Status DNSZoneDiscoveryStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// DNSZoneDiscoveryList contains a list of DNSZoneDiscovery
type DNSZoneDiscoveryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSZoneDiscovery `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DNSZoneDiscovery{}, &DNSZoneDiscoveryList{})
}
