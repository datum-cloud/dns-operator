// SPDX-License-Identifier: AGPL-3.0-only

package privatereplication

import (
	"context"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func projectNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "ns-uid"}}
}

func replicator(hub ctrl.Client, kind string) *Replicator {
	return &Replicator{GVK: gvk(kind), Hub: hub, ProjectUID: func(context.Context, multicluster.ClusterName) (types.UID, error) { return "project-uid", nil }}
}

func TestReplicatorCopiesPrivateObjectsAndWithdrawsThem(t *testing.T) {
	ctx := context.Background()
	s := scheme(t)
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "internal", Namespace: "default", Generation: 3, Labels: map[string]string{"team": "a"}}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "internal.", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	project := fake.NewClientBuilder().WithScheme(s).WithObjects(projectNamespace(), zone).Build()
	hub := fake.NewClientBuilder().WithScheme(s).Build()
	r := replicator(hub, "DNSZone")
	key := types.NamespacedName{Namespace: "default", Name: "internal"}
	for range 2 {
		if err := r.replicate(ctx, project, "project-a", key); err != nil {
			t.Fatal(err)
		}
	}
	var ns corev1.Namespace
	if err := hub.Get(ctx, ctrl.ObjectKey{Name: "ns-ns-uid"}, &ns); err != nil {
		t.Fatal(err)
	}
	if ns.Annotations[UpstreamClusterAnnotation] != "project-a" || ns.Annotations[ProjectUIDAnnotation] != "project-uid" || ns.Annotations[UpstreamNamespaceAnnotation] != "default" || ns.Labels[ReplicatedLabel] != "true" {
		t.Fatalf("mapped namespace = %#v", ns.ObjectMeta)
	}
	var copied dnsv1alpha1.DNSZone
	if err := hub.Get(ctx, ctrl.ObjectKey{Namespace: "ns-ns-uid", Name: "internal"}, &copied); err != nil {
		t.Fatal(err)
	}
	if copied.Spec.DomainName != "internal." || copied.Labels[OriginLabel] != OriginProject || copied.Labels["team"] != "a" || copied.Annotations[OriginGenerationAnnotation] != "3" {
		t.Fatalf("hub copy = %#v", copied)
	}

	if err := project.Delete(ctx, zone); err != nil {
		t.Fatal(err)
	}
	if err := r.replicate(ctx, project, "project-a", key); err != nil {
		t.Fatal(err)
	}
	if err := hub.Get(ctx, ctrl.ObjectKey{Namespace: "ns-ns-uid", Name: "internal"}, &copied); !apierrors.IsNotFound(err) {
		t.Fatalf("hub copy outlived the project object: %v", err)
	}
	if err := project.Get(ctx, key, zone); !apierrors.IsNotFound(err) {
		t.Fatalf("project object kept its finalizer after withdrawal: %v", err)
	}
}

func TestReplicatorSkipsPublicZonesAndRegionWrites(t *testing.T) {
	ctx := context.Background()
	s := scheme(t)
	public := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "public", Namespace: "default"}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "example.com.", Visibility: dnsv1alpha1.DNSZoneVisibilityPublic}}
	fromRegion := &dnsv1alpha1.DNSRecordContribution{ObjectMeta: metav1.ObjectMeta{Name: "instance", Namespace: "default", Labels: map[string]string{OriginLabel: "us-east-1"}}}
	project := fake.NewClientBuilder().WithScheme(s).WithObjects(projectNamespace(), public, fromRegion).Build()
	hub := fake.NewClientBuilder().WithScheme(s).Build()
	if err := replicator(hub, "DNSZone").replicate(ctx, project, "p", types.NamespacedName{Namespace: "default", Name: "public"}); err != nil {
		t.Fatal(err)
	}
	if err := replicator(hub, "DNSRecordContribution").replicate(ctx, project, "p", types.NamespacedName{Namespace: "default", Name: "instance"}); err != nil {
		t.Fatal(err)
	}
	var zones dnsv1alpha1.DNSZoneList
	var contributions dnsv1alpha1.DNSRecordContributionList
	if err := hub.List(ctx, &zones); err != nil {
		t.Fatal(err)
	}
	if err := hub.List(ctx, &contributions); err != nil {
		t.Fatal(err)
	}
	if len(zones.Items) != 0 || len(contributions.Items) != 0 {
		t.Fatalf("copied a public zone or a region's own write: %d zones, %d contributions", len(zones.Items), len(contributions.Items))
	}
}

func mappedNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns-ns-uid", Annotations: map[string]string{UpstreamClusterAnnotation: "p", UpstreamNamespaceAnnotation: "default"}}}
}

