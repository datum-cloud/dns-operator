// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// DNSObjectReference pins a namespaced object by API-assigned UID. Generation
// is used where a writer must also be fenced against policy changes.
type DNSObjectReference struct {
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=128
	UID types.UID `json:"uid,omitempty"`
	// +kubebuilder:validation:Minimum=0
	Generation int64 `json:"generation,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.resolverContextRef) || ((self.resolverContextRef.name.size() == 0 && !has(self.resolverContextRef.uid) && (!has(self.resolverContextRef.generation) || self.resolverContextRef.generation == 0)) || (self.resolverContextRef.name.size() > 0 && has(self.resolverContextRef.uid) && self.resolverContextRef.uid.size() > 0))",message="a nonempty resolverContextRef must pin name and UID"
// +kubebuilder:validation:XValidation:rule="has(self.dnsZoneRef.uid) && self.dnsZoneRef.uid.size() > 0",message="dnsZoneRef must pin the zone UID"
// +kubebuilder:validation:XValidation:rule="self.resolverContextRef.name.size() > 0 && has(self.resolverContextRef.uid) && self.resolverContextRef.uid.size() > 0",message="resolverContextRef is required"
type DNSZoneAssociationSpec struct {
	DNSZoneRef         DNSObjectReference `json:"dnsZoneRef"`
	ResolverContextRef DNSObjectReference `json:"resolverContextRef"`
	Managed            bool               `json:"managed,omitempty"`
}

type DNSZoneAssociationStatus struct {
	ResolvedDNSZoneRef         DNSObjectReference  `json:"resolvedDNSZoneRef,omitempty"`
	ResolvedResolverContextRef DNSObjectReference  `json:"resolvedResolverContextRef,omitempty"`
	BindingRef                 *DNSObjectReference `json:"bindingRef,omitempty"`
	Conditions                 []metav1.Condition  `json:"conditions,omitempty"`
}

// DNSResolverContext is the DNS-owned serving and namespace boundary created by
// a trusted network integration. consumerID is opaque to DNS and immutable.
// +kubebuilder:validation:XValidation:rule="oldSelf.consumerID == self.consumerID",message="consumerID is immutable"
type DNSResolverContextSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
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
	ManagedNamespace DNSResolverContextManagedNamespaceStatus `json:"managedNamespace,omitempty"`
	AccessEpoch      int64                                    `json:"accessEpoch,omitempty"`
	ServingTargets   []DNSResolverContextServingTarget        `json:"servingTargets,omitempty"`
	Conditions       []metav1.Condition                       `json:"conditions,omitempty"`
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
	Type DNSResolverQueryIdentityType `json:"type"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Value string `json:"value"`
}

type DNSResolverAccessVersion struct {
	// +kubebuilder:validation:Minimum=1
	Epoch int64 `json:"epoch"`
	// +kubebuilder:validation:Minimum=1
	Sequence int64 `json:"sequence"`
}

