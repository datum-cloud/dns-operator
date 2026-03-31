// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"
	"time"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func newFullTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		dnsv1alpha1.AddToScheme,
		networkingv1alpha.AddToScheme,
		corev1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatalf("add scheme: %v", err)
		}
	}
	return s
}

// annotatingZoneStrategy mirrors annotatingStrategy but for DNSZone GVK.
type annotatingZoneStrategy struct {
	fakeStrategy
}

func (s annotatingZoneStrategy) ObjectMetaFromUpstreamObject(_ context.Context, obj metav1.Object) (metav1.ObjectMeta, error) {
	return metav1.ObjectMeta{
		Namespace: s.namespace,
		Name:      obj.GetName(),
		Annotations: map[string]string{
			"meta.datumapis.com/upstream-namespace": obj.GetNamespace(),
		},
	}, nil
}

func (s annotatingZoneStrategy) SetControllerReference(_ context.Context, owner, controlled metav1.Object, _ ...controllerutil.OwnerReferenceOption) error {
	annotations := controlled.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations["meta.datumapis.com/upstream-cluster-name"] = "cluster-test"
	annotations["meta.datumapis.com/upstream-group"] = "dns.networking.miloapis.com"
	annotations["meta.datumapis.com/upstream-kind"] = kindDNSZone
	annotations["meta.datumapis.com/upstream-name"] = owner.GetName()
	annotations["meta.datumapis.com/upstream-namespace"] = owner.GetNamespace()
	controlled.SetAnnotations(annotations)
	return nil
}

// ---------------------------------------------------------------------------
// DNSRecordSetReplicator.updateStatus idempotency tests
// ---------------------------------------------------------------------------

// TestRecordSetReplicator_UpdateStatus_NoPatchWhenEqual verifies that when
// upstream status already matches downstream, no status patch is issued.
// A spurious patch here re-enqueues the reconciler and starts a loop.
func TestRecordSetReplicator_UpdateStatus_NoPatchWhenEqual(t *testing.T) {
	t.Parallel()
	scheme := newFullTestScheme(t)

	now := metav1.NewTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	// Shared status that both upstream and downstream have.
	sharedStatus := dnsv1alpha1.DNSRecordSetStatus{
		Conditions: []metav1.Condition{
			{
				Type:               CondAccepted,
				Status:             metav1.ConditionTrue,
				Reason:             ReasonAccepted,
				Message:            "Accepted",
				ObservedGeneration: 1,
				LastTransitionTime: now,
			},
			{
				Type:               CondProgrammed,
				Status:             metav1.ConditionTrue,
				Reason:             ReasonProgrammed,
				Message:            "Programmed",
				ObservedGeneration: 1,
				LastTransitionTime: now,
			},
		},
		RecordSets: []dnsv1alpha1.RecordSetStatus{
			{
				Name: "www",
				Conditions: []metav1.Condition{
					{
						Type:               CondProgrammed,
						Status:             metav1.ConditionTrue,
						Reason:             ReasonProgrammed,
						Message:            "RecordProgrammed",
						ObservedGeneration: 1,
						LastTransitionTime: now,
					},
				},
			},
		},
	}

	upstream := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "rs-a",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone-a"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"}}},
		},
		Status: *sharedStatus.DeepCopy(),
	}

	upstreamBase := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.DNSRecordSet{}).
		WithObjects(upstream).
		Build()

	upstreamClient := &countingClient{Client: upstreamBase}

	r := &DNSRecordSetReplicator{}

	downstreamStatus := sharedStatus.DeepCopy()
	if err := r.updateStatus(context.Background(), upstreamClient, upstream, downstreamStatus); err != nil {
		t.Fatalf("updateStatus: %v", err)
	}

	if upstreamClient.statusPatchCount != 0 {
		t.Fatalf("expected 0 status patches when upstream already matches downstream, got %d", upstreamClient.statusPatchCount)
	}
}

