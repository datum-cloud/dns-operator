// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/downstreamclient"
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

// TestEnsureDisplayAnnotations_SetsAnnotationsCorrectly verifies that
// ensureDisplayAnnotations correctly computes and sets display-name and
// display-value annotations on DNSRecordSet resources.
func TestEnsureDisplayAnnotations_SetsAnnotationsCorrectly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		rs               *dnsv1alpha1.DNSRecordSet
		zoneDomainName   string
		wantDisplayName  string
		wantDisplayValue string
		wantModified     bool
	}{
		{
			name: "A record without annotations sets display name and value",
			rs: &dnsv1alpha1.DNSRecordSet{
				ObjectMeta: metav1.ObjectMeta{Name: "www-record", Namespace: "default"},
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					DNSZoneRef: corev1.LocalObjectReference{Name: "my-zone"},
					RecordType: dnsv1alpha1.RRTypeA,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.10"}},
					},
				},
			},
			zoneDomainName:   "example.com",
			wantDisplayName:  "www.example.com",
			wantDisplayValue: "192.0.2.10",
			wantModified:     true,
		},
		{
			name: "CNAME record sets target as display value",
			rs: &dnsv1alpha1.DNSRecordSet{
				ObjectMeta: metav1.ObjectMeta{Name: "api-record", Namespace: "default"},
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					DNSZoneRef: corev1.LocalObjectReference{Name: "my-zone"},
					RecordType: dnsv1alpha1.RRTypeCNAME,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "api", CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "api.internal.example.com"}},
					},
				},
			},
			zoneDomainName:   "example.com",
			wantDisplayName:  "api.example.com",
			wantDisplayValue: "api.internal.example.com",
			wantModified:     true,
		},
		{
			name: "apex record uses zone domain as display name",
			rs: &dnsv1alpha1.DNSRecordSet{
				ObjectMeta: metav1.ObjectMeta{Name: "apex-record", Namespace: "default"},
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					DNSZoneRef: corev1.LocalObjectReference{Name: "my-zone"},
					RecordType: dnsv1alpha1.RRTypeA,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "@", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.1"}},
					},
				},
			},
			zoneDomainName:   "example.com",
			wantDisplayName:  "example.com",
			wantDisplayValue: "192.0.2.1",
			wantModified:     true,
		},
		{
			name: "already correct annotations returns false",
			rs: &dnsv1alpha1.DNSRecordSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "www-record",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationDisplayName:  "www.example.com",
						AnnotationDisplayValue: "192.0.2.10",
					},
				},
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					DNSZoneRef: corev1.LocalObjectReference{Name: "my-zone"},
					RecordType: dnsv1alpha1.RRTypeA,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.10"}},
					},
				},
			},
			zoneDomainName:   "example.com",
			wantDisplayName:  "www.example.com",
			wantDisplayValue: "192.0.2.10",
			wantModified:     false,
		},
		{
			name: "outdated annotations are updated",
			rs: &dnsv1alpha1.DNSRecordSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "www-record",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationDisplayName:  "www.example.com",
						AnnotationDisplayValue: "192.0.2.99", // old IP
					},
				},
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					DNSZoneRef: corev1.LocalObjectReference{Name: "my-zone"},
					RecordType: dnsv1alpha1.RRTypeA,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.10"}}, // new IP
					},
				},
			},
			zoneDomainName:   "example.com",
			wantDisplayName:  "www.example.com",
			wantDisplayValue: "192.0.2.10",
			wantModified:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rs := tt.rs.DeepCopy()
			modified := ensureDisplayAnnotations(rs, tt.zoneDomainName)

			if modified != tt.wantModified {
				t.Errorf("ensureDisplayAnnotations() modified = %v, want %v", modified, tt.wantModified)
			}
			if got := rs.Annotations[AnnotationDisplayName]; got != tt.wantDisplayName {
				t.Errorf("display-name = %q, want %q", got, tt.wantDisplayName)
			}
			if got := rs.Annotations[AnnotationDisplayValue]; got != tt.wantDisplayValue {
				t.Errorf("display-value = %q, want %q", got, tt.wantDisplayValue)
			}
		})
	}
}

