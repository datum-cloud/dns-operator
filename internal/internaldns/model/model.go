// Package model defines the durable wire contract shared by the internal DNS
// control plane and every regional serving replica.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/miekg/dns"
)

const APIVersion = "internal.dns.networking.miloapis.com/v1alpha2"

type Kind string

const (
	KindServingSnapshot     Kind = "ServingSnapshot"
	KindPublicationChunk    Kind = "PublicationChunk"
	KindPublicationManifest Kind = "PublicationManifest"
	KindMemberAck           Kind = "MemberAck"
)

// Envelope is the only value written to JetStream. Epoch and Revision are
// repeated outside Payload so consumers can reject stale events before decode.
type Envelope struct {
	APIVersion  string          `json:"apiVersion"`
	Kind        Kind            `json:"kind"`
	EventID     string          `json:"eventID"`
	Region      string          `json:"region"`
	Shard       string          `json:"shard"`
	ResourceUID string          `json:"resourceUID"`
	Epoch       uint64          `json:"epoch"`
	Revision    uint64          `json:"revision"`
	PublishedAt time.Time       `json:"publishedAt"`
	Payload     json.RawMessage `json:"payload"`
}

func NewEnvelope(kind Kind, eventID, region, shard, uid string, epoch, revision uint64, at time.Time, payload any) (Envelope, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("marshal %s payload: %w", kind, err)
	}
	e := Envelope{APIVersion: APIVersion, Kind: kind, EventID: eventID, Region: region, Shard: shard, ResourceUID: uid, Epoch: epoch, Revision: revision, PublishedAt: at.UTC(), Payload: b}
	return e, e.Validate()
}

func (e Envelope) Validate() error {
	if e.APIVersion != APIVersion {
		return fmt.Errorf("unsupported apiVersion %q", e.APIVersion)
	}
	switch e.Kind {
	case KindServingSnapshot, KindPublicationChunk, KindPublicationManifest, KindMemberAck:
	default:
		return fmt.Errorf("unsupported kind %q", e.Kind)
	}
	if e.EventID == "" || e.Region == "" || e.Shard == "" || e.ResourceUID == "" || e.Epoch == 0 || e.Revision == 0 || e.PublishedAt.IsZero() || len(e.Payload) == 0 {
		return errors.New("envelope identity, epoch, revision, timestamp, and payload are required")
	}
	if SafeToken(e.Region) != e.Region || SafeToken(e.Shard) != e.Shard {
		return errors.New("region and shard must be safe lowercase identifiers")
	}
	return nil
}

type Transport string

const (
	TransportUDP Transport = "UDP"
	TransportTCP Transport = "TCP"
)

type Authorization struct {
	IssuerEpoch uint64    `json:"issuerEpoch"`
	Revision    uint64    `json:"revision"`
	ValidUntil  time.Time `json:"validUntil"`
}

type Backend struct {
	MemberID string `json:"memberID"`
	Address  string `json:"address"`
	Port     uint16 `json:"port"`
}

type ZoneAttachment struct {
	ZoneUID                     string `json:"zoneUID"`
	Apex                        string `json:"apex"`
	RequiredPublicationEpoch    uint64 `json:"requiredPublicationEpoch"`
	RequiredPublicationRevision uint64 `json:"requiredPublicationRevision"`
}

// Binding is a logical cache and policy context in shared processes.
type Binding struct {
	BindingUID            string           `json:"bindingUID"`
	ProjectUID            string           `json:"projectUID"`
	ContextUID            string           `json:"contextUID"`
	BindingGeneration     uint64           `json:"bindingGeneration"`
	ConfigurationRevision uint64           `json:"configurationRevision"`
	ConsumerAddress       string           `json:"consumerAddress"`
	ClusterAddress        string           `json:"clusterAddress"`
	Port                  uint16           `json:"port"`
	Transports            []Transport      `json:"transports"`
	Zones                 []ZoneAttachment `json:"zones"`
	NodeBackends          []Backend        `json:"nodeBackends"`
	ClusterBackends       []Backend        `json:"clusterBackends"`
	Authorization         Authorization    `json:"authorization"`
	Tombstone             bool             `json:"tombstone,omitempty"`
}

func (b Binding) ViewName() string {
	return "context_" + OpaqueToken(b.ProjectUID+"/"+b.ContextUID) + "_g" + fmt.Sprint(b.BindingGeneration)
}