// TestRecordSetReplicator_UpdateStatus_NilVsEmptyRecordSets verifies that
// nil and empty RecordSets slices are treated as equivalent, preventing a
// spurious patch that would trigger re-reconciliation.
func TestRecordSetReplicator_UpdateStatus_NilVsEmptyRecordSets(t *testing.T) {
	t.Parallel()
	scheme := newFullTestScheme(t)

	now := metav1.NewTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	conditions := []metav1.Condition{
		{
			Type:               CondAccepted,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonAccepted,
			Message:            "Accepted",
			ObservedGeneration: 1,
			LastTransitionTime: now,
		},
		{
			Type:               CondProgrammed,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonProgrammed,
			Message:            "Programmed",
			ObservedGeneration: 1,
			LastTransitionTime: now,
		},
	}

	// Upstream has nil RecordSets (from JSON round-trip where field was empty).
	upstream := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "rs-a",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone-a"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"}}},
		},
		Status: dnsv1alpha1.DNSRecordSetStatus{
			Conditions: conditions,
			RecordSets: nil, // nil after JSON round-trip
		},
	}

	upstreamBase := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.DNSRecordSet{}).
		WithObjects(upstream).
		Build()

	upstreamClient := &countingClient{Client: upstreamBase}
	r := &DNSRecordSetReplicator{}

	// Downstream has empty (non-nil) RecordSets.
	downstreamStatus := &dnsv1alpha1.DNSRecordSetStatus{
		Conditions: conditions,
		RecordSets: []dnsv1alpha1.RecordSetStatus{}, // empty but non-nil
	}

	if err := r.updateStatus(context.Background(), upstreamClient, upstream, downstreamStatus); err != nil {
		t.Fatalf("updateStatus: %v", err)
	}

	if upstreamClient.statusPatchCount != 0 {
		t.Fatalf("nil vs empty RecordSets caused a spurious status patch (got %d patches); "+
			"this triggers a re-reconcile loop", upstreamClient.statusPatchCount)
	}
}

// TestRecordSetReplicator_UpdateStatus_NilVsEmptyConditions verifies that
// nil and empty Conditions slices are treated as equivalent.
func TestRecordSetReplicator_UpdateStatus_NilVsEmptyConditions(t *testing.T) {
	t.Parallel()
	scheme := newFullTestScheme(t)

	upstream := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "rs-a",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone-a"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"}}},
		},
		Status: dnsv1alpha1.DNSRecordSetStatus{
			Conditions: nil, // nil
			RecordSets: nil,
		},
	}

	upstreamBase := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.DNSRecordSet{}).
		WithObjects(upstream).
		Build()

	upstreamClient := &countingClient{Client: upstreamBase}
	r := &DNSRecordSetReplicator{}

	downstreamStatus := &dnsv1alpha1.DNSRecordSetStatus{
		Conditions: []metav1.Condition{}, // empty but non-nil
		RecordSets: nil,
	}

	if err := r.updateStatus(context.Background(), upstreamClient, upstream, downstreamStatus); err != nil {
		t.Fatalf("updateStatus: %v", err)
	}

	if upstreamClient.statusPatchCount != 0 {
		t.Fatalf("nil vs empty Conditions caused a spurious status patch (got %d patches); "+
			"this triggers a re-reconcile loop", upstreamClient.statusPatchCount)
	}
}

// TestRecordSetReplicator_UpdateStatus_NilVsEmptyPerRecordConditions verifies
// that per-record-name RecordSetStatus.Conditions nil vs empty doesn't cause
// a spurious patch.
func TestRecordSetReplicator_UpdateStatus_NilVsEmptyPerRecordConditions(t *testing.T) {
	t.Parallel()
	scheme := newFullTestScheme(t)

	now := metav1.NewTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	topConditions := []metav1.Condition{
		{
			Type:               CondAccepted,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonAccepted,
			Message:            "Accepted",
			ObservedGeneration: 1,
			LastTransitionTime: now,
		},
	}

	// Upstream per-record conditions: nil (from JSON round-trip)
	upstream := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "rs-a",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone-a"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"}}},
		},
		Status: dnsv1alpha1.DNSRecordSetStatus{
			Conditions: topConditions,
			RecordSets: []dnsv1alpha1.RecordSetStatus{
				{Name: "www", Conditions: nil},
			},
		},
	}

	upstreamBase := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.DNSRecordSet{}).
		WithObjects(upstream).
		Build()

	upstreamClient := &countingClient{Client: upstreamBase}
	r := &DNSRecordSetReplicator{}

	// Downstream per-record conditions: empty slice
	downstreamStatus := &dnsv1alpha1.DNSRecordSetStatus{
		Conditions: topConditions,
		RecordSets: []dnsv1alpha1.RecordSetStatus{
			{Name: "www", Conditions: []metav1.Condition{}},
		},
	}

	if err := r.updateStatus(context.Background(), upstreamClient, upstream, downstreamStatus); err != nil {
		t.Fatalf("updateStatus: %v", err)
	}

	if upstreamClient.statusPatchCount != 0 {
		t.Fatalf("nil vs empty per-record Conditions caused a spurious status patch (got %d patches); "+
			"this triggers a re-reconcile loop", upstreamClient.statusPatchCount)
	}
}

// ---------------------------------------------------------------------------
// DNSZoneReplicator.updateStatus idempotency tests
// ---------------------------------------------------------------------------

