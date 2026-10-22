// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type testAllocator struct{}

// accessListSnapshotClient models a source List taken before another
// controller commits a renewed platform binding. Gets remain authoritative.
type accessListSnapshotClient struct {
	client.Client
	accesses    []dnsv1alpha1.DNSResolverAccessBinding
	getErrorFor string
}

func (c *accessListSnapshotClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	_, access := obj.(*dnsv1alpha1.DNSResolverAccessBinding)
	_, resolverContext := obj.(*dnsv1alpha1.DNSResolverContext)
	if (access && c.getErrorFor == "access") || (resolverContext && c.getErrorFor == "context") {
		return fmt.Errorf("injected %s read uncertainty", c.getErrorFor)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *accessListSnapshotClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if accesses, ok := list.(*dnsv1alpha1.DNSResolverAccessBindingList); ok {
		accesses.Items = append([]dnsv1alpha1.DNSResolverAccessBinding(nil), c.accesses...)
		return nil
	}
	return c.Client.List(ctx, list, opts...)
}

func (testAllocator) Allocate(context.Context, types.UID, types.UID) (string, error) {
	return "fd54::10", nil
}

func (testAllocator) ClaimDestination(context.Context, types.UID, types.UID, string, string, int32) error {
	return nil
}

func (testAllocator) ReleaseDestinations(context.Context, types.UID, map[string]bool) error {
	return nil
}

func TestContextAccessPersistsUntilExplicitWithdrawal(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	contextObject := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "ctx", Namespace: "project", UID: "context-uid", Generation: 1}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer-uid"}, Status: dnsv1alpha1.DNSResolverContextStatus{}}
	setCondition(&contextObject.Status.Conditions, "Ready", metav1.ConditionTrue, "Ready", "", 1, now)
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "zone", Namespace: "project", UID: "zone-uid", Generation: 1}, Spec: dnsv1alpha1.DNSZoneSpec{Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	association := &dnsv1alpha1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: "assoc", Namespace: "project", UID: "assoc-uid", Generation: 1}, Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: "zone", UID: "zone-uid"}, ResolverContextRef: dnsv1alpha1.DNSObjectReference{Name: "ctx", UID: "context-uid"}}, Status: dnsv1alpha1.DNSZoneAssociationStatus{ResolvedDNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: "zone", UID: "zone-uid", Generation: 1}, ResolvedResolverContextRef: dnsv1alpha1.DNSObjectReference{Name: "ctx", UID: "context-uid"}}}
	setCondition(&association.Status.Conditions, "Accepted", metav1.ConditionTrue, "Accepted", "", 1, now)
	access := &dnsv1alpha1.DNSResolverAccessBinding{ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: "project", UID: "access-uid", Generation: 1}, Spec: dnsv1alpha1.DNSResolverAccessBindingSpec{ContextRef: dnsv1alpha1.DNSObjectReference{Name: "ctx", UID: "context-uid"}, Region: "central", DestinationAddress: "fd70:100::10"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(contextObject, zone, association, access).WithStatusSubresource(&dnsv1alpha1.DNSResolverContext{}, &dnsv1alpha1.DNSResolverAccessBinding{}, &dnsv1alpha1.DNSResolverAccessBinding{}, &dnsv1alpha1.DNSResolverBinding{}).Build()
	r := &Reconciler{Client: c, Scheme: s, Options: ReconcilerOptions{ProjectUID: "project-uid", PlatformNamespace: "platform", Region: "central", Shard: "shared", AddressAllocator: testAllocator{}}}
	if err := r.reconcileContextBindings(ctx, "project", []dnsv1alpha1.DNSZoneAssociation{*association}, map[string]*dnsv1alpha1.DNSZone{"zone": zone}, now); err != nil {
		t.Fatal(err)
	}
	var bindings dnsv1alpha1.DNSResolverBindingList
	if err := c.List(ctx, &bindings, client.InNamespace("platform")); err != nil {
		t.Fatal(err)
	}
	if len(bindings.Items) != 1 {
		t.Fatalf("got %d bindings", len(bindings.Items))
	}
	b := bindings.Items[0]
	if b.Spec.Configuration.Query.NodeAddress != "fd70:100::10" || b.Spec.Source.ResolverContextRef.UID != "context-uid" || b.Spec.Version.Epoch != 1 || b.Spec.Version.Sequence != 1 {
		t.Fatalf("access projection drifted: %#v", b.Spec)
	}
	if b.Spec.Source.ProjectUID != "project-uid" || b.Spec.Source.ResolverContextRef.Name != "ctx" || b.Spec.Source.AccessBindingRef.Name != "access" || b.Spec.Source.AccessBindingRef.UID != "access-uid" || b.Spec.Configuration.ZoneRefs[0].Name != "zone" || b.Spec.Configuration.ZoneRefs[0].UID != "zone-uid" || b.Spec.Placement.Region != "central" {
		t.Fatalf("source identity or zone references drifted: %#v", b.Spec)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(access), access); err != nil {
		t.Fatal(err)
	}
	if access.Status.ObservedGeneration != 1 {
		t.Fatalf("source generation not observed: %#v", access.Status)
	}
	extraZone := zone.DeepCopy()
	extraZone.Name = "extra"
	extraZone.UID = "extra-uid"
	extraAssociation := association.DeepCopy()
	extraAssociation.Name = "extra-assoc"
	extraAssociation.Spec.DNSZoneRef = dnsv1alpha1.DNSObjectReference{Name: extraZone.Name, UID: extraZone.UID}
	extraAssociation.Status.ResolvedDNSZoneRef = extraAssociation.Spec.DNSZoneRef
	if err := r.reconcileContextBindings(ctx, "project", []dnsv1alpha1.DNSZoneAssociation{*association, *extraAssociation}, map[string]*dnsv1alpha1.DNSZone{"zone": zone, "extra": extraZone}, now); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(&b), &b); err != nil {
		t.Fatal(err)
	}
	if b.Spec.Configuration.Generation != 2 || b.Spec.Configuration.Revision != 2 {
		t.Fatalf("zone membership change reused configuration fences: %#v", b.Spec.Configuration)
	}

	setCondition(&contextObject.Status.Conditions, "Ready", metav1.ConditionFalse, "InvalidIdentity", "", contextObject.Generation, now)
	if err := c.Status().Update(ctx, contextObject); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileContextBindings(ctx, "project", []dnsv1alpha1.DNSZoneAssociation{*association}, map[string]*dnsv1alpha1.DNSZone{"zone": zone}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(&b), &b); err != nil {
		t.Fatal(err)
	}
	if !b.Spec.Tombstone || b.Spec.Version.Sequence != 2 || b.Spec.Configuration.Revision != 3 {
		t.Fatalf("withdrawal failed to advance private fencing: %#v", b.Spec)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(access), access); err != nil {
		t.Fatal(err)
	}
	ready := apimeta.FindStatusCondition(access.Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("invalid access retained Ready: %#v", access.Status.Conditions)
	}
	staleAccess := *access.DeepCopy()
	if err := c.Update(ctx, access); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileContextBindings(ctx, "project", []dnsv1alpha1.DNSZoneAssociation{*association}, map[string]*dnsv1alpha1.DNSZone{"zone": zone}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(&b), &b); err != nil {
		t.Fatal(err)
	}
	if !b.Spec.Tombstone {
		t.Fatal("same-fence replay reopened access")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(access), access); err != nil {
		t.Fatal(err)
	}
	setCondition(&contextObject.Status.Conditions, "Ready", metav1.ConditionTrue, "Ready", "", contextObject.Generation, now)
	if err := c.Status().Update(ctx, contextObject); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, access); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileContextBindings(ctx, "project", []dnsv1alpha1.DNSZoneAssociation{*association}, map[string]*dnsv1alpha1.DNSZone{"zone": zone}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(&b), &b); err != nil {
		t.Fatal(err)
	}
	if b.Spec.Tombstone || b.Spec.Version.Sequence != 3 || b.Spec.Configuration.Revision != 4 {
		t.Fatalf("current context did not restore access: %#v", b.Spec)
	}
	for _, snapshot := range [][]dnsv1alpha1.DNSResolverAccessBinding{{staleAccess}, nil} {
		staleController := &Reconciler{Client: &accessListSnapshotClient{Client: c, accesses: snapshot}, Scheme: s, Options: r.Options}
		if err := staleController.reconcileContextBindings(ctx, "project", nil, nil, now.Add(3*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(&b), &b); err != nil {
			t.Fatal(err)
		}
		if b.Spec.Tombstone || b.Spec.Configuration.Revision != 4 || b.Spec.Version.Sequence != 3 {
			t.Fatalf("stale source snapshot withdrew renewed binding: %#v", b.Spec)
		}
	}
	for _, kind := range []string{"access", "context"} {
		uncertainController := &Reconciler{Client: &accessListSnapshotClient{Client: c, getErrorFor: kind}, Scheme: s, Options: r.Options}
		if err := uncertainController.reconcileContextBindings(ctx, "project", nil, nil, now.Add(3*time.Minute)); err == nil {
			t.Fatalf("%s read uncertainty was not surfaced", kind)
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(&b), &b); err != nil {
			t.Fatal(err)
		}
		if b.Spec.Tombstone || b.Spec.Configuration.Revision != 4 {
			t.Fatalf("%s read uncertainty withdrew access", kind)
		}
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(contextObject), contextObject); err != nil {
		t.Fatal(err)
	}
	setCondition(&contextObject.Status.Conditions, "Ready", metav1.ConditionFalse, "Reconciling", "", contextObject.Generation, now.Add(3*time.Minute))
	if err := c.Status().Update(ctx, contextObject); err != nil {
		t.Fatal(err)
	}
	uncertainController := &Reconciler{Client: &accessListSnapshotClient{Client: c}, Scheme: s, Options: r.Options}
	if err := uncertainController.reconcileContextBindings(ctx, "project", nil, nil, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(&b), &b); err != nil {
		t.Fatal(err)
	}
	if b.Spec.Tombstone || b.Spec.Configuration.Revision != 4 {
		t.Fatal("transient context readiness revoked a live authorization")
	}
}

