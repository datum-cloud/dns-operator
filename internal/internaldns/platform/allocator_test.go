// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func testClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(&dnsv1alpha1.DNSTransportOutbox{}, &dnsv1alpha1.DNSPublicationManifest{}, &dnsv1alpha1.DNSResolverBinding{}).Build()
}

func TestAddressClaimsAreConcurrentStableAndLifetimeScoped(t *testing.T) {
	ctx := context.Background()
	cl := testClient(t)
	allocator := Allocator{Client: cl, Namespace: "platform"}
	var wg sync.WaitGroup
	var mu sync.Mutex
	addresses := map[string]string{}
	var failures []error
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			uid := types.UID(fmt.Sprintf("vpc-%d", i))
			a, err := allocator.Allocate(ctx, "project", uid, "east")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			for _, address := range []string{a} {
				if previous, ok := addresses[address]; ok && previous != string(uid) {
					failures = append(failures, fmt.Errorf("cross-context address collision"))
				}
				addresses[address] = string(uid)
			}
		}(i)
	}
	wg.Wait()
	if len(failures) > 0 {
		t.Fatal(failures)
	}
	if len(addresses) != 40 {
		t.Fatalf("got %d address claims", len(addresses))
	}
	a, err := allocator.Allocate(ctx, "project", "vpc-1", "east")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := (&Allocator{Client: cl, Namespace: "platform"}).Allocate(ctx, "project", "vpc-1", "east")
	if err != nil || a != a2 {
		t.Fatal("allocator restart changed an existing lease")
	}
	replacement, err := allocator.Allocate(ctx, "project", "new-vpc-lifetime", "east")
	if err != nil || replacement == a {
		t.Fatal("new resource lifetime reused the old address")
	}
}

func TestDestinationClaimBelongsToContextLifetime(t *testing.T) {
	ctx := context.Background()
	cl := testClient(t)
	allocator := Allocator{Client: cl, Namespace: "platform"}
	for range 2 {
		if err := allocator.ClaimDestination(ctx, "project", "context", "east", "fd70::53", 53); err != nil {
			t.Fatal(err)
		}
	}
	if err := allocator.ClaimDestination(ctx, "project", "replacement-context", "east", "fd70::53", 53); err == nil {
		t.Fatal("a replacement context reused a quarantined destination")
	}
	if err := allocator.ClaimDestination(ctx, "another-project", "context", "east", "fd70::53", 53); err == nil {
		t.Fatal("another project reused a destination")
	}
}

