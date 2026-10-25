// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/platform"
	"go.miloapis.com/dns-operator/internal/internaldns/pubstore"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type recordingPublisher struct{ subjects []string }

func (p *recordingPublisher) Publish(_ context.Context, subject string, _ model.Envelope) (transport.PublishAck, error) {
	p.subjects = append(p.subjects, subject)
	return transport.PublishAck{Stream: "test", Sequence: uint64(len(p.subjects))}, nil
}

func TestPublicationStoreKeepsPublicationRecordsOffTheAPIServer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := runtime.NewScheme()
	if err := addReferenceStateToScheme(s); err != nil {
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
	assoc.Spec.ResolverContextRef = dnsv1alpha1.DNSNameReference{Name: "prod"}
	assoc.Status = dnsv1alpha1.DNSZoneAssociationStatus{}
	reg := input.Registrations[0].DeepCopy()
	reg.Namespace = "project"
	contribution := input.Contributions[0].DeepCopy()
	contribution.Namespace = "project"
	contribution.Generation = 1
	api := fake.NewClientBuilder().WithScheme(s).WithObjects(network, zone, assoc, reg, contribution).
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
	writerA := &Reconciler{Client: store, Scheme: s, Options: options}
	for i := 0; i < 8; i++ {
		if _, err := writerA.Reconcile(ctx, request); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	ownerKey := client.ObjectKey{Namespace: "platform", Name: "owner-" + model.OpaqueToken(string(zone.UID))[:20]}
	var owner dnsv1alpha1.DNSPublicationOwnership
	if err := store.Get(ctx, ownerKey, &owner); err != nil {
		t.Fatal(err)
	}
	if owner.Spec.ActiveManifestName == "" || owner.Spec.WriterEpoch != now.Unix() {
		t.Fatalf("store ownership not committed at a wall-clock epoch: %#v", owner.Spec)
	}
	for _, list := range []client.ObjectList{&dnsv1alpha1.DNSPublicationOwnershipList{}, &dnsv1alpha1.DNSPublicationManifestList{}, &dnsv1alpha1.DNSPublicationChunkList{}, &dnsv1alpha1.DNSTransportOutboxList{}} {
		if err := api.List(ctx, list); err != nil {
			t.Fatal(err)
		}
		if items := metaItems(list); items != 0 {
			t.Fatalf("%T reached the API server: %d items", list, items)
		}
	}

	publisher := &recordingPublisher{}
	outbox := &platform.Outbox{Client: store, Namespace: "platform", Region: "east", Publisher: publisher, PendingOnly: true}
	if err := outbox.Step(ctx); err != nil {
		t.Fatal(err)
	}
	var manifest dnsv1alpha1.DNSPublicationManifest
	if err := store.Get(ctx, client.ObjectKey{Namespace: "platform", Name: owner.Spec.ActiveManifestName}, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Status.ExportState != "Acknowledged" || len(publisher.subjects) < 2 {
		t.Fatalf("active publication was not exported in one outbox run: state %q, sent %v", manifest.Status.ExportState, publisher.subjects)
	}

	now = now.Add(11 * time.Second)
	options.Identity = "writer-b"
	writerB := &Reconciler{Client: store, Scheme: s, Options: options}
	if _, err := writerB.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	var taken dnsv1alpha1.DNSPublicationOwnership
	if err := store.Get(ctx, ownerKey, &taken); err != nil {
		t.Fatal(err)
	}
	if taken.Spec.HolderIdentity != "writer-b" || taken.Spec.WriterEpoch != now.Unix() {
		t.Fatalf("takeover did not move the epoch to the takeover time: before %d, after %#v", owner.Spec.WriterEpoch, taken.Spec)
	}
}

func metaItems(list client.ObjectList) int {
	switch l := list.(type) {
	case *dnsv1alpha1.DNSPublicationOwnershipList:
		return len(l.Items)
	case *dnsv1alpha1.DNSPublicationManifestList:
		return len(l.Items)
	case *dnsv1alpha1.DNSPublicationChunkList:
		return len(l.Items)
	case *dnsv1alpha1.DNSTransportOutboxList:
		return len(l.Items)
	}
	return 0
}
