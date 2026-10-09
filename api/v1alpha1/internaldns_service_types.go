// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// DNSResolverBinding is the DNS-service projection of a consumer context and access binding.
// +kubebuilder:validation:XValidation:rule="self.configuration.generation >= oldSelf.configuration.generation && self.configuration.revision >= oldSelf.configuration.revision",message="configuration fences must not decrease"
// +kubebuilder:validation:XValidation:rule="self.authorization.writerEpoch > oldSelf.authorization.writerEpoch || (self.authorization.writerEpoch == oldSelf.authorization.writerEpoch && self.authorization.sequence >= oldSelf.authorization.sequence)",message="authorization fences must not decrease"
// +kubebuilder:validation:XValidation:rule="self.authorization.writerEpoch != oldSelf.authorization.writerEpoch || self.authorization.sequence != oldSelf.authorization.sequence || self.authorization.validUntil == oldSelf.authorization.validUntil",message="changing an authorization deadline requires a new fence"
// +kubebuilder:validation:XValidation:rule="self.configuration.zoneRefs == oldSelf.configuration.zoneRefs || (self.configuration.generation > oldSelf.configuration.generation && self.configuration.revision > oldSelf.configuration.revision)",message="zone membership changes require a new cache generation and configuration revision"
// +kubebuilder:validation:XValidation:rule="(has(self.tombstone) && self.tombstone) == (has(oldSelf.tombstone) && oldSelf.tombstone) || self.configuration.revision > oldSelf.configuration.revision",message="changing withdrawal state requires a new configuration revision"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.tombstone) || !oldSelf.tombstone || (has(self.tombstone) && self.tombstone) || self.authorization.writerEpoch > oldSelf.authorization.writerEpoch || (self.authorization.writerEpoch == oldSelf.authorization.writerEpoch && self.authorization.sequence > oldSelf.authorization.sequence)",message="reactivating withdrawn access requires a new source authorization fence"
type DNSResolverBindingSpec struct {
	Source        DNSResolverBindingSource        `json:"source"`
	Placement     DNSResolverBindingPlacement     `json:"placement"`
	Configuration DNSResolverBindingConfiguration `json:"configuration"`
	Authorization DNSResolverAccessAuthorization  `json:"authorization"`
	Tombstone     bool                            `json:"tombstone,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="oldSelf == self",message="binding source identity is immutable"
// +kubebuilder:validation:XValidation:rule="self.projectUID.size() > 0 && self.resolverContextRef.name.size() > 0 && has(self.resolverContextRef.uid) && self.resolverContextRef.uid.size() > 0 && self.accessBindingRef.name.size() > 0 && has(self.accessBindingRef.uid) && self.accessBindingRef.uid.size() > 0",message="binding source requires project UID and UID-pinned context and access references"
type DNSResolverBindingSource struct {
	ProjectUID         types.UID          `json:"projectUID"`
	ResolverContextRef DNSObjectReference `json:"resolverContextRef"`
	AccessBindingRef   DNSObjectReference `json:"accessBindingRef"`
}

// +kubebuilder:validation:XValidation:rule="oldSelf == self",message="binding placement is immutable"
type DNSResolverBindingPlacement struct {
	Region string `json:"region"`
	Shard  string `json:"shard"`
}

// +kubebuilder:validation:XValidation:rule="self.zoneRefs.all(z, z.name.size() > 0 && has(z.uid) && z.uid.size() > 0)",message="zone references must pin name and UID"
type DNSResolverBindingConfiguration struct {
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
	// +kubebuilder:validation:Minimum=1
	Revision  int64                       `json:"revision"`
	Listeners DNSResolverBindingListeners `json:"listeners"`
	// +kubebuilder:validation:MaxItems=256
	ZoneRefs []DNSObjectReference `json:"zoneRefs"`
}

// +kubebuilder:validation:XValidation:rule="oldSelf == self",message="listener identity is immutable for an access binding"
type DNSResolverBindingListeners struct {
	Node     DNSResolverBindingListener `json:"node"`
	Regional DNSResolverBindingListener `json:"regional"`
}

type DNSResolverBindingListener struct {
	// +kubebuilder:validation:MinLength=1
	Address string `json:"address"`
	// +kubebuilder:validation:Enum=53
	Port int32 `json:"port"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=2
	// +kubebuilder:validation:items:Enum=UDP;TCP
	// +listType=set
	Transports []string `json:"transports"`
}

type DNSApplyAcknowledgement struct {
	MemberID                 string       `json:"memberID"`
	Phase                    string       `json:"phase"`
	Revision                 int64        `json:"revision"`
	WriterEpoch              int64        `json:"writerEpoch,omitempty"`
	SnapshotRevision         int64        `json:"snapshotRevision,omitempty"`
	AuthorizationIssuerEpoch int64        `json:"authorizationIssuerEpoch,omitempty"`
	AuthorizationRevision    int64        `json:"authorizationRevision,omitempty"`
	ObservedUnixNano         int64        `json:"observedUnixNano,omitempty"`
	ObservedAt               metav1.Time  `json:"observedAt"`
	ValidUntil               *metav1.Time `json:"validUntil,omitempty"`
	Error                    string       `json:"error,omitempty"`
}

