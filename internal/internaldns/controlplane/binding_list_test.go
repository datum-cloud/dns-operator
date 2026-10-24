// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestProjectBindingsLabelLegacyBindingsThenListByLabel(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	binding := func(name, project string) *dnsv1alpha1.DNSResolverBinding {
		return &dnsv1alpha1.DNSResolverBinding{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "platform"},
			Spec:       dnsv1alpha1.DNSResolverBindingSpec{Source: dnsv1alpha1.DNSResolverBindingSource{ProjectUID: types.UID(project)}},
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(binding("own", "project-a"), binding("other", "project-b")).Build()
	r := &Reconciler{Client: c, Options: ReconcilerOptions{ProjectUID: "project-a", PlatformNamespace: "platform"}}

	var first dnsv1alpha1.DNSResolverBindingList
	if err := r.listProjectBindings(context.Background(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 {
		t.Fatalf("first list must see every binding to find unlabelled ones, got %d", len(first.Items))
	}
	var own dnsv1alpha1.DNSResolverBinding
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "platform", Name: "own"}, &own); err != nil {
		t.Fatal(err)
	}
	if own.Labels[referenceProjectLabel] != "project-a" {
		t.Fatalf("own binding was not labelled: %v", own.Labels)
	}

	var second dnsv1alpha1.DNSResolverBindingList
	if err := r.listProjectBindings(context.Background(), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Items) != 1 || second.Items[0].Name != "own" {
		t.Fatalf("labelled list returned %v", second.Items)
	}

	r.bindingsLabelledAt = time.Now().Add(-bindingRelabelInterval)
	var third dnsv1alpha1.DNSResolverBindingList
	if err := r.listProjectBindings(context.Background(), &third); err != nil {
		t.Fatal(err)
	}
	if len(third.Items) != 2 {
		t.Fatalf("relabel pass must list every binding again, got %d", len(third.Items))
	}
}
