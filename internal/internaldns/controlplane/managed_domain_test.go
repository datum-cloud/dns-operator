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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestManagedDomainIsSharedWithSeparateContextZones(t *testing.T) {
	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	objects := make([]client.Object, 0, 2)
	for _, name := range []string{"vpc-a", "vpc-b"} {
		objects = append(objects, &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "project", UID: types.UID(name + "-context"), Generation: 1}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "project-uid/" + name, ManagedNamespace: dnsv1alpha1.DNSResolverContextManagedNamespace{Enabled: true}}})
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).WithStatusSubresource(&dnsv1alpha1.DNSResolverContext{}, &dnsv1alpha1.DNSManagedNamespace{}, &dnsv1alpha1.DNSZoneAssociation{}).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		obj.SetUID(types.UID(obj.GetName() + "-uid"))
		obj.SetGeneration(1)
		return cl.Create(ctx, obj, opts...)
	}}).Build()
	r := &Reconciler{Client: c, Scheme: s, Options: ReconcilerOptions{ProjectUID: "project-uid"}}
	r.Options.defaults()
	ctx := context.Background()
	now := time.Now()
	if err := r.reconcileResolverContexts(ctx, "project", now); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileManagedNamespaces(ctx, "project", now); err != nil {
		t.Fatal(err)
	}
	var zones dnsv1alpha1.DNSZoneList
	if err := c.List(ctx, &zones); err != nil {
		t.Fatal(err)
	}
	if len(zones.Items) != 2 || zones.Items[0].UID == zones.Items[1].UID {
		t.Fatalf("expected separate zones: %#v", zones.Items)
	}
	for _, zone := range zones.Items {
		if zone.Spec.DomainName != defaultManagedDomainSuffix {
			t.Fatalf("managed domain = %q", zone.Spec.DomainName)
		}
	}
	var associations dnsv1alpha1.DNSZoneAssociationList
	if err := c.List(ctx, &associations); err != nil {
		t.Fatal(err)
	}
	for i := range associations.Items {
		a := &associations.Items[i]
		a.Status.ResolvedDNSZoneRef = a.Spec.DNSZoneRef
		setCondition(&a.Status.Conditions, "Accepted", metav1.ConditionTrue, "Ready", "", a.Generation, now)
		if err := c.Status().Update(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.reconcileManagedNamespaces(ctx, "project", now); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileResolverContexts(ctx, "project", now); err != nil {
		t.Fatal(err)
	}
	var contexts dnsv1alpha1.DNSResolverContextList
	if err := c.List(ctx, &contexts); err != nil {
		t.Fatal(err)
	}
	for _, resolver := range contexts.Items {
		if resolver.Status.ManagedNamespace.Suffix != defaultManagedDomainSuffix || resolver.Status.ManagedNamespace.DNSZoneRef.UID == "" {
			t.Fatalf("unallocated context: %#v", resolver.Status)
		}
	}
	if contexts.Items[0].Status.ManagedNamespace.DNSZoneRef.UID == contexts.Items[1].Status.ManagedNamespace.DNSZoneRef.UID {
		t.Fatal("contexts share a zone lifetime")
	}
	zone := &zones.Items[0]
	zone.Spec.DomainName = "different.internal"
	if err := c.Update(ctx, zone); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileManagedNamespaces(ctx, "project", now); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileResolverContexts(ctx, "project", now); err != nil {
		t.Fatal(err)
	}
	if err := c.List(ctx, &contexts); err != nil {
		t.Fatal(err)
	}
	for _, resolver := range contexts.Items {
		if resolver.Status.ManagedNamespace.DNSZoneRef.UID == zone.UID {
			t.Fatal("a mismatched zone domain remained allocated")
		}
	}
}