type DNSResolverBindingStatus struct {
	ObservedConfigurationRevision int64                     `json:"observedConfigurationRevision,omitempty"`
	Phase                         string                    `json:"phase,omitempty"`
	MemberAcknowledgements        []DNSApplyAcknowledgement `json:"memberAcknowledgements,omitempty"`
	Conditions                    []metav1.Condition        `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type DNSResolverBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DNSResolverBindingSpec   `json:"spec"`
	Status            DNSResolverBindingStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DNSResolverBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSResolverBinding `json:"items"`
}

type DNSPublicationChunkReference struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int32  `json:"size"`
}

// +kubebuilder:validation:XValidation:rule="oldSelf == self",message="publication manifests are immutable"
type DNSPublicationManifestSpec struct {
	ZoneRef            DNSObjectReference             `json:"zoneRef"`
	ZoneApex           string                         `json:"zoneApex"`
	ContextUIDs        []types.UID                    `json:"contextUIDs"`
	WriterEpoch        int64                          `json:"writerEpoch"`
	Revision           int64                          `json:"revision"`
	PreviousRevision   int64                          `json:"previousRevision,omitempty"`
	Tombstone          bool                           `json:"tombstone,omitempty"`
	Chunks             []DNSPublicationChunkReference `json:"chunks"`
	ContentHash        string                         `json:"contentHash"`
	GeneratedAt        metav1.Time                    `json:"generatedAt"`
	ContributionFences []DNSContributionFence         `json:"contributionFences,omitempty"`
	RegistrationFences []DNSRegistrationFence         `json:"registrationFences,omitempty"`
	ServingTargets     []DNSPublicationTarget         `json:"servingTargets,omitempty"`
}

type DNSPublicationTarget struct {
	Region string `json:"region"`
	Shard  string `json:"shard"`
}

type DNSRegistrationFence struct {
	UID        types.UID `json:"uid"`
	Generation int64     `json:"generation"`
}

// DNSContributionFence persists an authenticated high-water sequence even
// while its observation is unhealthy, rejected, or expired.
type DNSContributionFence struct {
	UID        types.UID   `json:"uid"`
	GrantUID   types.UID   `json:"grantUID"`
	Epoch      int64       `json:"epoch"`
	Sequence   int64       `json:"sequence"`
	ValidUntil metav1.Time `json:"validUntil,omitempty"`
}

type DNSPublicationManifestStatus struct {
	ExportState             string                    `json:"exportState,omitempty"`
	ExportAttempts          int32                     `json:"exportAttempts,omitempty"`
	ExportedAt              *metav1.Time              `json:"exportedAt,omitempty"`
	StreamSequence          uint64                    `json:"streamSequence,omitempty"`
	ReplicaAcknowledgements []DNSApplyAcknowledgement `json:"replicaAcknowledgements,omitempty"`
	Conditions              []metav1.Condition        `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type DNSPublicationManifest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DNSPublicationManifestSpec   `json:"spec"`
	Status            DNSPublicationManifestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DNSPublicationManifestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSPublicationManifest `json:"items"`
}

// +kubebuilder:validation:XValidation:rule="oldSelf == self",message="publication chunks are immutable"
type DNSPublicationChunkSpec struct {
	ManifestUID types.UID `json:"manifestUID,omitempty"`
	WriterEpoch int64     `json:"writerEpoch"`
	Revision    int64     `json:"revision"`
	Index       int32     `json:"index"`
	SHA256      string    `json:"sha256"`
	Payload     []byte    `json:"payload"`
}

// +kubebuilder:object:root=true
type DNSPublicationChunk struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DNSPublicationChunkSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type DNSPublicationChunkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSPublicationChunk `json:"items"`
}

// +kubebuilder:validation:XValidation:rule="oldSelf == self",message="outbox entries are immutable"
type DNSTransportOutboxSpec struct {
	Subject      string             `json:"subject"`
	ResourceUID  types.UID          `json:"resourceUID"`
	WriterEpoch  int64              `json:"writerEpoch"`
	Revision     int64              `json:"revision"`
	ManifestRef  DNSObjectReference `json:"manifestRef"`
	OwnershipRef DNSObjectReference `json:"ownershipRef,omitempty"`
	PayloadHash  string             `json:"payloadHash"`
	Payload      []byte             `json:"payload"`
	Activation   bool               `json:"activation,omitempty"`
	DependsOn    []string           `json:"dependsOn,omitempty"`
}

type DNSTransportOutboxStatus struct {
	State          string       `json:"state,omitempty"`
	Attempts       int32        `json:"attempts,omitempty"`
	AcknowledgedAt *metav1.Time `json:"acknowledgedAt,omitempty"`
	StreamSequence uint64       `json:"streamSequence,omitempty"`
	LastError      string       `json:"lastError,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:selectablefield:JSONPath=".status.state"
type DNSTransportOutbox struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DNSTransportOutboxSpec   `json:"spec"`
	Status            DNSTransportOutboxStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type DNSTransportOutboxList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSTransportOutbox `json:"items"`
}

// DNSPublicationOwnership is a durable compare-and-swap record for a zone
// compiler. A lease takeover increments WriterEpoch and fences delayed holders.
type DNSPublicationOwnershipSpec struct {
	ZoneUID            types.UID   `json:"zoneUID"`
	HolderIdentity     string      `json:"holderIdentity"`
	WriterEpoch        int64       `json:"writerEpoch"`
	NextRevision       int64       `json:"nextRevision"`
	LeaseUntil         metav1.Time `json:"leaseUntil"`
	LastContentHash    string      `json:"lastContentHash,omitempty"`
	ActiveManifestName string      `json:"activeManifestName,omitempty"`
}

// +kubebuilder:object:root=true
type DNSPublicationOwnership struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              DNSPublicationOwnershipSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type DNSPublicationOwnershipList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DNSPublicationOwnership `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DNSResolverBinding{}, &DNSResolverBindingList{}, &DNSPublicationManifest{}, &DNSPublicationManifestList{}, &DNSPublicationChunk{}, &DNSPublicationChunkList{}, &DNSTransportOutbox{}, &DNSTransportOutboxList{}, &DNSPublicationOwnership{}, &DNSPublicationOwnershipList{})
}
