// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:validation:Enum=hmac-md5;hmac-sha1;hmac-sha224;hmac-sha256;hmac-sha384;hmac-sha512
type TSIGAlgorithm string

const (
	TSIGAlgorithmHMACMD5    TSIGAlgorithm = "hmac-md5"
	TSIGAlgorithmHMACSHA1   TSIGAlgorithm = "hmac-sha1"
	TSIGAlgorithmHMACSHA224 TSIGAlgorithm = "hmac-sha224"
	TSIGAlgorithmHMACSHA256 TSIGAlgorithm = "hmac-sha256"
	TSIGAlgorithmHMACSHA384 TSIGAlgorithm = "hmac-sha384"
	TSIGAlgorithmHMACSHA512 TSIGAlgorithm = "hmac-sha512"
)

// DNSZoneTSIGKeySpec defines the desired state of DNSZoneTSIGKey.
type DNSZoneTSIGKeySpec struct {
	// DNSZoneRef references the DNSZone (same namespace) this TSIG key is associated with.
	// The controller derives provider configuration from the referenced zone.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="dnsZoneRef.name must be set"
	DNSZoneRef corev1.LocalObjectReference `json:"dnsZoneRef"`

	// KeyName is the provider-visible name for this TSIG key (the "wire name").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// Allow DNS name syntax with optional trailing dot. TSIG key names are DNS names (case-insensitive).
	// +kubebuilder:validation:Pattern=`^([A-Za-z0-9_](?:[-A-Za-z0-9_]{0,61}[A-Za-z0-9_])?)(?:\.([A-Za-z0-9_](?:[-A-Za-z0-9_]{0,61}[A-Za-z0-9_])?))*\.?$`
	// +kubebuilder:validation:XValidation:message="keyName is immutable and cannot be changed after creation",rule="oldSelf == '' || self == oldSelf"
	KeyName string `json:"keyName"`

	// Algorithm is the TSIG algorithm used for the key.
	// +kubebuilder:default=hmac-md5
	// +optional
	Algorithm TSIGAlgorithm `json:"algorithm,omitempty"`

	// SecretRef references an existing Secret containing TSIG material (BYO secret).
	// When set, the controller reads and validates the Secret, but must not mutate it.
	// When omitted, the controller generates and manages a Secret named deterministically.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == null || self.name != ''",message="secretRef.name must be set"
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`
}

// DNSZoneTSIGKeyStatus defines the observed state of DNSZoneTSIGKey.
type DNSZoneTSIGKeyStatus struct {
	// SecretName is the name of the Secret used for this TSIG key (generated or referenced).
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// TSIGKeyName is the provider-visible name for this TSIG key.
	// +optional
	TSIGKeyName string `json:"tsigKeyName,omitempty"`

	// Conditions tracks state such as Accepted and Programmed readiness.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=.status.conditions[?(@.type=="Accepted")].status
// +kubebuilder:printcolumn:name="Programmed",type=string,JSONPath=.status.conditions[?(@.type=="Programmed")].status
// +kubebuilder:selectablefield:JSONPath=".spec.dnsZoneRef.name"
// +kubebuilder:resource:path=dnszonetsigkeys,shortName=tsig

// DNSZoneTSIGKey is the Schema for the DNSZone TSIG keys API.
type DNSZoneTSIGKey struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of DNSZoneTSIGKey
	// +required
	Spec DNSZoneTSIGKeySpec `json:"spec"`

	// status defines the observed state of DNSZoneTSIGKey
	// +optional
	Status DNSZoneTSIGKeyStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// DNSZoneTSIGKeyList contains a list of DNSZoneTSIGKey.
type DNSZoneTSIGKeyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSZoneTSIGKey `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DNSZoneTSIGKey{}, &DNSZoneTSIGKeyList{})
}
