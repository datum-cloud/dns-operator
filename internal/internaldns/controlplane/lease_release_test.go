// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/platform"
	"go.miloapis.com/dns-operator/internal/internaldns/pubstore"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestTakeoverExportsUnchangedPublication(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expire    bool
		tombstone bool
	}{
		{name: "released"},
		{name: "expired", expire: true},
		{name: "released tombstone", tombstone: true},
		{name: "expired tombstone", expire: true, tombstone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			now := time.Date(2026, 10, 26, 1, 45, 0, 0, time.UTC)
			scheme := runtime.NewScheme()
			if err := addReferenceStateToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			input := fixture(now)
			zone := input.Zone.DeepCopy()
			zone.Namespace = "project"
			zone.Finalizers = []string{privateZoneFinalizer}
			assoc := input.Associations[0].DeepCopy()
			assoc.Namespace, assoc.Name = "project", "zone-prod"
			assoc.Spec.ResolverContextRef = dnsv1alpha1.DNSNameReference{Name: "prod"}
			assoc.Status = dnsv1alpha1.DNSZoneAssociationStatus{}
			reg := input.Registrations[0].DeepCopy()
			reg.Namespace = "project"
			contribution := input.Contributions[0].DeepCopy()
			contribution.Namespace, contribution.Generation = "project", 1
			network := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "prod", Namespace: "project", UID: "vpc-a"}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer-a"}}
			api := fake.NewClientBuilder().WithScheme(scheme).WithObjects(network, zone, assoc, reg, contribution).
				WithStatusSubresource(&dnsv1alpha1.DNSResolverContext{}, &dnsv1alpha1.DNSResolverAccessBinding{}, &dnsv1alpha1.DNSZoneAssociation{}, &dnsv1alpha1.DNSRegistration{}, &dnsv1alpha1.DNSRecordContribution{}, &dnsv1alpha1.DNSPublicationManifest{}, &dnsv1alpha1.DNSTransportOutbox{}).Build()
			store := pubstore.New(api, pubstore.NewMemoryKV())
			if err := store.Start(ctx); err != nil {
				t.Fatal(err)
			}
			options := ReconcilerOptions{
				ProjectUID: "project-a", ProjectNamespace: "project", PlatformNamespace: "platform",
				Identity: "writer-a", LeaseDuration: 10 * time.Second,
				ServingRegions: []ServingRegion{{Region: "east", Shard: "shared"}}, AddressAllocator: testAllocator{}, Now: func() time.Time { return now },
			}
			request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "project", Name: "project"}}
			reconcile := func(r *Reconciler) {
				t.Helper()
				if _, err := r.Reconcile(ctx, request); err != nil {
					t.Fatal(err)
				}
			}
			active := func() (dnsv1alpha1.DNSPublicationOwnership, dnsv1alpha1.DNSPublicationManifest) {
				t.Helper()
				var owner dnsv1alpha1.DNSPublicationOwnership
				if err := store.Get(ctx, client.ObjectKey{Namespace: "platform", Name: ownershipName(zone.UID)}, &owner); err != nil {
					t.Fatal(err)
				}
				var manifest dnsv1alpha1.DNSPublicationManifest
				if err := store.Get(ctx, client.ObjectKey{Namespace: "platform", Name: owner.Spec.ActiveManifestName}, &manifest); err != nil {
					t.Fatal(err)
				}
				return owner, manifest
			}
			writerA := &Reconciler{Client: store, Scheme: scheme, Options: options}
			for range 8 {
				reconcile(writerA)
			}
			if tc.tombstone {
				if err := api.Delete(ctx, zone); err != nil {
					t.Fatal(err)
				}
				reconcile(writerA)
			}
			before, pending := active()
			if pending.Status.ExportState == "Acknowledged" || pending.Spec.Tombstone != tc.tombstone || len(pending.Spec.ServingTargets) == 0 {
				t.Fatalf("expected an unsent targeted publication: %+v", pending)
			}
			if tc.expire {
				now = now.Add(11 * time.Second)
			} else {
				released, skipped, err := ReleaseOwnership(ctx, store, "platform", "writer-a", now)
				if err != nil || released != 1 || skipped != 0 {
					t.Fatalf("release: released=%d skipped=%d err=%v", released, skipped, err)
				}
			}
			options.Identity = "writer-b"
			writerB := &Reconciler{Client: store, Scheme: scheme, Options: options}
			for range 3 {
				reconcile(writerB)
			}
			taken, replacement := active()
			if taken.Spec.HolderIdentity != "writer-b" || taken.Spec.WriterEpoch <= before.Spec.WriterEpoch {
				t.Fatalf("takeover did not advance ownership: %+v", taken.Spec)
			}
			if replacement.Name == pending.Name || replacement.Spec.WriterEpoch != taken.Spec.WriterEpoch || replacement.Spec.PreviousRevision != 0 {
				t.Fatalf("takeover left publication on the old epoch: owner=%+v manifest=%+v", taken.Spec, replacement.Spec)
			}
			if replacement.Spec.ContentHash != pending.Spec.ContentHash || taken.Spec.LastContentHash != before.Spec.LastContentHash || replacement.Spec.Tombstone != tc.tombstone {
				t.Fatal("takeover changed the publication content")
			}
			reconcile(writerB)
			_, unchanged := active()
			if unchanged.Name != replacement.Name {
				t.Fatal("unchanged content republished within the same epoch")
			}
			publisher := &recordingPublisher{}
			outbox := &platform.Outbox{Client: store, Namespace: "platform", Region: "east", Publisher: publisher, PendingOnly: true}
			if err := outbox.Step(ctx); err != nil {
				t.Fatal(err)
			}
			_, exported := active()
			if exported.Status.ExportState != "Acknowledged" || len(publisher.subjects) == 0 {
				t.Fatalf("takeover publication was not exported: state=%q subjects=%v", exported.Status.ExportState, publisher.subjects)
			}
		})
	}
}

func TestReleasedLeasesAreTakenOverAtOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 1, 45, 0, 0, time.UTC)
	s := runtime.NewScheme()
	if err := addReferenceStateToScheme(s); err != nil {
		t.Fatal(err)
	}
	owner := func(zone types.UID, holder string, until time.Time) *dnsv1alpha1.DNSPublicationOwnership {
		return &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: ownershipName(zone), Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ZoneUID: zone, HolderIdentity: holder, WriterEpoch: 7, NextRevision: 3, LeaseUntil: metav1.NewTime(until)}}
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(
		owner("mine", "old", now.Add(2*time.Minute)),
		owner("theirs", "other", now.Add(2*time.Minute)),
		owner("expired", "old", now.Add(-time.Minute)),
	).Build()

	released, skipped, err := ReleaseOwnership(ctx, c, "platform", "old", now)
	if err != nil || released != 1 || skipped != 0 {
		t.Fatalf("released %d leases, err %v; want only the live lease this process holds", released, err)
	}
	get := func(zone types.UID) dnsv1alpha1.DNSPublicationOwnership {
		var o dnsv1alpha1.DNSPublicationOwnership
		if err := c.Get(ctx, client.ObjectKey{Namespace: "platform", Name: ownershipName(zone)}, &o); err != nil {
			t.Fatal(err)
		}
		return o
	}
	if o := get("theirs"); !o.Spec.LeaseUntil.Equal(&metav1.Time{Time: now.Add(2 * time.Minute)}) {
		t.Fatal("released a lease another holder owns")
	}

	next := &Reconciler{Client: c, Options: ReconcilerOptions{PlatformNamespace: "platform", Identity: "new", LeaseDuration: time.Minute}}
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{UID: "mine"}}
	own, retryAt, err := next.acquireOwnership(ctx, zone, now)
	if err != nil || own == nil {
		t.Fatalf("new process could not take over a released lease: own=%v retryAt=%v err=%v", own, retryAt, err)
	}
	if own.Spec.HolderIdentity != "new" || own.Spec.WriterEpoch <= 7 {
		t.Fatalf("takeover kept the old holder or epoch: %+v", own.Spec)
	}
}

func TestStoppingReconcilerTakesNoLease(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 1, 45, 0, 0, time.UTC)
	s := runtime.NewScheme()
	if err := addReferenceStateToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	r := &Reconciler{Client: c, Options: ReconcilerOptions{PlatformNamespace: "platform", Identity: "old", LeaseDuration: time.Minute, Stopping: func() bool { return true }}}
	own, _, err := r.acquireOwnership(ctx, &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{UID: "zone"}}, now)
	if err != nil || own != nil {
		t.Fatalf("a stopping reconciler took a lease: %v %v", own, err)
	}
	var owners dnsv1alpha1.DNSPublicationOwnershipList
	if err := c.List(ctx, &owners); err != nil || len(owners.Items) != 0 {
		t.Fatalf("a stopping reconciler created %d ownership records: %v", len(owners.Items), err)
	}
}