type Member struct {
	MemberID  string `json:"memberID"`
	ReplicaID string `json:"replicaID,omitempty"`
	Role      string `json:"role"`
}

type PublicationRequirement struct {
	ZoneUID  string `json:"zoneUID"`
	Epoch    uint64 `json:"epoch"`
	Revision uint64 `json:"revision"`
}

// ServingSnapshot is complete for a shard. Omission therefore means removal,
// but only after the entire snapshot has passed validation and been staged.
type ServingSnapshot struct {
	Region                string                   `json:"region"`
	Shard                 string                   `json:"shard"`
	ConfigurationEpoch    uint64                   `json:"configurationEpoch"`
	ConfigurationRevision uint64                   `json:"configurationRevision"`
	Bindings              []Binding                `json:"bindings"`
	Members               []Member                 `json:"members"`
	RequiredPublications  []PublicationRequirement `json:"requiredPublications,omitempty"`
	GeneratedAt           time.Time                `json:"generatedAt"`
}

func (s ServingSnapshot) Validate() error {
	if s.Region == "" || s.Shard == "" || s.ConfigurationEpoch == 0 || s.ConfigurationRevision == 0 || s.GeneratedAt.IsZero() {
		return errors.New("serving snapshot identity, epoch, revision, and timestamp are required")
	}

	seenVIP := map[string]string{}
	seenUID := map[string]struct{}{}
	seenMembers := map[string]struct{}{}
	for _, member := range s.Members {
		if member.MemberID == "" || (member.Role != "resolver" && member.Role != "node" && member.Role != "cluster" && member.Role != "regional") {
			return fmt.Errorf("invalid serving member %q with role %q", member.MemberID, member.Role)
		}
		key := member.MemberID + "\x00" + member.ReplicaID
		if _, ok := seenMembers[key]; ok {
			return fmt.Errorf("duplicate serving member %q replica %q", member.MemberID, member.ReplicaID)
		}
		seenMembers[key] = struct{}{}
	}
	seenRequirements := map[string]struct{}{}
	for _, requirement := range s.RequiredPublications {
		if requirement.ZoneUID == "" || requirement.Epoch == 0 || requirement.Revision == 0 {
			return errors.New("required publication has incomplete identity")
		}
		if _, ok := seenRequirements[requirement.ZoneUID]; ok {
			return fmt.Errorf("duplicate required publication %q", requirement.ZoneUID)
		}
		seenRequirements[requirement.ZoneUID] = struct{}{}
	}
	for i, b := range s.Bindings {
		if b.BindingUID == "" || b.ProjectUID == "" || b.ContextUID == "" || b.BindingGeneration == 0 || b.ConfigurationRevision == 0 {
			return fmt.Errorf("binding %d has incomplete identity", i)
		}
		if _, ok := seenUID[b.BindingUID]; ok {
			return fmt.Errorf("duplicate binding UID %q", b.BindingUID)
		}
		seenUID[b.BindingUID] = struct{}{}
		for name, raw := range map[string]string{"consumerAddress": b.ConsumerAddress, "clusterAddress": b.ClusterAddress} {
			addr, err := netip.ParseAddr(raw)
			if err != nil || addr.IsUnspecified() {
				return fmt.Errorf("binding %q invalid %s %q", b.BindingUID, name, raw)
			}
			if old, ok := seenVIP[raw]; ok && old != b.BindingUID {
				return fmt.Errorf("address %s assigned to bindings %q and %q", raw, old, b.BindingUID)
			}
			seenVIP[raw] = b.BindingUID
		}
		if b.Port == 0 || len(b.Transports) == 0 || b.Authorization.IssuerEpoch == 0 || b.Authorization.Revision == 0 || b.Authorization.ValidUntil.IsZero() {
			return fmt.Errorf("binding %q has incomplete transport or authorization", b.BindingUID)
		}
		if err := validateTransports(b.Transports); err != nil {
			return fmt.Errorf("binding %q: %w", b.BindingUID, err)
		}
		for _, group := range [][]Backend{b.NodeBackends, b.ClusterBackends} {
			backendMembers := map[string]string{}
			backendAddresses := map[string]string{}
			for _, backend := range group {
				if backend.MemberID == "" || backend.Port == 0 {
					return fmt.Errorf("binding %q has incomplete backend", b.BindingUID)
				}
				if addr, err := netip.ParseAddr(backend.Address); err != nil || addr.IsUnspecified() {
					return fmt.Errorf("binding %q has invalid backend address %q", b.BindingUID, backend.Address)
				}
				endpoint := netip.MustParseAddr(backend.Address).String() + ":" + fmt.Sprint(backend.Port)
				if old, ok := backendMembers[backend.MemberID]; ok && old != endpoint {
					return fmt.Errorf("binding %q backend member %q has conflicting endpoints", b.BindingUID, backend.MemberID)
				}
				if old, ok := backendAddresses[endpoint]; ok && old != backend.MemberID {
					return fmt.Errorf("binding %q backend endpoint %q has conflicting member IDs", b.BindingUID, endpoint)
				}
				backendMembers[backend.MemberID] = endpoint
				backendAddresses[endpoint] = backend.MemberID
			}
		}
		zoneApex := map[string]string{}
		for _, z := range b.Zones {
			if z.ZoneUID == "" || z.RequiredPublicationEpoch == 0 || z.RequiredPublicationRevision == 0 {
				return fmt.Errorf("binding %q has incomplete zone attachment", b.BindingUID)
			}
			apex := AbsoluteName(z.Apex)
			if _, ok := dns.IsDomainName(apex); apex == "." || !ok {
				return fmt.Errorf("binding %q has invalid zone apex", b.BindingUID)
			}
			if old, ok := zoneApex[apex]; ok && old != z.ZoneUID {
				return fmt.Errorf("binding %q attaches zones %q and %q at %s", b.BindingUID, old, z.ZoneUID, apex)
			}
			zoneApex[apex] = z.ZoneUID
		}
	}
	return nil
}

