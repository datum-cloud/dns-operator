// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestExpiredOwnershipTakeoverFencesResumedWriter(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "zone", Namespace: "project", UID: "zone-a"}}
	ownerName := "owner-" + model.OpaqueToken(string(zone.UID))[:20]
	owner := &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: ownerName, Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{
		ZoneUID: zone.UID, HolderIdentity: "writer-a", WriterEpoch: 4, NextRevision: 12,
		ActiveManifestName: "active-e4-r11", LeaseUntil: metav1.NewTime(now.Add(-time.Second)),
	}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(owner).Build()
	writerB := &Reconciler{Client: c, Options: ReconcilerOptions{PlatformNamespace: "platform", Identity: "writer-b", LeaseDuration: 30 * time.Second}}
	acquired, err := writerB.acquireOwnership(ctx, zone, now)
	if err != nil || acquired == nil {
		t.Fatalf("takeover failed: owner=%#v err=%v", acquired, err)
	}
	if acquired.Spec.HolderIdentity != "writer-b" || acquired.Spec.WriterEpoch != 5 || acquired.Spec.NextRevision != 12 || acquired.Spec.ActiveManifestName != "active-e4-r11" {
		t.Fatalf("takeover corrupted the durable fence: %#v", acquired.Spec)
	}
	writerA := &Reconciler{Client: c, Options: ReconcilerOptions{PlatformNamespace: "platform", Identity: "writer-a", LeaseDuration: 30 * time.Second}}
	resumed, err := writerA.acquireOwnership(ctx, zone, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if resumed != nil {
		t.Fatalf("resumed old writer reacquired a live lease: %#v", resumed.Spec)
	}
	var current dnsv1alpha1.DNSPublicationOwnership
	if err := c.Get(ctx, client.ObjectKey{Namespace: "platform", Name: ownerName}, &current); err != nil {
		t.Fatal(err)
	}
	if current.Spec.HolderIdentity != "writer-b" || current.Spec.WriterEpoch != 5 || current.Spec.ActiveManifestName != "active-e4-r11" {
		t.Fatalf("resumed writer regressed ownership: %#v", current.Spec)
	}
}

func TestContributionDeadlineCannotDelayOwnershipRenewal(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	lease := 10 * time.Second
	if got := nextProjectRequeue(now, now.Add(90*time.Second), lease); got != 5*time.Second {
		t.Fatalf("long-lived contribution delayed lease renewal: got %s want 5s", got)
	}
	if got := nextProjectRequeue(now, now.Add(2*time.Second), lease); got != 2*time.Second {
		t.Fatalf("near contribution expiry was not honored: got %s want 2s", got)
	}
}

func TestQuiescentSurvivorRenewsOwnershipBeyondLeaseWindow(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	network := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "project", UID: "vpc-a"}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer-a"}}
	input := fixture(now)
	zone := input.Zone.DeepCopy()
	zone.Namespace = "project"
	zone.Finalizers = []string{privateZoneFinalizer}
	assoc := input.Associations[0].DeepCopy()
	assoc.Namespace = "project"
	assoc.Name = "zone-prod"
	assoc.Spec.VPCRef = dnsv1alpha1.DNSObjectReference{}
	assoc.Spec.ResolverContextRef = dnsv1alpha1.DNSObjectReference{Name: "prod", UID: "vpc-a"}
	assoc.Status = dnsv1alpha1.DNSZoneAssociationStatus{}
	reg := input.Registrations[0].DeepCopy()
	reg.Namespace = "project"
	grant := input.Grants[0].DeepCopy()
	grant.Namespace = "project"
	grant.Status = dnsv1alpha1.DNSContributionGrantStatus{}
	contribution := input.Contributions[0].DeepCopy()
	contribution.Namespace = "project"
	contribution.Status.WriterEpoch = 0
	contribution.Status.Sequence = 1
	until := metav1.NewTime(now.Add(90 * time.Second))
	contribution.Status.ValidUntil = &until
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(network, zone, assoc, reg, grant, contribution).
		WithStatusSubresource(&dnsv1alpha1.DNSResolverContext{}, &dnsv1alpha1.DNSResolverAccessBinding{}, &dnsv1alpha1.DNSZoneAssociation{}, &dnsv1alpha1.DNSRegistration{}, &dnsv1alpha1.DNSContributionGrant{}, &dnsv1alpha1.DNSRecordContribution{}, &dnsv1alpha1.DNSPublicationManifest{}).Build()
	options := ReconcilerOptions{
		ProjectUID: "project-a", SourceClusterUID: "cluster-a", ProjectNamespace: "project", PlatformNamespace: "platform",
		Identity: "writer-a", LeaseDuration: 10 * time.Second,
		ServingRegions: []ServingRegion{{Region: "east", Shard: "shared"}}, AddressAllocator: testAllocator{}, Now: func() time.Time { return now },
	}
	writerA := &Reconciler{Client: c, Scheme: s, Options: options}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "project", Name: "project"}}
	for i := 0; i < 8; i++ {
		if _, err := writerA.Reconcile(ctx, request); err != nil {
			t.Fatalf("initial reconcile %d: %v", i, err)
		}
	}

	// Stop A for longer than its lease, then let B take over the otherwise
	// quiescent 90-second publication.
	now = now.Add(11 * time.Second)
	options.Identity = "writer-b"
	writerB := &Reconciler{Client: c, Scheme: s, Options: options}
	result, err := writerB.Reconcile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter > 5*time.Second {
		t.Fatalf("survivor scheduled after its lease-renewal boundary: %s", result.RequeueAfter)
	}

	// Run B at each requested renewal and resume A after it. More than a full
	// lease elapses, but A must never reacquire while B remains healthy.
	for i := 0; i < 3; i++ {
		now = now.Add(result.RequeueAfter)
		result, err = writerB.Reconcile(ctx, request)
		if err != nil {
			t.Fatalf("survivor renewal %d: %v", i, err)
		}
		if result.RequeueAfter > 5*time.Second {
			t.Fatalf("survivor renewal %d scheduled too late: %s", i, result.RequeueAfter)
		}
		if _, err := writerA.Reconcile(ctx, request); err != nil {
			t.Fatalf("resumed writer reconcile %d: %v", i, err)
		}
	}
	var owner dnsv1alpha1.DNSPublicationOwnership
	ownerName := "owner-" + model.OpaqueToken(string(zone.UID))[:20]
	if err := c.Get(ctx, client.ObjectKey{Namespace: "platform", Name: ownerName}, &owner); err != nil {
		t.Fatal(err)
	}
	if owner.Spec.HolderIdentity != "writer-b" || owner.Spec.WriterEpoch != 2 {
		t.Fatalf("resumed old writer displaced healthy survivor: %#v", owner.Spec)
	}
}