// TestZoneReplicator_UpdateStatus_NoPatchWhenSteadyState verifies that
// calling updateStatus when the zone is already in a fully-converged state
// does not issue a status patch.
func TestZoneReplicator_UpdateStatus_NoPatchWhenSteadyState(t *testing.T) {
	t.Parallel()
	scheme := newFullTestScheme(t)

	now := metav1.NewTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "zone-a",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "pdns",
		},
		Status: dnsv1alpha1.DNSZoneStatus{
			Nameservers: []string{"ns1.example.com", "ns2.example.com"},
			RecordCount: 2,
			DomainRef: &dnsv1alpha1.DomainRef{
				Name: "example.com",
				Status: dnsv1alpha1.DomainRefStatus{
					Nameservers: []networkingv1alpha.Nameserver{
						{
							Hostname: "ns1.example.com",
							IPs:      []networkingv1alpha.NameserverIP{{Address: "192.0.2.5"}},
						},
						{
							Hostname: "ns2.example.com",
							IPs: []networkingv1alpha.NameserverIP{
								{Address: "192.0.2.10"},
								{Address: "192.0.2.20"},
							},
						},
					},
				},
			},
			Conditions: []metav1.Condition{
				{
					Type:               CondAccepted,
					Status:             metav1.ConditionTrue,
					Reason:             ReasonAccepted,
					Message:            "Nameservers retrieved from downstream",
					ObservedGeneration: 1,
					LastTransitionTime: now,
				},
				{
					Type:               CondProgrammed,
					Status:             metav1.ConditionTrue,
					Reason:             ReasonProgrammed,
					Message:            "Default records ensured",
					ObservedGeneration: 1,
					LastTransitionTime: now,
				},
			},
		},
	}

	domain := &networkingv1alpha.Domain{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "example.com",
			Namespace: "default",
		},
		Spec: networkingv1alpha.DomainSpec{
			DomainName: "example.com",
		},
		Status: networkingv1alpha.DomainStatus{
			Nameservers: []networkingv1alpha.Nameserver{
				{
					Hostname: "ns1.example.com",
					IPs:      []networkingv1alpha.NameserverIP{{Address: "192.0.2.5"}},
				},
				{
					Hostname: "ns2.example.com",
					IPs: []networkingv1alpha.NameserverIP{
						{Address: "192.0.2.10"},
						{Address: "192.0.2.20"},
					},
				},
			},
		},
	}

	soa := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "zone-a-soa", Namespace: "default"},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
			RecordType: dnsv1alpha1.RRTypeSOA,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "@", SOA: &dnsv1alpha1.SOARecordSpec{MName: "ns1.example.com", RName: "hostmaster.example.com."}}},
		},
	}
	ns := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "zone-a-ns", Namespace: "default"},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
			RecordType: dnsv1alpha1.RRTypeNS,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "@", NS: &dnsv1alpha1.NSRecordSpec{Content: "ns1.example.com"}}},
		},
	}

	downstreamZone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "shadow-zone-a", Namespace: "downstream"},
		Status:     dnsv1alpha1.DNSZoneStatus{Nameservers: []string{"ns1.example.com", "ns2.example.com"}},
	}

	upstreamBase := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.DNSZone{}).
		WithObjects(zone, domain, soa, ns).
		WithIndex(&dnsv1alpha1.DNSRecordSet{}, "spec.dnsZoneRef.name", func(obj client.Object) []string {
			rs := obj.(*dnsv1alpha1.DNSRecordSet)
			if rs.Spec.DNSZoneRef.Name == "" {
				return nil
			}
			return []string{rs.Spec.DNSZoneRef.Name}
		}).
		WithIndex(&networkingv1alpha.Domain{}, "spec.domainName", func(obj client.Object) []string {
			d := obj.(*networkingv1alpha.Domain)
			if d.Spec.DomainName == "" {
				return nil
			}
			return []string{d.Spec.DomainName}
		}).
		Build()

	upstreamClient := &countingClient{Client: upstreamBase}
	downstreamClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(downstreamZone).Build()

	r := &DNSZoneReplicator{DownstreamClient: downstreamClient}
	strategy := fakeStrategy{namespace: downstreamZone.Namespace, name: downstreamZone.Name, client: downstreamClient}

	if err := r.updateStatus(context.Background(), upstreamClient, strategy, zone); err != nil {
		t.Fatalf("updateStatus: %v", err)
	}

	if upstreamClient.statusPatchCount != 0 {
		t.Fatalf("expected 0 status patches in steady state, got %d", upstreamClient.statusPatchCount)
	}
}

