// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func addReferenceStateToScheme(scheme *runtime.Scheme) error {
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	return dnsv1alpha1.AddToScheme(scheme)
}

func seedReferenceState(t *testing.T, r *Reconciler, source client.Object, selected map[string]dnsv1alpha1.DNSObjectReference) {
	t.Helper()
	if _, valid, err := r.pinReferences(context.Background(), source, selected); err != nil || !valid {
		t.Fatalf("reference state: valid=%t err=%v", valid, err)
	}
}

func TestReferenceStateIsPrivateScopedAndCollected(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := addReferenceStateToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cl := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &Reconciler{Client: cl, Options: Options{ProjectUID: "project-a", PlatformNamespace: "platform"}}
	other := &Reconciler{Client: cl, Options: Options{ProjectUID: "project-b", PlatformNamespace: "platform"}}
	source := &dnsv1alpha1.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "project", UID: "registration-original"}}
	selected := map[string]dnsv1alpha1.DNSObjectReference{"zone": {Name: "private", UID: "zone-original", Generation: 1}}
	if _, valid, err := r.pinReferences(ctx, source, selected); err != nil || !valid {
		t.Fatalf("initial reference: valid=%t err=%v", valid, err)
	}
	if _, valid, err := other.pinReferences(ctx, source, selected); err != nil || !valid {
		t.Fatalf("other project reference: valid=%t err=%v", valid, err)
	}
	selected["zone"] = dnsv1alpha1.DNSObjectReference{Name: "private", UID: "zone-replacement", Generation: 1}
	pins, valid, err := r.pinReferences(ctx, source, selected)
	if err != nil || valid || pins["zone"].UID != "zone-original" {
		t.Fatalf("replacement transferred a resolved lifetime: pins=%v valid=%t err=%v", pins, valid, err)
	}
	var public corev1.ConfigMapList
	if err := cl.List(ctx, &public, client.InNamespace(source.Namespace)); err != nil || len(public.Items) != 0 {
		t.Fatalf("reference state appeared in the project: count=%d err=%v", len(public.Items), err)
	}
	if err := r.collectReferenceStates(ctx, nil); err != nil {
		t.Fatal(err)
	}
	var states corev1.ConfigMapList
	if err := cl.List(ctx, &states, client.InNamespace("platform")); err != nil || len(states.Items) != 1 || states.Items[0].Labels[referenceProjectLabel] != "project-b" {
		t.Fatalf("cleanup removed another project's state: states=%v err=%v", states.Items, err)
	}
	if err := cl.Create(ctx, source); err != nil {
		t.Fatal(err)
	}
	if err := other.collectReferenceStates(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := cl.List(ctx, &states, client.InNamespace("platform")); err != nil || len(states.Items) != 1 {
		t.Fatalf("collector removed a live source absent from the list snapshot: count=%d err=%v", len(states.Items), err)
	}
	if err := cl.Delete(ctx, source); err != nil {
		t.Fatal(err)
	}
	if err := other.collectReferenceStates(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := cl.List(ctx, &states, client.InNamespace("platform")); err != nil || len(states.Items) != 0 {
		t.Fatalf("deleted source retained private state: count=%d err=%v", len(states.Items), err)
	}
}
