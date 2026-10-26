// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// DNSResolverBinding is the DNS-service projection of a consumer context and access binding.
// +kubebuilder:validation:XValidation:rule="self.configuration.generation >= oldSelf.configuration.generation && self.configuration.revision >= oldSelf.configuration.revision",message="configuration fences must not decrease"
// +kubebuilder:validation:XValidation:rule="self.version.epoch > oldSelf.version.epoch || (self.version.epoch == oldSelf.version.epoch && self.version.sequence >= oldSelf.version.sequence)",message="access versions must not decrease"
// +kubebuilder:validation:XValidation:rule="self.configuration.zoneRefs == oldSelf.configuration.zoneRefs || (self.configuration.generation > oldSelf.configuration.generation && self.configuration.revision > oldSelf.configuration.revision)",message="zone membership changes require a new cache generation and configuration revision"
// +kubebuilder:validation:XValidation:rule="(has(self.tombstone) && self.tombstone) == (has(oldSelf.tombstone) && oldSelf.tombstone) || self.configuration.revision > oldSelf.configuration.revision",message="changing withdrawal state requires a new configuration revision"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.tombstone) || !oldSelf.tombstone || (has(self.tombstone) && self.tombstone) || self.version.epoch > oldSelf.version.epoch || (self.version.epoch == oldSelf.version.epoch && self.version.sequence > oldSelf.version.sequence)",message="reactivating withdrawn access requires a new access version"
type DNSResolverBindingSpec struct {
	Source        DNSResolverBindingSource        `json:"source"`
	Placement     DNSResolverBindingPlacement     `json:"placement"`
	Configuration DNSResolverBindingConfiguration `json:"configuration"`
	Version       DNSResolverAccessVersion        `json:"version"`
	Tombstone     bool                            `json:"tombstone,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="oldSelf == self",message="binding source identity is immutable"
// +kubebuilder:validation:XValidation:rule="self.projectUID.size() > 0 && self.resolverContextRef.name.size() > 0 && has(self.resolverContextRef.uid) && self.resolverContextRef.uid.size() > 0 && self.accessBindingRef.name.size() > 0 && has(self.accessBindingRef.uid) && self.accessBindingRef.uid.size() > 0",message="binding source requires project UID and UID-pinned context and access references"
type DNSResolverBindingSource struct {
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=128
	ProjectUID         types.UID          `json:"projectUID"`
	ResolverContextRef DNSObjectReference `json:"resolverContextRef"`
	AccessBindingRef   DNSObjectReference `json:"accessBindingRef"`
}

// +kubebuilder:validation:XValidation:rule="oldSelf == self",message="binding placement is immutable"
type DNSResolverBindingPlacement struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Region string `json:"region"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Shard string `json:"shard"`
}

// +kubebuilder:validation:XValidation:rule="self.zoneRefs.all(z, z.name.size() > 0 && has(z.uid) && z.uid.size() > 0)",message="zone references must pin name and UID"
type DNSResolverBindingConfiguration struct {
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
	// +kubebuilder:validation:Minimum=1
	Revision int64                   `json:"revision"`
	Query    DNSResolverBindingQuery `json:"query"`
	// +kubebuilder:validation:MaxItems=256
	ZoneRefs []DNSObjectReference `json:"zoneRefs"`
	// ZoneApices reserves each private namespace before publication is available.
	// +kubebuilder:validation:MaxProperties=256
	ZoneApices map[string]string `json:"zoneApices,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="oldSelf == self",message="query destinations are immutable for an access binding"
type DNSResolverBindingQuery struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	NodeAddress string `json:"nodeAddress"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	RegionalAddress string `json:"regionalAddress"`
}

type DNSApplyAcknowledgement struct {
	MemberID         string       `json:"memberID"`
	Phase            string       `json:"phase"`
	Revision         int64        `json:"revision"`
	WriterEpoch      int64        `json:"writerEpoch,omitempty"`
	SnapshotRevision int64        `json:"snapshotRevision,omitempty"`
	AccessEpoch      int64        `json:"accessEpoch,omitempty"`
	AccessSequence   int64        `json:"accessSequence,omitempty"`
	ObservedUnixNano int64        `json:"observedUnixNano,omitempty"`
	ObservedAt       metav1.Time  `json:"observedAt"`
	ValidUntil       *metav1.Time `json:"validUntil,omitempty"`
	Error            string       `json:"error,omitempty"`
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
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:Pattern=`^[a-fA-F0-9]{64}$`
	SHA256 string `json:"sha256"`
	// +kubebuilder:validation:Minimum=1
	Size int32 `json:"size"`
}

