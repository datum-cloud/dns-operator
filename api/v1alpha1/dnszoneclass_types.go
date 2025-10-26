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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DNSZoneClassSpec defines the desired state of DNSZoneClass
type DNSZoneClassSpec struct {
	// ControllerName identifies the downstream controller/backend implementation (e.g., "powerdns", "hickory").
	ControllerName string `json:"controllerName"`

	// NameServerPolicy defines how nameservers are assigned for zones using this class.
	NameServerPolicy NameServerPolicy `json:"nameServerPolicy"`

	// Defaults provides optional default values applied to managed zones.
	// +optional
	Defaults *ZoneDefaults `json:"defaults,omitempty"`
}

// NameServerPolicy specifies the policy for nameserver assignment.
type NameServerPolicy struct {
	// Mode defines which policy to use.
	Mode NameServerPolicyMode `json:"mode"`
	// Static contains a static list of authoritative nameservers when Mode == "Static".
	// +optional
	Static *StaticNS `json:"static,omitempty"`
}

// +kubebuilder:validation:Enum=Static
type NameServerPolicyMode string

const (
	NameServerPolicyModeStatic NameServerPolicyMode = "Static"
)

// StaticNS lists static authoritative nameserver hostnames.
type StaticNS struct {
	Servers []string `json:"servers"`
}

// ZoneDefaults holds optional default settings for zones.
type ZoneDefaults struct {
	// DefaultTTL is the default TTL applied to records when not otherwise specified.
	// +optional
	DefaultTTL *int64 `json:"defaultTTL,omitempty"`
}

// DNSZoneClassStatus defines the observed state of DNSZoneClass.
type DNSZoneClassStatus struct {
	// Conditions represent the current state of the resource. Common types include
	// "Accepted" and "Programmed" to standardize readiness reporting across controllers.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=.status.conditions[?(@.type=="Accepted")].status
// +kubebuilder:printcolumn:name="Programmed",type=string,JSONPath=.status.conditions[?(@.type=="Programmed")].status

// DNSZoneClass is the Schema for the dnszoneclasses API
type DNSZoneClass struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of DNSZoneClass
	// +required
	Spec DNSZoneClassSpec `json:"spec"`

	// status defines the observed state of DNSZoneClass
	// +optional
	Status DNSZoneClassStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// DNSZoneClassList contains a list of DNSZoneClass
type DNSZoneClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSZoneClass `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DNSZoneClass{}, &DNSZoneClassList{})
}