// +kubebuilder:validation:XValidation:rule="self.contextRef.name.size() > 0 && has(self.contextRef.uid) && self.contextRef.uid.size() > 0",message="contextRef requires name and UID"
// +kubebuilder:validation:XValidation:rule="self.contextRef == oldSelf.contextRef",message="contextRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.region == oldSelf.region && self.queryIdentity == oldSelf.queryIdentity && self.port == oldSelf.port && self.transports == oldSelf.transports",message="access identity and transport are immutable"
// +kubebuilder:validation:XValidation:rule="self.version.epoch == oldSelf.version.epoch",message="epoch is immutable for an access binding"
// +kubebuilder:validation:XValidation:rule="self.version == oldSelf.version || self.version.sequence > oldSelf.version.sequence",message="version changes require an increasing sequence"
type DNSResolverAccessBindingSpec struct {
	ContextRef DNSObjectReference `json:"contextRef"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Region        string                   `json:"region"`
	QueryIdentity DNSResolverQueryIdentity `json:"queryIdentity"`
	// +kubebuilder:validation:Enum=53
	Port int32 `json:"port"`
	// +kubebuilder:validation:MinItems=2
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:items:Enum=UDP;TCP
	// +listType=set
	Transports []string                 `json:"transports"`
	Version    DNSResolverAccessVersion `json:"version"`
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

const DNSRegistrationMaxTTLSeconds int32 = 30

// +kubebuilder:validation:XValidation:rule="has(self.dnsZoneRef.uid) && self.dnsZoneRef.uid.size() > 0",message="dnsZoneRef must pin the zone UID"
type DNSRegistrationSpec struct {
	DNSZoneRef        DNSObjectReference   `json:"dnsZoneRef"`
	Name              string               `json:"name"`
	RecordTypes       []RRType             `json:"recordTypes"`
	PublicationPolicy DNSPublicationPolicy `json:"publicationPolicy"`
	// TTLSeconds bounds cached dynamic answers to at most 30 seconds after withdrawal.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=30
	TTLSeconds          int32    `json:"ttlSeconds"`
	ReservedDescendants []string `json:"reservedDescendants,omitempty"`
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

type DNSContributionRecordSet struct {
	RecordType RRType        `json:"recordType"`
	Records    []RecordEntry `json:"records"`
}

// +kubebuilder:validation:XValidation:rule="self.registrationRef.name.size() > 0 && has(self.registrationRef.uid) && self.registrationRef.uid.size() > 0 && has(self.registrationRef.generation) && self.registrationRef.generation > 0",message="registrationRef requires name, UID, and generation"
// +kubebuilder:validation:XValidation:rule="self.registrationRef == oldSelf.registrationRef",message="contribution registration reference is immutable"
type DNSRecordContributionSpec struct {
	RegistrationRef DNSObjectReference         `json:"registrationRef"`
	RecordSets      []DNSContributionRecordSet `json:"recordSets"`
}

// +kubebuilder:validation:XValidation:rule="!has(oldSelf.sequence) || (has(self.sequence) && self.sequence >= oldSelf.sequence)",message="observation sequence must not decrease"
// +kubebuilder:validation:XValidation:rule="(has(self.eligible) && self.eligible) == (has(oldSelf.eligible) && oldSelf.eligible) || (has(self.sequence) && (!has(oldSelf.sequence) || self.sequence > oldSelf.sequence))",message="eligibility changes require a newer observation sequence"
type DNSRecordContributionStatus struct {
	// +kubebuilder:validation:Minimum=1
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +kubebuilder:validation:Minimum=1
	Sequence          int64              `json:"sequence,omitempty"`
	Eligible          bool               `json:"eligible,omitempty"`
	Reason            string             `json:"reason,omitempty"`
	PublishedRevision int64              `json:"publishedRevision,omitempty"`
	Conditions        []metav1.Condition `json:"conditions,omitempty"`
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

// +kubebuilder:validation:XValidation:rule="!has(self.resolverContextRef) || ((self.resolverContextRef.name.size() == 0 && !has(self.resolverContextRef.uid) && (!has(self.resolverContextRef.generation) || self.resolverContextRef.generation == 0)) || (self.resolverContextRef.name.size() > 0 && has(self.resolverContextRef.uid) && self.resolverContextRef.uid.size() > 0))",message="a nonempty resolverContextRef must pin name and UID"
// +kubebuilder:validation:XValidation:rule="!has(self.dnsZoneRef) || self.dnsZoneRef.name.size() == 0 || (has(self.dnsZoneRef.uid) && self.dnsZoneRef.uid.size() > 0)",message="a selected dnsZoneRef must pin the zone UID"
// +kubebuilder:validation:XValidation:rule="self.resolverContextRef.name.size() > 0 && has(self.resolverContextRef.uid) && self.resolverContextRef.uid.size() > 0",message="resolverContextRef is required"
type DNSNamingPolicySpec struct {
	ResolverContextRef DNSObjectReference      `json:"resolverContextRef"`
	AdditionalNames    []DNSAdditionalNameRule `json:"additionalNames"`
	DNSZoneRef         DNSObjectReference      `json:"dnsZoneRef,omitempty"`
	ScopePrefix        string                  `json:"scopePrefix,omitempty"`
	Priority           int32                   `json:"priority,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.dnsZoneRef.uid) && self.dnsZoneRef.uid.size() > 0",message="dnsZoneRef must pin the zone UID"
type DNSAdditionalNameRule struct {
	Selector   *DNSRegistrationSelector `json:"selector,omitempty"`
	DNSZoneRef DNSObjectReference       `json:"dnsZoneRef"`
	NamePrefix string                   `json:"namePrefix"`
}

// DNSRegistrationSelector matches registration labels using Kubernetes selector semantics.
type DNSRegistrationSelector struct {
	// +kubebuilder:validation:MaxProperties=64
	MatchLabels map[string]string `json:"matchLabels,omitempty"`
	// +kubebuilder:validation:MaxItems=64
	MatchExpressions []DNSRegistrationSelectorRequirement `json:"matchExpressions,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="(self.operator in ['In', 'NotIn']) ? (has(self.values) && self.values.size() > 0) : (!has(self.values) || self.values.size() == 0)",message="selector operator requires compatible values"
type DNSRegistrationSelectorRequirement struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=317
	Key string `json:"key"`
	// +kubebuilder:validation:Enum=In;NotIn;Exists;DoesNotExist
	Operator metav1.LabelSelectorOperator `json:"operator"`
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$`
	Values []string `json:"values,omitempty"`
}

// AsLabelSelector converts the API selector for Kubernetes label matching.
func (s *DNSRegistrationSelector) AsLabelSelector() *metav1.LabelSelector {
	if s == nil {
		return nil
	}
	out := &metav1.LabelSelector{MatchLabels: s.MatchLabels}
	for _, requirement := range s.MatchExpressions {
		out.MatchExpressions = append(out.MatchExpressions, metav1.LabelSelectorRequirement{
			Key: requirement.Key, Operator: requirement.Operator, Values: requirement.Values,
		})
	}
	return out
}

type DNSNamingPolicyStatus struct {
	ResolvedResolverContextRef DNSObjectReference              `json:"resolvedResolverContextRef,omitempty"`
	ResolvedDNSZoneRef         DNSObjectReference              `json:"resolvedDNSZoneRef,omitempty"`
	ResolvedAdditionalNames    []DNSResolvedAdditionalNameRule `json:"resolvedAdditionalNames,omitempty"`
	Conditions                 []metav1.Condition              `json:"conditions,omitempty"`
}

type DNSResolvedAdditionalNameRule struct {
	Selector   *metav1.LabelSelector `json:"selector,omitempty"`
	DNSZoneRef DNSObjectReference    `json:"dnsZoneRef"`
	NamePrefix string                `json:"namePrefix"`
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

// DNSManagedNamespace tracks a resolver context's canonical private namespace.
type DNSManagedNamespaceSpec struct {
	ProjectUID         types.UID          `json:"projectUID"`
	ResolverContextRef DNSObjectReference `json:"resolverContextRef"`
	DomainSuffix       string             `json:"domainSuffix"`
	DNSZoneClassName   string             `json:"dnsZoneClassName"`
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
	SchemeBuilder.Register(&DNSZoneAssociation{}, &DNSZoneAssociationList{}, &DNSResolverContext{}, &DNSResolverContextList{}, &DNSResolverAccessBinding{}, &DNSResolverAccessBindingList{}, &DNSRegistration{}, &DNSRegistrationList{}, &DNSRecordContribution{}, &DNSRecordContributionList{}, &DNSNamingPolicy{}, &DNSNamingPolicyList{}, &DNSManagedNamespace{}, &DNSManagedNamespaceList{})
}
