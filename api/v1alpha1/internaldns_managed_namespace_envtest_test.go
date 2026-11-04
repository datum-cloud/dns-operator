// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func validateManagedNamespaceSuffix(t *testing.T, cl client.Client) {
	t.Helper()
	ctx := context.Background()
	newContext := func(name string, managed DNSResolverContextManagedNamespace) *DNSResolverContext {
		return &DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "validation"}, Spec: DNSResolverContextSpec{ConsumerID: name, ManagedNamespace: managed}}
	}
	t.Run("managed-namespace-suffix", func(t *testing.T) {
		if err := cl.Create(ctx, newContext("enabled-without-suffix", DNSResolverContextManagedNamespace{Enabled: true})); !apierrors.IsInvalid(err) {
			t.Fatalf("enabling a managed namespace without a suffix must be rejected, got %v", err)
		}
		disabled := newContext("enable-later", DNSResolverContextManagedNamespace{})
		if err := cl.Create(ctx, disabled); err != nil {
			t.Fatal(err)
		}
		disabled.Spec.ManagedNamespace.Enabled = true
		if err := cl.Update(ctx, disabled); !apierrors.IsInvalid(err) {
			t.Fatalf("enabling a managed namespace later without a suffix must be rejected, got %v", err)
		}
		enabled := newContext("enabled", DNSResolverContextManagedNamespace{Enabled: true, DomainSuffix: "datum.internal"})
		if err := cl.Create(ctx, enabled); err != nil {
			t.Fatal(err)
		}
		enabled.Finalizers = []string{"example.com/hold"}
		if err := cl.Update(ctx, enabled); err != nil {
			t.Fatalf("an unchanged managed namespace must not block updates: %v", err)
		}
	})
}
