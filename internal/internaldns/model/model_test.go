package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestVerifyManifestRequiresEveryExactChunk(t *testing.T) {
	t.Parallel()
	plan := PublicationPlan{ZoneUID: "zone-a", Apex: "internal.example", RRSets: []RRSet{{Name: "api.internal.example", Type: "A", TTL: 30, Records: []RRRecord{{Content: "192.0.2.1", Eligible: true}}}}}
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	cut := len(payload) / 2
	parts := [][]byte{payload[:cut], payload[cut:]}
	chunks := make([]PublicationChunk, 0, 2)
	refs := make([]ChunkRef, 0, 2)
	for i, p := range parts {
		chunks = append(chunks, PublicationChunk{ManifestUID: "manifest-a", ZoneUID: "zone-a", WriterEpoch: 3, Revision: 9, Index: i, SHA256: Hash(p), Payload: p})
		refs = append(refs, ChunkRef{Index: i, SHA256: Hash(p), Size: len(p)})
	}
	m := PublicationManifest{ManifestUID: "manifest-a", ZoneUID: "zone-a", Apex: "internal.example", WriterEpoch: 3, Revision: 9, Chunks: refs, ContentHash: Hash(payload), GeneratedAt: time.Now()}
	got, err := VerifyManifest(m, chunks)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload differs")
	}
	if _, err := VerifyManifest(m, chunks[:1]); err == nil {
		t.Fatal("missing chunk accepted")
	}
	chunks[1].Payload[0] ^= 1
	if _, err := VerifyManifest(m, chunks); err == nil {
		t.Fatal("corrupt chunk accepted")
	}
}

func TestServingSnapshotRejectsSharedDestination(t *testing.T) {
	t.Parallel()
	now := time.Now()
	b := func(uid, vpc string) Binding {
		return Binding{BindingUID: uid, ProjectUID: "project", ContextUID: vpc, BindingGeneration: 1, ConfigurationRevision: 1, ConsumerAddress: "2001:db8::1", ClusterAddress: "2001:db8::2", Port: 53, Transports: []Transport{TransportUDP, TransportTCP}, Authorization: Authorization{IssuerEpoch: 1, Revision: 1, ValidUntil: now.Add(time.Minute)}}
	}
	s := ServingSnapshot{Region: "east", Shard: "s1", ConfigurationEpoch: 1, ConfigurationRevision: 1, GeneratedAt: now, Bindings: []Binding{b("one", "vpc-a"), b("two", "vpc-b")}}
	if err := s.Validate(); err == nil {
		t.Fatal("duplicate trusted destination accepted")
	}
}

func TestServingSnapshotRejectsUnknownMemberRole(t *testing.T) {
	s := ServingSnapshot{Region: "east", Shard: "s1", ConfigurationEpoch: 1, ConfigurationRevision: 1, GeneratedAt: time.Now(), Members: []Member{{MemberID: "old", Role: "unassigned"}}}
	if s.Validate() == nil {
		t.Fatal("unknown serving role accepted")
	}
}

func TestCompareFencesOldEpoch(t *testing.T) {
	t.Parallel()
	if Compare(2, 1000, 3, 1) >= 0 {
		t.Fatal("old epoch won by revision")
	}
	if Compare(4, 1, 3, 1000) <= 0 {
		t.Fatal("new epoch lost")
	}
}

func TestViewNameScopesContextByProject(t *testing.T) {
	a := Binding{ProjectUID: "project-a", ContextUID: "remote-context", BindingGeneration: 1}
	b := a
	b.ProjectUID = "project-b"
	if a.ViewName() == b.ViewName() {
		t.Fatal("identical remote context UID shared a cache namespace across projects")
	}
	b = a
	b.BindingGeneration = 2
	if a.ViewName() == b.ViewName() {
		t.Fatal("new binding generation reused an old cache namespace")
	}
}

func TestEnvelopeRejectsObsoleteWireVersion(t *testing.T) {
	current, err := NewEnvelope(KindMemberAck, "event", "central", "shared", "uid", 1, 1, time.Now(), MemberAck{MemberID: "member"})
	if err != nil {
		t.Fatal(err)
	}
	current.APIVersion = "internal.dns.networking.miloapis.com/v1alpha1"
	if current.Validate() == nil {
		t.Fatal("obsolete transport version accepted")
	}
}
