// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/downstreamclient"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type annotatingStrategy struct {
	fakeStrategy
}

func (s annotatingStrategy) ObjectMetaFromUpstreamObject(_ context.Context, obj metav1.Object) (metav1.ObjectMeta, error) {
	return metav1.ObjectMeta{
		Namespace: s.namespace,
		Name:      obj.GetName(),
		Annotations: map[string]string{
			downstreamclient.UpstreamOwnerNamespaceAnnotation: obj.GetNamespace(),
		},
	}, nil
}

func (s annotatingStrategy) SetControllerReference(_ context.Context, owner, controlled metav1.Object, _ ...controllerutil.OwnerReferenceOption) error {
	annotations := controlled.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[downstreamclient.UpstreamOwnerClusterNameAnnotation] = "cluster-test"
	annotations[downstreamclient.UpstreamOwnerGroupAnnotation] = "dns.networking.miloapis.com"
	annotations[downstreamclient.UpstreamOwnerKindAnnotation] = "DNSRecordSet"
	annotations[downstreamclient.UpstreamOwnerNameAnnotation] = owner.GetName()
	annotations[downstreamclient.UpstreamOwnerNamespaceAnnotation] = owner.GetNamespace()
	controlled.SetAnnotations(annotations)
	return nil
}

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		dnsv1alpha1.AddToScheme,
		corev1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("add scheme: %v", err)
		}
	}
	return s
}

func TestEnsureDownstreamRecordSet_NewShadowGetsAnnotations(t *testing.T) {
	t.Parallel()
	scheme := newTestScheme(t)

	longName := "v4-2ed208a11f54412a92e8a2619eb662ea-prism-staging-env-datum-net-a-a59c082e"

	upstream := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      longName,
			Namespace: "default",
			UID:       "upstream-uid-1",
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone-a"},
			RecordType: dnsv1alpha1.RRTypeA,
		},
	}

	downstreamClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	strategy := annotatingStrategy{fakeStrategy{
		namespace: "ns-downstream",
		client:    downstreamClient,
	}}

	r := &DNSRecordSetReplicator{DownstreamClient: downstreamClient}

	res, err := r.ensureDownstreamRecordSet(context.Background(), strategy, upstream)
	if err != nil {
		t.Fatalf("ensureDownstreamRecordSet: %v", err)
	}
	if res != controllerutil.OperationResultCreated {
		t.Fatalf("expected Created, got %s", res)
	}

	var shadow dnsv1alpha1.DNSRecordSet
	if err := downstreamClient.Get(context.Background(), types.NamespacedName{
		Namespace: "ns-downstream",
		Name:      longName,
	}, &shadow); err != nil {
		t.Fatalf("get shadow: %v", err)
	}

	wantAnnotations := map[string]string{
		downstreamclient.UpstreamOwnerClusterNameAnnotation: "cluster-test",
		downstreamclient.UpstreamOwnerGroupAnnotation:       "dns.networking.miloapis.com",
		downstreamclient.UpstreamOwnerKindAnnotation:        "DNSRecordSet",
		downstreamclient.UpstreamOwnerNameAnnotation:        longName,
		downstreamclient.UpstreamOwnerNamespaceAnnotation:   "default",
	}
	for k, want := range wantAnnotations {
		if got := shadow.Annotations[k]; got != want {
			t.Errorf("annotation %s = %q, want %q", k, got, want)
		}
	}

	if len(shadow.Labels) != 0 {
		t.Errorf("expected no labels on new shadow, got %v", shadow.Labels)
	}
}

func TestEnsureDownstreamRecordSet_ExistingLabelsPreserved(t *testing.T) {
	t.Parallel()
	scheme := newTestScheme(t)

	upstream := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "short-name",
			Namespace: "default",
			UID:       "upstream-uid-2",
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone-a"},
			RecordType: dnsv1alpha1.RRTypeA,
		},
	}

	oldLabels := map[string]string{
		downstreamclient.UpstreamOwnerClusterNameAnnotation: "cluster-test",
		downstreamclient.UpstreamOwnerGroupAnnotation:       "dns.networking.miloapis.com",
		downstreamclient.UpstreamOwnerKindAnnotation:        "DNSRecordSet",
		downstreamclient.UpstreamOwnerNameAnnotation:        "short-name",
		downstreamclient.UpstreamOwnerNamespaceAnnotation:   "default",
	}

	existingShadow := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "short-name",
			Namespace: "ns-downstream",
			Labels:    oldLabels,
		},
		Spec: upstream.Spec,
	}

	downstreamClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(existingShadow).
		Build()

	strategy := annotatingStrategy{fakeStrategy{
		namespace: "ns-downstream",
		client:    downstreamClient,
	}}

	r := &DNSRecordSetReplicator{DownstreamClient: downstreamClient}

	_, err := r.ensureDownstreamRecordSet(context.Background(), strategy, upstream)
	if err != nil {
		t.Fatalf("ensureDownstreamRecordSet: %v", err)
	}

	var shadow dnsv1alpha1.DNSRecordSet
	if err := downstreamClient.Get(context.Background(), types.NamespacedName{
		Namespace: "ns-downstream",
		Name:      "short-name",
	}, &shadow); err != nil {
		t.Fatalf("get shadow: %v", err)
	}

	for k, want := range oldLabels {
		if got := shadow.Labels[k]; got != want {
			t.Errorf("old label %s = %q, want %q (should be preserved)", k, got, want)
		}
	}

	if shadow.Annotations[downstreamclient.UpstreamOwnerNameAnnotation] != "short-name" {
		t.Errorf("expected name annotation to be set, got %q",
			shadow.Annotations[downstreamclient.UpstreamOwnerNameAnnotation])
	}
}
