// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"encoding/json"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// DNSObjectReference pins a namespaced object by API-assigned UID. Generation
// is used where a writer must also be fenced against policy changes.
type DNSObjectReference struct {
	Name       string    `json:"name"`
	UID        types.UID `json:"uid,omitempty"`
	Generation int64     `json:"generation,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.vpcRef) && has(self.vpcRef.name) && self.vpcRef.name.size() > 0 && has(self.vpcRef.uid) && self.vpcRef.uid.size() > 0) != (has(self.resolverContextRef) && has(self.resolverContextRef.name) && self.resolverContextRef.name.size() > 0 && has(self.resolverContextRef.uid) && self.resolverContextRef.uid.size() > 0)",message="exactly one UID-pinned vpcRef or resolverContextRef is required"
// +kubebuilder:validation:XValidation:rule="!has(self.vpcRef) || ((self.vpcRef.name.size() == 0 && !has(self.vpcRef.uid) && (!has(self.vpcRef.generation) || self.vpcRef.generation == 0)) || (self.vpcRef.name.size() > 0 && has(self.vpcRef.uid) && self.vpcRef.uid.size() > 0))",message="a nonempty vpcRef must pin name and UID"
// +kubebuilder:validation:XValidation:rule="!has(self.resolverContextRef) || ((self.resolverContextRef.name.size() == 0 && !has(self.resolverContextRef.uid) && (!has(self.resolverContextRef.generation) || self.resolverContextRef.generation == 0)) || (self.resolverContextRef.name.size() > 0 && has(self.resolverContextRef.uid) && self.resolverContextRef.uid.size() > 0))",message="a nonempty resolverContextRef must pin name and UID"
type DNSZoneAssociationSpec struct {
	DNSZoneRef         DNSObjectReference `json:"dnsZoneRef"`
	VPCRef             DNSObjectReference `json:"vpcRef,omitempty"`
	ResolverContextRef DNSObjectReference `json:"resolverContextRef,omitempty"`
	Managed            bool               `json:"managed,omitempty"`
}

type DNSZoneAssociationStatus struct {
	ResolvedDNSZoneRef         DNSObjectReference  `json:"resolvedDNSZoneRef,omitempty"`
	ResolvedVPCRef             DNSObjectReference  `json:"resolvedVPCRef,omitempty"`
	ResolvedResolverContextRef DNSObjectReference  `json:"resolvedResolverContextRef,omitempty"`
	BindingRef                 *DNSObjectReference `json:"bindingRef,omitempty"`
	Conditions                 []metav1.Condition  `json:"conditions,omitempty"`
}

// DNSResolverContext is the DNS-owned serving and namespace boundary created by
// a trusted network integration. consumerID is opaque to DNS and immutable.
// +kubebuilder:validation:XValidation:rule="oldSelf.consumerID == self.consumerID",message="consumerID is immutable"
type DNSResolverContextSpec struct {
	ConsumerID       string                             `json:"consumerID"`
	ManagedNamespace DNSResolverContextManagedNamespace `json:"managedNamespace,omitempty"`
}

type DNSResolverContextManagedNamespace struct {
	Enabled bool `json:"enabled,omitempty"`
}

type DNSResolverContextManagedNamespaceStatus struct {
	DNSZoneRef DNSObjectReference `json:"dnsZoneRef,omitempty"`
	Suffix     string             `json:"suffix,omitempty"`
}

type DNSResolverContextStatus struct {
	ManagedNamespace  DNSResolverContextManagedNamespaceStatus `json:"managedNamespace,omitempty"`
	AccessWriterEpoch int64                                    `json:"accessWriterEpoch,omitempty"`
	ServingTargets    []DNSResolverContextServingTarget        `json:"servingTargets,omitempty"`
	Conditions        []metav1.Condition                       `json:"conditions,omitempty"`
}

type DNSResolverContextServingTarget struct {
	Region string `json:"region"`
	Shard  string `json:"shard"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Project"
type DNSResolverContext struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DNSResolverContextSpec   `json:"spec"`
	Status            DNSResolverContextStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DNSResolverContextList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSResolverContext `json:"items"`
}

