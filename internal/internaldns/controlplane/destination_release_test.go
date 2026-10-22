// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/platform"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestReplacedDestinationClaimIsReleased(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	contextObject := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "ctx", Namespace: "project", UID: "context-uid", Generation: 1}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer-uid"}}
	setCondition(&contextObject.Status.Conditions, "Ready", metav1.ConditionTrue, "Ready", "", 1, now)
	access := func(name, address string) *dnsv1alpha1.DNSResolverAccessBinding {
		return &dnsv1alpha1.DNSResolverAccessBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "project", UID: types.UID("uid-" + name), Generation: 1}, Spec: dnsv1alpha1.DNSResolverAccessBindingSpec{ContextRef: dnsv1alpha1.DNSObjectReference{Name: "ctx", UID: "context-uid"}, Region: "central", DestinationAddress: address}}
	}
	original := access("original", "fd70:100::10")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(contextObject, original).WithStatusSubresource(&dnsv1alpha1.DNSResolverContext{}, &dnsv1alpha1.DNSResolverAccessBinding{}, &dnsv1alpha1.DNSResolverBinding{}).Build()
	r := &Reconciler{Client: c, Scheme: s, Options: ReconcilerOptions{ProjectUID: "project-uid", PlatformNamespace: "platform", Region: "central", Shard: "shared", AddressAllocator: &platform.Allocator{Client: c, Namespace: "platform"}}}
	claims := func() map[string]bool {
		t.Helper()
		var list corev1.ConfigMapList
		if err := c.List(ctx, &list, client.InNamespace("platform")); err != nil {
			t.Fatal(err)
		}
		addresses := map[string]bool{}
		for _, cm := range list.Items {
			if strings.HasPrefix(cm.Name, "dns-destination-claim-") {
				addresses[cm.Data["address"]] = true
			}
		}
		return addresses
	}
	if err := r.reconcileContextBindings(ctx, "project", nil, map[string]*dnsv1alpha1.DNSZone{}, now); err != nil {
		t.Fatal(err)
	}
	if !claims()["fd70:100::10"] {
		t.Fatal("active destination was not claimed")
	}
	if err := r.reconcileContextBindings(ctx, "project", nil, map[string]*dnsv1alpha1.DNSZone{}, now); err != nil {
		t.Fatal(err)
	}
	if !claims()["fd70:100::10"] {
		t.Fatal("active destination was released")
	}
	if err := c.Delete(ctx, original); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, access("replacement", "fd70:100::20")); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileContextBindings(ctx, "project", nil, map[string]*dnsv1alpha1.DNSZone{}, now); err != nil {
		t.Fatal(err)
	}
	got := claims()
	if got["fd70:100::10"] || !got["fd70:100::20"] {
		t.Fatalf("replaced destination claims = %v", got)
	}
}