func TestBindingAckCurrentRequiresVerifiedExactAuthorization(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	ackDeadline := metav1.NewTime(now.Add(30 * time.Second))
	b := &dnsv1alpha1.DNSResolverBinding{Spec: dnsv1alpha1.DNSResolverBindingSpec{Configuration: dnsv1alpha1.DNSResolverBindingConfiguration{Revision: 7}, Version: dnsv1alpha1.DNSResolverAccessVersion{Epoch: 3, Sequence: 11}}, Status: dnsv1alpha1.DNSResolverBindingStatus{MemberAcknowledgements: []dnsv1alpha1.DNSApplyAcknowledgement{{Phase: string(model.AckVerified), Revision: 7, AccessEpoch: 3, AccessSequence: 11, ValidUntil: &ackDeadline}}}}
	if !bindingAckCurrent(b, now) {
		t.Fatal("exact verified ACK was rejected")
	}
	b.Status.MemberAcknowledgements[0].Phase = string(model.AckRejected)
	if bindingAckCurrent(b, now) {
		t.Fatal("rejected ACK was accepted")
	}
	b.Status.MemberAcknowledgements[0].Phase = string(model.AckVerified)
	b.Status.MemberAcknowledgements[0].AccessSequence = 10
	if bindingAckCurrent(b, now) {
		t.Fatal("stale authorization ACK was accepted")
	}
	b.Status.MemberAcknowledgements[0].AccessSequence = 11
}

