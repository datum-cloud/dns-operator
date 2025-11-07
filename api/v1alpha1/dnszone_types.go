/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DNSZoneSpec defines the desired state of DNSZone
type DNSZoneSpec struct {
	// DomainName is the FQDN of the zone (e.g., "example.com").
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +kubebuilder:validation:XValidation:message="A domain name is immutable and cannot be changed after creation",rule="oldSelf == '' || self == oldSelf"
	// +kubebuilder:validation:XValidation:message="Must have at least two segments separated by dots",rule="self.indexOf('.') != -1"
	DomainName string `json:"domainName"`

	// DNSZoneClassName references the DNSZoneClass used to provision this zone.
	// +optional
	DNSZoneClassName string `json:"dnsZoneClassName,omitempty"`
}

type DomainRefStatus struct {
	Nameservers []networkingv1alpha.Nameserver `json:"nameservers,omitempty"`
}

type DomainRef struct {
	Name   string          `json:"name"`
	Status DomainRefStatus `json:"status,omitempty"`
}

// DNSZoneStatus defines the observed state of DNSZone.
type DNSZoneStatus struct {
	// Nameservers lists the active authoritative nameservers for this zone.
	// +optional
	Nameservers []string `json:"nameservers,omitempty"`

	// RecordCount is the number of DNSRecordSet resources in this namespace that reference this zone.
	// +optional
	RecordCount int `json:"recordCount,omitempty"`

	// Conditions tracks state such as Accepted and Programmed readiness.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// DomainRef references the Domain this zone belongs to.
	// +optional
	DomainRef *DomainRef `json:"domainRef,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=.status.conditions[?(@.type=="Accepted")].status
// +kubebuilder:printcolumn:name="Programmed",type=string,JSONPath=.status.conditions[?(@.type=="Programmed")].status
// +kubebuilder:printcolumn:name="Records",type=integer,JSONPath=.status.recordCount

// DNSZone is the Schema for the dnszones API
type DNSZone struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of DNSZone
	// +required
	Spec DNSZoneSpec `json:"spec"`

	// status defines the observed state of DNSZone
	// +optional
	Status DNSZoneStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// DNSZoneList contains a list of DNSZone
type DNSZoneList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSZone `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DNSZone{}, &DNSZoneList{})
}
