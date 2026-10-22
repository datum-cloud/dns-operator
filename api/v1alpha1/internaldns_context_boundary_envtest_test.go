// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func validateResolverContextBoundaries(t *testing.T, cl client.Client) {
	t.Helper()
	for _, kind := range []string{"DNSZoneAssociation", "DNSNamingPolicy"} {
		t.Run(kind+"-context-boundary", func(t *testing.T) {
			newObject := func(name string) *unstructured.Unstructured {
				return &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": GroupVersion.String(), "kind": kind,
					"metadata": map[string]any{"name": name, "namespace": "validation"},
					"spec": map[string]any{
						"dnsZoneRef":      map[string]any{"name": "zone", "uid": "zone-uid"},
						"additionalNames": []any{},
					},
				}}
			}
			ctx := context.Background()
			for _, ref := range []map[string]any{nil, {"name": ""}, {"name": "", "uid": "context-uid"}} {
				o := newObject("context-required")
				if ref != nil {
					if err := unstructured.SetNestedMap(o.Object, ref, "spec", "resolverContextRef"); err != nil {
						t.Fatal(err)
					}
				}
				if err := unstructured.SetNestedMap(o.Object, map[string]any{"name": "network", "uid": "network-uid"}, "spec", "vpcRef"); err != nil {
					t.Fatal(err)
				}
				if err := cl.Create(ctx, o); !apierrors.IsInvalid(err) {
					t.Fatalf("expected rejection without a named DNS context, got %v", err)
				}
			}
			o := newObject("context-selected")
			if err := unstructured.SetNestedMap(o.Object, map[string]any{"name": "context", "uid": "context-uid"}, "spec", "resolverContextRef"); err != nil {
				t.Fatal(err)
			}
			if err := unstructured.SetNestedMap(o.Object, map[string]any{"name": "network", "uid": "network-uid"}, "spec", "vpcRef"); err != nil {
				t.Fatal(err)
			}
			if err := cl.Create(ctx, o); err != nil {
				t.Fatal(err)
			}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(o), o); err != nil {
				t.Fatal(err)
			}
			if _, found, err := unstructured.NestedMap(o.Object, "spec", "vpcRef"); err != nil || found {
				t.Fatalf("network reference was retained in the DNS API: found=%t err=%v", found, err)
			}
			if err := unstructured.SetNestedMap(o.Object, map[string]any{"name": "", "uid": "context-uid"}, "spec", "resolverContextRef"); err != nil {
				t.Fatal(err)
			}
			if err := cl.Update(ctx, o); !apierrors.IsInvalid(err) {
				t.Fatalf("expected rejection when clearing the DNS context, got %v", err)
			}
		})
	}
}