func TestContextNamingPolicyResolvesMultipleZonesWithoutNetworking(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	resolverContext := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "ctx", Namespace: "project", UID: "ctx-uid", Generation: 2}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer-uid"}}
	setCondition(&resolverContext.Status.Conditions, "Ready", metav1.ConditionTrue, "Ready", "", 2, now)
	z1 := dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "managed", Namespace: "project", UID: "zone-1", Generation: 1}, Spec: dnsv1alpha1.DNSZoneSpec{Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	z2 := dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "services", Namespace: "project", UID: "zone-2", Generation: 3}, Spec: dnsv1alpha1.DNSZoneSpec{Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	associations := []dnsv1alpha1.DNSZoneAssociation{{Status: dnsv1alpha1.DNSZoneAssociationStatus{ResolvedDNSZoneRef: dnsv1alpha1.DNSObjectReference{UID: z1.UID}, ResolvedResolverContextRef: dnsv1alpha1.DNSObjectReference{UID: resolverContext.UID}}}, {Status: dnsv1alpha1.DNSZoneAssociationStatus{ResolvedDNSZoneRef: dnsv1alpha1.DNSObjectReference{UID: z2.UID}, ResolvedResolverContextRef: dnsv1alpha1.DNSObjectReference{UID: resolverContext.UID}}}}
	for i := range associations {
		setCondition(&associations[i].Status.Conditions, "Accepted", metav1.ConditionTrue, "Accepted", "", 1, now)
	}
	policy := &dnsv1alpha1.DNSNamingPolicy{ObjectMeta: metav1.ObjectMeta{Name: "names", Namespace: "project", UID: "policy-uid", Generation: 1}, Spec: dnsv1alpha1.DNSNamingPolicySpec{ResolverContextRef: dnsv1alpha1.DNSObjectReference{Name: resolverContext.Name, UID: resolverContext.UID}, AdditionalNames: []dnsv1alpha1.DNSAdditionalNameRule{{Selector: nil, DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: z1.Name, UID: z1.UID, Generation: z1.Generation}, NamePrefix: "instances"}, {Selector: nil, DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: z2.Name, UID: z2.UID, Generation: z2.Generation}, NamePrefix: "services"}}}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(resolverContext, policy).WithStatusSubresource(&dnsv1alpha1.DNSResolverContext{}, &dnsv1alpha1.DNSResolverAccessBinding{}, &dnsv1alpha1.DNSNamingPolicy{}).Build()
	r := &Reconciler{Client: c, Options: ReconcilerOptions{}}
	if err := r.reconcileNamingPolicies(ctx, []dnsv1alpha1.DNSNamingPolicy{*policy}, associations, map[string]*dnsv1alpha1.DNSZone{z1.Name: &z1, z2.Name: &z2}, now); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(policy), policy); err != nil {
		t.Fatal(err)
	}
	accepted := apimeta.FindStatusCondition(policy.Status.Conditions, "Accepted")
	if accepted == nil || accepted.Status != metav1.ConditionTrue || len(policy.Status.ResolvedAdditionalNames) != 2 {
		t.Fatalf("context naming policy not resolved: %#v", policy.Status)
	}
	if policy.Status.ResolvedResolverContextRef.UID != resolverContext.UID {
		t.Fatalf("identity projection incorrect: %#v", policy.Status)
	}
}

