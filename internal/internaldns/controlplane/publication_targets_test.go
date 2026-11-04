// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	eastS1 = ServingRegion{Region: "east", Shard: "s1"}
	westS2 = ServingRegion{Region: "west", Shard: "s2"}
)

func targetBinding(name string, contextUID types.UID, region, shard string) *dnsv1alpha1.DNSResolverBinding {
	return &dnsv1alpha1.DNSResolverBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "internal-dns-system", Labels: map[string]string{referenceProjectLabel: "project-a"}},
		Spec: dnsv1alpha1.DNSResolverBindingSpec{
			Source:    dnsv1alpha1.DNSResolverBindingSource{ProjectUID: "project-a", ResolverContextRef: dnsv1alpha1.DNSObjectReference{UID: contextUID}},
			Placement: dnsv1alpha1.DNSResolverBindingPlacement{Region: region, Shard: shard},
		},
	}
}

func targetReconciler(t *testing.T, objects ...client.Object) *Reconciler {
	t.Helper()
	s := runtime.NewScheme()
	if err := addReferenceStateToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).WithStatusSubresource(&dnsv1alpha1.DNSTransportOutbox{}).Build()
	return &Reconciler{Client: c, Scheme: s, Options: ReconcilerOptions{ProjectUID: "project-a", PlatformNamespace: "internal-dns-system",
		ServingRegions: []ServingRegion{eastS1, westS2}}}
}

func activeManifest(name string, serving, withdrawn []ServingRegion) (*dnsv1alpha1.DNSPublicationOwnership, *dnsv1alpha1.DNSPublicationManifest) {
	m := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "internal-dns-system", Labels: map[string]string{withdrawalsTrackedLabel: "true"}}}
	for _, target := range serving {
		m.Spec.ServingTargets = append(m.Spec.ServingTargets, dnsv1alpha1.DNSPublicationTarget{Region: target.Region, Shard: target.Shard})
	}
	for _, target := range withdrawn {
		m.Spec.WithdrawnTargets = append(m.Spec.WithdrawnTargets, dnsv1alpha1.DNSPublicationTarget{Region: target.Region, Shard: target.Shard})
	}
	return &dnsv1alpha1.DNSPublicationOwnership{Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ActiveManifestName: name}}, m
}

