// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type countingAllocator struct {
	testAllocator
	calls *int
}

func (a countingAllocator) Allocate(context.Context, types.UID, types.UID, string) (string, error) {
	*a.calls++
	return "fd54::20", nil
}

func TestRevivedTombstoneIsAllocatedAgain(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	s := runtime.NewScheme()
	if err := addReferenceStateToScheme(s); err != nil {
		t.Fatal(err)
	}
	contextObject := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "ctx", Namespace: "project", UID: "context-uid", Generation: 1}}
	setCondition(&contextObject.Status.Conditions, "Ready", metav1.ConditionTrue, "Ready", "", 1, now)
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "zone", Namespace: "project", UID: "zone-uid", Generation: 1}, Spec: dnsv1alpha1.DNSZoneSpec{Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	assoc := &dnsv1alpha1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: "assoc", Namespace: "project", UID: "assoc-uid", Generation: 1}, Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: "zone"}, ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: "ctx"}}, Status: dnsv1alpha1.DNSZoneAssociationStatus{ResolvedDNSZoneRef: dnsv1alpha1.DNSNameReference{Name: "zone"}, ResolvedResolverContextRef: dnsv1alpha1.DNSNameReference{Name: "ctx"}}}
	setCondition(&assoc.Status.Conditions, "Accepted", metav1.ConditionTrue, "Accepted", "", 1, now)
	access := &dnsv1alpha1.DNSResolverAccessBinding{ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: "project", UID: "access-uid", Generation: 1}, Spec: dnsv1alpha1.DNSResolverAccessBindingSpec{ContextRef: dnsv1alpha1.DNSObjectReference{Name: "ctx", UID: "context-uid"}, Region: "central", DestinationAddress: "fd70:100::10"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(contextObject, zone, assoc, access).WithStatusSubresource(&dnsv1alpha1.DNSResolverContext{}, &dnsv1alpha1.DNSResolverAccessBinding{}, &dnsv1alpha1.DNSResolverBinding{}).Build()
	calls := 0
	r := &Reconciler{Client: c, Scheme: s, Options: ReconcilerOptions{ProjectUID: "project-uid", PlatformNamespace: "platform", Region: "central", Shard: "shared", AddressAllocator: countingAllocator{calls: &calls}}}
	seedReferenceState(t, r, assoc, map[string]dnsv1alpha1.DNSObjectReference{"zone": {Name: zone.Name, UID: zone.UID, Generation: zone.Generation}, "context": {Name: contextObject.Name, UID: contextObject.UID}})
	reconcile := func() dnsv1alpha1.DNSResolverBinding {
		t.Helper()
		if err := r.reconcileContextBindings(ctx, "project", []dnsv1alpha1.DNSZoneAssociation{*assoc}, map[string]*dnsv1alpha1.DNSZone{"zone": zone}, now); err != nil {
			t.Fatal(err)
		}
		var bindings dnsv1alpha1.DNSResolverBindingList
		if err := c.List(ctx, &bindings, client.InNamespace("platform")); err != nil || len(bindings.Items) != 1 {
			t.Fatalf("expected one binding: %v", err)
		}
		return bindings.Items[0]
	}
	b := reconcile()
	if calls != 1 {
		t.Fatalf("allocated %d times for a new binding", calls)
	}
	reconcile()
	if calls != 1 {
		t.Fatal("a live binding was allocated again")
	}
	b.Spec.Tombstone = true
	b.Spec.Configuration.Query.RegionalAddress = "fd54::10"
	if err := c.Update(ctx, &b); err != nil {
		t.Fatal(err)
	}
	if revived := reconcile(); calls != 2 || revived.Spec.Tombstone || revived.Spec.Configuration.Query.RegionalAddress != "fd54::20" {
		t.Fatalf("revived tombstone kept an address that may have been freed: calls %d, %#v", calls, revived.Spec)
	}
}