func validateTransports(ts []Transport) error {
	seen := map[Transport]bool{}
	for _, t := range ts {
		if t != TransportUDP && t != TransportTCP {
			return fmt.Errorf("unsupported transport %q", t)
		}
		if seen[t] {
			return fmt.Errorf("duplicate transport %q", t)
		}
		seen[t] = true
	}
	return nil
}

type ChunkRef struct {
	Index  int    `json:"index"`
	SHA256 string `json:"sha256"`
	Size   int    `json:"size"`
}

type PublicationManifest struct {
	ManifestUID      string     `json:"manifestUID"`
	ZoneUID          string     `json:"zoneUID"`
	Apex             string     `json:"apex"`
	WriterEpoch      uint64     `json:"writerEpoch"`
	Revision         uint64     `json:"revision"`
	PreviousRevision uint64     `json:"previousRevision,omitempty"`
	Tombstone        bool       `json:"tombstone,omitempty"`
	Chunks           []ChunkRef `json:"chunks"`
	ContentHash      string     `json:"contentHash"`
	GeneratedAt      time.Time  `json:"generatedAt"`
}

func (m PublicationManifest) Validate() error {
	if m.ManifestUID == "" || m.ZoneUID == "" || AbsoluteName(m.Apex) == "." || m.WriterEpoch == 0 || m.Revision == 0 || m.GeneratedAt.IsZero() {
		return errors.New("publication manifest identity, zone, epoch, revision, and timestamp are required")
	}
	if m.PreviousRevision >= m.Revision && m.PreviousRevision != 0 {
		return errors.New("publication manifest previous revision must precede revision")
	}
	if _, ok := dns.IsDomainName(AbsoluteName(m.Apex)); !ok {
		return errors.New("publication manifest apex is invalid")
	}
	if m.Tombstone {
		if len(m.Chunks) != 0 || m.ContentHash != EmptyContentHash() {
			return errors.New("tombstone must have no chunks and the empty content hash")
		}
		return nil
	}
	if len(m.Chunks) == 0 || !validHash(m.ContentHash) {
		return errors.New("non-tombstone manifest requires chunks and a SHA-256 content hash")
	}
	refs := append([]ChunkRef(nil), m.Chunks...)
	sort.Slice(refs, func(i, j int) bool { return refs[i].Index < refs[j].Index })
	for i, ref := range refs {
		if ref.Index != i || ref.Size <= 0 || !validHash(ref.SHA256) {
			return fmt.Errorf("invalid chunk reference at ordered index %d", i)
		}
	}
	return nil
}

type PublicationChunk struct {
	ManifestUID string `json:"manifestUID"`
	ZoneUID     string `json:"zoneUID"`
	WriterEpoch uint64 `json:"writerEpoch"`
	Revision    uint64 `json:"revision"`
	Index       int    `json:"index"`
	SHA256      string `json:"sha256"`
	Payload     []byte `json:"payload"`
}

