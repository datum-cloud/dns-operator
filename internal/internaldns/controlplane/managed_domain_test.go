// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
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
	if err := addReferenceStateToScheme(s); err != nil {
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
	r := &Reconciler{Client: c, Scheme: s, Options: ReconcilerOptions{ProjectUID: "project-uid", ManagedDomainSuffixes: []string{"internal", "example.net"}}}
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
	zoneByName := map[string]*dnsv1alpha1.DNSZone{}
	for i := range zones.Items {
		zoneByName[zones.Items[i].Name] = &zones.Items[i]
	}
	if err := r.reconcileAssociations(ctx, associations.Items, zoneByName, now); err != nil {
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
		if resolver.Status.ManagedNamespace.Suffix != domains[resolver.Name] || resolver.Status.ManagedNamespace.DNSZoneRef.Name == "" {
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
	if contexts.Items[0].Status.ManagedNamespace.DNSZoneRef.Name == contexts.Items[1].Status.ManagedNamespace.DNSZoneRef.Name {
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
		if resolver.Status.ManagedNamespace.DNSZoneRef.Name == zone.Name {
			t.Fatal("a mismatched zone domain remained allocated")
		}
	}
}

func TestManagedNamespaceRequiresRequestedDomain(t *testing.T) {
	for _, domain := range []string{"", "bad..internal", "*.internal"} {
		t.Run(domain, func(t *testing.T) {
			s := runtime.NewScheme()
			if err := addReferenceStateToScheme(s); err != nil {
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
	if err := addReferenceStateToScheme(s); err != nil {
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
		var zone dnsv1alpha1.DNSZone
		if err := c.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: a.Spec.DNSZoneRef.Name}, &zone); err != nil {
			t.Fatal(err)
		}
		var owner dnsv1alpha1.DNSResolverContext
		if err := c.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: a.Spec.ResolverContextRef.Name}, &owner); err != nil {
			t.Fatal(err)
		}
		a.Status.ResolvedDNSZoneRef = a.Spec.DNSZoneRef
		a.Status.ResolvedResolverContextRef = a.Spec.ResolverContextRef
		setCondition(&a.Status.Conditions, "Accepted", status, "Test", "", a.Generation, now)
		if err := c.Status().Update(ctx, a); err != nil {
			t.Fatal(err)
		}
		seedReferenceState(t, r, a, map[string]dnsv1alpha1.DNSObjectReference{
			"zone":    {Name: zone.Name, UID: zone.UID, Generation: zone.Generation},
			"context": {Name: owner.Name, UID: owner.UID},
		})
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
	if known.Suffix != "datum.internal" || known.DNSZoneRef.Name == "" {
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

func TestManagedDomainRequiresPermittedSuffix(t *testing.T) {
	for _, tc := range []struct {
		domain    string
		permitted []string
		ok        bool
	}{
		{domain: "datum.internal", ok: true},
		{domain: "vpc.datum.internal", ok: true},
		{domain: "internal"},
		{domain: "com"},
		{domain: "example.com"},
		{domain: "cluster.local"},
		{domain: "svc.cluster.local"},
		{domain: "notinternal"},
		{domain: "vpc.corp.example", permitted: []string{"corp.example"}, ok: true},
		{domain: "corp.example", permitted: []string{"corp.example"}},
		{domain: "datum.internal", permitted: []string{"corp.example"}},
	} {
		t.Run(tc.domain, func(t *testing.T) {
			s := runtime.NewScheme()
			if err := addReferenceStateToScheme(s); err != nil {
				t.Fatal(err)
			}
			resolver := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: "project", UID: "context-uid", Generation: 1}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "opaque-consumer", ManagedNamespace: dnsv1alpha1.DNSResolverContextManagedNamespace{Enabled: true, DomainSuffix: tc.domain}}}
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(resolver).WithStatusSubresource(resolver).Build()
			r := &Reconciler{Client: c, Scheme: s, Options: ReconcilerOptions{ManagedDomainSuffixes: tc.permitted}}
			if err := r.reconcileResolverContexts(context.Background(), "project", time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(resolver), resolver); err != nil {
				t.Fatal(err)
			}
			rejected := apimeta.FindStatusCondition(resolver.Status.Conditions, "Ready").Reason == "InvalidManagedDomain"
			var zones dnsv1alpha1.DNSZoneList
			if err := c.List(context.Background(), &zones); err != nil {
				t.Fatal(err)
			}
			if rejected == tc.ok || (len(zones.Items) == 0) == tc.ok {
				t.Fatalf("domain %q with permitted %v: rejected=%v zones=%d", tc.domain, tc.permitted, rejected, len(zones.Items))
			}
		})
	}
}

func TestRefusedManagedDomainWithdrawsExistingZone(t *testing.T) {
	s := runtime.NewScheme()
	if err := addReferenceStateToScheme(s); err != nil {
		t.Fatal(err)
	}
	resolver := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: "project", UID: "context-uid", Generation: 1}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "opaque-consumer", ManagedNamespace: dnsv1alpha1.DNSResolverContextManagedNamespace{Enabled: true, DomainSuffix: "corp.example"}}}
	owner := []metav1.OwnerReference{*metav1.NewControllerRef(resolver, dnsv1alpha1.GroupVersion.WithKind("DNSResolverContext"))}
	zoneName := managedZoneName(resolver)
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: zoneName, Namespace: "project", UID: "zone-uid", OwnerReferences: owner}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "corp.example", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	association := &dnsv1alpha1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: zoneName + "-context", Namespace: "project", UID: "association-uid", OwnerReferences: owner}, Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: zoneName}, ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: resolver.Name}}}
	foreign := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "managed-other", Namespace: "project", UID: "other-uid"}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "corp.example"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(resolver, zone, association, foreign).WithStatusSubresource(resolver).Build()
	r := &Reconciler{Client: c, Scheme: s}
	if err := r.reconcileResolverContexts(context.Background(), "project", time.Now()); err != nil {
		t.Fatal(err)
	}
	var zones dnsv1alpha1.DNSZoneList
	var associations dnsv1alpha1.DNSZoneAssociationList
	if err := c.List(context.Background(), &zones); err != nil {
		t.Fatal(err)
	}
	if err := c.List(context.Background(), &associations); err != nil {
		t.Fatal(err)
	}
	if len(associations.Items) != 0 || len(zones.Items) != 1 || zones.Items[0].Name != foreign.Name {
		t.Fatalf("refused domain kept publishing: zones=%v associations=%d", zones.Items, len(associations.Items))
	}
}