func sameTargets(got, want []ServingRegion) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestZonePublishesToRegionsAnyContextIsBoundIn(t *testing.T) {
	ctx := context.Background()
	r := targetReconciler(t, targetBinding("vpc-a-east", "vpc-a", "east", "s1"))
	own := &dnsv1alpha1.DNSPublicationOwnership{}

	targets, withdrawn, err := r.publicationTargets(ctx, own, []types.UID{"vpc-a", "vpc-b"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !sameTargets(targets, []ServingRegion{eastS1}) || len(withdrawn) != 0 {
		t.Fatalf("a zone whose only bound context is in east must publish only to east, got %#v withdrawn %#v", targets, withdrawn)
	}

	// A zone with associations whose contexts are bound nowhere yet waits in
	// the regions those contexts are assigned, so its status stays pending
	// instead of reporting a publication nothing serves.
	targets, _, err = r.publicationTargets(ctx, own, []types.UID{"vpc-b"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("a zone bound nowhere keeps every assigned shard, got %#v", targets)
	}
}

func TestZoneWithdrawsFromRegionItLeaves(t *testing.T) {
	ctx := context.Background()
	own, previous := activeManifest("pub-r1", []ServingRegion{eastS1, westS2}, nil)
	r := targetReconciler(t, targetBinding("vpc-a-east", "vpc-a", "east", "s1"), previous)

	targets, withdrawn, err := r.publicationTargets(ctx, own, []types.UID{"vpc-a"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !sameTargets(targets, []ServingRegion{eastS1}) || !sameTargets(withdrawn, []ServingRegion{westS2}) {
		t.Fatalf("a region the zone left must be withdrawn, got serving %#v withdrawn %#v", targets, withdrawn)
	}
}

func TestZoneWithNoAssociationsWithdrawsEverywhere(t *testing.T) {
	ctx := context.Background()
	own, previous := activeManifest("pub-r1", []ServingRegion{eastS1, westS2}, nil)
	r := targetReconciler(t, targetBinding("vpc-a-east", "vpc-a", "east", "s1"), previous)

	targets, withdrawn, err := r.publicationTargets(ctx, own, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 0 || !sameTargets(withdrawn, []ServingRegion{eastS1, westS2}) {
		t.Fatalf("a zone with no associations must be sent nowhere and withdrawn everywhere, got serving %#v withdrawn %#v", targets, withdrawn)
	}
}

func TestWithdrawalRepeatsUntilDelivered(t *testing.T) {
	ctx := context.Background()
	own, previous := activeManifest("pub-r2", []ServingRegion{eastS1}, []ServingRegion{westS2})
	pending := &dnsv1alpha1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: model.PublicationActivationName("pub-r2", "west", "s2"), Namespace: "internal-dns-system"}}
	r := targetReconciler(t, targetBinding("vpc-a-east", "vpc-a", "east", "s1"), previous, pending)

	_, withdrawn, err := r.publicationTargets(ctx, own, []types.UID{"vpc-a"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !sameTargets(withdrawn, []ServingRegion{westS2}) {
		t.Fatalf("an undelivered withdrawal must be carried into the next revision, got %#v", withdrawn)
	}

	pending.Status.State = outboxAcknowledged
	if err := r.Status().Update(ctx, pending); err != nil {
		t.Fatal(err)
	}
	_, withdrawn, err = r.publicationTargets(ctx, own, []types.UID{"vpc-a"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(withdrawn) != 0 {
		t.Fatalf("a delivered withdrawal must not be sent again, got %#v", withdrawn)
	}
}

func TestRegionThatReturnsIsServedNotWithdrawn(t *testing.T) {
	ctx := context.Background()
	own, previous := activeManifest("pub-r2", []ServingRegion{eastS1}, []ServingRegion{westS2})
	r := targetReconciler(t, targetBinding("vpc-a-east", "vpc-a", "east", "s1"), targetBinding("vpc-a-west", "vpc-a", "west", "s2"), previous)

	targets, withdrawn, err := r.publicationTargets(ctx, own, []types.UID{"vpc-a"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !sameTargets(targets, []ServingRegion{eastS1, westS2}) || len(withdrawn) != 0 {
		t.Fatalf("a region bound again must be served, got serving %#v withdrawn %#v", targets, withdrawn)
	}
}

func TestDeleteAfterLeavingReachesEveryEarlierRegion(t *testing.T) {
	ctx := context.Background()
	own, previous := activeManifest("pub-r2", []ServingRegion{eastS1}, []ServingRegion{westS2})
	r := targetReconciler(t, previous)

	targets, withdrawn, err := r.publicationTargets(ctx, own, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !sameTargets(targets, []ServingRegion{eastS1}) || !sameTargets(withdrawn, []ServingRegion{westS2}) {
		t.Fatalf("a deletion must reach serving and undelivered withdrawn regions, got serving %#v withdrawn %#v", targets, withdrawn)
	}
}

func TestZoneThatLeftARegionBeforeTrackingIsWithdrawnOnce(t *testing.T) {
	ctx := context.Background()
	own, previous := activeManifest("pub-r1", []ServingRegion{eastS1}, nil)
	previous.Labels = nil
	r := targetReconciler(t, targetBinding("vpc-a-east", "vpc-a", "east", "s1"), previous)

	_, withdrawn, err := r.publicationTargets(ctx, own, []types.UID{"vpc-a"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !sameTargets(withdrawn, []ServingRegion{westS2}) {
		t.Fatalf("an untracked manifest must withdraw every other configured shard, got %#v", withdrawn)
	}
	_, withdrawn, err = r.publicationTargets(ctx, own, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !sameTargets(withdrawn, []ServingRegion{westS2}) {
		t.Fatalf("deleting a zone with an untracked manifest must reach every other configured shard, got %#v", withdrawn)
	}
}

func TestRemovedShardIsNotWithdrawnForever(t *testing.T) {
	ctx := context.Background()
	gone := ServingRegion{Region: "retired", Shard: "s9"}
	own, previous := activeManifest("pub-r2", []ServingRegion{eastS1, gone}, []ServingRegion{gone})
	r := targetReconciler(t, targetBinding("vpc-a-east", "vpc-a", "east", "s1"), previous)

	_, withdrawn, err := r.publicationTargets(ctx, own, []types.UID{"vpc-a"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(withdrawn) != 0 {
		t.Fatalf("a shard no longer configured has no exporter and must not be withdrawn, got %#v", withdrawn)
	}
}