func (c PublicationChunk) Validate() error {
	if c.ManifestUID == "" || c.ZoneUID == "" || c.WriterEpoch == 0 || c.Revision == 0 || c.Index < 0 || len(c.Payload) == 0 || !validHash(c.SHA256) {
		return errors.New("publication chunk identity, revision, hash, and payload are required")
	}
	if Hash(c.Payload) != c.SHA256 {
		return errors.New("publication chunk SHA-256 mismatch")
	}
	return nil
}

type RRRecord struct {
	Content         string    `json:"content"`
	ContributionUID string    `json:"contributionUID,omitempty"`
	WriterEpoch     uint64    `json:"writerEpoch,omitempty"`
	Sequence        uint64    `json:"sequence,omitempty"`
	Eligible        bool      `json:"eligible"`
	ValidUntil      time.Time `json:"validUntil,omitempty"`
}

type RRSet struct {
	Name    string     `json:"name"`
	Type    string     `json:"type"`
	TTL     uint32     `json:"ttl"`
	Records []RRRecord `json:"records"`
}

type PublicationPlan struct {
	ZoneUID           string              `json:"zoneUID"`
	Apex              string              `json:"apex"`
	Registrations     []RegistrationFence `json:"registrations,omitempty"`
	ObservationFences []ObservationFence  `json:"observationFences,omitempty"`
	Owners            []string            `json:"owners,omitempty"`
	RRSets            []RRSet             `json:"rrsets"`
}

type RegistrationFence struct {
	UID        string `json:"uid"`
	Generation uint64 `json:"generation"`
}

// ObservationFence carries the accepted high-water mark even when an
// unhealthy or withdrawn observation produces no records. ValidUntil is the
// original contributor deadline and is part of the immutable fence identity.
type ObservationFence struct {
	ContributionUID string    `json:"contributionUID"`
	GrantUID        string    `json:"grantUID"`
	WriterEpoch     uint64    `json:"writerEpoch"`
	Sequence        uint64    `json:"sequence"`
	ValidUntil      time.Time `json:"validUntil"`
}

func (p PublicationPlan) Validate() error {
	if p.ZoneUID == "" || AbsoluteName(p.Apex) == "." {
		return errors.New("publication plan zone identity is required")
	}
	apex := AbsoluteName(p.Apex)
	if _, ok := dns.IsDomainName(apex); !ok {
		return errors.New("publication plan apex is invalid")
	}
	previousRegistration := ""
	for _, registration := range p.Registrations {
		if registration.UID == "" || registration.Generation == 0 {
			return errors.New("publication registration fence is incomplete")
		}
		if previousRegistration != "" && registration.UID <= previousRegistration {
			return errors.New("publication registration fences must be unique and sorted by UID")
		}
		previousRegistration = registration.UID
	}
	observationByUID := make(map[string]ObservationFence, len(p.ObservationFences))
	previousContribution := ""
	for _, observation := range p.ObservationFences {
		if observation.ContributionUID == "" || observation.GrantUID == "" || observation.WriterEpoch == 0 || observation.Sequence == 0 || observation.ValidUntil.IsZero() {
			return errors.New("publication observation fence is incomplete")
		}
		if previousContribution != "" && observation.ContributionUID <= previousContribution {
			return errors.New("publication observation fences must be unique and sorted by contribution UID")
		}
		previousContribution = observation.ContributionUID
		observationByUID[observation.ContributionUID] = observation
	}
	for _, owner := range p.Owners {
		owner = AbsoluteName(owner)
		if _, ok := dns.IsDomainName(owner); !ok || !dns.IsSubDomain(apex, owner) {
			return fmt.Errorf("owned name %q is outside zone %q", owner, apex)
		}
	}
	seen := map[string]struct{}{}
	for _, rr := range p.RRSets {
		owner := strings.ToLower(AbsoluteName(rr.Name))
		rrType := strings.ToUpper(rr.Type)
		key := owner + "\x00" + rrType
		if rr.TTL == 0 || len(rr.Records) == 0 || rr.Type == "" {
			return fmt.Errorf("RRset %q is incomplete", key)
		}
		if _, ok := dns.IsDomainName(owner); !ok || !dns.IsSubDomain(apex, owner) {
			return fmt.Errorf("RRset owner %q is outside zone %q", owner, apex)
		}
		if _, ok := dns.StringToType[rrType]; !ok {
			return fmt.Errorf("RRset %q has unsupported type %q", owner, rr.Type)
		}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate RRset %q", key)
		}
		seen[key] = struct{}{}
		for _, rec := range rr.Records {
			if rec.Content == "" {
				return fmt.Errorf("RRset %q contains an invalid record", key)
			}
			if rec.ContributionUID == "" {
				if rec.WriterEpoch != 0 || rec.Sequence != 0 || !rec.ValidUntil.IsZero() {
					return fmt.Errorf("RRset %q static record contains contribution fence data", key)
				}
			} else {
				observation, ok := observationByUID[rec.ContributionUID]
				if !ok || rec.WriterEpoch != observation.WriterEpoch || rec.Sequence != observation.Sequence || !rec.ValidUntil.Equal(observation.ValidUntil) {
					return fmt.Errorf("RRset %q record fence does not match contribution observation %q", key, rec.ContributionUID)
				}
			}
			if _, err := dns.NewRR(fmt.Sprintf("%s %d IN %s %s", owner, rr.TTL, rrType, rec.Content)); err != nil {
				return fmt.Errorf("RRset %q contains invalid RDATA: %w", key, err)
			}
		}
	}
	return nil
}