func TestStableOneShardPerRegionAssignment(t *testing.T) {
	r := &Reconciler{Options: ReconcilerOptions{ProjectUID: "project", ServingRegions: []ServingRegion{{Region: "central", Shard: "a"}, {Region: "central", Shard: "b"}, {Region: "west", Shard: "w"}}}}
	first := r.selectServingTargets("vpc-1", nil)
	if len(first) != 2 {
		t.Fatalf("got %d targets, want one per region", len(first))
	}
	central := first[0]
	if central.Region != "central" {
		t.Fatalf("targets not sorted: %#v", first)
	}
	// Find another immutable VPC identity that rendezvous assigns to the other
	// shard, proving the pool is shared rather than duplicated per VPC.
	different := false
	for i := 2; i < 200; i++ {
		candidate := r.selectServingTargets(types.UID(fmt.Sprintf("vpc-%d", i)), nil)
		if candidate[0].Shard != central.Shard {
			different = true
			break
		}
	}
	if !different {
		t.Fatal("rendezvous assignment never used the other shard")
	}
	existing := []dnsv1alpha1.DNSResolverBinding{{Spec: dnsv1alpha1.DNSResolverBindingSpec{Source: dnsv1alpha1.DNSResolverBindingSource{ProjectUID: "project", ResolverContextRef: dnsv1alpha1.DNSObjectReference{Name: string("vpc-1"), UID: "vpc-1"}}, Placement: dnsv1alpha1.DNSResolverBindingPlacement{Region: "central", Shard: central.Shard}}}}
	r.Options.ServingRegions = append(r.Options.ServingRegions, ServingRegion{Region: "central", Shard: "c"})
	after := r.selectServingTargets("vpc-1", existing)
	if after[0].Shard != central.Shard {
		t.Fatalf("adding a shard moved live VPC: before=%s after=%s", central.Shard, after[0].Shard)
	}
}

