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
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=128
	UID types.UID `json:"uid,omitempty"`
	// +kubebuilder:validation:Minimum=0
	Generation int64 `json:"generation,omitempty"`
}

// DNSNameReference selects a resource in the same project namespace.
// DNS records its resolved lifetime in controller-owned status.
type DNSNameReference struct {
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.vpcRef) && has(self.vpcRef.name) && self.vpcRef.name.size() > 0) != (has(self.resolverContextRef) && has(self.resolverContextRef.name) && self.resolverContextRef.name.size() > 0)",message="exactly one named vpcRef or resolverContextRef is required"
// +kubebuilder:validation:XValidation:rule="self.dnsZoneRef.name.size() > 0",message="dnsZoneRef requires a name"
type DNSZoneAssociationSpec struct {
	DNSZoneRef         DNSNameReference `json:"dnsZoneRef"`
	VPCRef             DNSNameReference `json:"vpcRef,omitempty"`
	ResolverContextRef DNSNameReference `json:"resolverContextRef,omitempty"`
	Managed            bool             `json:"managed,omitempty"`
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
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.managedNamespace) || !has(oldSelf.managedNamespace.domainSuffix) || (has(self.managedNamespace) && has(self.managedNamespace.domainSuffix) && self.managedNamespace.domainSuffix == oldSelf.managedNamespace.domainSuffix)",message="managed namespace domainSuffix is immutable"
type DNSResolverContextSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	ConsumerID       string                             `json:"consumerID"`
	ManagedNamespace DNSResolverContextManagedNamespace `json:"managedNamespace,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.enabled) || !self.enabled || (has(self.domainSuffix) && self.domainSuffix.size() > 0)",message="domainSuffix is required when the managed namespace is enabled"
type DNSResolverContextManagedNamespace struct {
	Enabled bool `json:"enabled,omitempty"`
	// DomainSuffix is the zone apex requested by the trusted consumer integration.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`
	DomainSuffix string `json:"domainSuffix,omitempty"`
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
	Type DNSResolverQueryIdentityType `json:"type"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Value string `json:"value"`
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

// +kubebuilder:validation:XValidation:rule="self.contextRef.name.size() > 0 && has(self.contextRef.uid) && self.contextRef.uid.size() > 0",message="contextRef requires name and UID"
// +kubebuilder:validation:XValidation:rule="self.contextRef == oldSelf.contextRef",message="contextRef is immutable"
// +kubebuilder:validation:XValidation:rule="self.region == oldSelf.region && self.queryIdentity == oldSelf.queryIdentity && self.port == oldSelf.port && self.transports == oldSelf.transports",message="access identity and transport are immutable"
// +kubebuilder:validation:XValidation:rule="self.authorization.writerEpoch == oldSelf.authorization.writerEpoch",message="writerEpoch is immutable for an access binding"
// +kubebuilder:validation:XValidation:rule="self.authorization == oldSelf.authorization || self.authorization.sequence > oldSelf.authorization.sequence",message="authorization changes require an increasing sequence"
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

type DNSProducerPrincipal struct {
	// +kubebuilder:validation:MinLength=1
	ClusterUID string `json:"clusterUID"`
	// +kubebuilder:validation:MinLength=1
	Subject string `json:"subject"`
}

// +kubebuilder:validation:XValidation:rule="self.registrationRef.name.size() > 0 && has(self.registrationRef.uid) && self.registrationRef.uid.size() > 0",message="registrationRef requires name and UID"
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

// +kubebuilder:validation:XValidation:rule="self.registrationRef.name.size() > 0 && has(self.registrationRef.uid) && self.registrationRef.uid.size() > 0 && has(self.registrationRef.generation) && self.registrationRef.generation > 0",message="registrationRef requires name, UID, and generation"
// +kubebuilder:validation:XValidation:rule="self.grantRef.name.size() > 0 && has(self.grantRef.uid) && self.grantRef.uid.size() > 0",message="grantRef requires name and UID"
// +kubebuilder:validation:XValidation:rule="self.registrationRef == oldSelf.registrationRef && self.grantRef == oldSelf.grantRef",message="contribution authority references are immutable"
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

// +kubebuilder:validation:XValidation:rule="(has(self.vpcRef) && has(self.vpcRef.name) && self.vpcRef.name.size() > 0) != (has(self.resolverContextRef) && has(self.resolverContextRef.name) && self.resolverContextRef.name.size() > 0)",message="exactly one named vpcRef or resolverContextRef is required"
type DNSNamingPolicySpec struct {
	VPCRef             DNSNameReference `json:"vpcRef,omitempty"`
	ResolverContextRef DNSNameReference `json:"resolverContextRef,omitempty"`
	// +kubebuilder:validation:MaxItems=256
	AdditionalNames []DNSAdditionalNameRule `json:"additionalNames"`
	DNSZoneRef      DNSNameReference        `json:"dnsZoneRef,omitempty"`
	ScopePrefix     string                  `json:"scopePrefix,omitempty"`
	Priority        int32                   `json:"priority,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.dnsZoneRef.name.size() > 0",message="dnsZoneRef requires a name"
type DNSAdditionalNameRule struct {
	// Selector matches DNSRegistration metadata labels within this DNS context.
	// An omitted or empty selector matches every registration in the context.
	Selector   *DNSRegistrationSelector `json:"selector,omitempty"`
	DNSZoneRef DNSNameReference         `json:"dnsZoneRef"`
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
	ResolvedVPCRef             DNSObjectReference              `json:"resolvedVPCRef,omitempty"`
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
