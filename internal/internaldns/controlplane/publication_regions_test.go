// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"slices"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// regionStore sends publication records to one region's own store and
// everything else to the shared API, as a regional control plane's
// publication store does.
type regionStore struct {
	client.Client
	store client.Client
}

func publicationRecord(obj runtime.Object) bool {
	switch obj.(type) {
	case *dnsv1alpha1.DNSPublicationOwnership, *dnsv1alpha1.DNSPublicationOwnershipList,
		*dnsv1alpha1.DNSPublicationManifest, *dnsv1alpha1.DNSPublicationManifestList,
		*dnsv1alpha1.DNSPublicationChunk, *dnsv1alpha1.DNSPublicationChunkList,
		*dnsv1alpha1.DNSTransportOutbox, *dnsv1alpha1.DNSTransportOutboxList:
		return true
	}
	return false
}

func (c regionStore) pick(obj runtime.Object) client.Client {
	if publicationRecord(obj) {
		return c.store
	}
	return c.Client
}

func (c regionStore) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.pick(obj).Get(ctx, key, obj, opts...)
}
func (c regionStore) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.pick(list).List(ctx, list, opts...)
}
func (c regionStore) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	return c.pick(obj).Create(ctx, obj, opts...)
}
func (c regionStore) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	return c.pick(obj).Patch(ctx, obj, patch, opts...)
}
func (c regionStore) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	return c.pick(obj).Update(ctx, obj, opts...)
}
func (c regionStore) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	return c.pick(obj).Delete(ctx, obj, opts...)
}