// TestZoneReplicator_UpdateStatus_CalledTwiceNoPatch verifies that calling
// updateStatus twice in sequence (as the Reconcile method does) does not
// produce a patch on the second call when everything is already converged.
func TestZoneReplicator_UpdateStatus_CalledTwiceNoPatch(t *testing.T) {
	t.Parallel()
	scheme := newFullTestScheme(t)

	now := metav1.NewTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "zone-a",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "pdns",
		},
		Status: dnsv1alpha1.DNSZoneStatus{
			Nameservers: []string{"ns1.example.com", "ns2.example.com"},
			RecordCount: 2,
			DomainRef: &dnsv1alpha1.DomainRef{
				Name: "example.com",
				Status: dnsv1alpha1.DomainRefStatus{
					Nameservers: []networkingv1alpha.Nameserver{
						{Hostname: "ns1.example.com", IPs: []networkingv1alpha.NameserverIP{{Address: "192.0.2.5"}}},
						{Hostname: "ns2.example.com", IPs: []networkingv1alpha.NameserverIP{{Address: "192.0.2.10"}}},
					},
				},
			},
			Conditions: []metav1.Condition{
				{Type: CondAccepted, Status: metav1.ConditionTrue, Reason: ReasonAccepted, Message: "Nameservers retrieved from downstream", ObservedGeneration: 1, LastTransitionTime: now},
				{Type: CondProgrammed, Status: metav1.ConditionTrue, Reason: ReasonProgrammed, Message: "Default records ensured", ObservedGeneration: 1, LastTransitionTime: now},
			},
		},
	}

	domain := &networkingv1alpha.Domain{
		ObjectMeta: metav1.ObjectMeta{Name: "example.com", Namespace: "default"},
		Spec:       networkingv1alpha.DomainSpec{DomainName: "example.com"},
		Status: networkingv1alpha.DomainStatus{
			Nameservers: []networkingv1alpha.Nameserver{
				{Hostname: "ns1.example.com", IPs: []networkingv1alpha.NameserverIP{{Address: "192.0.2.5"}}},
				{Hostname: "ns2.example.com", IPs: []networkingv1alpha.NameserverIP{{Address: "192.0.2.10"}}},
			},
		},
	}

	soa := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "zone-a-soa", Namespace: "default"},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
			RecordType: dnsv1alpha1.RRTypeSOA,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "@", SOA: &dnsv1alpha1.SOARecordSpec{MName: "ns1.example.com", RName: "hostmaster.example.com."}}},
		},
	}
	ns := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "zone-a-ns", Namespace: "default"},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
			RecordType: dnsv1alpha1.RRTypeNS,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "@", NS: &dnsv1alpha1.NSRecordSpec{Content: "ns1.example.com"}}},
		},
	}

	downstreamZone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "shadow-zone-a", Namespace: "downstream"},
		Status:     dnsv1alpha1.DNSZoneStatus{Nameservers: []string{"ns1.example.com", "ns2.example.com"}},
	}

	upstreamBase := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.DNSZone{}).
		WithObjects(zone, domain, soa, ns).
		WithIndex(&dnsv1alpha1.DNSRecordSet{}, "spec.dnsZoneRef.name", func(obj client.Object) []string {
			rs := obj.(*dnsv1alpha1.DNSRecordSet)
			if rs.Spec.DNSZoneRef.Name == "" {
				return nil
			}
			return []string{rs.Spec.DNSZoneRef.Name}
		}).
		WithIndex(&networkingv1alpha.Domain{}, "spec.domainName", func(obj client.Object) []string {
			d := obj.(*networkingv1alpha.Domain)
			if d.Spec.DomainName == "" {
				return nil
			}
			return []string{d.Spec.DomainName}
		}).
		Build()

	upstreamClient := &countingClient{Client: upstreamBase}
	downstreamClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(downstreamZone).Build()

	r := &DNSZoneReplicator{DownstreamClient: downstreamClient}
	strategy := fakeStrategy{namespace: downstreamZone.Namespace, name: downstreamZone.Name, client: downstreamClient}

	// First call - should be no-op in steady state
	if err := r.updateStatus(context.Background(), upstreamClient, strategy, zone); err != nil {
		t.Fatalf("updateStatus call 1: %v", err)
	}
	// Second call - mirrors how Reconcile calls updateStatus twice
	if err := r.updateStatus(context.Background(), upstreamClient, strategy, zone); err != nil {
		t.Fatalf("updateStatus call 2: %v", err)
	}

	if upstreamClient.statusPatchCount != 0 {
		t.Fatalf("expected 0 status patches across 2 calls in steady state, got %d", upstreamClient.statusPatchCount)
	}
}

