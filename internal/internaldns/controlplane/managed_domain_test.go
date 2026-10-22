// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
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
	domains := map[string]string{"vpc-a": "datum.internal", "vpc-b": "datum.internal", "custom": "private.example.net"}
	objects := make([]client.Object, 0, len(domains))
	for name, domain := range domains {
		objects = append(objects, &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "project", UID: types.UID(name + "-context"), Generation: 1}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "project-uid/" + name, ManagedNamespace: dnsv1alpha1.DNSResolverContextManagedNamespace{Enabled: true, DomainSuffix: domain}}})
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).WithStatusSubresource(&dnsv1alpha1.DNSResolverContext{}, &dnsv1alpha1.DNSZoneAssociation{}).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
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

	var zones dnsv1alpha1.DNSZoneList
	if err := c.List(ctx, &zones); err != nil {
		t.Fatal(err)
	}
	if len(zones.Items) != len(domains) || zones.Items[0].UID == zones.Items[1].UID {
		t.Fatalf("expected separate zones: %#v", zones.Items)
	}
	var associations dnsv1alpha1.DNSZoneAssociationList
	if err := c.List(ctx, &associations); err != nil {
		t.Fatal(err)
	}
	for i := range associations.Items {
		a := &associations.Items[i]
		a.Status.ResolvedDNSZoneRef = a.Spec.DNSZoneRef
		a.Status.ResolvedResolverContextRef = a.Spec.ResolverContextRef
		setCondition(&a.Status.Conditions, "Accepted", metav1.ConditionTrue, "Ready", "", a.Generation, now)
		if err := c.Status().Update(ctx, a); err != nil {
			t.Fatal(err)
		}
	}

	if err := r.reconcileResolverContexts(ctx, "project", now); err != nil {
		t.Fatal(err)
	}
	var contexts dnsv1alpha1.DNSResolverContextList
	if err := c.List(ctx, &contexts); err != nil {
		t.Fatal(err)
	}
	for _, resolver := range contexts.Items {
		if resolver.Status.ManagedNamespace.Suffix != domains[resolver.Name] || resolver.Status.ManagedNamespace.DNSZoneRef.UID == "" {
			t.Fatalf("unallocated context: %#v", resolver.Status)
		}
		var zone dnsv1alpha1.DNSZone
		if err := c.Get(ctx, client.ObjectKey{Namespace: "project", Name: resolver.Status.ManagedNamespace.DNSZoneRef.Name}, &zone); err != nil {
			t.Fatal(err)
		}
		if zone.Spec.DomainName != domains[resolver.Name] {
			t.Fatalf("requested %q, allocated %q", domains[resolver.Name], zone.Spec.DomainName)
		}
		if !metav1.IsControlledBy(&zone, &resolver) {
			t.Fatal("managed zone must share the resolver context lifecycle")
		}
		var association dnsv1alpha1.DNSZoneAssociation
		if err := c.Get(ctx, client.ObjectKey{Namespace: "project", Name: zone.Name + "-context"}, &association); err != nil {
			t.Fatal(err)
		}
		if !metav1.IsControlledBy(&association, &resolver) {
			t.Fatal("managed association must share the resolver context lifecycle")
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

func TestManagedNamespaceRequiresRequestedDomain(t *testing.T) {
	for _, domain := range []string{"", "bad..internal", "*.internal"} {
		t.Run(domain, func(t *testing.T) {
			s := runtime.NewScheme()
			if err := dnsv1alpha1.AddToScheme(s); err != nil {
				t.Fatal(err)
			}
			resolver := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: "project", UID: "context-uid", Generation: 1}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "opaque-consumer", ManagedNamespace: dnsv1alpha1.DNSResolverContextManagedNamespace{Enabled: true, DomainSuffix: domain}}}
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(resolver).WithStatusSubresource(resolver).Build()
			r := &Reconciler{Client: c, Scheme: s}
			if err := r.reconcileResolverContexts(context.Background(), "project", time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(resolver), resolver); err != nil {
				t.Fatal(err)
			}
			if len(resolver.Status.Conditions) != 1 || resolver.Status.Conditions[0].Reason != "InvalidManagedDomain" {
				t.Fatalf("invalid domain was accepted: %#v", resolver.Status)
			}
			var namespaces dnsv1alpha1.DNSZoneList
			if err := c.List(context.Background(), &namespaces); err != nil || len(namespaces.Items) != 0 {
				t.Fatalf("invalid domain allocated a namespace: %#v, %v", namespaces.Items, err)
			}
		})
	}
}

func TestManagedNamespaceStatusSurvivesPendingAssociation(t *testing.T) {
	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	resolver := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "vpc", Namespace: "project", UID: "vpc-context", Generation: 1}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "project-uid/vpc", ManagedNamespace: dnsv1alpha1.DNSResolverContextManagedNamespace{Enabled: true, DomainSuffix: "datum.internal"}}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(resolver).WithStatusSubresource(&dnsv1alpha1.DNSResolverContext{}, &dnsv1alpha1.DNSZoneAssociation{}).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		obj.SetUID(types.UID(obj.GetName() + "-uid"))
		obj.SetGeneration(1)
		return cl.Create(ctx, obj, opts...)
	}}).Build()
	r := &Reconciler{Client: c, Scheme: s, Options: ReconcilerOptions{ProjectUID: "project-uid"}}
	r.Options.defaults()
	ctx := context.Background()
	now := time.Now()
	accept := func(status metav1.ConditionStatus) {
		t.Helper()
		var associations dnsv1alpha1.DNSZoneAssociationList
		if err := c.List(ctx, &associations); err != nil || len(associations.Items) != 1 {
			t.Fatalf("expected one managed association: %v", err)
		}
		a := &associations.Items[0]
		a.Status.ResolvedDNSZoneRef = a.Spec.DNSZoneRef
		a.Status.ResolvedResolverContextRef = a.Spec.ResolverContextRef
		setCondition(&a.Status.Conditions, "Accepted", status, "Test", "", a.Generation, now)
		if err := c.Status().Update(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	reconcile := func() dnsv1alpha1.DNSResolverContext {
		t.Helper()
		if err := r.reconcileResolverContexts(ctx, "project", now); err != nil {
			t.Fatal(err)
		}
		var got dnsv1alpha1.DNSResolverContext
		if err := c.Get(ctx, client.ObjectKeyFromObject(resolver), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	reconcile()
	accept(metav1.ConditionTrue)
	known := reconcile().Status.ManagedNamespace
	if known.Suffix != "datum.internal" || known.DNSZoneRef.UID == "" {
		t.Fatalf("managed namespace was not reported: %#v", known)
	}
	accept(metav1.ConditionFalse)
	got := reconcile()
	if got.Status.ManagedNamespace != known {
		t.Fatalf("managed namespace flapped while the association was pending: %#v", got.Status.ManagedNamespace)
	}
	if ready := apimeta.FindStatusCondition(got.Status.Conditions, "Ready"); ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("pending association left the context ready: %#v", got.Status.Conditions)
	}
}
