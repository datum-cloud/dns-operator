// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"fmt"
	"sync"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
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
			a, b, err := allocator.Allocate(ctx, "project", uid)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failures = append(failures, err)
				return
			}
			for _, address := range []string{a, b} {
				if previous, ok := addresses[address]; ok && previous != string(uid) {
					failures = append(failures, fmt.Errorf("cross-VPC address collision"))
				}
				addresses[address] = string(uid)
			}
		}(i)
	}
	wg.Wait()
	if len(failures) > 0 {
		t.Fatal(failures)
	}
	if len(addresses) != 80 {
		t.Fatalf("got %d address claims", len(addresses))
	}
	a, b, err := allocator.Allocate(ctx, "project", "vpc-1")
	if err != nil {
		t.Fatal(err)
	}
	a2, b2, err := (&Allocator{Client: cl, Namespace: "platform"}).Allocate(ctx, "project", "vpc-1")
	if err != nil || a != a2 || b != b2 {
		t.Fatal("allocator restart changed an existing lease")
	}
	replacement, _, err := allocator.Allocate(ctx, "project", "new-vpc-lifetime")
	if err != nil || replacement == a {
		t.Fatal("new resource lifetime reused the old address")
	}
}