// TestZoneReplicator_UpdateStatus_DomainRefNilVsEmpty verifies that a zone
// with no DomainRef (nil) and a Domain with empty nameservers does not cause
// a spurious patch due to nil-vs-empty slice differences in DomainRefStatus.
func TestZoneReplicator_UpdateStatus_DomainRefNilVsEmpty(t *testing.T) {
	t.Parallel()
	scheme := newFullTestScheme(t)

	now := metav1.NewTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "zone-a",
			Namespace:  "default",
			Generation: 1,
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "pdns",
		},
		Status: dnsv1alpha1.DNSZoneStatus{
			Nameservers: []string{"ns1.example.com"},
			RecordCount: 2,
			DomainRef: &dnsv1alpha1.DomainRef{
				Name: "example.com",
				Status: dnsv1alpha1.DomainRefStatus{
					Nameservers: nil, // nil after JSON round-trip when domain has no nameservers
				},
			},
			Conditions: []metav1.Condition{
				{Type: CondAccepted, Status: metav1.ConditionTrue, Reason: ReasonAccepted, Message: "Nameservers retrieved from downstream", ObservedGeneration: 1, LastTransitionTime: now},
				{Type: CondProgrammed, Status: metav1.ConditionTrue, Reason: ReasonProgrammed, Message: "Default records ensured", ObservedGeneration: 1, LastTransitionTime: now},
			},
		},
	}

	// Domain exists but has empty (non-nil) nameservers.
	domain := &networkingv1alpha.Domain{
		ObjectMeta: metav1.ObjectMeta{Name: "example.com", Namespace: "default"},
		Spec:       networkingv1alpha.DomainSpec{DomainName: "example.com"},
		Status: networkingv1alpha.DomainStatus{
			Nameservers: []networkingv1alpha.Nameserver{}, // empty but non-nil
		},
	}

	soa := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "zone-a-soa", Namespace: "default"},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
			RecordType: dnsv1alpha1.RRTypeSOA,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "@", SOA: &dnsv1alpha1.SOARecordSpec{MName: "ns1.example.com", RName: "hostmaster.example.com."}}},
		},
	}
	ns := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "zone-a-ns", Namespace: "default"},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
			RecordType: dnsv1alpha1.RRTypeNS,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "@", NS: &dnsv1alpha1.NSRecordSpec{Content: "ns1.example.com"}}},
		},
	}

	downstreamZone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "shadow-zone-a", Namespace: "downstream"},
		Status:     dnsv1alpha1.DNSZoneStatus{Nameservers: []string{"ns1.example.com"}},
	}

	upstreamBase := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.DNSZone{}).
		WithObjects(zone, domain, soa, ns).
		WithIndex(&dnsv1alpha1.DNSRecordSet{}, "spec.dnsZoneRef.name", func(obj client.Object) []string {
			rs := obj.(*dnsv1alpha1.DNSRecordSet)
			if rs.Spec.DNSZoneRef.Name == "" {
				return nil
			}
			return []string{rs.Spec.DNSZoneRef.Name}
		}).
		WithIndex(&networkingv1alpha.Domain{}, "spec.domainName", func(obj client.Object) []string {
			d := obj.(*networkingv1alpha.Domain)
			if d.Spec.DomainName == "" {
				return nil
			}
			return []string{d.Spec.DomainName}
		}).
		Build()

	upstreamClient := &countingClient{Client: upstreamBase}
	downstreamClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(downstreamZone).Build()

	r := &DNSZoneReplicator{DownstreamClient: downstreamClient}
	strategy := fakeStrategy{namespace: downstreamZone.Namespace, name: downstreamZone.Name, client: downstreamClient}

	if err := r.updateStatus(context.Background(), upstreamClient, strategy, zone); err != nil {
		t.Fatalf("updateStatus: %v", err)
	}

	if upstreamClient.statusPatchCount != 0 {
		t.Fatalf("DomainRef nil-vs-empty nameservers caused a spurious status patch (got %d patches); "+
			"this triggers a re-reconcile loop", upstreamClient.statusPatchCount)
	}
}

// ---------------------------------------------------------------------------
// ensureDownstream* idempotency tests — these write to the downstream cluster
// and any spurious patch triggers the downstream watch → upstream re-enqueue.
// ---------------------------------------------------------------------------