func TestDestinationClaimConversionRequiresPinnedBinding(t *testing.T) {
	ctx := context.Background()
	cl := testClient(t)
	allocator := Allocator{Client: cl, Namespace: "platform"}
	if err := allocator.ClaimDestination(ctx, "project", "access", "east", "fd70::53", 53); err != nil {
		t.Fatal(err)
	}
	if err := allocator.ClaimDestination(ctx, "project", "context", "east", "fd70::53", 53); err == nil {
		t.Fatal("claim changed ownership without lifetime proof")
	}
	binding := &dnsv1alpha1.DNSResolverBinding{}
	binding.Namespace, binding.Name = "platform", "binding"
	binding.Spec.Source.ProjectUID = "project"
	binding.Spec.Source.ResolverContextRef.UID = "context"
	binding.Spec.Source.AccessBindingRef.UID = "access"
	binding.Spec.Placement.Region = "east"
	binding.Spec.Configuration.Query.NodeAddress = "fd70::53"
	if err := cl.Create(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if err := allocator.ClaimDestination(ctx, "project", "context", "east", "fd70::53", 53); err != nil {
		t.Fatal(err)
	}
	if err := allocator.ClaimDestination(ctx, "project", "access", "east", "fd70::53", 53); err == nil {
		t.Fatal("converted claim returned to access lifetime")
	}
}

func TestServiceDestinationsCannotOverlapRegionalAddressPool(t *testing.T) {
	ctx := context.Background()
	for _, prefix := range []string{"", "fd80::/96"} {
		allocator := Allocator{Client: testClient(t), Namespace: "platform", ClusterPrefix: prefix}
		address, err := allocator.Allocate(ctx, "project-a", "context-a", "east")
		if err != nil {
			t.Fatal(err)
		}
		if err := allocator.ClaimDestination(ctx, "project-b", "context-b", "region", address, 53); err == nil {
			t.Fatal("service destination reused another context's regional address")
		}
		for _, address := range []string{"127.0.0.1", "::1", "fe80::1", "169.254.1.1", "ff02::1", "224.0.0.1", "::"} {
			if err := allocator.ClaimDestination(ctx, "project-a", "context-a", "region", address, 53); err == nil {
				t.Fatalf("non-unicast service destination accepted: %s", address)
			}
		}
		if err := allocator.ClaimDestination(ctx, "project-a", "context-a", "region", "fd70::1", 53); err != nil {
			t.Fatal(err)
		}
	}
}

func TestServiceDestinationCannotReuseRetainedRegionalClaim(t *testing.T) {
	ctx := context.Background()
	allocator := Allocator{Client: testClient(t), Namespace: "platform"}
	address, err := allocator.Allocate(ctx, "project-a", "context-a", "east")
	if err != nil {
		t.Fatal(err)
	}
	allocator.ClusterPrefix = "fd80::/96"
	if err := allocator.ClaimDestination(ctx, "project-b", "context-b", "region", address, 53); err == nil {
		t.Fatal("service destination reused a retained regional allocation")
	}
}

func TestServiceDestinationRejectsZonedAddresses(t *testing.T) {
	ctx := context.Background()
	allocator := Allocator{Client: testClient(t), Namespace: "platform"}
	for _, address := range []string{"fd54::1%eth0", "fd70::53%1", "2001:db8::53%eth0"} {
		err := allocator.ClaimDestination(ctx, "project", "context", "region", address, 53)
		if !errors.Is(err, model.ErrDestinationRefused) {
			t.Fatalf("zoned destination %s was not refused: %v", address, err)
		}
	}
	var claims corev1.ConfigMapList
	if err := allocator.Client.List(ctx, &claims); err != nil {
		t.Fatal(err)
	}
	if len(claims.Items) != 0 {
		t.Fatalf("zoned destination left %d claims", len(claims.Items))
	}
}

func TestRegionalAllocationSkipsClaimedServiceDestination(t *testing.T) {
	ctx := context.Background()
	cl := testClient(t)
	prefix := netip.MustParsePrefix("fd54::/64")
	taken := addressInPrefix(prefix, "project-a/context-a/cluster", 0)
	other := Allocator{Client: cl, Namespace: "platform", ClusterPrefix: "fd80::/96"}
	if err := other.ClaimDestination(ctx, "project-b", "context-b", "region", taken, 53); err != nil {
		t.Fatal(err)
	}
	address, err := (&Allocator{Client: cl, Namespace: "platform"}).Allocate(ctx, "project-a", "context-a", "east")
	if err != nil {
		t.Fatal(err)
	}
	if address == taken {
		t.Fatal("regional allocation reused a claimed service destination")
	}
	var claim corev1.ConfigMap
	if err := cl.Get(ctx, client.ObjectKey{Namespace: "platform", Name: "dns-address-claim-" + model.OpaqueToken(taken)}, &claim); !apierrors.IsNotFound(err) {
		t.Fatalf("skipped candidate kept its regional claim: %v", err)
	}
}

func TestServiceDestinationYieldsToRacingRegionalClaim(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	const address = "fd70::53"
	cl := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if err := c.Create(ctx, obj, opts...); err != nil {
			return err
		}
		if strings.HasPrefix(obj.GetName(), "dns-destination-claim-") {
			regional := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "dns-address-claim-" + model.OpaqueToken(address), Namespace: "platform"}, Data: map[string]string{"owner": "project-a/context-a", "address": address}}
			return c.Create(ctx, regional)
		}
		return nil
	}}).Build()
	allocator := Allocator{Client: cl, Namespace: "platform", ClusterPrefix: "fd80::/96"}
	if err := allocator.ClaimDestination(ctx, "project-b", "context-b", "region", address, 53); !errors.Is(err, model.ErrDestinationRefused) {
		t.Fatalf("destination claim ignored a racing regional claim: %v", err)
	}
	var claims corev1.ConfigMapList
	if err := cl.List(ctx, &claims, client.InNamespace("platform")); err != nil {
		t.Fatal(err)
	}
	for _, claim := range claims.Items {
		if strings.HasPrefix(claim.Name, "dns-destination-claim-") {
			t.Fatal("refused destination kept its claim")
		}
	}
}