func TestStatusMirrorMergesEveryRegionsEntry(t *testing.T) {
	ctx := context.Background()
	s := scheme(t)
	reg := &dnsv1alpha1.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default", Generation: 2}}
	project := fake.NewClientBuilder().WithScheme(s).WithObjects(reg).WithStatusSubresource(reg).Build()
	rb := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"resource": map[string]any{"apiVersion": "dns.networking.miloapis.com/v1alpha1", "kind": "DNSRegistration", "name": "web", "namespace": "ns-ns-uid"}},
		"status": map[string]any{"aggregatedStatus": []any{
			map[string]any{"clusterName": "lab-central", "status": map[string]any{"regions": []any{map[string]any{"region": "us-central-1", "observedGeneration": int64(2), "published": true}}}},
			map[string]any{"clusterName": "lab-east", "status": map[string]any{"regions": []any{map[string]any{"region": "us-east-1", "observedGeneration": int64(2), "published": false}}}},
		}},
	}}
	rb.SetGroupVersionKind(ResourceBindingGVK)
	rb.SetNamespace("ns-ns-uid")
	rb.SetName("web-dnsregistration")
	hub := fake.NewClientBuilder().WithScheme(s).WithObjects(mappedNamespace(), rb).Build()
	m := &StatusMirror{Hub: hub}
	resolve := func(string) (ctrl.Client, string, error) { return project, "default", nil }
	if err := m.mirror(ctx, types.NamespacedName{Namespace: "ns-ns-uid", Name: "web-dnsregistration"}, resolve); err != nil {
		t.Fatal(err)
	}
	if err := project.Get(ctx, ctrl.ObjectKeyFromObject(reg), reg); err != nil {
		t.Fatal(err)
	}
	if len(reg.Status.Regions) != 2 {
		t.Fatalf("regions = %#v", reg.Status.Regions)
	}
	c := apimeta.FindStatusCondition(reg.Status.Conditions, "Published")
	if c == nil || c.Status != metav1.ConditionFalse || c.Message != "publication awaits required replica verification in us-east-1" {
		t.Fatalf("Published = %#v", c)
	}
}

func TestObjectMirrorRecordsRegionWritesInTheProject(t *testing.T) {
	ctx := context.Background()
	s := scheme(t)
	written := &dnsv1alpha1.DNSRecordContribution{ObjectMeta: metav1.ObjectMeta{Name: "instance", Namespace: "ns-ns-uid", Labels: map[string]string{OriginLabel: "us-east-1", ReplicatedLabel: "true"}}, Spec: dnsv1alpha1.DNSRecordContributionSpec{RegistrationRef: dnsv1alpha1.DNSObjectReference{Name: "web"}}}
	hub := fake.NewClientBuilder().WithScheme(s).WithObjects(mappedNamespace(), written).Build()
	project := fake.NewClientBuilder().WithScheme(s).Build()
	m := &ObjectMirror{GVK: gvk("DNSRecordContribution"), Hub: hub}
	resolve := func(string) (ctrl.Client, string, error) { return project, "default", nil }
	key := types.NamespacedName{Namespace: "ns-ns-uid", Name: "instance"}
	for range 2 {
		if err := m.mirror(ctx, key, resolve); err != nil {
			t.Fatal(err)
		}
	}
	var up dnsv1alpha1.DNSRecordContribution
	if err := project.Get(ctx, ctrl.ObjectKey{Namespace: "default", Name: "instance"}, &up); err != nil {
		t.Fatal(err)
	}
	if up.Labels[OriginLabel] != "us-east-1" || up.Spec.RegistrationRef.Name != "web" || up.Labels[ReplicatedLabel] != "" {
		t.Fatalf("project copy = %#v", up)
	}

	if err := hub.Delete(ctx, written); err != nil {
		t.Fatal(err)
	}
	if err := m.mirror(ctx, key, resolve); err != nil {
		t.Fatal(err)
	}
	if err := project.Get(ctx, ctrl.ObjectKey{Namespace: "default", Name: "instance"}, &up); !apierrors.IsNotFound(err) {
		t.Fatalf("project copy outlived the region's delete: %v", err)
	}
}

func TestObjectMirrorNeverOverwritesAnotherOrigin(t *testing.T) {
	ctx := context.Background()
	s := scheme(t)
	written := &dnsv1alpha1.DNSRecordContribution{ObjectMeta: metav1.ObjectMeta{Name: "instance", Namespace: "ns-ns-uid", Labels: map[string]string{OriginLabel: "us-east-1"}}}
	userOwned := &dnsv1alpha1.DNSRecordContribution{ObjectMeta: metav1.ObjectMeta{Name: "instance", Namespace: "default"}}
	hub := fake.NewClientBuilder().WithScheme(s).WithObjects(mappedNamespace(), written).Build()
	project := fake.NewClientBuilder().WithScheme(s).WithObjects(userOwned).Build()
	m := &ObjectMirror{GVK: gvk("DNSRecordContribution"), Hub: hub}
	if err := m.mirror(ctx, types.NamespacedName{Namespace: "ns-ns-uid", Name: "instance"}, func(string) (ctrl.Client, string, error) { return project, "default", nil }); err == nil {
		t.Fatal("a region's write replaced an object the project owns")
	}
}