// TestEnsureDisplayAnnotations_IdempotentAfterFirstCall verifies that
// calling ensureDisplayAnnotations twice in a row returns false on the
// second call (no modifications needed), which is critical for avoiding
// infinite reconciliation loops.
func TestEnsureDisplayAnnotations_IdempotentAfterFirstCall(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "www-record", Namespace: "default"},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "my-zone"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.10"}},
			},
		},
	}

	// First call should modify
	firstCall := ensureDisplayAnnotations(rs, "example.com")
	if !firstCall {
		t.Fatal("first call should return true (annotations were set)")
	}

	// Second call with same data should NOT modify
	secondCall := ensureDisplayAnnotations(rs, "example.com")
	if secondCall {
		t.Fatal("second call should return false (no changes needed)")
	}
}

// trackingClient wraps a client.Client to track Create/Patch operations.
// Used to verify that downstream operations occur even when annotations are updated.
type trackingClient struct {
	client.Client
	createCount int
	patchCount  int
}

func (c *trackingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.createCount++
	return c.Client.Create(ctx, obj, opts...)
}

func (c *trackingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patchCount++
	return c.Client.Patch(ctx, obj, patch, opts...)
}

// TestDNSRecordSetReplicator_EnsureDownstreamRecordSet_WorksWithAnnotationUpdates
// verifies that ensureDownstreamRecordSet works correctly when called after
// display annotations are updated. This test validates the components work
// together but does NOT test the Reconcile control flow directly.
//
// NOTE: A full integration test that calls Reconcile() would be needed to catch
// control flow bugs (like early returns). Such a test would require mocking the
// multicluster runtime manager. This test serves as a unit test for the
// individual components.
func TestDNSRecordSetReplicator_EnsureDownstreamRecordSet_CalledAfterAnnotationUpdate(t *testing.T) {
	t.Parallel()

	scheme := newTestScheme(t)

	// Create zone and recordset without display annotations
	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-zone",
			Namespace: "default",
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "pdns",
		},
	}

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "www-record",
			Namespace: "default",
			// NO annotations - this triggers the annotation update path
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.10"}},
			},
		},
	}

	upstreamClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.DNSRecordSet{}).
		WithObjects(zone, rs).
		Build()

	downstreamBase := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.DNSRecordSet{}).
		Build()
	downstreamClient := &trackingClient{Client: downstreamBase}

	strategy := fakeStrategy{
		namespace: "downstream",
		name:      "shadow-www-record",
		client:    downstreamClient,
	}

	r := &DNSRecordSetReplicator{
		DownstreamClient: downstreamClient,
	}

	// Simulate what happens in Reconcile:
	// 1. Fetch upstream recordset
	// 2. Check/set display annotations (this used to cause early return)
	// 3. Call ensureDownstreamRecordSet

	ctx := context.Background()

	// Re-fetch to get current state
	var upstream dnsv1alpha1.DNSRecordSet
	if err := upstreamClient.Get(ctx, client.ObjectKeyFromObject(rs), &upstream); err != nil {
		t.Fatalf("get upstream: %v", err)
	}

	// Step 1: Check annotations - should need update
	base := upstream.DeepCopy()
	needsAnnotations := ensureDisplayAnnotations(&upstream, zone.Spec.DomainName)
	if !needsAnnotations {
		t.Fatal("expected annotations to need update for new recordset")
	}

	// Step 2: Patch annotations (simulating what Reconcile does)
	if err := upstreamClient.Patch(ctx, &upstream, client.MergeFrom(base)); err != nil {
		t.Fatalf("patch annotations: %v", err)
	}

	// Step 3: THIS IS THE CRITICAL CHECK - ensure downstream is still created
	// In the buggy code, we would have returned early before reaching this point
	_, err := r.ensureDownstreamRecordSet(ctx, strategy, &upstream)
	if err != nil {
		t.Fatalf("ensureDownstreamRecordSet: %v", err)
	}

	// Verify downstream object was created
	if downstreamClient.createCount == 0 && downstreamClient.patchCount == 0 {
		t.Fatal("ensureDownstreamRecordSet did not create or patch downstream object")
	}

	// Verify annotations are set on upstream
	var updated dnsv1alpha1.DNSRecordSet
	if err := upstreamClient.Get(ctx, client.ObjectKeyFromObject(rs), &updated); err != nil {
		t.Fatalf("get updated: %v", err)
	}

	if updated.Annotations[AnnotationDisplayName] != "www.example.com" {
		t.Errorf("display-name = %q, want %q", updated.Annotations[AnnotationDisplayName], "www.example.com")
	}
	if updated.Annotations[AnnotationDisplayValue] != "192.0.2.10" {
		t.Errorf("display-value = %q, want %q", updated.Annotations[AnnotationDisplayValue], "192.0.2.10")
	}
}
