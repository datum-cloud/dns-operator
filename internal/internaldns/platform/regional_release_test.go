// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns/api/v1alpha1"
	"go.miloapis.com/dns/internal/internaldns/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// regionalState lists the regional claims (address -> owner) and the owner
// leases (owner -> address) in the platform namespace.
func regionalState(t *testing.T, cl client.Client) (map[string]string, map[string]string) {
	t.Helper()
	var maps corev1.ConfigMapList
	if err := cl.List(context.Background(), &maps, client.InNamespace("platform")); err != nil {
		t.Fatal(err)
	}
	claims, leases := map[string]string{}, map[string]string{}
	for _, cm := range maps.Items {
		switch {
		case strings.HasPrefix(cm.Name, regionalClaimPrefix):
			claims[cm.Data["address"]] = cm.Data["owner"]
		case strings.HasPrefix(cm.Name, regionalLeasePrefix):
			var lease AddressLease
			if err := json.Unmarshal([]byte(cm.Data["lease"]), &lease); err != nil {
				t.Fatal(err)
			}
			leases[lease.Owner] = lease.ClusterAddress
		}
	}
	return claims, leases
}

func regionalBinding(name, resolverContext, region, address string) *dnsv1alpha1.DNSResolverBinding {
	return &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "platform", UID: types.UID(name + "-uid")}, Spec: dnsv1alpha1.DNSResolverBindingSpec{
		Source:        dnsv1alpha1.DNSResolverBindingSource{ProjectUID: "project-a", ResolverContextRef: dnsv1alpha1.DNSObjectReference{Name: resolverContext, UID: types.UID(resolverContext)}},
		Placement:     dnsv1alpha1.DNSResolverBindingPlacement{Region: region, Shard: "shared"},
		Configuration: dnsv1alpha1.DNSResolverBindingConfiguration{Generation: 1, Revision: 1, Query: dnsv1alpha1.DNSResolverBindingQuery{NodeAddress: "fd70::53", RegionalAddress: address}},
		Version:       dnsv1alpha1.DNSResolverAccessVersion{Epoch: 1, Sequence: 1},
	}}
}