func TestNamespaceReconciliationCannotWithdrawAnotherNamespace(t *testing.T) {
	for _, labeled := range []bool{false, true} {
		t.Run(fmt.Sprintf("namespace-label-%t", labeled), func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			access := &dnsv1alpha1.DNSResolverAccessBinding{ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: "other", UID: "other-access"}}
			binding := &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding", Namespace: "platform", Labels: map[string]string{"internal-dns.miloapis.com/context-uid": "other-context"}}, Spec: dnsv1alpha1.DNSResolverBindingSpec{Source: dnsv1alpha1.DNSResolverBindingSource{ProjectUID: "project", AccessBindingRef: dnsv1alpha1.DNSObjectReference{Name: access.Name, UID: access.UID}, ResolverContextRef: dnsv1alpha1.DNSObjectReference{Name: "context", UID: "other-context"}}}}
			if labeled {
				binding.Labels["internal-dns.miloapis.com/source-namespace"] = access.Namespace
			}
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(access, binding).Build()
			reconciler := &Reconciler{Client: cl, APIReader: cl, Options: ReconcilerOptions{ProjectUID: "project", PlatformNamespace: "platform"}}
			if err := reconciler.reconcileContextBindings(ctx, "different", nil, nil, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(binding), binding); err != nil {
				t.Fatal(err)
			}
			if binding.Spec.Tombstone {
				t.Fatal("another source namespace was withdrawn")
			}
		})
	}
}

func TestAllNamespaceReconcileIncludesPlatformNamedSourceNamespace(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	contextObject := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "context", Namespace: "platform", UID: "context", Generation: 1}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "network"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(contextObject).WithStatusSubresource(contextObject).Build()
	r := &Reconciler{Client: cl, APIReader: cl, Options: ReconcilerOptions{ProjectUID: "project", PlatformNamespace: "platform"}}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(contextObject)}); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(contextObject), contextObject); err != nil {
		t.Fatal(err)
	}
	if !apimeta.IsStatusConditionTrue(contextObject.Status.Conditions, "Ready") {
		t.Fatal("source namespace was excluded because it shares the managed namespace name")
	}
}

func TestUnobservedProjectIdentityPreventsSourceCompilation(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	resolver := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "context", Namespace: "project", UID: "context"}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "network"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(resolver).WithStatusSubresource(resolver).Build()
	unavailable := errors.New("identity unavailable")
	r := &Reconciler{Client: cl, Options: ReconcilerOptions{ProjectUID: "project", VerifyProjectIdentity: func(context.Context) error { return unavailable }}}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(resolver)}); !errors.Is(err, unavailable) {
		t.Fatalf("identity error not preserved: %v", err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(resolver), resolver); err != nil {
		t.Fatal(err)
	}
	if len(resolver.Status.Conditions) != 0 {
		t.Fatal("unobserved identity changed source state")
	}
}

func TestNewContextCannotDisplaceExistingConsumerOwner(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	older := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "z-established", Namespace: "project", UID: "old", CreationTimestamp: metav1.NewTime(now.Add(-time.Hour)), Generation: 1}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(older).WithStatusSubresource(&dnsv1alpha1.DNSResolverContext{}).Build()
	r := &Reconciler{Client: c, Scheme: s, Options: ReconcilerOptions{PlatformNamespace: "platform"}}
	if err := r.reconcileResolverContexts(ctx, "project", now); err != nil {
		t.Fatal(err)
	}
	newer := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "a-new", Namespace: "project", UID: "new", CreationTimestamp: metav1.NewTime(now), Generation: 1}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer"}}
	if err := c.Create(ctx, newer); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileResolverContexts(ctx, "project", now); err != nil {
		t.Fatal(err)
	}
	for _, object := range []*dnsv1alpha1.DNSResolverContext{older, newer} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			t.Fatal(err)
		}
	}
	if condition := apimeta.FindStatusCondition(older.Status.Conditions, "Ready"); condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatal("new context displaced established consumer ownership")
	}
	if condition := apimeta.FindStatusCondition(newer.Status.Conditions, "Ready"); condition == nil || condition.Reason != "DuplicateConsumerID" || condition.Status != metav1.ConditionFalse {
		t.Fatal("duplicate consumer was not rejected")
	}
}