// +kubebuilder:validation:Enum=DestinationAddress
type DNSResolverQueryIdentityType string

const DNSResolverQueryIdentityDestinationAddress DNSResolverQueryIdentityType = "DestinationAddress"

type DNSResolverQueryIdentity struct {
	Type  DNSResolverQueryIdentityType `json:"type"`
	Value string                       `json:"value"`
}

type DNSResolverAccessAuthorization struct {
	// +kubebuilder:validation:Minimum=1
	WriterEpoch int64 `json:"writerEpoch"`
	// +kubebuilder:validation:Minimum=1
	Sequence   int64       `json:"sequence"`
	ValidUntil metav1.Time `json:"validUntil"`
}

// MarshalJSON preserves the source authorization deadline exactly. metav1.Time
// accepts fractional seconds on input but its default JSON encoder drops them,
// which would mutate a same-fence deadline when projecting an access binding.
func (a DNSResolverAccessAuthorization) MarshalJSON() ([]byte, error) {
	var deadline *time.Time
	if !a.ValidUntil.IsZero() {
		instant := a.ValidUntil.UTC()
		deadline = &instant
	}
	return json.Marshal(struct {
		WriterEpoch int64      `json:"writerEpoch"`
		Sequence    int64      `json:"sequence"`
		ValidUntil  *time.Time `json:"validUntil"`
	}{WriterEpoch: a.WriterEpoch, Sequence: a.Sequence, ValidUntil: deadline})
}

type DNSResolverAccessBindingSpec struct {
	ContextRef    DNSObjectReference             `json:"contextRef"`
	Region        string                         `json:"region"`
	QueryIdentity DNSResolverQueryIdentity       `json:"queryIdentity"`
	Port          int32                          `json:"port"`
	Transports    []string                       `json:"transports"`
	Authorization DNSResolverAccessAuthorization `json:"authorization"`
}

type DNSResolverAccessBindingStatus struct {
	ObservedSequence int64               `json:"observedSequence,omitempty"`
	BindingRef       *DNSObjectReference `json:"bindingRef,omitempty"`
	Conditions       []metav1.Condition  `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Project"
type DNSResolverAccessBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DNSResolverAccessBindingSpec   `json:"spec"`
	Status            DNSResolverAccessBindingStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DNSResolverAccessBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSResolverAccessBinding `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Project"
type DNSZoneAssociation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DNSZoneAssociationSpec   `json:"spec"`
	Status            DNSZoneAssociationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DNSZoneAssociationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSZoneAssociation `json:"items"`
}

// +kubebuilder:validation:Enum=EligibleContributions;Persistent
type DNSPublicationPolicy string

const (
	DNSPublicationPolicyEligibleContributions DNSPublicationPolicy = "EligibleContributions"
	DNSPublicationPolicyPersistent            DNSPublicationPolicy = "Persistent"
)

type DNSRegistrationSpec struct {
	DNSZoneRef          DNSObjectReference   `json:"dnsZoneRef"`
	Name                string               `json:"name"`
	RecordTypes         []RRType             `json:"recordTypes"`
	PublicationPolicy   DNSPublicationPolicy `json:"publicationPolicy"`
	TTLSeconds          int32                `json:"ttlSeconds"`
	ReservedDescendants []string             `json:"reservedDescendants,omitempty"`
}

type DNSRegistrationStatus struct {
	CanonicalFQDN          string             `json:"canonicalFQDN,omitempty"`
	ObservedGeneration     int64              `json:"observedGeneration,omitempty"`
	PublicationWriterEpoch int64              `json:"publicationWriterEpoch,omitempty"`
	PublicationRevision    int64              `json:"publicationRevision,omitempty"`
	Conditions             []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Project"
type DNSRegistration struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DNSRegistrationSpec   `json:"spec"`
	Status            DNSRegistrationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DNSRegistrationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSRegistration `json:"items"`
}