func TestAssociationInsideReservedScopesIsNotAccepted(t *testing.T) {
	for _, createManaged := range []bool{false, true} {
		t.Run(fmt.Sprintf("managed-zone-%v", createManaged), func(t *testing.T) {
			for _, scope := range []string{"instances", "databases"} {
				t.Run(scope, func(t *testing.T) {
					s := runtime.NewScheme()
					if err := addReferenceStateToScheme(s); err != nil {
						t.Fatal(err)
					}
					resolver := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "vpc", Namespace: "project", UID: "vpc-context", Generation: 1}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "project-uid/vpc", ManagedNamespace: dnsv1alpha1.DNSResolverContextManagedNamespace{Enabled: true, DomainSuffix: "datum.internal"}}}
					shadow := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "shadow", Namespace: "project", UID: "shadow-uid", Generation: 1}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "web." + scope + ".datum.internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
					shadowAssociation := &dnsv1alpha1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: "shadow", Namespace: "project", UID: "shadow-association", Generation: 1}, Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: shadow.Name}, ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: resolver.Name}}}
					c := fake.NewClientBuilder().WithScheme(s).WithObjects(resolver, shadow, shadowAssociation).WithStatusSubresource(&dnsv1alpha1.DNSResolverContext{}, &dnsv1alpha1.DNSZoneAssociation{}).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						obj.SetUID(types.UID(obj.GetName() + "-uid"))
						obj.SetGeneration(1)
						return cl.Create(ctx, obj, opts...)
					}}).Build()
					r := &Reconciler{Client: c, Scheme: s, Options: ReconcilerOptions{ProjectUID: "project-uid", ReservedScopes: model.ReservedScopes{{Name: scope}}, ManagedDomainSuffixes: []string{"internal"}}}
					r.Options.defaults()
					ctx := context.Background()
					now := time.Now()
					if createManaged {
						if err := r.reconcileResolverContexts(ctx, "project", now); err != nil {
							t.Fatal(err)
						}
					}
					var zones dnsv1alpha1.DNSZoneList
					if err := c.List(ctx, &zones); err != nil {
						t.Fatal(err)
					}
					zoneByName := map[string]*dnsv1alpha1.DNSZone{}
					for i := range zones.Items {
						zoneByName[zones.Items[i].Name] = &zones.Items[i]
					}
					var associations dnsv1alpha1.DNSZoneAssociationList
					if err := c.List(ctx, &associations); err != nil {
						t.Fatal(err)
					}
					if err := r.reconcileAssociations(ctx, associations.Items, zoneByName, now); err != nil {
						t.Fatal(err)
					}
					if err := c.List(ctx, &associations); err != nil {
						t.Fatal(err)
					}
					for _, a := range associations.Items {
						accepted := apimeta.FindStatusCondition(a.Status.Conditions, dnsValueAccepted)
						if accepted == nil {
							t.Fatalf("%s has no Accepted condition", a.Name)
						}
						if a.Name == shadow.Name {
							if accepted.Status != metav1.ConditionFalse || accepted.Reason != "ReservedScope" {
								t.Fatalf("shadowing association accepted: %#v", accepted)
							}
						} else if accepted.Status != metav1.ConditionTrue {
							t.Fatalf("managed association %s not accepted: %#v", a.Name, accepted)
						}
					}

				})
			}

		})
	}
}
