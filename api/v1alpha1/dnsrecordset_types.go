// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:validation:Enum=A;AAAA;CNAME;TXT;MX;SRV;CAA;NS;SOA;PTR;TLSA;HTTPS;SVCB
type RRType string

const (
	RRTypeA     RRType = "A"
	RRTypeAAAA  RRType = "AAAA"
	RRTypeCNAME RRType = "CNAME"
	RRTypeTXT   RRType = "TXT"
	RRTypeMX    RRType = "MX"
	RRTypeSRV   RRType = "SRV"
	RRTypeCAA   RRType = "CAA"
	RRTypeNS    RRType = "NS"
	RRTypeSOA   RRType = "SOA"
	RRTypePTR   RRType = "PTR"
	RRTypeTLSA  RRType = "TLSA"
	RRTypeHTTPS RRType = "HTTPS"
	RRTypeSVCB  RRType = "SVCB"
)

// DNSRecordSetSpec defines the desired state of DNSRecordSet
type DNSRecordSetSpec struct {
	// DNSZoneRef references the DNSZone (same namespace) this recordset belongs to.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self.name != ''",message="dnsZoneRef.name must be set"
	DNSZoneRef corev1.LocalObjectReference `json:"dnsZoneRef"`

	// RecordType is the DNS RR type for this recordset.
	// +kubebuilder:validation:Required
	RecordType RRType `json:"recordType"`

	// Records contains one or more owner names with values appropriate for the RecordType.
	// +kubebuilder:validation:MinItems=1
	Records []RecordEntry `json:"records"`
}

// RecordEntry represents one owner name and its values.
type RecordEntry struct {
	// Name is the owner name (relative to the zone or FQDN).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^(@|[A-Za-z0-9*._-]+)$`
	Name string `json:"name"`
	// TTL optionally overrides TTL for this owner/RRset.
	// +optional
	TTL *int64 `json:"ttl,omitempty"`

	// Exactly one of the following type-specific fields should be set matching RecordType.
	// +optional
	A *ARecordSpec `json:"a,omitempty"`
	// +optional
	AAAA *AAAARecordSpec `json:"aaaa,omitempty"`
	// +optional
	CNAME *CNAMERecordSpec `json:"cname,omitempty"`
	// +optional
	NS *NSRecordSpec `json:"ns,omitempty"`
	// +optional
	TXT *TXTRecordSpec `json:"txt,omitempty"`
	// +optional
	SOA *SOARecordSpec `json:"soa,omitempty"`
	// +optional
	CAA *CAARecordSpec `json:"caa,omitempty"`
	// +optional
	MX *MXRecordSpec `json:"mx,omitempty"`
	// +optional
	SRV *SRVRecordSpec `json:"srv,omitempty"`
	// +optional
	TLSA *TLSARecordSpec `json:"tlsa,omitempty"`
	// +optional
	HTTPS *HTTPSRecordSpec `json:"https,omitempty"`
	// +optional
	SVCB *HTTPSRecordSpec `json:"svcb,omitempty"`

	// +optional
	PTR *PTRRecordSpec `json:"ptr,omitempty"`
}

type PTRRecordSpec struct {
	Content string `json:"content"`
}

type TXTRecordSpec struct {
	Content string `json:"content"`
}

type ARecordSpec struct {
	// +kubebuilder:validation:Format=ipv4
	Content string `json:"content"`
}

type AAAARecordSpec struct {
	// +kubebuilder:validation:Format=ipv6
	Content string `json:"content"`
}

type CNAMERecordSpec struct {
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^([A-Za-z0-9_](?:[-A-Za-z0-9_]{0,61}[A-Za-z0-9_])?)(?:\.([A-Za-z0-9_](?:[-A-Za-z0-9_]{0,61}[A-Za-z0-9_])?))*\.?$`
	// +kubebuilder:validation:MinLength=1
	Content string `json:"content"`
}

type NSRecordSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// Require a hostname (FQDN or relative), allow optional trailing dot, no underscores.
	// Labels: 1-63 chars, alphanum with interior hyphens, total length <=253.
	// +kubebuilder:validation:Pattern=`^([A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)(?:\.([A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?))*\.?$`
	Content string `json:"content"`
}

type SRVRecordSpec struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	Priority uint16 `json:"priority"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	Weight uint16 `json:"weight"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	Port uint16 `json:"port"`
	// +kubebuilder:validation:MinLength=1
	Target string `json:"target"`
}

type MXRecordSpec struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	Preference uint16 `json:"preference"`
	// +kubebuilder:validation:MinLength=1
	Exchange string `json:"exchange"`
}

type CAARecordSpec struct {
	// 0–255 flag
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=255
	Flag uint8 `json:"flag"`
	// RFC-style tags: keep it simple: [a-z0-9]+
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+$`
	Tag string `json:"tag"`
	// +kubebuilder:validation:MinLength=1
	Value string `json:"value"`
}

type TLSARecordSpec struct {
	Usage        uint8  `json:"usage"`
	Selector     uint8  `json:"selector"`
	MatchingType uint8  `json:"matchingType"`
	CertData     string `json:"certData"`
}

type HTTPSRecordSpec struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	Priority uint16 `json:"priority"`
	Target   string `json:"target"`
	// +optional
	Params map[string]string `json:"params,omitempty"`
}

type SOARecordSpec struct {
	// +kubebuilder:validation:MinLength=1
	MName string `json:"mname"`
	// +kubebuilder:validation:MinLength=1
	RName string `json:"rname"`
	// +optional
	Serial uint32 `json:"serial,omitempty"`
	// +optional
	Refresh uint32 `json:"refresh,omitempty"`
	// +optional
	Retry uint32 `json:"retry,omitempty"`
	// +optional
	Expire uint32 `json:"expire,omitempty"`
	// +optional
	TTL uint32 `json:"ttl,omitempty"`
}

// DNSRecordSetStatus defines the observed state of DNSRecordSet.
// +kubebuilder:default={conditions: {{type: "Accepted", status: "Unknown", reason:"Pending", message:"Waiting for controller", lastTransitionTime: "1970-01-01T00:00:00Z"},{type: "Programmed", status: "Unknown", reason:"Pending", message:"Waiting for controller", lastTransitionTime: "1970-01-01T00:00:00Z"}}}
type DNSRecordSetStatus struct {
	// Conditions includes Accepted and Programmed readiness.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// RecordSets captures per-owner (per name) status and conditions.
	// +optional
	RecordSets []RecordSetStatus `json:"recordSets,omitempty"`
}

type RecordSetStatus struct {
	// Name is the owner name this status pertains to.
	Name string `json:"name"`

	// Conditions captures per-name readiness information such as RecordProgrammed.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=.status.conditions[?(@.type=="Accepted")].status
// +kubebuilder:printcolumn:name="Programmed",type=string,JSONPath=.status.conditions[?(@.type=="Programmed")].status
// +kubebuilder:selectablefield:JSONPath=".spec.dnsZoneRef.name"
// +kubebuilder:selectablefield:JSONPath=".spec.recordType"

// DNSRecordSet is the Schema for the dnsrecordsets API
type DNSRecordSet struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of DNSRecordSet
	// +required
	Spec DNSRecordSetSpec `json:"spec"`

	// status defines the observed state of DNSRecordSet
	// +optional
	Status DNSRecordSetStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// DNSRecordSetList contains a list of DNSRecordSet
type DNSRecordSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSRecordSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DNSRecordSet{}, &DNSRecordSetList{})
}
