// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestAssociationsRefusedPastZoneLimit(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	scheme := runtime.NewScheme()
	if err := addReferenceStateToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	resolver := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: "project", UID: "context", Generation: 1}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer"}}
	setCondition(&resolver.Status.Conditions, "Ready", metav1.ConditionTrue, "Ready", "", resolver.Generation, now)
	objects := []client.Object{resolver}
	var associations []client.Object
	created := now.Add(-time.Hour)
	addZone := func(name string, createdAt time.Time) {
		objects = append(objects, &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "project", UID: types.UID("zone-" + name), Generation: 1}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: name + ".internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}})
		association := &dnsv1alpha1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "project", UID: types.UID("association-" + name), Generation: 1, CreationTimestamp: metav1.NewTime(createdAt)}, Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: name}, ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: resolver.Name}}}
		objects = append(objects, association)
		associations = append(associations, association)
	}
	for i := range maxZonesPerResolverContext {
		addZone(fmt.Sprintf("zone-%03d", i), created.Add(time.Duration(i)*time.Second))
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(associations...).Build()
	r := &Reconciler{Client: cl}
	reconcile := func() map[string]*metav1.Condition {
		t.Helper()
		var zones dnsv1alpha1.DNSZoneList
		var assocs dnsv1alpha1.DNSZoneAssociationList
		if err := cl.List(ctx, &zones, client.InNamespace("project")); err != nil {
			t.Fatal(err)
		}
		if err := cl.List(ctx, &assocs, client.InNamespace("project")); err != nil {
			t.Fatal(err)
		}
		byName := map[string]*dnsv1alpha1.DNSZone{}
		for i := range zones.Items {
			byName[zones.Items[i].Name] = &zones.Items[i]
		}
		if err := r.reconcileAssociations(ctx, assocs.Items, byName, now); err != nil {
			t.Fatal(err)
		}
		if err := cl.List(ctx, &assocs, client.InNamespace("project")); err != nil {
			t.Fatal(err)
		}
		conditions := map[string]*metav1.Condition{}
		for i := range assocs.Items {
			conditions[assocs.Items[i].Name] = apimeta.FindStatusCondition(assocs.Items[i].Status.Conditions, dnsValueAccepted)
		}
		return conditions
	}
	count := func(conditions map[string]*metav1.Condition) (accepted int) {
		for _, c := range conditions {
			if c != nil && c.Status == metav1.ConditionTrue {
				accepted++
			}
		}
		return accepted
	}
	// The first pass records reference pins; the second accepts.
	reconcile()
	if got := count(reconcile()); got != maxZonesPerResolverContext {
		t.Fatalf("accepted %d associations, want %d", got, maxZonesPerResolverContext)
	}

	// A new association that sorts first by name and by age must not
	// displace a zone that is already accepted.
	extra := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "aaa", Namespace: "project", UID: "zone-aaa", Generation: 1}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "aaa.internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	extraAssociation := &dnsv1alpha1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: "aaa", Namespace: "project", UID: "association-aaa", Generation: 1, CreationTimestamp: metav1.NewTime(created.Add(-time.Hour))}, Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: "aaa"}, ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: resolver.Name}}}
	for _, object := range []client.Object{extra, extraAssociation} {
		if err := cl.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()
	conditions := reconcile()
	if got := count(conditions); got != maxZonesPerResolverContext {
		t.Fatalf("accepted %d associations, want %d", got, maxZonesPerResolverContext)
	}
	if c := conditions["aaa"]; c == nil || c.Status != metav1.ConditionFalse || c.Reason != "ZoneLimitExceeded" {
		t.Fatalf("association past the limit was not refused: %v", c)
	}

	// Removing an accepted association makes room for the refused one.
	if err := cl.Delete(ctx, &dnsv1alpha1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: "zone-000", Namespace: "project"}}); err != nil {
		t.Fatal(err)
	}
	conditions = reconcile()
	if c := conditions["aaa"]; c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("refused association was not accepted after room was made: %v", c)
	}
	if got := count(conditions); got != maxZonesPerResolverContext {
		t.Fatalf("accepted %d associations, want %d", got, maxZonesPerResolverContext)
	}

	// If another writer accepts one association too many, the zone accepted
	// most recently gives way, even when it is the oldest by creation time.
	late := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "aab", Namespace: "project", UID: "zone-aab", Generation: 1}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "aab.internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	lateAssociation := &dnsv1alpha1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: "aab", Namespace: "project", UID: "association-aab", Generation: 1, CreationTimestamp: metav1.NewTime(created.Add(-2 * time.Hour))}, Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: "aab"}, ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: resolver.Name}}}
	for _, object := range []client.Object{late, lateAssociation} {
		if err := cl.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()
	if err := cl.Get(ctx, client.ObjectKeyFromObject(lateAssociation), lateAssociation); err != nil {
		t.Fatal(err)
	}
	apimeta.RemoveStatusCondition(&lateAssociation.Status.Conditions, dnsValueAccepted)
	setCondition(&lateAssociation.Status.Conditions, dnsValueAccepted, metav1.ConditionTrue, dnsValueAccepted, "", lateAssociation.Generation, now.Add(time.Hour))
	if err := cl.Status().Update(ctx, lateAssociation); err != nil {
		t.Fatal(err)
	}
	conditions = reconcile()
	if c := conditions["aab"]; c == nil || c.Status != metav1.ConditionFalse || c.Reason != "ZoneLimitExceeded" {
		t.Fatalf("most recently accepted association should give way: %v", c)
	}
	if got := count(conditions); got != maxZonesPerResolverContext {
		t.Fatalf("accepted %d associations, want %d", got, maxZonesPerResolverContext)
	}
}

