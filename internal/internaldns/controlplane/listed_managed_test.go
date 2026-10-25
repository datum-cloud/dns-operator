// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"fmt"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestGetOrCreateUsesListedObjectsAndFlagsCreates(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	listedZone := dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "listed", Namespace: "project", UID: "zone-uid"}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "a.internal"}}
	gets := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		gets++
		if key.Name == "listed" {
			return fmt.Errorf("listed object must not be read again")
		}
		return cl.Get(ctx, key, obj, opts...)
	}}).Build()
	r := &Reconciler{Client: c}
	listed := newListedManagedObjects([]dnsv1alpha1.DNSZone{listedZone}, nil)
	ctx := withListedManagedObjects(context.Background(), listed)

	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "listed", Namespace: "project"}}
	if err := r.getOrCreate(ctx, zone); err != nil {
		t.Fatal(err)
	}
	if zone.UID != "zone-uid" || zone.Spec.DomainName != "a.internal" || gets != 0 || listed.changed {
		t.Fatalf("listed zone was not reused: %#v gets=%d changed=%v", zone, gets, listed.changed)
	}

	missing := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "missing", Namespace: "project"}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "b.internal"}}
	if err := r.getOrCreate(ctx, missing); err != nil {
		t.Fatal(err)
	}
	if missing.ResourceVersion == "" || !listed.changed {
		t.Fatalf("missing zone was not created and flagged: %#v changed=%v", missing, listed.changed)
	}
}
