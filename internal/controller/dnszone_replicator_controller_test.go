// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"reflect"
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

type fakeStrategy struct {
	namespace string
	name      string
	client    client.Client
}

func (f fakeStrategy) GetClient() client.Client { return f.client }

func (f fakeStrategy) ObjectMetaFromUpstreamObject(context.Context, metav1.Object) (metav1.ObjectMeta, error) {
	return metav1.ObjectMeta{Namespace: f.namespace, Name: f.name}, nil
}

func (f fakeStrategy) SetControllerReference(context.Context, metav1.Object, metav1.Object, ...controllerutil.OwnerReferenceOption) error {
	return nil
}

func (f fakeStrategy) SetOwnerReference(context.Context, metav1.Object, metav1.Object, ...controllerutil.OwnerReferenceOption) error {
	return nil
}

func (f fakeStrategy) DeleteAnchorForObject(context.Context, client.Object) error {
	return nil
}

type countingStatusWriter struct {
	client.StatusWriter
	patchCount *int
}

func (w *countingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	*w.patchCount++
	return w.StatusWriter.Patch(ctx, obj, patch, opts...)
}

type countingClient struct {
	client.Client
	statusPatchCount int
}

func (c *countingClient) Status() client.StatusWriter {
	return &countingStatusWriter{
		StatusWriter: c.Client.Status(),
		patchCount:   &c.statusPatchCount,
	}
}

func TestDNSZoneReplicatorUpdateStatus_NoPatchForOrderOnlyChanges(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add dns scheme: %v", err)
	}
	if err := networkingv1alpha.AddToScheme(scheme); err != nil {
		t.Fatalf("add networking scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}

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
			Nameservers: []string{"ns2.example.com", "ns1.example.com"},
			RecordCount: 2,
			DomainRef: &dnsv1alpha1.DomainRef{
				Name: "example.com",
				Status: dnsv1alpha1.DomainRefStatus{
					Nameservers: []networkingv1alpha.Nameserver{
						{
							Hostname: "ns2.example.com",
							IPs: []networkingv1alpha.NameserverIP{
								{Address: "192.0.2.20"},
								{Address: "192.0.2.10"},
							},
						},
						{
							Hostname: "ns1.example.com",
							IPs: []networkingv1alpha.NameserverIP{
								{Address: "192.0.2.5"},
							},
						},
					},
				},
			},
		},
	}

	now := metav1.NewTime(time.Now())
	zone.Status.Conditions = []metav1.Condition{
		{
			Type:               CondAccepted,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonAccepted,
			Message:            "Nameservers retrieved from downstream",
			ObservedGeneration: zone.Generation,
			LastTransitionTime: now,
		},
		{
			Type:               CondProgrammed,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonProgrammed,
			Message:            "Default records ensured",
			ObservedGeneration: zone.Generation,
			LastTransitionTime: now,
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
					IPs: []networkingv1alpha.NameserverIP{
						{Address: "192.0.2.5"},
					},
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
		ObjectMeta: metav1.ObjectMeta{
			Name:      "soa-records",
			Namespace: "default",
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
			RecordType: dnsv1alpha1.RRTypeSOA,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "@", SOA: &dnsv1alpha1.SOARecordSpec{MName: "ns1.example.com"}},
			},
		},
	}
	ns := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ns-records",
			Namespace: "default",
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
			RecordType: dnsv1alpha1.RRTypeNS,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "@", NS: &dnsv1alpha1.NSRecordSpec{Content: "ns1.example.com"}},
			},
		},
	}

	downstreamZone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shadow-zone-a",
			Namespace: "downstream",
		},
		Status: dnsv1alpha1.DNSZoneStatus{
			Nameservers: []string{"ns1.example.com", "ns2.example.com"},
		},
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

	r := &DNSZoneReplicator{
		DownstreamClient: downstreamClient,
	}

	strategy := fakeStrategy{
		namespace: downstreamZone.Namespace,
		name:      downstreamZone.Name,
		client:    downstreamClient,
	}

	initialStatus := zone.Status.DeepCopy()
	if err := r.updateStatus(context.Background(), upstreamClient, strategy, zone); err != nil {
		t.Fatalf("updateStatus: %v", err)
	}

	if upstreamClient.statusPatchCount != 0 {
		t.Fatalf("expected no status patch, got %d", upstreamClient.statusPatchCount)
	}
	if !statusEqualIgnoringTransitionTime(zone.Status, *initialStatus) {
		t.Fatalf("expected in-memory status unchanged (ignoring transition time)")
	}

	var stored dnsv1alpha1.DNSZone
	if err := upstreamClient.Get(context.Background(), client.ObjectKeyFromObject(zone), &stored); err != nil {
		t.Fatalf("get stored zone: %v", err)
	}
	if !statusEqualIgnoringTransitionTime(stored.Status, *initialStatus) {
		t.Fatalf("expected stored status unchanged (ignoring transition time)")
	}
	if apimeta.FindStatusCondition(stored.Status.Conditions, CondAccepted) == nil {
		t.Fatalf("expected accepted condition to remain present")
	}
}

func statusEqualIgnoringTransitionTime(a, b dnsv1alpha1.DNSZoneStatus) bool {
	if !reflect.DeepEqual(a.Nameservers, b.Nameservers) {
		return false
	}
	if a.RecordCount != b.RecordCount {
		return false
	}
	if !reflect.DeepEqual(a.DomainRef, b.DomainRef) {
		return false
	}
	return conditionsEqualIgnoringTransitionTime(a.Conditions, b.Conditions)
}

func conditionsEqualIgnoringTransitionTime(a, b []metav1.Condition) bool {
	if len(a) != len(b) {
		return false
	}
	byType := make(map[string]metav1.Condition, len(b))
	for i := range b {
		byType[b[i].Type] = b[i]
	}
	for i := range a {
		other, ok := byType[a[i].Type]
		if !ok {
			return false
		}
		if a[i].Status != other.Status ||
			a[i].Reason != other.Reason ||
			a[i].Message != other.Message ||
			a[i].ObservedGeneration != other.ObservedGeneration {
			return false
		}
	}
	return true
}
