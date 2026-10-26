// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

type perZoneHarness struct {
	t   *testing.T
	ctx context.Context
	c   client.Client
	r   *Reconciler
}

func newPerZoneHarness(t *testing.T) (*perZoneHarness, *dnsv1alpha1.DNSZone) {
	t.Helper()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s := runtime.NewScheme()
	if err := addReferenceStateToScheme(s); err != nil {
		t.Fatal(err)
	}
	network := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "project", UID: "vpc-a"}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer-a"}}
	zone := fixture(now).Zone.DeepCopy()
	zone.Namespace = "project"
	zone.Finalizers = []string{privateZoneFinalizer}
	assoc := fixture(now).Associations[0].DeepCopy()
	assoc.Namespace, assoc.Name = "project", "zone-prod"
	assoc.Spec.ResolverContextRef = dnsv1alpha1.DNSNameReference{Name: "prod"}
	assoc.Status = dnsv1alpha1.DNSZoneAssociationStatus{}
	reg := fixture(now).Registrations[0].DeepCopy()
	reg.Namespace = "project"
	contrib := fixture(now).Contributions[0].DeepCopy()
	contrib.Namespace = "project"
	contrib.Generation = 1
	access := &dnsv1alpha1.DNSResolverAccessBinding{ObjectMeta: metav1.ObjectMeta{Name: "prod-east", Namespace: "project", UID: "access-east"}, Spec: dnsv1alpha1.DNSResolverAccessBindingSpec{ContextRef: dnsv1alpha1.DNSObjectReference{Name: "prod", UID: "vpc-a"}, Region: "east", DestinationAddress: "fd53::11"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(network, zone, assoc, reg, contrib, access).WithStatusSubresource(&dnsv1alpha1.DNSResolverContext{}, &dnsv1alpha1.DNSResolverAccessBinding{}, &dnsv1alpha1.DNSZoneAssociation{}, &dnsv1alpha1.DNSRegistration{}, &dnsv1alpha1.DNSRecordContribution{}, &dnsv1alpha1.DNSRecordSet{}, &dnsv1alpha1.DNSZone{}, &dnsv1alpha1.DNSPublicationManifest{}).Build()
	r := &Reconciler{Client: c, Scheme: s, zoneQueue: make(chan event.GenericEvent, 64), Options: ReconcilerOptions{ProjectUID: "project-a", ProjectNamespace: "project", PlatformNamespace: "internal-dns-system", Identity: "compiler-a", ServingRegions: []ServingRegion{{Region: "east", Shard: "s1"}}, AddressAllocator: testAllocator{}, Now: func() time.Time { return now }, PerZoneReconcile: true}}
	return &perZoneHarness{t: t, ctx: context.Background(), c: c, r: r}, zone
}

func (h *perZoneHarness) run(name string) ctrl.Result {
	h.t.Helper()
	res, err := h.r.Reconcile(h.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "project", Name: name}})
	if err != nil {
		h.t.Fatalf("reconcile %s: %v", name, err)
	}
	return res
}

// settle runs enough project passes, and every zone they queue, for bindings,
// leases and the first publication to exist.
func (h *perZoneHarness) settle() {
	h.t.Helper()
	for i := 0; i < 5; i++ {
		h.run(projectQueueKey)
		for len(h.r.zoneQueue) > 0 {
			e := <-h.r.zoneQueue
			h.run(zoneQueuePrefix + e.Object.GetName())
		}
	}
}

func (h *perZoneHarness) publishedNames() []string {
	h.t.Helper()
	var owners dnsv1alpha1.DNSPublicationOwnershipList
	if err := h.c.List(h.ctx, &owners); err != nil || len(owners.Items) != 1 {
		h.t.Fatalf("want one ownership, got %d: %v", len(owners.Items), err)
	}
	var m dnsv1alpha1.DNSPublicationManifest
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: "internal-dns-system", Name: owners.Items[0].Spec.ActiveManifestName}, &m); err != nil {
		h.t.Fatal(err)
	}
	payload := make([]byte, 0, h.r.Options.ChunkSize)
	for _, ref := range m.Spec.Chunks {
		var chunk dnsv1alpha1.DNSPublicationChunk
		if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: "internal-dns-system", Name: ref.Name}, &chunk); err != nil {
			h.t.Fatal(err)
		}
		payload = append(payload, chunk.Spec.Payload...)
	}
	var plan model.PublicationPlan
	if err := json.Unmarshal(payload, &plan); err != nil {
		h.t.Fatal(err)
	}
	names := make([]string, 0, len(plan.RRSets))
	for _, rr := range plan.RRSets {
		names = append(names, rr.Name)
	}
	return names
}

func TestPerZoneProjectPassHandsZonesToZoneKeys(t *testing.T) {
	h, _ := newPerZoneHarness(t)
	// Early passes create bindings and leases; the zone is handed over once
	// its lease is held.
	h.settle()
	var manifests dnsv1alpha1.DNSPublicationManifestList
	if err := h.c.List(h.ctx, &manifests); err != nil {
		t.Fatal(err)
	}
	if len(manifests.Items) == 0 {
		t.Fatal("zone keys never published the zone")
	}
	if names := h.publishedNames(); len(names) != 1 {
		t.Fatalf("published %v, want the contributed record", names)
	}
}