type AckPhase string

const (
	AckAccepted AckPhase = "Accepted"
	AckApplied  AckPhase = "Applied"
	AckVerified AckPhase = "Verified"
	AckExpired  AckPhase = "Expired"
	AckRejected AckPhase = "Rejected"
)

type MemberAck struct {
	MemberID    string    `json:"memberID"`
	ReplicaID   string    `json:"replicaID,omitempty"`
	ResourceUID string    `json:"resourceUID"`
	Kind        Kind      `json:"kind"`
	Epoch       uint64    `json:"epoch"`
	Revision    uint64    `json:"revision"`
	Phase       AckPhase  `json:"phase"`
	ObservedAt  time.Time `json:"observedAt"`
	ValidUntil  time.Time `json:"validUntil,omitempty"`
	Error       string    `json:"error,omitempty"`
}

func Hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func EmptyContentHash() string { return Hash(nil) }

func VerifyManifest(m PublicationManifest, chunks []PublicationChunk) ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if m.Tombstone {
		return nil, nil
	}
	if len(chunks) != len(m.Chunks) {
		return nil, fmt.Errorf("manifest references %d chunks but %d were supplied", len(m.Chunks), len(chunks))
	}
	byIndex := make(map[int]PublicationChunk, len(chunks))
	for _, c := range chunks {
		if err := c.Validate(); err != nil {
			return nil, err
		}
		if c.ManifestUID != m.ManifestUID || c.ZoneUID != m.ZoneUID || c.WriterEpoch != m.WriterEpoch || c.Revision != m.Revision {
			return nil, fmt.Errorf("chunk %d identity does not match manifest", c.Index)
		}
		if _, exists := byIndex[c.Index]; exists {
			return nil, fmt.Errorf("duplicate chunk index %d", c.Index)
		}
		byIndex[c.Index] = c
	}
	var payload []byte
	for _, ref := range m.Chunks {
		c, ok := byIndex[ref.Index]
		if !ok || len(c.Payload) != ref.Size || c.SHA256 != ref.SHA256 {
			return nil, fmt.Errorf("chunk %d is missing or differs from manifest", ref.Index)
		}
		payload = append(payload, c.Payload...)
	}
	if Hash(payload) != m.ContentHash {
		return nil, errors.New("publication content SHA-256 mismatch")
	}
	return payload, nil
}

func Compare(epoch, revision, currentEpoch, currentRevision uint64) int {
	if epoch < currentEpoch || (epoch == currentEpoch && revision < currentRevision) {
		return -1
	}
	if epoch == currentEpoch && revision == currentRevision {
		return 0
	}
	return 1
}

func AbsoluteName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "."
	}
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

var unsafeToken = regexp.MustCompile(`[^a-z0-9_-]+`)

func SafeToken(s string) string {
	s = unsafeToken.ReplaceAllString(strings.ToLower(s), "_")
	s = strings.Trim(s, "_")
	if s == "" {
		return "unknown"
	}
	return s
}

// OpaqueToken avoids placing customer-controlled identifiers in NATS subjects.
func OpaqueToken(uid string) string {
	sum := sha256.Sum256([]byte(uid))
	return hex.EncodeToString(sum[:12])
}

func validHash(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