func TestRegionalReleaseFreesOnlyUnroutedAddresses(t *testing.T) {
	ctx := context.Background()
	cl := testClient(t)
	allocator := &Allocator{Client: cl, Namespace: "platform"}
	later := time.Now().Add(time.Hour)
	addresses := map[string]string{}
	for _, owner := range []string{"live", "gone", "tombstone"} {
		address, err := allocator.Allocate(ctx, "project-a", types.UID(owner), "east")
		if err != nil {
			t.Fatal(err)
		}
		addresses[owner] = address
	}
	if err := cl.Create(ctx, regionalBinding("live", "live", "east", addresses["live"])); err != nil {
		t.Fatal(err)
	}
	tombstone := regionalBinding("tombstone", "tombstone", "east", addresses["tombstone"])
	tombstone.Spec.Tombstone = true
	if err := cl.Create(ctx, tombstone); err != nil {
		t.Fatal(err)
	}

	dry, err := allocator.ReleaseRegionalAddresses(ctx, later, 10*time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Released != 1 || dry.InUse != 2 {
		t.Fatalf("dry run counted %+v", dry)
	}
	if claims, _ := regionalState(t, cl); len(claims) != 3 {
		t.Fatal("a dry run freed a claim")
	}

	result, err := allocator.ReleaseRegionalAddresses(ctx, later, 10*time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Released != 1 {
		t.Fatalf("released %+v", result)
	}
	claims, leases := regionalState(t, cl)
	if _, ok := claims[addresses["gone"]]; ok {
		t.Fatal("an address no binding routes kept its claim")
	}
	if _, ok := leases["project-a/gone"]; ok {
		t.Fatal("an address no binding routes kept its owner lease")
	}
	if claims[addresses["live"]] != "project-a/live" || leases["project-a/live"] != addresses["live"] {
		t.Fatal("a live binding's address was freed")
	}
	if claims[addresses["tombstone"]] != "project-a/tombstone" {
		t.Fatal("an address was freed before its tombstone was withdrawn")
	}
	again, err := allocator.Allocate(ctx, "project-a", "live", "east")
	if err != nil || again != addresses["live"] {
		t.Fatal("a kept lease changed its address")
	}
}

func TestRegionalReleaseLeavesNewAllocations(t *testing.T) {
	ctx := context.Background()
	cl := testClient(t)
	allocator := &Allocator{Client: cl, Namespace: "platform"}
	if _, err := allocator.Allocate(ctx, "project-a", "context-a", "east"); err != nil {
		t.Fatal(err)
	}
	result, err := allocator.ReleaseRegionalAddresses(ctx, time.Now(), 10*time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	if claims, leases := regionalState(t, cl); result.Released != 0 || len(claims) != 1 || len(leases) != 1 {
		t.Fatal("an allocation was freed before its binding could be written")
	}
}

func TestRegionalReleaseWaitsForWithdrawnSnapshot(t *testing.T) {
	ctx := context.Background()
	now := time.Now().Add(time.Hour)
	region, shard, ns := "east", "shared", "platform"
	state, _ := json.Marshal(shardState{Holder: "planner", Epoch: 1, NextRevision: 1, LeaseUntil: now.Add(time.Minute)})
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "dns-shard-owner-" + model.OpaqueToken(region+"/"+shard), Namespace: ns, UID: "shard-owner-uid"}, Data: map[string]string{"state": string(state)}}
	cl := testClient(t, owner)
	allocator := &Allocator{Client: cl, Namespace: ns}
	address, err := allocator.Allocate(ctx, "project-a", "context-a", region)
	if err != nil {
		t.Fatal(err)
	}
	binding := regionalBinding("binding", "context-a", region, address)
	binding.Labels = map[string]string{"internal-dns.miloapis.com/region": region, "internal-dns.miloapis.com/shard": shard}
	if err := cl.Create(ctx, binding); err != nil {
		t.Fatal(err)
	}
	p := Planner{Client: cl, Allocator: allocator, Config: PlannerConfig{Namespace: ns, Region: region, Shard: shard, Identity: "planner", ClusterBackends: []model.Backend{{MemberID: "cluster-1", Address: "fd00::20", Port: 5300}}}, Now: func() time.Time { return now }}
	if err := p.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if snapshot := latestPlannerSnapshot(t, &p); len(snapshot.Bindings) != 1 || snapshot.Bindings[0].ClusterAddress != address {
		t.Fatalf("binding was not published: %#v", snapshot.Bindings)
	}
	release := func() map[string]string {
		t.Helper()
		if _, err := allocator.ReleaseRegionalAddresses(ctx, now, 10*time.Minute, false); err != nil {
			t.Fatal(err)
		}
		claims, _ := regionalState(t, cl)
		return claims
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(binding), binding); err != nil {
		t.Fatal(err)
	}
	binding.Spec.Tombstone = true
	binding.Spec.Configuration.Revision++
	binding.Spec.Version.Sequence++
	if err := cl.Update(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if _, ok := release()[address]; !ok {
		t.Fatal("address was freed while the committed snapshot still routed it")
	}
	if err := p.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := release()[address]; !ok {
		t.Fatal("address was freed before every member proved withdrawal")
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(binding), binding); err != nil {
		t.Fatal(err)
	}
	binding.Status.ObservedConfigurationRevision = binding.Spec.Configuration.Revision
	binding.Status.Phase = dnsValueWithdrawn
	condition(&binding.Status.Conditions, "FullyReplicated", true, "ReplicaVerification", binding.Generation, now)
	if err := cl.Status().Update(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if _, ok := release()[address]; ok {
		t.Fatal("fully withdrawn address was not freed")
	}
}

func TestRegionalReleaseRestoresClaimForLiveBinding(t *testing.T) {
	ctx := context.Background()
	cl := testClient(t)
	allocator := &Allocator{Client: cl, Namespace: "platform", ClusterPrefix: "fd70:200::/96", RegionalPrefixes: map[string]string{"west": "fd70:300::/96"}}
	for _, b := range []*dnsv1alpha1.DNSResolverBinding{
		regionalBinding("shared", "context-a", "east", "fd70:200::7"),
		regionalBinding("own", "context-b", "west", "fd70:300::7"),
	} {
		if err := cl.Create(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	result, err := allocator.ReleaseRegionalAddresses(ctx, time.Now(), 10*time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	claims, _ := regionalState(t, cl)
	if result.Restored != 2 || claims["fd70:200::7"] != "project-a/context-a" || claims["fd70:300::7"] != "project-a/context-b/west" {
		t.Fatalf("live bindings' claims were not restored: %+v %v", result, claims)
	}
	// A restored claim keeps the address from a new allocation.
	if err := allocator.ClaimDestination(ctx, "project-b", "context-c", "east", "fd70:200::7", 53); err == nil {
		t.Fatal("a restored regional address was handed out as a destination")
	}
}

func TestRegionalControlPlaneReleasesOnlyItsRegionsPools(t *testing.T) {
	ctx := context.Background()
	cl := testClient(t)
	all := &Allocator{Client: cl, Namespace: "platform", ClusterPrefix: "fd70:200::/96", RegionalPrefixes: map[string]string{"west": "fd70:300::/96", "north": "fd70:400::/96"}}
	shared, err := all.Allocate(ctx, "project-a", "context-a", "east")
	if err != nil {
		t.Fatal(err)
	}
	west, err := all.Allocate(ctx, "project-a", "context-a", "west")
	if err != nil {
		t.Fatal(err)
	}
	north, err := all.Allocate(ctx, "project-a", "context-a", "north")
	if err != nil {
		t.Fatal(err)
	}
	regional := *all
	regional.Regions = []string{"west"}
	if _, err := regional.ReleaseRegionalAddresses(ctx, time.Now().Add(time.Hour), 10*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	claims, _ := regionalState(t, cl)
	if _, ok := claims[west]; ok {
		t.Fatal("this region's unrouted claim was not freed")
	}
	if _, ok := claims[north]; !ok {
		t.Fatal("another region's claim was freed by this region's control plane")
	}
	if _, ok := claims[shared]; !ok {
		t.Fatal("a shared-pool claim was freed by a control plane that does not publish every region")
	}
}

func TestRegionalReleaseFreesOrphanedLeaseAndClaim(t *testing.T) {
	ctx := context.Background()
	cl := testClient(t)
	allocator := &Allocator{Client: cl, Namespace: "platform"}
	first, err := allocator.Allocate(ctx, "project-a", "lease-only", "east")
	if err != nil {
		t.Fatal(err)
	}
	second, err := allocator.Allocate(ctx, "project-a", "claim-only", "east")
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: regionalClaimPrefix + model.OpaqueToken(first)}}); err != nil {
		t.Fatal(err)
	}
	if err := cl.Delete(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: regionalLeasePrefix + model.OpaqueToken("project-a/claim-only")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := allocator.ReleaseRegionalAddresses(ctx, time.Now().Add(time.Hour), 10*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if claims, leases := regionalState(t, cl); len(claims) != 0 || len(leases) != 0 {
		t.Fatalf("orphans were kept: claims %v leases %v (addresses %s %s)", claims, leases, first, second)
	}
}

func TestRegionalReleaseLosesToAConcurrentReuse(t *testing.T) {
	ctx := context.Background()
	base := testClient(t)
	allocator := &Allocator{Client: base, Namespace: "platform"}
	address, err := allocator.Allocate(ctx, "project-a", "context-a", "east")
	if err != nil {
		t.Fatal(err)
	}
	raced := false
	cl := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if !raced {
			raced = true
			if again, err := allocator.Allocate(ctx, "project-a", "context-a", "east"); err != nil || again != address {
				t.Fatalf("reuse changed the address: %v", err)
			}
		}
		return c.Delete(ctx, obj, opts...)
	}})
	sweeper := &Allocator{Client: cl, Namespace: "platform"}
	if _, err := sweeper.ReleaseRegionalAddresses(ctx, time.Now().Add(time.Hour), 10*time.Minute, false); err == nil {
		t.Fatal("a sweep deleted a lease reused after it was read")
	}
	claims, leases := regionalState(t, base)
	if claims[address] != "project-a/context-a" || leases["project-a/context-a"] != address {
		t.Fatalf("a reused allocation lost its claim or lease: %v %v", claims, leases)
	}
	if _, err := sweeper.ReleaseRegionalAddresses(ctx, time.Now(), 10*time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if claims, _ := regionalState(t, base); claims[address] == "" {
		t.Fatal("a just-reused lease was freed within its grace")
	}
}

func TestRegionalReleaseKeepsClaimsWhenSnapshotsFail(t *testing.T) {
	ctx := context.Background()
	base := testClient(t)
	allocator := &Allocator{Client: base, Namespace: "platform"}
	for _, owner := range []string{"a", "b", "c"} {
		if _, err := allocator.Allocate(ctx, "project-a", types.UID(owner), "east"); err != nil {
			t.Fatal(err)
		}
	}
	if err := base.Create(ctx, regionalBinding("other", "other", "east", "fd54::ffff")); err != nil {
		t.Fatal(err)
	}
	loads := 0
	cl := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if strings.HasPrefix(key.Name, "dns-shard-owner-") {
			loads++
			return errors.New("broker unavailable")
		}
		return c.Get(ctx, key, obj, opts...)
	}})
	result, err := (&Allocator{Client: cl, Namespace: "platform"}).ReleaseRegionalAddresses(ctx, time.Now().Add(time.Hour), 10*time.Minute, false)
	if err == nil || loads != 1 || result.Released != 0 {
		t.Fatalf("an unreadable snapshot did not hold every claim once: loads %d, %+v, %v", loads, result, err)
	}
	if claims, _ := regionalState(t, base); len(claims) != 4 {
		t.Fatalf("claims were freed without the snapshot: %v", claims)
	}
}
