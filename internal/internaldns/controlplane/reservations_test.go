package controlplane

import (
	"context"
	"testing"

	dns "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLivePolicyFencesPreviouslyAcceptedAssociation(t *testing.T) {
	for _, scope := range []string{"instances", "databases"} {
		t.Run(scope, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := addReferenceStateToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			resolver := &dns.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "vpc", Namespace: "project", UID: "vpc"}, Spec: dns.DNSResolverContextSpec{ManagedNamespace: dns.DNSResolverContextManagedNamespace{Enabled: true, DomainSuffix: "datum.internal"}}}
			zone := &dns.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "tenant", Namespace: "project", UID: "tenant"}, Spec: dns.DNSZoneSpec{Visibility: dns.DNSZoneVisibilityPrivate, DomainName: scope + ".datum.internal"}}
			assoc := &dns.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: "tenant", Namespace: "project", UID: "association"}, Spec: dns.DNSZoneAssociationSpec{DNSZoneRef: dns.DNSNameReference{Name: zone.Name}, ResolverContextRef: dns.DNSNameReference{Name: resolver.Name}}, Status: dns.DNSZoneAssociationStatus{Conditions: []metav1.Condition{{Type: dnsValueAccepted, Status: metav1.ConditionTrue}}}}
			root := &dns.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: "policy", Namespace: "project", UID: "policy", Generation: 1}, Spec: dns.DNSRegistrationSpec{DNSZoneRef: dns.DNSNameReference{Name: model.ManagedZoneName(string(resolver.UID))}, Name: scope, ReservedDescendants: []string{scope}}}
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone, resolver, assoc).Build()
			r := Reconciler{Client: cl, APIReader: cl, Scheme: scheme}
			ctx := context.Background()
			before, err := r.zoneAccessSnapshot(ctx, zone)
			if err != nil {
				t.Fatal(err)
			}
			if len(before.reservations) != 0 {
				t.Fatal("unexpected policy")
			}
			if err := cl.Create(ctx, root); err != nil {
				t.Fatal(err)
			}
			safe, err := r.reservationSafeAssociations(ctx, zone, []dns.DNSZoneAssociation{*assoc})
			if err != nil {
				t.Fatal(err)
			}
			if apimeta.IsStatusConditionTrue(safe[0].Status.Conditions, dnsValueAccepted) {
				t.Fatal("zone worker reused association accepted before policy")
			}
			if !apimeta.IsStatusConditionTrue(assoc.Status.Conditions, dnsValueAccepted) {
				t.Fatal("mutated cached association")
			}
			after, err := r.zoneAccessSnapshot(ctx, zone)
			if err != nil {
				t.Fatal(err)
			}
			if len(after.reservations) != 1 {
				t.Fatal("cache access fence omitted new reservation")
			}
			// A different resolver context, even with the same domain suffix, has its
			// own managed namespace and is unaffected by this service declaration.
			other := resolver.DeepCopy()
			other.Name = "other"
			other.UID = "other"
			other.ResourceVersion = ""
			if err := cl.Create(ctx, other); err != nil {
				t.Fatal(err)
			}
			assoc.Spec.ResolverContextRef.Name = other.Name
			safe, err = r.reservationSafeAssociations(ctx, zone, []dns.DNSZoneAssociation{*assoc})
			if err != nil {
				t.Fatal(err)
			}
			if !apimeta.IsStatusConditionTrue(safe[0].Status.Conditions, dnsValueAccepted) {
				t.Fatal("policy crossed resolver context boundary")
			}
		})
	}
}

func TestManagedInputsReadLiveReservationsDespiteEmptyCache(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := dns.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	controller := true
	z := &dns.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: model.ManagedZoneName("ctx"), Namespace: "project", OwnerReferences: []metav1.OwnerReference{{APIVersion: dns.GroupVersion.String(), Kind: "DNSResolverContext", UID: "ctx", Controller: &controller}}}}
	root := &dns.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "project", UID: "service"}, Spec: dns.DNSRegistrationSpec{DNSZoneRef: dns.DNSNameReference{Name: z.Name}, ReservedDescendants: []string{"databases"}}}
	live := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root).Build()
	stale := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&dns.DNSRecordSet{}, inputZoneIndex, func(client.Object) []string { return nil }).
		WithIndex(&dns.DNSRecordContribution{}, inputRegistrationIndex, func(client.Object) []string { return nil }).Build()
	r := Reconciler{Client: live, APIReader: live, InputReader: stale}
	inputs, err := r.readZoneInputs(context.Background(), z, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs.registrations) != 1 || inputs.registrations[0].UID != root.UID {
		t.Fatal("cached inputs lost live service policy")
	}
}

func TestReservationChangeInvalidatesCachedPublication(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := addReferenceStateToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	controller := true
	z := &dns.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: model.ManagedZoneName("ctx"), Namespace: "project", UID: "zone", OwnerReferences: []metav1.OwnerReference{{APIVersion: dns.GroupVersion.String(), Kind: "DNSResolverContext", UID: "ctx", Controller: &controller}}}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(z).Build()
	r := Reconciler{Client: cl, APIReader: cl, Scheme: scheme}
	ctx := context.Background()
	guard, err := r.guardZoneAccess(ctx, z, []dns.DNSZoneAssociation{})
	if err != nil {
		t.Fatal(err)
	}
	if err := guard(CompileInput{}); err != nil {
		t.Fatalf("initial snapshot rejected: %v", err)
	}
	root := &dns.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: "service", Namespace: "project", UID: "service", Generation: 1}, Spec: dns.DNSRegistrationSpec{DNSZoneRef: dns.DNSNameReference{Name: z.Name}, ReservedDescendants: []string{"databases"}}}
	if err := cl.Create(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := guard(CompileInput{}); !apierrors.IsConflict(err) {
		t.Fatalf("new policy failed to fence publication: %v", err)
	}
	guard, err = r.guardZoneAccess(ctx, z, []dns.DNSZoneAssociation{})
	if err != nil {
		t.Fatal(err)
	}
	if err := guard(CompileInput{}); !apierrors.IsConflict(err) {
		t.Fatalf("inputs missing live policy were accepted: %v", err)
	}
}