type DNSProducerPrincipal struct {
	ClusterUID string `json:"clusterUID"`
	Subject    string `json:"subject"`
}

type DNSContributionGrantSpec struct {
	RegistrationRef DNSObjectReference   `json:"registrationRef"`
	ProducerID      string               `json:"producerID"`
	Principal       DNSProducerPrincipal `json:"principal"`
	RecordTypes     []RRType             `json:"recordTypes"`
	NameScopes      []string             `json:"nameScopes,omitempty"`
}

type DNSContributionGrantStatus struct {
	ActiveWriterEpoch              int64              `json:"activeWriterEpoch,omitempty"`
	ObservedGrantGeneration        int64              `json:"observedGrantGeneration,omitempty"`
	ObservedRegistrationGeneration int64              `json:"observedRegistrationGeneration,omitempty"`
	Conditions                     []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Project"
type DNSContributionGrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DNSContributionGrantSpec   `json:"spec"`
	Status            DNSContributionGrantStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DNSContributionGrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSContributionGrant `json:"items"`
}

type DNSContributionRecordSet struct {
	RecordType RRType        `json:"recordType"`
	Records    []RecordEntry `json:"records"`
}

type DNSRecordContributionSpec struct {
	RegistrationRef DNSObjectReference         `json:"registrationRef"`
	GrantRef        DNSObjectReference         `json:"grantRef"`
	RecordSets      []DNSContributionRecordSet `json:"recordSets"`
}

type DNSRecordContributionStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	WriterEpoch        int64              `json:"writerEpoch,omitempty"`
	Sequence           int64              `json:"sequence,omitempty"`
	Eligible           bool               `json:"eligible,omitempty"`
	Reason             string             `json:"reason,omitempty"`
	ValidUntil         *metav1.Time       `json:"validUntil,omitempty"`
	PublishedRevision  int64              `json:"publishedRevision,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Project"
type DNSRecordContribution struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DNSRecordContributionSpec   `json:"spec"`
	Status            DNSRecordContributionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DNSRecordContributionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSRecordContribution `json:"items"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.vpcRef) && has(self.vpcRef.name) && self.vpcRef.name.size() > 0 && has(self.vpcRef.uid) && self.vpcRef.uid.size() > 0) != (has(self.resolverContextRef) && has(self.resolverContextRef.name) && self.resolverContextRef.name.size() > 0 && has(self.resolverContextRef.uid) && self.resolverContextRef.uid.size() > 0)",message="exactly one UID-pinned vpcRef or resolverContextRef is required"
// +kubebuilder:validation:XValidation:rule="!has(self.vpcRef) || ((self.vpcRef.name.size() == 0 && !has(self.vpcRef.uid) && (!has(self.vpcRef.generation) || self.vpcRef.generation == 0)) || (self.vpcRef.name.size() > 0 && has(self.vpcRef.uid) && self.vpcRef.uid.size() > 0))",message="a nonempty vpcRef must pin name and UID"
// +kubebuilder:validation:XValidation:rule="!has(self.resolverContextRef) || ((self.resolverContextRef.name.size() == 0 && !has(self.resolverContextRef.uid) && (!has(self.resolverContextRef.generation) || self.resolverContextRef.generation == 0)) || (self.resolverContextRef.name.size() > 0 && has(self.resolverContextRef.uid) && self.resolverContextRef.uid.size() > 0))",message="a nonempty resolverContextRef must pin name and UID"
type DNSNamingPolicySpec struct {
	VPCRef             DNSObjectReference      `json:"vpcRef,omitempty"`
	ResolverContextRef DNSObjectReference      `json:"resolverContextRef,omitempty"`
	AdditionalNames    []DNSAdditionalNameRule `json:"additionalNames"`
	// Legacy single-rule fields are retained during the v1alpha1 transition.
	DNSZoneRef  DNSObjectReference `json:"dnsZoneRef,omitempty"`
	Product     string             `json:"product,omitempty"`
	ScopePrefix string             `json:"scopePrefix,omitempty"`
	Priority    int32              `json:"priority,omitempty"`
}

