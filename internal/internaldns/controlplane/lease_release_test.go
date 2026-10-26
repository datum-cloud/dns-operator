// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

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