func destinationClaims(t *testing.T, cl client.Client) map[string]string {
	t.Helper()
	var claims corev1.ConfigMapList
	if err := cl.List(context.Background(), &claims, client.InNamespace("platform")); err != nil {
		t.Fatal(err)
	}
	owners := map[string]string{}
	for _, claim := range claims.Items {
		if strings.HasPrefix(claim.Name, "dns-destination-claim-") {
			owners[claim.Data["address"]] = claim.Data["owner"]
		}
	}
	return owners
}

func TestReleaseDestinationsFreesOnlyUnheldUnroutedProjectClaims(t *testing.T) {
	ctx := context.Background()
	cl := testClient(t)
	allocator := Allocator{Client: cl, Namespace: "platform"}
	for _, claim := range []struct{ project, context, address string }{
		{"project-a", "context-a", "fd70::1"},
		{"project-a", "context-a", "fd70::2"},
		{"project-a", "context-b", "fd70::3"},
		{"project-b", "context-c", "fd70::4"},
	} {
		if err := allocator.ClaimDestination(ctx, types.UID(claim.project), types.UID(claim.context), "east", claim.address, 53); err != nil {
			t.Fatal(err)
		}
	}
	live := &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{Name: "live", Namespace: "platform"}, Spec: dnsv1alpha1.DNSResolverBindingSpec{Source: dnsv1alpha1.DNSResolverBindingSource{ProjectUID: "project-a"}, Placement: dnsv1alpha1.DNSResolverBindingPlacement{Region: "east", Shard: "shared"}, Configuration: dnsv1alpha1.DNSResolverBindingConfiguration{Query: dnsv1alpha1.DNSResolverBindingQuery{NodeAddress: "fd70::3"}}}}
	if err := cl.Create(ctx, live); err != nil {
		t.Fatal(err)
	}
	if err := allocator.ReleaseDestinations(ctx, "project-a", map[string]bool{model.DestinationKey("east", "fd70::1"): true}); err != nil {
		t.Fatal(err)
	}
	got := destinationClaims(t, cl)
	if _, ok := got["fd70::1"]; !ok {
		t.Fatal("held destination was released")
	}
	if _, ok := got["fd70::2"]; ok {
		t.Fatal("unused destination was not released")
	}
	if _, ok := got["fd70::3"]; !ok {
		t.Fatal("destination routed by a live binding was released")
	}
	if got["fd70::4"] != "project-b/context-c" {
		t.Fatal("another project's destination was released")
	}
	if err := allocator.ClaimDestination(ctx, "project-b", "context-d", "east", "fd70::2", 53); err != nil {
		t.Fatalf("released destination stayed burned: %v", err)
	}
}

func TestReleaseDestinationsWaitsForWithdrawnSnapshot(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	region, shard, ns := "east", "shared", "platform"
	binding := &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding", Namespace: ns, UID: "binding-uid", Labels: map[string]string{"internal-dns.miloapis.com/region": region, "internal-dns.miloapis.com/shard": shard}}, Spec: dnsv1alpha1.DNSResolverBindingSpec{Source: dnsv1alpha1.DNSResolverBindingSource{ProjectUID: "project-a", ResolverContextRef: dnsv1alpha1.DNSObjectReference{Name: "context-a", UID: "context-a"}}, Placement: dnsv1alpha1.DNSResolverBindingPlacement{Region: region, Shard: shard}, Configuration: dnsv1alpha1.DNSResolverBindingConfiguration{Generation: 1, Revision: 1, Query: dnsv1alpha1.DNSResolverBindingQuery{NodeAddress: "fd70::1", RegionalAddress: "fd54::1"}}, Version: dnsv1alpha1.DNSResolverAccessVersion{Epoch: 1, Sequence: 1}}}
	state, _ := json.Marshal(shardState{Holder: "planner", Epoch: 1, NextRevision: 1, LeaseUntil: now.Add(time.Minute)})
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "dns-shard-owner-" + model.OpaqueToken(region+"/"+shard), Namespace: ns, UID: "shard-owner-uid"}, Data: map[string]string{"state": string(state)}}
	cl := testClient(t, binding, owner)
	allocator := &Allocator{Client: cl, Namespace: ns}
	if err := allocator.ClaimDestination(ctx, "project-a", "context-a", region, "fd70::1", 53); err != nil {
		t.Fatal(err)
	}
	p := Planner{Client: cl, Allocator: allocator, Config: PlannerConfig{Namespace: ns, Region: region, Shard: shard, Identity: "planner", ClusterBackends: []model.Backend{{MemberID: "cluster-1", Address: "fd00::20", Port: 5300}}}, Now: func() time.Time { return now }}
	if err := p.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if snapshot := latestPlannerSnapshot(t, &p); len(snapshot.Bindings) != 1 {
		t.Fatalf("binding was not published: %#v", snapshot.Bindings)
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
	if err := allocator.ReleaseDestinations(ctx, "project-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := destinationClaims(t, cl)["fd70::1"]; !ok {
		t.Fatal("destination was released while the committed snapshot still routed it")
	}
	if err := p.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if snapshot := latestPlannerSnapshot(t, &p); len(snapshot.Bindings) != 0 {
		t.Fatalf("tombstone was not withdrawn: %#v", snapshot.Bindings)
	}
	if err := allocator.ReleaseDestinations(ctx, "project-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := destinationClaims(t, cl)["fd70::1"]; ok {
		t.Fatal("withdrawn destination was not released")
	}
}