// TestRecordSetReplicator_EnsureDownstream_NoPatchOnSecondCall verifies that
// calling ensureDownstreamRecordSet twice does NOT produce a patch on the
// second call. The first call creates/updates the shadow; the second call
// should be a no-op. A spurious patch triggers the downstream watch and
// re-enqueues upstream, creating a reconcile loop.
func TestRecordSetReplicator_EnsureDownstream_NoPatchOnSecondCall(t *testing.T) {
	t.Parallel()
	scheme := newFullTestScheme(t)

	upstream := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rs-a",
			Namespace: "default",
			UID:       "upstream-uid-1",
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone-a"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"}}},
		},
	}

	downstreamBase := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()
	downstreamClient := &trackingClient{Client: downstreamBase}

	strategy := annotatingStrategy{fakeStrategy{
		namespace: "ns-downstream",
		client:    downstreamClient,
	}}

	r := &DNSRecordSetReplicator{DownstreamClient: downstreamClient}

	// First call — creates the shadow.
	_, err := r.ensureDownstreamRecordSet(context.Background(), strategy, upstream)
	if err != nil {
		t.Fatalf("ensureDownstreamRecordSet (call 1): %v", err)
	}

	// Reset counters for second call.
	downstreamClient.createCount = 0
	downstreamClient.patchCount = 0

	// Second call — should be a no-op.
	res, err := r.ensureDownstreamRecordSet(context.Background(), strategy, upstream)
	if err != nil {
		t.Fatalf("ensureDownstreamRecordSet (call 2): %v", err)
	}

	if downstreamClient.patchCount != 0 {
		var afterShadow dnsv1alpha1.DNSRecordSet
		_ = downstreamClient.Get(context.Background(), client.ObjectKey{Namespace: "ns-downstream", Name: upstream.Name}, &afterShadow)
		t.Logf("annotations after call 2: %v", afterShadow.Annotations)
		t.Fatalf("ensureDownstreamRecordSet patched downstream on second call (result=%s, patches=%d); "+
			"this triggers the downstream watch and re-enqueues upstream", res, downstreamClient.patchCount)
	}
}

// TestZoneReplicator_EnsureDownstream_NoPatchOnSecondCall verifies that
// calling ensureDownstreamZone twice does NOT produce a patch on the second call.
func TestZoneReplicator_EnsureDownstream_NoPatchOnSecondCall(t *testing.T) {
	t.Parallel()
	scheme := newFullTestScheme(t)

	upstream := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "zone-a",
			Namespace: "default",
			UID:       "upstream-uid-1",
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "pdns",
		},
	}

	downstreamBase := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()
	downstreamClient := &trackingClient{Client: downstreamBase}

	// Use an annotating strategy for zone too.
	strategy := annotatingZoneStrategy{fakeStrategy{
		namespace: "ns-downstream",
		client:    downstreamClient,
	}}

	r := &DNSZoneReplicator{DownstreamClient: downstreamClient}

	// First call — creates the shadow.
	_, err := r.ensureDownstreamZone(context.Background(), strategy, upstream)
	if err != nil {
		t.Fatalf("ensureDownstreamZone (call 1): %v", err)
	}

	// Reset counters.
	downstreamClient.createCount = 0
	downstreamClient.patchCount = 0

	// Second call — should be a no-op.
	res, err := r.ensureDownstreamZone(context.Background(), strategy, upstream)
	if err != nil {
		t.Fatalf("ensureDownstreamZone (call 2): %v", err)
	}

	if downstreamClient.patchCount != 0 {
		t.Fatalf("ensureDownstreamZone patched downstream on second call (result=%s, patches=%d); "+
			"this triggers the downstream watch and re-enqueues upstream", res, downstreamClient.patchCount)
	}
}

// ---------------------------------------------------------------------------
// Full reconcile steady-state tests — count ALL writes (upstream + downstream)
// ---------------------------------------------------------------------------

// allTrackingClient wraps a client.Client to track every mutating operation.
type allTrackingClient struct {
	client.Client
	creates     int
	patches     int
	updates     int
	statusPatch int
}

func (c *allTrackingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.creates++
	return c.Client.Create(ctx, obj, opts...)
}

func (c *allTrackingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.patches++
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *allTrackingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.updates++
	return c.Client.Update(ctx, obj, opts...)
}

func (c *allTrackingClient) Status() client.StatusWriter {
	return &allTrackingStatusWriter{
		StatusWriter: c.Client.Status(),
		patchCount:   &c.statusPatch,
	}
}

func (c *allTrackingClient) totalWrites() int {
	return c.creates + c.patches + c.updates + c.statusPatch
}

type allTrackingStatusWriter struct {
	client.StatusWriter
	patchCount *int
}

func (w *allTrackingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	*w.patchCount++
	return w.StatusWriter.Patch(ctx, obj, patch, opts...)
}