func TestEachRegionPublishesItsOwnTargets(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s := runtime.NewScheme()
	if err := addReferenceStateToScheme(s); err != nil {
		t.Fatal(err)
	}
	network := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "project", UID: "vpc-a"}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer-a"}}
	zone := fixture(now).Zone.DeepCopy()
	zone.Namespace = "project"
	// A zone held by the control plane that published every region.
	zone.Finalizers = []string{privateZoneFinalizer}
	assoc := fixture(now).Associations[0].DeepCopy()
	assoc.Namespace = "project"
	assoc.Name = "zone-prod"
	assoc.Spec.ResolverContextRef = dnsv1alpha1.DNSNameReference{Name: "prod"}
	assoc.Status = dnsv1alpha1.DNSZoneAssociationStatus{}
	reg := fixture(now).Registrations[0].DeepCopy()
	reg.Namespace = "project"
	contrib := fixture(now).Contributions[0].DeepCopy()
	contrib.Namespace = "project"
	contrib.Generation = 1
	accessEast := &dnsv1alpha1.DNSResolverAccessBinding{ObjectMeta: metav1.ObjectMeta{Name: "prod-east", Namespace: "project", UID: "access-east"}, Spec: dnsv1alpha1.DNSResolverAccessBindingSpec{ContextRef: dnsv1alpha1.DNSObjectReference{Name: "prod", UID: "vpc-a"}, Region: "east", DestinationAddress: "fd53::11"}}
	accessWest := accessEast.DeepCopy()
	accessWest.Name = "prod-west"
	accessWest.UID = "access-west"
	accessWest.Spec.Region = "west"
	accessWest.Spec.DestinationAddress = "fd53::12"
	status := []client.Object{&dnsv1alpha1.DNSResolverContext{}, &dnsv1alpha1.DNSResolverAccessBinding{}, &dnsv1alpha1.DNSZoneAssociation{}, &dnsv1alpha1.DNSRegistration{}, &dnsv1alpha1.DNSRecordContribution{}, &dnsv1alpha1.DNSPublicationManifest{}}
	shared := fake.NewClientBuilder().WithScheme(s).WithObjects(network, zone, assoc, reg, contrib, accessEast, accessWest).WithStatusSubresource(status...).Build()
	stores := map[string]client.Client{
		"east": fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(status...).Build(),
		"west": fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(status...).Build(),
	}
	reconcilers := map[string]*Reconciler{}
	for region, store := range stores {
		reconcilers[region] = &Reconciler{Client: regionStore{Client: shared, store: store}, Scheme: s, Options: ReconcilerOptions{ProjectUID: "project-a", ProjectNamespace: "project", PlatformNamespace: "internal-dns-system", Identity: "compiler-" + region,
			ServingRegions: []ServingRegion{{Region: "east", Shard: "s1"}, {Region: "west", Shard: "s2"}}, Regions: []string{region}, AddressAllocator: testAllocator{}, Now: func() time.Time { return now }}}
	}
	for i := 0; i < 8; i++ {
		for _, region := range []string{"east", "west"} {
			if _, err := reconcilers[region].Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "project", Name: "project"}}); err != nil {
				t.Fatalf("%s reconcile %d: %v", region, i, err)
			}
		}
	}

	var bindings dnsv1alpha1.DNSResolverBindingList
	if err := shared.List(ctx, &bindings); err != nil {
		t.Fatal(err)
	}
	if len(bindings.Items) != 2 {
		t.Fatalf("each region binds its own access: got %d bindings", len(bindings.Items))
	}
	for region, store := range stores {
		var manifests dnsv1alpha1.DNSPublicationManifestList
		if err := store.List(ctx, &manifests); err != nil {
			t.Fatal(err)
		}
		if len(manifests.Items) != 1 {
			t.Fatalf("%s: want one manifest in its own store, got %d", region, len(manifests.Items))
		}
		m := manifests.Items[0]
		if len(m.Spec.ServingTargets) != 1 || m.Spec.ServingTargets[0].Region != region {
			t.Fatalf("%s: a region publishes only its own targets, got %#v", region, m.Spec.ServingTargets)
		}
		if !slices.Equal(m.Spec.ExpectedRegions, []string{"east", "west"}) {
			t.Fatalf("%s: expected regions = %v", region, m.Spec.ExpectedRegions)
		}
		var outboxes dnsv1alpha1.DNSTransportOutboxList
		if err := store.List(ctx, &outboxes); err != nil {
			t.Fatal(err)
		}
		for _, o := range outboxes.Items {
			if o.Labels["internal-dns.miloapis.com/region"] != region {
				t.Fatalf("%s: store holds an outbox for %s", region, o.Labels["internal-dns.miloapis.com/region"])
			}
		}
	}
	var current dnsv1alpha1.DNSZone
	if err := shared.Get(ctx, client.ObjectKeyFromObject(zone), &current); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{privateZoneFinalizer, privateZoneFinalizer + ".east", privateZoneFinalizer + ".west"} {
		if !controllerutil.ContainsFinalizer(&current, f) {
			t.Fatalf("zone finalizers = %v, missing %s", current.Finalizers, f)
		}
	}
	var registration dnsv1alpha1.DNSRegistration
	if err := shared.Get(ctx, client.ObjectKeyFromObject(reg), &registration); err != nil {
		t.Fatal(err)
	}
	if len(registration.Status.Regions) != 2 || registration.Status.PublicationRevision != 0 {
		t.Fatalf("want one status entry per region and no shared revision, got %#v", registration.Status)
	}
	c := apimeta.FindStatusCondition(registration.Status.Conditions, "Published")
	if c == nil || c.Status != metav1.ConditionFalse || c.Message != "publication awaits required replica verification in east, west" {
		t.Fatalf("Published = %#v", c)
	}

	// East's serving members verify its publication; west's do not.
	var eastManifests dnsv1alpha1.DNSPublicationManifestList
	if err := stores["east"].List(ctx, &eastManifests); err != nil {
		t.Fatal(err)
	}
	m := eastManifests.Items[0]
	apimeta.SetStatusCondition(&m.Status.Conditions, metav1.Condition{Type: "Published", Status: metav1.ConditionTrue, Reason: "ReplicaVerification", LastTransitionTime: metav1.NewTime(now)})
	if err := stores["east"].Status().Update(ctx, &m); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcilers["east"].Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "project", Name: "project"}}); err != nil {
		t.Fatal(err)
	}
	if err := shared.Get(ctx, client.ObjectKeyFromObject(reg), &registration); err != nil {
		t.Fatal(err)
	}
	c = apimeta.FindStatusCondition(registration.Status.Conditions, "Published")
	if c == nil || c.Status != metav1.ConditionFalse || c.Message != "publication awaits required replica verification in west" {
		t.Fatalf("with only east verified, Published = %#v", c)
	}
	var contribution dnsv1alpha1.DNSRecordContribution
	if err := shared.Get(ctx, client.ObjectKeyFromObject(contrib), &contribution); err != nil {
		t.Fatal(err)
	}
	for _, e := range contribution.Status.Regions {
		if e.Published != (e.Region == "east") {
			t.Fatalf("contribution regions = %#v, want only east published", contribution.Status.Regions)
		}
	}
}

func TestLastRegionReleasesTheSharedFinalizer(t *testing.T) {
	east := &Reconciler{Options: ReconcilerOptions{Regions: []string{"east"}}}
	west := &Reconciler{Options: ReconcilerOptions{Regions: []string{"west"}}}
	z := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{privateZoneFinalizer, privateZoneFinalizer + ".east", privateZoneFinalizer + ".west", "other/finalizer"}}}
	east.releaseZone(z)
	if !slices.Equal(z.Finalizers, []string{privateZoneFinalizer, privateZoneFinalizer + ".west", "other/finalizer"}) {
		t.Fatalf("after east withdrew: %v", z.Finalizers)
	}
	west.releaseZone(z)
	if !slices.Equal(z.Finalizers, []string{"other/finalizer"}) {
		t.Fatalf("after the last region withdrew: %v", z.Finalizers)
	}
	shared := &Reconciler{}
	z.Finalizers = []string{privateZoneFinalizer, privateZoneFinalizer + ".east"}
	shared.releaseZone(z)
	if !slices.Equal(z.Finalizers, []string{privateZoneFinalizer + ".east"}) {
		t.Fatalf("a control plane for every region removed a regional finalizer: %v", z.Finalizers)
	}
}