// +kubebuilder:validation:XValidation:rule="oldSelf == self",message="publication manifests are immutable"
// +kubebuilder:validation:XValidation:rule="self.zoneRef.name.size() > 0 && has(self.zoneRef.uid) && self.zoneRef.uid.size() > 0",message="zoneRef requires name and UID"
// +kubebuilder:validation:XValidation:rule="!has(self.previousRevision) || (self.previousRevision >= 0 && self.previousRevision < self.revision)",message="previousRevision must precede revision"
// +kubebuilder:validation:XValidation:rule="(has(self.tombstone) && self.tombstone) ? (self.chunks.size() == 0 && self.contentHash == 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855') : self.chunks.size() > 0",message="withdrawal requires empty content; publication requires chunks"
type DNSPublicationManifestSpec struct {
	ZoneRef DNSObjectReference `json:"zoneRef"`
	// +kubebuilder:validation:MinLength=1
	ZoneApex    string      `json:"zoneApex"`
	ContextUIDs []types.UID `json:"contextUIDs"`
	// +kubebuilder:validation:Minimum=1
	WriterEpoch int64 `json:"writerEpoch"`
	// +kubebuilder:validation:Minimum=1
	Revision         int64                          `json:"revision"`
	PreviousRevision int64                          `json:"previousRevision,omitempty"`
	Tombstone        bool                           `json:"tombstone,omitempty"`
	Chunks           []DNSPublicationChunkReference `json:"chunks"`
	// +kubebuilder:validation:Pattern=`^[a-fA-F0-9]{64}$`
	ContentHash        string                 `json:"contentHash"`
	GeneratedAt        metav1.Time            `json:"generatedAt"`
	ContributionFences []DNSContributionFence `json:"contributionFences,omitempty"`
	RegistrationFences []DNSRegistrationFence `json:"registrationFences,omitempty"`
	ServingTargets     []DNSPublicationTarget `json:"servingTargets,omitempty"`
	// ExpectedRegions names every region the zone is served in, including
	// regions another control plane publishes. Set only when a control plane
	// runs in each region; ServingTargets then lists only this one's targets.
	// +listType=set
	ExpectedRegions []string `json:"expectedRegions,omitempty"`
}

type DNSPublicationTarget struct {
	Region string `json:"region"`
	Shard  string `json:"shard"`
}

type DNSRegistrationFence struct {
	UID        types.UID `json:"uid"`
	Generation int64     `json:"generation"`
}

// DNSContributionFence retains desired-state generations and retired lifetimes.
type DNSContributionFence struct {
	UID        types.UID `json:"uid"`
	Generation int64     `json:"generation"`
	Sequence   int64     `json:"sequence"`
	Retired    bool      `json:"retired,omitempty"`
	SourceName string    `json:"sourceName,omitempty"`
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
	// +kubebuilder:validation:Minimum=1
	WriterEpoch int64 `json:"writerEpoch"`
	// +kubebuilder:validation:Minimum=1
	Revision int64 `json:"revision"`
	// +kubebuilder:validation:Minimum=0
	Index int32 `json:"index"`
	// +kubebuilder:validation:Pattern=`^[a-fA-F0-9]{64}$`
	SHA256 string `json:"sha256"`
	// +kubebuilder:validation:MinLength=1
	Payload []byte `json:"payload"`
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
// +kubebuilder:validation:XValidation:rule="self.writerEpoch >= oldSelf.writerEpoch && self.nextRevision >= oldSelf.nextRevision",message="publication ownership fences must not decrease"
// +kubebuilder:validation:XValidation:rule="self.holderIdentity == oldSelf.holderIdentity || self.writerEpoch > oldSelf.writerEpoch",message="ownership transfer requires a new writer epoch"
type DNSPublicationOwnershipSpec struct {
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="zoneUID is immutable"
	ZoneUID types.UID `json:"zoneUID"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	HolderIdentity string `json:"holderIdentity"`
	// +kubebuilder:validation:Minimum=1
	WriterEpoch int64 `json:"writerEpoch"`
	// +kubebuilder:validation:Minimum=1
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