// TestZoneReplicator_SteadyState_NoWrites verifies that a fully-converged
// DNSZone reconcile produces zero writes to either upstream or downstream.
// Any write triggers a watch event that re-enqueues the reconciler.
func TestZoneReplicator_SteadyState_NoWrites(t *testing.T) {
	t.Parallel()
	scheme := newFullTestScheme(t)

	now := metav1.NewTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "zone-a",
			Namespace:  "default",
			Generation: 1,
			Finalizers: []string{"dns.networking.miloapis.com/finalize-dnszone"},
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "pdns",
		},
		Status: dnsv1alpha1.DNSZoneStatus{
			Nameservers: []string{"ns1.example.com", "ns2.example.com"},
			RecordCount: 2,
			DomainRef: &dnsv1alpha1.DomainRef{
				Name: "example.com",
				Status: dnsv1alpha1.DomainRefStatus{
					Nameservers: []networkingv1alpha.Nameserver{
						{Hostname: "ns1.example.com", IPs: []networkingv1alpha.NameserverIP{{Address: "192.0.2.5"}}},
						{Hostname: "ns2.example.com", IPs: []networkingv1alpha.NameserverIP{{Address: "192.0.2.10"}}},
					},
				},
			},
			Conditions: []metav1.Condition{
				{Type: CondAccepted, Status: metav1.ConditionTrue, Reason: ReasonAccepted, Message: "Nameservers retrieved from downstream", ObservedGeneration: 1, LastTransitionTime: now},
				{Type: CondProgrammed, Status: metav1.ConditionTrue, Reason: ReasonProgrammed, Message: "Default records ensured", ObservedGeneration: 1, LastTransitionTime: now},
			},
		},
	}

	zoneClass := &dnsv1alpha1.DNSZoneClass{
		ObjectMeta: metav1.ObjectMeta{Name: "pdns"},
		Spec:       dnsv1alpha1.DNSZoneClassSpec{ControllerName: "powerdns"},
	}

	domain := &networkingv1alpha.Domain{
		ObjectMeta: metav1.ObjectMeta{Name: "example.com", Namespace: "default"},
		Spec:       networkingv1alpha.DomainSpec{DomainName: "example.com"},
		Status: networkingv1alpha.DomainStatus{
			Nameservers: []networkingv1alpha.Nameserver{
				{Hostname: "ns1.example.com", IPs: []networkingv1alpha.NameserverIP{{Address: "192.0.2.5"}}},
				{Hostname: "ns2.example.com", IPs: []networkingv1alpha.NameserverIP{{Address: "192.0.2.10"}}},
			},
		},
	}

	soa := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "zone-a-soa", Namespace: "default"},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
			RecordType: dnsv1alpha1.RRTypeSOA,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "@", SOA: &dnsv1alpha1.SOARecordSpec{MName: "ns1.example.com", RName: "hostmaster.example.com."}}},
		},
	}
	ns := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "zone-a-ns", Namespace: "default"},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
			RecordType: dnsv1alpha1.RRTypeNS,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "@", NS: &dnsv1alpha1.NSRecordSpec{Content: "ns1.example.com"}}},
		},
	}

	// Downstream zone shadow with matching annotations.
	downstreamZone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "zone-a",
			Namespace: "ns-downstream",
			Annotations: map[string]string{
				"meta.datumapis.com/upstream-namespace":    "default",
				"meta.datumapis.com/upstream-cluster-name": "cluster-test",
				"meta.datumapis.com/upstream-group":        "dns.networking.miloapis.com",
				"meta.datumapis.com/upstream-kind":         kindDNSZone,
				"meta.datumapis.com/upstream-name":         "zone-a",
			},
		},
		Spec:   zone.Spec,
		Status: dnsv1alpha1.DNSZoneStatus{Nameservers: []string{"ns1.example.com", "ns2.example.com"}},
	}

	// Downstream zone accounting ConfigMap.
	accountingCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "example.com", Namespace: "dns-zone-accounting"},
		Data:       map[string]string{"owner": "single/default/zone-a"},
	}
	accountingNS := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "dns-zone-accounting"},
	}

	upstreamBase := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.DNSZone{}).
		WithObjects(zone, zoneClass, domain, soa, ns).
		WithIndex(&dnsv1alpha1.DNSRecordSet{}, "spec.dnsZoneRef.name", func(obj client.Object) []string {
			rs := obj.(*dnsv1alpha1.DNSRecordSet)
			if rs.Spec.DNSZoneRef.Name == "" {
				return nil
			}
			return []string{rs.Spec.DNSZoneRef.Name}
		}).
		WithIndex(&dnsv1alpha1.DNSRecordSet{}, "spec.recordType", func(obj client.Object) []string {
			rs := obj.(*dnsv1alpha1.DNSRecordSet)
			return []string{string(rs.Spec.RecordType)}
		}).
		WithIndex(&networkingv1alpha.Domain{}, "spec.domainName", func(obj client.Object) []string {
			d := obj.(*networkingv1alpha.Domain)
			if d.Spec.DomainName == "" {
				return nil
			}
			return []string{d.Spec.DomainName}
		}).
		Build()
	upstreamClient := &allTrackingClient{Client: upstreamBase}

	downstreamBase := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(downstreamZone, accountingCM, accountingNS).
		Build()
	downstreamClient := &allTrackingClient{Client: downstreamBase}

	strategy := annotatingZoneStrategy{fakeStrategy{
		namespace: "ns-downstream",
		client:    downstreamClient,
	}}

	r := &DNSZoneReplicator{
		DownstreamClient:    downstreamClient,
		AccountingNamespace: "dns-zone-accounting",
	}

	// Simulate a steady-state reconcile:
	// 1. ensureZoneAccounting (read only)
	owned, err := r.ensureZoneAccounting(context.Background(), zone, "single/default/zone-a")
	if err != nil {
		t.Fatalf("ensureZoneAccounting: %v", err)
	}
	if !owned {
		t.Fatalf("expected zone to be owned")
	}

	// 2. ensureDownstreamZone (should be no-op)
	_, err = r.ensureDownstreamZone(context.Background(), strategy, zone)
	if err != nil {
		t.Fatalf("ensureDownstreamZone: %v", err)
	}

	// 3. ensureDomain (already exists)
	if err := r.ensureDomain(context.Background(), upstreamClient, zone); err != nil {
		t.Fatalf("ensureDomain: %v", err)
	}

	// 4. updateStatus (first call)
	if err := r.updateStatus(context.Background(), upstreamClient, strategy, zone); err != nil {
		t.Fatalf("updateStatus call 1: %v", err)
	}

	// 5. ensureSOARecordSet (already exists)
	if err := r.ensureSOARecordSet(context.Background(), upstreamClient, zone); err != nil {
		t.Fatalf("ensureSOARecordSet: %v", err)
	}

	// 6. ensureNSRecordSet (already exists)
	if err := r.ensureNSRecordSet(context.Background(), upstreamClient, zone); err != nil {
		t.Fatalf("ensureNSRecordSet: %v", err)
	}

	// 7. updateStatus (second call)
	if err := r.updateStatus(context.Background(), upstreamClient, strategy, zone); err != nil {
		t.Fatalf("updateStatus call 2: %v", err)
	}

	if upstreamClient.totalWrites() != 0 {
		t.Fatalf("steady-state zone reconcile produced %d upstream writes (creates=%d, patches=%d, updates=%d, statusPatches=%d); "+
			"each write triggers re-enqueue via watch",
			upstreamClient.totalWrites(), upstreamClient.creates, upstreamClient.patches, upstreamClient.updates, upstreamClient.statusPatch)
	}
	if downstreamClient.totalWrites() != 0 {
		t.Fatalf("steady-state zone reconcile produced %d downstream writes (creates=%d, patches=%d, updates=%d, statusPatches=%d); "+
			"each write triggers re-enqueue via downstream watch",
			downstreamClient.totalWrites(), downstreamClient.creates, downstreamClient.patches, downstreamClient.updates, downstreamClient.statusPatch)
	}
}

