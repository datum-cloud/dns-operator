// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
			a, err := allocator.Allocate(ctx, "project", uid)
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
	a, err := allocator.Allocate(ctx, "project", "vpc-1")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := (&Allocator{Client: cl, Namespace: "platform"}).Allocate(ctx, "project", "vpc-1")
	if err != nil || a != a2 {
		t.Fatal("allocator restart changed an existing lease")
	}
	replacement, err := allocator.Allocate(ctx, "project", "new-vpc-lifetime")
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
		address, err := allocator.Allocate(ctx, "project-a", "context-a")
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
	address, err := allocator.Allocate(ctx, "project-a", "context-a")
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