func TestInvalidManagedDomainIsReported(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	contextObject := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "ctx", Namespace: "project", UID: "context-uid", Generation: 1}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer", ManagedNamespace: dnsv1alpha1.DNSResolverContextManagedNamespace{Enabled: true, DomainSuffix: "not a domain"}}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(contextObject).WithStatusSubresource(&dnsv1alpha1.DNSResolverContext{}).Build()
	r := &Reconciler{Client: c, Scheme: s, Options: ReconcilerOptions{PlatformNamespace: "platform"}}
	if err := r.reconcileResolverContexts(ctx, "project", now); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(contextObject), contextObject); err != nil {
		t.Fatal(err)
	}
	if condition := apimeta.FindStatusCondition(contextObject.Status.Conditions, "Ready"); condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "InvalidManagedDomain" {
		t.Fatalf("invalid managed domain was not reported: %#v", contextObject.Status.Conditions)
	}
}

type claimAllocator struct {
	testAllocator
	err error
}

func (a claimAllocator) ClaimDestination(context.Context, types.UID, types.UID, string, string, int32) error {
	return a.err
}

func TestDestinationClaimAPIErrorsAreNotRefusals(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	contextObject := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "ctx", Namespace: "project", UID: "context-uid", Generation: 1}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer-uid"}}
	setCondition(&contextObject.Status.Conditions, "Ready", metav1.ConditionTrue, "Ready", "", 1, now)
	access := &dnsv1alpha1.DNSResolverAccessBinding{ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: "project", UID: "access-uid", Generation: 1}, Spec: dnsv1alpha1.DNSResolverAccessBindingSpec{ContextRef: dnsv1alpha1.DNSObjectReference{Name: "ctx", UID: "context-uid"}, Region: "central", DestinationAddress: "fd70:100::10"}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(contextObject, access).WithStatusSubresource(&dnsv1alpha1.DNSResolverContext{}, &dnsv1alpha1.DNSResolverAccessBinding{}, &dnsv1alpha1.DNSResolverBinding{}).Build()
	r := &Reconciler{Client: c, Scheme: s, Options: ReconcilerOptions{ProjectUID: "project-uid", PlatformNamespace: "platform", Region: "central", Shard: "shared", AddressAllocator: claimAllocator{err: fmt.Errorf("etcdserver: request timed out")}}}
	if err := r.reconcileContextBindings(ctx, "project", nil, map[string]*dnsv1alpha1.DNSZone{}, now); err == nil {
		t.Fatal("transient destination claim failure was swallowed")
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(access), access); err != nil {
		t.Fatal(err)
	}
	if ready := apimeta.FindStatusCondition(access.Status.Conditions, "Ready"); ready != nil && ready.Reason == "AccessRejected" {
		t.Fatalf("transient claim failure rejected access: %#v", access.Status.Conditions)
	}
	r.Options.AddressAllocator = claimAllocator{err: fmt.Errorf("%w: service destination is already reserved", model.ErrDestinationRefused)}
	if err := r.reconcileContextBindings(ctx, "project", nil, map[string]*dnsv1alpha1.DNSZone{}, now); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(access), access); err != nil {
		t.Fatal(err)
	}
	if ready := apimeta.FindStatusCondition(access.Status.Conditions, "Ready"); ready == nil || ready.Reason != "AccessRejected" {
		t.Fatalf("refused destination was not rejected: %#v", access.Status.Conditions)
	}
}