// ---------------------------------------------------------------------------
// Condition idempotency tests
// ---------------------------------------------------------------------------

// TestZoneReplicator_UpdateStatus_ConditionsAlreadySet verifies that
// re-setting the same conditions (Accepted=True, Programmed=True) with a
// fresh time.Now() does not cause SetStatusCondition to report a change.
func TestZoneReplicator_UpdateStatus_ConditionsAlreadySet(t *testing.T) {
	t.Parallel()

	// Directly test that SetStatusCondition returns false when re-applied.
	now := metav1.NewTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	conditions := []metav1.Condition{
		{
			Type:               CondAccepted,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonAccepted,
			Message:            "Nameservers retrieved from downstream",
			ObservedGeneration: 1,
			LastTransitionTime: now,
		},
	}

	// Re-apply with a DIFFERENT time but same Status/Reason/Message/Generation.
	changed := apimeta.SetStatusCondition(&conditions, metav1.Condition{
		Type:               CondAccepted,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonAccepted,
		Message:            "Nameservers retrieved from downstream",
		ObservedGeneration: 1,
		LastTransitionTime: metav1.NewTime(time.Now()), // different time
	})

	if changed {
		t.Fatal("SetStatusCondition reported changed=true when re-applying identical condition with different LastTransitionTime; " +
			"this means every reconcile loop will patch status and re-enqueue")
	}

	// Verify LastTransitionTime was NOT updated
	cond := apimeta.FindStatusCondition(conditions, CondAccepted)
	if !cond.LastTransitionTime.Equal(&now) {
		t.Fatalf("LastTransitionTime was mutated from %v to %v", now, cond.LastTransitionTime)
	}
}