// +kubebuilder:validation:Enum=InstanceIdentity;ServiceDiscovery;ServiceVIP;ServiceExport
type DNSRegistrationClass string

const (
	DNSRegistrationClassInstanceIdentity DNSRegistrationClass = "InstanceIdentity"
	DNSRegistrationClassServiceDiscovery DNSRegistrationClass = "ServiceDiscovery"
	DNSRegistrationClassServiceVIP       DNSRegistrationClass = "ServiceVIP"
	DNSRegistrationClassServiceExport    DNSRegistrationClass = "ServiceExport"
)

type DNSAdditionalNameRule struct {
	RegistrationClass DNSRegistrationClass `json:"registrationClass"`
	DNSZoneRef        DNSObjectReference   `json:"dnsZoneRef"`
	NamePrefix        string               `json:"namePrefix"`
}

type DNSNamingPolicyStatus struct {
	ResolvedVPCRef             DNSObjectReference              `json:"resolvedVPCRef,omitempty"`
	ResolvedResolverContextRef DNSObjectReference              `json:"resolvedResolverContextRef,omitempty"`
	ResolvedDNSZoneRef         DNSObjectReference              `json:"resolvedDNSZoneRef,omitempty"`
	ResolvedAdditionalNames    []DNSResolvedAdditionalNameRule `json:"resolvedAdditionalNames,omitempty"`
	Conditions                 []metav1.Condition              `json:"conditions,omitempty"`
}

type DNSResolvedAdditionalNameRule struct {
	RegistrationClass DNSRegistrationClass `json:"registrationClass"`
	DNSZoneRef        DNSObjectReference   `json:"dnsZoneRef"`
	NamePrefix        string               `json:"namePrefix"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type DNSNamingPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DNSNamingPolicySpec   `json:"spec"`
	Status            DNSNamingPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DNSNamingPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSNamingPolicy `json:"items"`
}

// DNSManagedNamespace is the DNS-owned lookup and lifecycle record for the
// canonical private namespace of a VPC. Product controllers read its status;
// only the DNS platform controller writes the object.
type DNSManagedNamespaceSpec struct {
	ProjectUID       types.UID          `json:"projectUID"`
	VPCRef           DNSObjectReference `json:"vpcRef"`
	DomainSuffix     string             `json:"domainSuffix"`
	DNSZoneClassName string             `json:"dnsZoneClassName"`
}

type DNSManagedNamespaceStatus struct {
	DNSZoneRef      DNSObjectReference `json:"dnsZoneRef,omitempty"`
	AssociationRef  DNSObjectReference `json:"associationRef,omitempty"`
	CanonicalSuffix string             `json:"canonicalSuffix,omitempty"`
	Conditions      []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type DNSManagedNamespace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DNSManagedNamespaceSpec   `json:"spec"`
	Status            DNSManagedNamespaceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DNSManagedNamespaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSManagedNamespace `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DNSZoneAssociation{}, &DNSZoneAssociationList{}, &DNSResolverContext{}, &DNSResolverContextList{}, &DNSResolverAccessBinding{}, &DNSResolverAccessBindingList{}, &DNSRegistration{}, &DNSRegistrationList{}, &DNSContributionGrant{}, &DNSContributionGrantList{}, &DNSRecordContribution{}, &DNSRecordContributionList{}, &DNSNamingPolicy{}, &DNSNamingPolicyList{}, &DNSManagedNamespace{}, &DNSManagedNamespaceList{})
}