func TestPerZoneRecordChangePublishesWithoutAProjectPass(t *testing.T) {
	h, zone := newPerZoneHarness(t)
	h.settle()
	before := len(h.publishedNames())

	ttl := int64(30)
	rs := &dnsv1alpha1.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "static", Namespace: "project", UID: "static"}, Spec: dnsv1alpha1.DNSRecordSetSpec{DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name}, RecordType: dnsv1alpha1.RRTypeA, Records: []dnsv1alpha1.RecordEntry{{Name: "added", TTL: &ttl, A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.10"}}}}}
	if err := h.c.Create(h.ctx, rs); err != nil {
		t.Fatal(err)
	}
	for _, req := range recordSetZone(rs) {
		h.run(req.Name)
	}
	if after := h.publishedNames(); len(after) != before+1 {
		t.Fatalf("zone key published %v; want the new record next to the %d before", after, before)
	}
}

func TestPerZoneContributionKeyForwardsToItsZone(t *testing.T) {
	h, _ := newPerZoneHarness(t)
	h.settle()
	var contribs dnsv1alpha1.DNSRecordContributionList
	if err := h.c.List(h.ctx, &contribs); err != nil || len(contribs.Items) != 1 {
		t.Fatalf("contributions: %v %v", contribs.Items, err)
	}
	reqs := contributionRegistration(&contribs.Items[0])
	if len(reqs) != 1 {
		t.Fatalf("contribution keys %v", reqs)
	}
	for len(h.r.zoneQueue) > 0 {
		<-h.r.zoneQueue
	}
	h.run(reqs[0].Name)
	if len(h.r.zoneQueue) != 1 {
		t.Fatalf("registration key queued %d zone keys, want 1", len(h.r.zoneQueue))
	}
	if e := <-h.r.zoneQueue; e.Object.GetName() != "private" {
		t.Fatalf("registration key queued zone %q", e.Object.GetName())
	}
}

func TestPerZoneWaitsForAssociationPinsAtTheZoneGeneration(t *testing.T) {
	h, zone := newPerZoneHarness(t)
	h.settle()
	var owners dnsv1alpha1.DNSPublicationOwnershipList
	if err := h.c.List(h.ctx, &owners); err != nil || len(owners.Items) != 1 {
		t.Fatalf("ownership: %v", err)
	}
	published := owners.Items[0].Spec.ActiveManifestName

	var current dnsv1alpha1.DNSZone
	if err := h.c.Get(h.ctx, client.ObjectKeyFromObject(zone), &current); err != nil {
		t.Fatal(err)
	}
	current.Generation++
	if err := h.c.Update(h.ctx, &current); err != nil {
		t.Fatal(err)
	}
	res := h.run(zoneQueuePrefix + zone.Name)
	if res.RequeueAfter != staleAssociationRetry {
		t.Fatalf("zone key compiled against a stale association pin: %v", res)
	}
	if err := h.c.List(h.ctx, &owners); err != nil {
		t.Fatal(err)
	}
	if owners.Items[0].Spec.ActiveManifestName != published {
		t.Fatal("zone key published while its association was pinned to an older zone generation")
	}
}

func TestReadsBehindComparesCachedGenerationsWithPublishedFences(t *testing.T) {
	previous := &PublicationPlan{
		Registrations: []RegistrationFence{{UID: "reg", Generation: 3}},
		Contributions: []ContributionFence{{UID: "contrib", Generation: 5}},
	}
	reg := func(g int64) []dnsv1alpha1.DNSRegistration {
		return []dnsv1alpha1.DNSRegistration{{ObjectMeta: metav1.ObjectMeta{UID: "reg", Generation: g}}}
	}
	contrib := func(g int64) []dnsv1alpha1.DNSRecordContribution {
		return []dnsv1alpha1.DNSRecordContribution{{ObjectMeta: metav1.ObjectMeta{UID: "contrib", Generation: g}}}
	}
	for _, tt := range []struct {
		name   string
		regs   []dnsv1alpha1.DNSRegistration
		cs     []dnsv1alpha1.DNSRecordContribution
		behind bool
	}{
		{"current", reg(3), contrib(5), false},
		{"newer", reg(4), contrib(6), false},
		{"registration behind", reg(2), contrib(5), true},
		{"contribution behind", reg(3), contrib(4), true},
		{"deleted since", nil, nil, false},
	} {
		if got := readsBehind(previous, tt.regs, tt.cs); got != tt.behind {
			t.Errorf("%s: behind=%v, want %v", tt.name, got, tt.behind)
		}
	}
	if readsBehind(nil, reg(1), contrib(1)) {
		t.Error("a zone with no publication cannot be behind it")
	}
}

type staleContributions struct {
	client.Reader
}

func (s staleContributions) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := s.Reader.List(ctx, list, opts...); err != nil {
		return err
	}
	if cs, ok := list.(*dnsv1alpha1.DNSRecordContributionList); ok {
		for i := range cs.Items {
			cs.Items[i].Generation--
		}
	}
	return nil
}

func TestPerZoneWaitsForACacheBehindThePublication(t *testing.T) {
	h, zone := newPerZoneHarness(t)
	h.settle()
	var owners dnsv1alpha1.DNSPublicationOwnershipList
	if err := h.c.List(h.ctx, &owners); err != nil || len(owners.Items) != 1 {
		t.Fatalf("ownership: %v", err)
	}
	published := owners.Items[0].Spec.ActiveManifestName

	h.r.ZoneReader = staleContributions{Reader: h.c}
	if res := h.run(zoneQueuePrefix + zone.Name); res.RequeueAfter != cacheBehindRetry {
		t.Fatalf("zone key compiled from a cache older than its publication: %v", res)
	}
	if err := h.c.List(h.ctx, &owners); err != nil {
		t.Fatal(err)
	}
	if owners.Items[0].Spec.ActiveManifestName != published {
		t.Fatal("zone key published from a stale cache")
	}
	h.r.ZoneReader = h.c
	h.run(zoneQueuePrefix + zone.Name)
}