func TestZoneLimitAlwaysAdmitsManagedZone(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	scheme := runtime.NewScheme()
	if err := addReferenceStateToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	resolver := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: "project", UID: "context", Generation: 1}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer"}}
	setCondition(&resolver.Status.Conditions, "Ready", metav1.ConditionTrue, "Ready", "", resolver.Generation, now)
	objects := make([]client.Object, 0, 1+2*maxZonesPerResolverContext)
	objects = append(objects, resolver)
	associations := make([]client.Object, 0, maxZonesPerResolverContext)
	created := now.Add(-time.Hour)
	for i := range maxZonesPerResolverContext {
		name := fmt.Sprintf("zone-%03d", i)
		objects = append(objects, &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "project", UID: types.UID("zone-" + name), Generation: 1}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: name + ".internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}})
		association := &dnsv1alpha1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "project", UID: types.UID("association-" + name), Generation: 1, CreationTimestamp: metav1.NewTime(created.Add(time.Duration(i) * time.Second))}, Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: name}, ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: resolver.Name}}}
		objects = append(objects, association)
		associations = append(associations, association)
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(associations...).Build()
	r := &Reconciler{Client: cl}
	reconcile := func() map[string]*metav1.Condition {
		t.Helper()
		var zones dnsv1alpha1.DNSZoneList
		var assocs dnsv1alpha1.DNSZoneAssociationList
		if err := cl.List(ctx, &zones, client.InNamespace("project")); err != nil {
			t.Fatal(err)
		}
		if err := cl.List(ctx, &assocs, client.InNamespace("project")); err != nil {
			t.Fatal(err)
		}
		byName := map[string]*dnsv1alpha1.DNSZone{}
		for i := range zones.Items {
			byName[zones.Items[i].Name] = &zones.Items[i]
		}
		if err := r.reconcileAssociations(ctx, assocs.Items, byName, now); err != nil {
			t.Fatal(err)
		}
		if err := cl.List(ctx, &assocs, client.InNamespace("project")); err != nil {
			t.Fatal(err)
		}
		conditions := map[string]*metav1.Condition{}
		for i := range assocs.Items {
			conditions[assocs.Items[i].Name] = apimeta.FindStatusCondition(assocs.Items[i].Status.Conditions, dnsValueAccepted)
		}
		return conditions
	}
	reconcile()
	reconcile()

	// Enabling the managed namespace on a full network adds the context's
	// own association last. It must still be accepted, or the context can
	// never become ready.
	owner := []metav1.OwnerReference{{APIVersion: dnsv1alpha1.GroupVersion.String(), Kind: "DNSResolverContext", Name: resolver.Name, UID: resolver.UID, Controller: ptr.To(true)}}
	managedZone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "managed", Namespace: "project", UID: "zone-managed", Generation: 1, OwnerReferences: owner}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "network.internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	managedAssociation := &dnsv1alpha1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: "managed-context", Namespace: "project", UID: "association-managed", Generation: 1, OwnerReferences: owner, CreationTimestamp: metav1.NewTime(now)}, Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: managedZone.Name}, ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: resolver.Name}}}
	for _, object := range []client.Object{managedZone, managedAssociation} {
		if err := cl.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()
	conditions := reconcile()
	if c := conditions["managed-context"]; c == nil || c.Status != metav1.ConditionTrue {
		t.Fatalf("managed association was not accepted on a full network: %v", c)
	}
	last := fmt.Sprintf("zone-%03d", maxZonesPerResolverContext-1)
	if c := conditions[last]; c == nil || c.Status != metav1.ConditionFalse || c.Reason != "ZoneLimitExceeded" {
		t.Fatalf("newest customer association should make room for the managed zone: %v", c)
	}
}