func TestClaimDestinationLabelsEarlierClaimsForRelease(t *testing.T) {
	ctx := context.Background()
	cl := testClient(t)
	allocator := Allocator{Client: cl, Namespace: "platform"}
	if err := allocator.ClaimDestination(ctx, "project-a", "context-a", "east", "fd70::1", 53); err != nil {
		t.Fatal(err)
	}
	var claims corev1.ConfigMapList
	if err := cl.List(ctx, &claims, client.InNamespace("platform")); err != nil || len(claims.Items) != 1 {
		t.Fatalf("expected one claim: %v", err)
	}
	earlier := &claims.Items[0]
	delete(earlier.Labels, destinationProjectLabel)
	if err := cl.Update(ctx, earlier); err != nil {
		t.Fatal(err)
	}
	if err := allocator.ClaimDestination(ctx, "project-a", "context-a", "east", "fd70::1", 53); err != nil {
		t.Fatal(err)
	}
	if err := allocator.ReleaseDestinations(ctx, "project-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := destinationClaims(t, cl)["fd70::1"]; ok {
		t.Fatal("a claim made before project labels was never released")
	}
}

func TestRegionsWithTheirOwnPoolLeaseSeparately(t *testing.T) {
	ctx := context.Background()
	cl := fake.NewClientBuilder().Build()
	allocator := &Allocator{Client: cl, Namespace: "platform", ClusterPrefix: "fd70:200::/96",
		RegionalPrefixes: map[string]string{"west": "fd70:300::/96"}}

	shared, err := allocator.Allocate(ctx, "project", "vpc-1", "east")
	if err != nil {
		t.Fatal(err)
	}
	again, err := allocator.Allocate(ctx, "project", "vpc-1", "north")
	if err != nil {
		t.Fatal(err)
	}
	if again != shared {
		t.Fatalf("regions on the shared pool must keep one lease, got %s and %s", shared, again)
	}
	west, err := allocator.Allocate(ctx, "project", "vpc-1", "west")
	if err != nil {
		t.Fatal(err)
	}
	if !netip.MustParsePrefix("fd70:300::/96").Contains(netip.MustParseAddr(west)) {
		t.Fatalf("west address %s is outside its own pool", west)
	}
	if !netip.MustParsePrefix("fd70:200::/96").Contains(netip.MustParseAddr(shared)) {
		t.Fatalf("shared address %s is outside the shared pool", shared)
	}
	if west2, err := allocator.Allocate(ctx, "project", "vpc-1", "west"); err != nil || west2 != west {
		t.Fatalf("west lease must be stable, got %s, %v", west2, err)
	}

	err = allocator.ClaimDestination(ctx, "project", "vpc-1", "west", "fd70:300::53", 53)
	if !errors.Is(err, model.ErrDestinationRefused) {
		t.Fatalf("a destination inside a regional pool must be refused, got %v", err)
	}
}
