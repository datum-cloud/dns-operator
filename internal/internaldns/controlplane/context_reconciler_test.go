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

func (testAllocator) Allocate(context.Context, types.UID, types.UID) (string, string, error) {
	return "fd53::10", "fd54::10", nil
}

func (testAllocator) ClaimDestination(context.Context, types.UID, types.UID, string, string, int32) error {
	return nil
}

func TestContextAccessPersistsUntilExplicitWithdrawal(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := runtime.NewScheme()
	if err := addReferenceStateToScheme(s); err != nil {
		t.Fatal(err)
	}
	contextObject := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "ctx", Namespace: "project", UID: "context-uid", Generation: 1}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer-uid"}, Status: dnsv1alpha1.DNSResolverContextStatus{AccessWriterEpoch: 4}}
	setCondition(&contextObject.Status.Conditions, "Ready", metav1.ConditionTrue, "Ready", "", 1, now)
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "zone", Namespace: "project", UID: "zone-uid", Generation: 1}, Spec: dnsv1alpha1.DNSZoneSpec{Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	association := &dnsv1alpha1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: "assoc", Namespace: "project", UID: "assoc-uid", Generation: 1}, Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: "zone"}, ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: "ctx"}}, Status: dnsv1alpha1.DNSZoneAssociationStatus{ResolvedDNSZoneRef: dnsv1alpha1.DNSNameReference{Name: "zone"}, ResolvedResolverContextRef: dnsv1alpha1.DNSNameReference{Name: "ctx"}}}
	setCondition(&association.Status.Conditions, "Accepted", metav1.ConditionTrue, "Accepted", "", 1, now)
	access := &dnsv1alpha1.DNSResolverAccessBinding{ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: "project", UID: "access-uid", Generation: 1}, Spec: dnsv1alpha1.DNSResolverAccessBindingSpec{ContextRef: dnsv1alpha1.DNSObjectReference{Name: "ctx", UID: "context-uid"}, Region: "central", QueryIdentity: dnsv1alpha1.DNSResolverQueryIdentity{Type: dnsv1alpha1.DNSResolverQueryIdentityDestinationAddress, Value: "fd70:100::10"}, Port: 53, Transports: []string{"UDP", "TCP"}, Authorization: dnsv1alpha1.DNSResolverAccessAuthorization{WriterEpoch: 4, Sequence: 12}}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(contextObject, zone, association, access).WithStatusSubresource(&dnsv1alpha1.DNSResolverContext{}, &dnsv1alpha1.DNSResolverAccessBinding{}, &dnsv1alpha1.DNSResolverAccessBinding{}, &dnsv1alpha1.DNSResolverBinding{}).Build()
	r := &Reconciler{Client: c, Scheme: s, Options: ReconcilerOptions{ProjectUID: "project-uid", PlatformNamespace: "platform", Region: "central", Shard: "shared", AddressAllocator: testAllocator{}}}
	seedReferenceState(t, r, association, map[string]dnsv1alpha1.DNSObjectReference{"zone": {Name: zone.Name, UID: zone.UID, Generation: zone.Generation}, "context": {Name: contextObject.Name, UID: contextObject.UID}})
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
	if b.Spec.Configuration.Listeners.Node.Address != "fd70:100::10" || b.Spec.Source.ResolverContextRef.UID != "context-uid" || b.Spec.Authorization.WriterEpoch != 4 || b.Spec.Authorization.Sequence != 12 {
		t.Fatalf("access projection drifted: %#v", b.Spec)
	}
	if b.Spec.Source.ProjectUID != "project-uid" || b.Spec.Source.ResolverContextRef.Name != "ctx" || b.Spec.Source.AccessBindingRef.Name != "access" || b.Spec.Source.AccessBindingRef.UID != "access-uid" || b.Spec.Configuration.ZoneRefs[0].Name != "zone" || b.Spec.Configuration.ZoneRefs[0].UID != "zone-uid" || b.Spec.Placement.Region != "central" {
		t.Fatalf("source identity or zone references drifted: %#v", b.Spec)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(access), access); err != nil {
		t.Fatal(err)
	}
	if access.Status.ObservedSequence != 12 {
		t.Fatalf("sequence fence not persisted: %#v", access.Status)
	}
	extraZone := zone.DeepCopy()
	extraZone.Name = "extra"
	extraZone.UID = "extra-uid"
	extraAssociation := association.DeepCopy()
	extraAssociation.Name = "extra-assoc"
	extraAssociation.Spec.DNSZoneRef = dnsv1alpha1.DNSNameReference{Name: extraZone.Name}
	extraAssociation.UID = "extra-association-uid"
	extraAssociation.Status.ResolvedDNSZoneRef = dnsv1alpha1.DNSNameReference{Name: extraZone.Name}
	seedReferenceState(t, r, extraAssociation, map[string]dnsv1alpha1.DNSObjectReference{"zone": {Name: extraZone.Name, UID: extraZone.UID, Generation: extraZone.Generation}, "context": {Name: contextObject.Name, UID: contextObject.UID}})
	if err := r.reconcileContextBindings(ctx, "project", []dnsv1alpha1.DNSZoneAssociation{*association, *extraAssociation}, map[string]*dnsv1alpha1.DNSZone{"zone": zone, "extra": extraZone}, now); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(&b), &b); err != nil {
		t.Fatal(err)
	}
	if b.Spec.Configuration.Generation != 2 || b.Spec.Configuration.Revision != 2 {
		t.Fatalf("zone membership change reused configuration fences: %#v", b.Spec.Configuration)
	}

	contextObject.Status.AccessWriterEpoch++
	if err := c.Status().Update(ctx, contextObject); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileContextBindings(ctx, "project", []dnsv1alpha1.DNSZoneAssociation{*association}, map[string]*dnsv1alpha1.DNSZone{"zone": zone}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(&b), &b); err != nil {
		t.Fatal(err)
	}
	if !b.Spec.Tombstone || b.Spec.Authorization.Sequence != 12 || b.Spec.Configuration.Revision != 3 {
		t.Fatalf("withdrawal changed integration authority or failed to advance configuration: %#v", b.Spec)
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
	access.Spec.Authorization.WriterEpoch = contextObject.Status.AccessWriterEpoch
	access.Spec.Authorization.Sequence++
	if err := c.Update(ctx, access); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileContextBindings(ctx, "project", []dnsv1alpha1.DNSZoneAssociation{*association}, map[string]*dnsv1alpha1.DNSZone{"zone": zone}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(&b), &b); err != nil {
		t.Fatal(err)
	}
	if b.Spec.Tombstone || b.Spec.Authorization.Sequence != 13 || b.Spec.Configuration.Revision != 4 {
		t.Fatalf("ordinary next source sequence failed to renew access: %#v", b.Spec)
	}
	for _, snapshot := range [][]dnsv1alpha1.DNSResolverAccessBinding{{staleAccess}, nil} {
		staleController := &Reconciler{Client: &accessListSnapshotClient{Client: c, accesses: snapshot}, Scheme: s, Options: r.Options}
		if err := staleController.reconcileContextBindings(ctx, "project", nil, nil, now.Add(3*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(&b), &b); err != nil {
			t.Fatal(err)
		}
		if b.Spec.Tombstone || b.Spec.Configuration.Revision != 4 || b.Spec.Authorization.Sequence != 13 {
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
			t.Fatalf("%s read uncertainty revoked authorization", kind)
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
	b := &dnsv1alpha1.DNSResolverBinding{Spec: dnsv1alpha1.DNSResolverBindingSpec{Configuration: dnsv1alpha1.DNSResolverBindingConfiguration{Revision: 7}, Authorization: dnsv1alpha1.DNSResolverAccessAuthorization{WriterEpoch: 3, Sequence: 11}}, Status: dnsv1alpha1.DNSResolverBindingStatus{MemberAcknowledgements: []dnsv1alpha1.DNSApplyAcknowledgement{{Phase: string(model.AckVerified), Revision: 7, AuthorizationIssuerEpoch: 3, AuthorizationRevision: 11, ValidUntil: &ackDeadline}}}}
	if !bindingAckCurrent(b, now) {
		t.Fatal("exact verified ACK was rejected")
	}
	b.Status.MemberAcknowledgements[0].Phase = string(model.AckRejected)
	if bindingAckCurrent(b, now) {
		t.Fatal("rejected ACK was accepted")
	}
	b.Status.MemberAcknowledgements[0].Phase = string(model.AckVerified)
	b.Status.MemberAcknowledgements[0].AuthorizationRevision = 10
	if bindingAckCurrent(b, now) {
		t.Fatal("stale authorization ACK was accepted")
	}
	b.Status.MemberAcknowledgements[0].AuthorizationRevision = 11
}

func TestContextNamingPolicyResolvesMultipleZonesWithoutNetworking(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := runtime.NewScheme()
	if err := addReferenceStateToScheme(s); err != nil {
		t.Fatal(err)
	}
	resolverContext := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "ctx", Namespace: "project", UID: "ctx-uid", Generation: 2}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer-uid"}}
	setCondition(&resolverContext.Status.Conditions, "Ready", metav1.ConditionTrue, "Ready", "", 2, now)
	z1 := dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "managed", Namespace: "project", UID: "zone-1", Generation: 1}, Spec: dnsv1alpha1.DNSZoneSpec{Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	z2 := dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "services", Namespace: "project", UID: "zone-2", Generation: 3}, Spec: dnsv1alpha1.DNSZoneSpec{Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	associations := []dnsv1alpha1.DNSZoneAssociation{{Status: dnsv1alpha1.DNSZoneAssociationStatus{ResolvedDNSZoneRef: dnsv1alpha1.DNSNameReference{}, ResolvedResolverContextRef: dnsv1alpha1.DNSNameReference{}}}, {Status: dnsv1alpha1.DNSZoneAssociationStatus{ResolvedDNSZoneRef: dnsv1alpha1.DNSNameReference{}, ResolvedResolverContextRef: dnsv1alpha1.DNSNameReference{}}}}
	for i := range associations {
		zone := []dnsv1alpha1.DNSZone{z1, z2}[i]
		associations[i].Name = zone.Name + "-association"
		associations[i].Namespace = "project"
		associations[i].UID = types.UID(associations[i].Name)
		associations[i].Generation = 1
		setCondition(&associations[i].Status.Conditions, "Accepted", metav1.ConditionTrue, "Accepted", "", 1, now)
	}
	policy := &dnsv1alpha1.DNSNamingPolicy{ObjectMeta: metav1.ObjectMeta{Name: "names", Namespace: "project", UID: "policy-uid", Generation: 1}, Spec: dnsv1alpha1.DNSNamingPolicySpec{ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: resolverContext.Name}, AdditionalNames: []dnsv1alpha1.DNSAdditionalNameRule{{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: z1.Name}, NamePrefix: "instances"}, {DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: z2.Name}, NamePrefix: "services"}}}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(resolverContext, policy).WithStatusSubresource(&dnsv1alpha1.DNSResolverContext{}, &dnsv1alpha1.DNSResolverAccessBinding{}, &dnsv1alpha1.DNSNamingPolicy{}).Build()
	r := &Reconciler{Client: c, Options: ReconcilerOptions{}}
	for i := range associations {
		zone := []dnsv1alpha1.DNSZone{z1, z2}[i]
		seedReferenceState(t, r, &associations[i], map[string]dnsv1alpha1.DNSObjectReference{"zone": {Name: zone.Name, UID: zone.UID, Generation: zone.Generation}, "context": {Name: resolverContext.Name, UID: resolverContext.UID}})
	}
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
	if policy.Status.ResolvedResolverContextRef.Name != resolverContext.Name {
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

func TestAccessPlacementMatchesContextPublicationTargets(t *testing.T) {
	r := &Reconciler{Options: ReconcilerOptions{ProjectUID: "project", ServingRegions: []ServingRegion{{Region: "east", Shard: "a"}, {Region: "east", Shard: "b"}, {Region: "west", Shard: "a"}}}}
	for i := range 100 {
		uid := types.UID(fmt.Sprintf("context-%d", i))
		for _, target := range r.selectServingTargets(uid, nil) {
			if shard := r.shardForContextRegion(uid, target.Region, nil); shard != target.Shard {
				t.Fatalf("binding shard %s does not match publication target %s", shard, target.Shard)
			}
		}
	}
}