func TestOwnershipTakeoverLoadsOldEpochHighWater(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	deadline := now.Add(-time.Minute).Add(101582 * time.Microsecond)
	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(model.PublicationPlan{ZoneUID: "zone-a", Apex: "corp.internal", ObservationFences: []model.ObservationFence{{ContributionUID: "contribution-a", GrantUID: "grant-a", WriterEpoch: 7, Sequence: 19, ValidUntil: deadline}}, RRSets: []model.RRSet{}})
	if err != nil {
		t.Fatal(err)
	}
	chunk := &dnsv1alpha1.DNSPublicationChunk{
		ObjectMeta: metav1.ObjectMeta{Name: "old-000", Namespace: "internal-dns-system"},
		Spec:       dnsv1alpha1.DNSPublicationChunkSpec{WriterEpoch: 1, Revision: 9, Index: 0, SHA256: model.Hash(payload), Payload: payload},
	}
	manifest := &dnsv1alpha1.DNSPublicationManifest{
		ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "internal-dns-system"},
		Spec: dnsv1alpha1.DNSPublicationManifestSpec{
			ZoneRef: dnsv1alpha1.DNSObjectReference{UID: "zone-a"}, WriterEpoch: 1, Revision: 9,
			Chunks:      []dnsv1alpha1.DNSPublicationChunkReference{{Name: chunk.Name, SHA256: chunk.Spec.SHA256, Size: int32(len(payload))}},
			ContentHash: model.Hash(payload),
			// The metadata deliberately represents the precision an API server may
			// retain. Recovery must use the exact immutable wire observation.
			ContributionFences: []dnsv1alpha1.DNSContributionFence{{UID: "contribution-a", GrantUID: "grant-a", Epoch: 7, Sequence: 19, ValidUntil: metav1.NewTime(deadline.Truncate(time.Second))}},
		},
	}
	owner := &dnsv1alpha1.DNSPublicationOwnership{
		ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "internal-dns-system"},
		Spec:       dnsv1alpha1.DNSPublicationOwnershipSpec{ZoneUID: "zone-a", WriterEpoch: 2, ActiveManifestName: manifest.Name},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(chunk, manifest, owner).Build()
	r := &Reconciler{Client: c, Options: ReconcilerOptions{PlatformNamespace: "internal-dns-system"}}
	revision, err := r.activeRevision(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if revision != 0 {
		t.Fatalf("new writer epoch inherited old revision %d", revision)
	}
	previous, err := r.loadPrevious(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(previous.Contributions) != 1 || previous.Contributions[0].Sequence != 19 {
		t.Fatalf("old epoch high-water fence was lost: %#v", previous)
	}
	if !previous.Contributions[0].ValidUntil.Equal(deadline) {
		t.Fatalf("wire deadline precision was lost: got %s want %s", previous.Contributions[0].ValidUntil, deadline)
	}
	in := fixture(now)
	in.Previous = previous
	in.Contributions[0].Status.Sequence = 18
	compiled, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Reasons["contribution-a"] != "NonIncreasingSequence" || len(compiled.Plan.RRsets) != 0 {
		t.Fatalf("ownership takeover replayed a stale contribution: %#v", compiled)
	}
}

func TestLoadPreviousFailsWhenActiveChunkIsMissing(t *testing.T) {
	ctx := context.Background()
	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	manifest := &dnsv1alpha1.DNSPublicationManifest{
		ObjectMeta: metav1.ObjectMeta{Name: "active", Namespace: "internal-dns-system"},
		Spec: dnsv1alpha1.DNSPublicationManifestSpec{
			WriterEpoch: 1, Revision: 1, ContentHash: model.Hash([]byte("missing")),
			Chunks: []dnsv1alpha1.DNSPublicationChunkReference{{Name: "missing", SHA256: model.Hash([]byte("missing")), Size: 7}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(manifest).Build()
	r := &Reconciler{Client: c, Options: ReconcilerOptions{PlatformNamespace: "internal-dns-system"}}
	owner := &dnsv1alpha1.DNSPublicationOwnership{Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ActiveManifestName: manifest.Name}}
	if _, err := r.loadPrevious(ctx, owner); err == nil {
		t.Fatal("missing active chunk was silently ignored")
	}
}

func TestLoadPreviousRejectsHashValidMalformedPayload(t *testing.T) {
	ctx := context.Background()
	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"zoneUID":`)
	chunk := &dnsv1alpha1.DNSPublicationChunk{ObjectMeta: metav1.ObjectMeta{Name: "chunk", Namespace: "internal-dns-system"}, Spec: dnsv1alpha1.DNSPublicationChunkSpec{WriterEpoch: 1, Revision: 1, Index: 0, SHA256: model.Hash(payload), Payload: payload}}
	manifest := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: "active", Namespace: "internal-dns-system"}, Spec: dnsv1alpha1.DNSPublicationManifestSpec{
		WriterEpoch: 1, Revision: 1, ContentHash: model.Hash(payload), Chunks: []dnsv1alpha1.DNSPublicationChunkReference{{Name: chunk.Name, SHA256: chunk.Spec.SHA256, Size: int32(len(payload))}},
	}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(manifest, chunk).Build()
	r := &Reconciler{Client: c, Options: ReconcilerOptions{PlatformNamespace: "internal-dns-system"}}
	owner := &dnsv1alpha1.DNSPublicationOwnership{Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ActiveManifestName: manifest.Name}}
	if _, err := r.loadPrevious(ctx, owner); err == nil || !strings.Contains(err.Error(), "decode active publication") {
		t.Fatalf("hash-valid malformed payload was silently accepted: %v", err)
	}
}

func TestEmptyPublicationUsesJSONArraysForRequiredSlices(t *testing.T) {
	ctx := context.Background()
	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	r := &Reconciler{Client: c, Options: ReconcilerOptions{PlatformNamespace: "internal-dns-system", ChunkSize: 1024}}
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "orphan", UID: "zone-orphan"}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "orphan.internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	manifest, _, err := r.persistPublication(ctx, zone, "empty", 1, 1, 0, true, nil, model.EmptyContentHash(), nil, nil, nil, nil, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Spec.ContextUIDs == nil || manifest.Spec.Chunks == nil {
		t.Fatalf("required manifest slices must be non-nil: %#v", manifest.Spec)
	}
	b, err := json.Marshal(manifest.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"contextUIDs":[]`) || !strings.Contains(string(b), `"chunks":[]`) {
		t.Fatalf("required slices encoded as null: %s", b)
	}
}

func TestReconcilerPersistsFencedMultiRegionOutbox(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	network := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "project", UID: "vpc-a"}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer-a"}}
	zone := fixture(now).Zone.DeepCopy()
	zone.Namespace = "project"
	zone.Finalizers = []string{privateZoneFinalizer}
	assoc := fixture(now).Associations[0].DeepCopy()
	assoc.Namespace = "project"
	assoc.Name = "zone-prod"
	assoc.Spec.VPCRef = dnsv1alpha1.DNSObjectReference{}
	assoc.Spec.ResolverContextRef = dnsv1alpha1.DNSObjectReference{Name: "prod", UID: "vpc-a"}
	assoc.Status = dnsv1alpha1.DNSZoneAssociationStatus{}
	reg := fixture(now).Registrations[0].DeepCopy()
	reg.Namespace = "project"
	grant := fixture(now).Grants[0].DeepCopy()
	grant.Namespace = "project"
	grant.Status = dnsv1alpha1.DNSContributionGrantStatus{}
	contrib := fixture(now).Contributions[0].DeepCopy()
	contrib.Namespace = "project"
	contrib.Status.WriterEpoch = 0
	contrib.Status.Sequence = 1
	accessEast := &dnsv1alpha1.DNSResolverAccessBinding{ObjectMeta: metav1.ObjectMeta{Name: "prod-east", Namespace: "project", UID: "access-east"}, Spec: dnsv1alpha1.DNSResolverAccessBindingSpec{ContextRef: dnsv1alpha1.DNSObjectReference{Name: "prod", UID: "vpc-a"}, Region: "east", QueryIdentity: dnsv1alpha1.DNSResolverQueryIdentity{Type: dnsv1alpha1.DNSResolverQueryIdentityDestinationAddress, Value: "fd53::11"}, Port: 53, Transports: []string{"UDP", "TCP"}, Authorization: dnsv1alpha1.DNSResolverAccessAuthorization{WriterEpoch: 1, Sequence: 1, ValidUntil: metav1.NewTime(now.Add(3 * time.Minute))}}}
	accessWest := accessEast.DeepCopy()
	accessWest.Name = "prod-west"
	accessWest.UID = "access-west"
	accessWest.Spec.Region = "west"
	accessWest.Spec.QueryIdentity.Value = "fd53::12"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(network, zone, assoc, reg, grant, contrib, accessEast, accessWest).WithStatusSubresource(&dnsv1alpha1.DNSResolverContext{}, &dnsv1alpha1.DNSResolverAccessBinding{}, &dnsv1alpha1.DNSZoneAssociation{}, &dnsv1alpha1.DNSRegistration{}, &dnsv1alpha1.DNSContributionGrant{}, &dnsv1alpha1.DNSRecordContribution{}, &dnsv1alpha1.DNSPublicationManifest{}).Build()
	r := &Reconciler{Client: c, Scheme: s, Options: ReconcilerOptions{ProjectUID: "project-a", SourceClusterUID: "cluster-a", ProjectNamespace: "project", PlatformNamespace: "internal-dns-system", Identity: "compiler-a", ServingRegions: []ServingRegion{{Region: "east", Shard: "s1"}, {Region: "west", Shard: "s2"}}, AddressAllocator: testAllocator{}, Now: func() time.Time { return now }}}
	for i := 0; i < 8; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "project", Name: "project"}}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	var bindings dnsv1alpha1.DNSResolverBindingList
	if err := c.List(ctx, &bindings); err != nil {
		t.Fatal(err)
	}
	if len(bindings.Items) != 2 {
		t.Fatalf("want two regional bindings, got %d", len(bindings.Items))
	}
	var manifests dnsv1alpha1.DNSPublicationManifestList
	if err := c.List(ctx, &manifests); err != nil {
		t.Fatal(err)
	}
	if len(manifests.Items) != 1 {
		t.Fatalf("want one shared manifest, got %d: %#v", len(manifests.Items), manifests.Items)
	}
	m := manifests.Items[0]
	var chunks dnsv1alpha1.DNSPublicationChunkList
	if err := c.List(ctx, &chunks); err != nil {
		t.Fatal(err)
	}
	if len(chunks.Items) != 1 {
		t.Fatalf("want one chunk, got %d", len(chunks.Items))
	}
	var plan model.PublicationPlan
	if err := json.Unmarshal(chunks.Items[0].Spec.Payload, &plan); err != nil {
		t.Fatal(err)
	}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(plan.RRSets) != 1 {
		t.Fatalf("record was not published: %#v", plan)
	}
	var outboxes dnsv1alpha1.DNSTransportOutboxList
	if err := c.List(ctx, &outboxes); err != nil {
		t.Fatal(err)
	}
	if len(outboxes.Items) != 4 {
		t.Fatalf("want chunk+activation per region, got %d", len(outboxes.Items))
	}
	events := map[string]bool{}
	for _, o := range outboxes.Items {
		var e model.Envelope
		if err := json.Unmarshal(o.Spec.Payload, &e); err != nil {
			t.Fatal(err)
		}
		if err := e.Validate(); err != nil {
			t.Fatal(err)
		}
		if events[e.EventID] {
			t.Fatalf("duplicate multi-region event ID %q", e.EventID)
		}
		events[e.EventID] = true
		if o.Spec.Activation && (len(o.Spec.DependsOn) != 1 || o.Spec.OwnershipRef.Name == "") {
			t.Fatalf("activation was not fenced: %#v", o.Spec)
		}
	}
	if len(m.Spec.ContributionFences) != 1 || m.Spec.ContributionFences[0].Sequence != 1 {
		t.Fatalf("missing durable contribution high-water: %#v", m.Spec.ContributionFences)
	}
}
