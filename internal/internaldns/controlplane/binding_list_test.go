// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"fmt"
	"sort"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func projectBinding(name, project string, labelled bool) *dnsv1alpha1.DNSResolverBinding {
	b := &dnsv1alpha1.DNSResolverBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "platform"},
		Spec:       dnsv1alpha1.DNSResolverBindingSpec{Source: dnsv1alpha1.DNSResolverBindingSource{ProjectUID: types.UID(project)}},
	}
	if labelled {
		b.Labels = map[string]string{referenceProjectLabel: project}
	}
	return b
}

func bindingNames(list dnsv1alpha1.DNSResolverBindingList) []string {
	names := make([]string, 0, len(list.Items))
	for _, b := range list.Items {
		names = append(names, b.Name)
	}
	sort.Strings(names)
	return names
}

func TestProjectBindingsIncludeAndLabelUnlabelledOwnBindings(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		projectBinding("own-labelled", "project-a", true),
		projectBinding("own-legacy", "project-a", false),
		projectBinding("other-labelled", "project-b", true),
		projectBinding("other-legacy", "project-b", false),
	).Build()
	r := &Reconciler{Client: c, Options: ReconcilerOptions{ProjectUID: "project-a", PlatformNamespace: "platform"}}

	for pass := range 2 {
		var list dnsv1alpha1.DNSResolverBindingList
		if err := r.listProjectBindings(context.Background(), &list); err != nil {
			t.Fatal(err)
		}
		if got := bindingNames(list); fmt.Sprint(got) != "[own-labelled own-legacy]" {
			t.Fatalf("pass %d listed %v, want only this project's bindings", pass, got)
		}
	}
	for name, want := range map[string]string{"own-legacy": "project-a", "other-legacy": ""} {
		var b dnsv1alpha1.DNSResolverBinding
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: "platform", Name: name}, &b); err != nil {
			t.Fatal(err)
		}
		if got := b.Labels[referenceProjectLabel]; got != want {
			t.Fatalf("%s project label = %q, want %q", name, got, want)
		}
	}
}

func TestProjectBindingsSurviveAFailedRelabel(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(projectBinding("own-legacy", "project-a", false)).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
			return fmt.Errorf("patch refused")
		}}).Build()
	r := &Reconciler{Client: c, Options: ReconcilerOptions{ProjectUID: "project-a", PlatformNamespace: "platform"}}
	var list dnsv1alpha1.DNSResolverBindingList
	if err := r.listProjectBindings(context.Background(), &list); err != nil {
		t.Fatalf("a failed relabel must not fail the list: %v", err)
	}
	if got := bindingNames(list); fmt.Sprint(got) != "[own-legacy]" {
		t.Fatalf("listed %v, want the unlabelled binding", got)
	}
}
