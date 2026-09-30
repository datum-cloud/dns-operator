// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"
	"time"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	testZoneMsgAccepted   = "Nameservers retrieved from downstream"
	testZoneMsgProgrammed = "Default records ensured"
)

var testConditionTime = metav1.NewTime(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))

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
	for k, v := range upstreamZoneAnnotations(owner.GetNamespace(), owner.GetName()) {
		annotations[k] = v
	}
	controlled.SetAnnotations(annotations)
	return nil
}

func upstreamZoneAnnotations(namespace, name string) map[string]string {
	return map[string]string{
		"meta.datumapis.com/upstream-namespace":    namespace,
		"meta.datumapis.com/upstream-cluster-name": "cluster-test",
		"meta.datumapis.com/upstream-group":        "dns.networking.miloapis.com",
		"meta.datumapis.com/upstream-kind":         "DNSZone",
		"meta.datumapis.com/upstream-name":         name,
	}
}

func testCondition(condType, reason, message string) metav1.Condition {
	return metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: 1,
		LastTransitionTime: testConditionTime,
	}
}

func testNameserver(hostname string, addresses ...string) networkingv1alpha.Nameserver {
	ns := networkingv1alpha.Nameserver{Hostname: hostname}
	for _, a := range addresses {
		ns.IPs = append(ns.IPs, networkingv1alpha.NameserverIP{Address: a})
	}
	return ns
}

func testRecordSet(opts ...func(*dnsv1alpha1.DNSRecordSet)) *dnsv1alpha1.DNSRecordSet {
	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs-a", Namespace: "default", Generation: 1},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone-a"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"}}},
		},
	}
	for _, o := range opts {
		o(rs)
	}
	return rs
}

func testZone(opts ...func(*dnsv1alpha1.DNSZone)) *dnsv1alpha1.DNSZone {
	z := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "zone-a", Namespace: "default", Generation: 1},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "example.com", DNSZoneClassName: "pdns"},
	}
	for _, o := range opts {
		o(z)
	}
	return z
}

func withConvergedZoneStatus(hostnames []string, domainNS []networkingv1alpha.Nameserver) func(*dnsv1alpha1.DNSZone) {
	return func(z *dnsv1alpha1.DNSZone) {
		z.Status = dnsv1alpha1.DNSZoneStatus{
			Nameservers: hostnames,
			RecordCount: 2,
			DomainRef: &dnsv1alpha1.DomainRef{
				Name:   z.Spec.DomainName,
				Status: dnsv1alpha1.DomainRefStatus{Nameservers: domainNS},
			},
			Conditions: []metav1.Condition{
				testCondition(CondAccepted, ReasonAccepted, testZoneMsgAccepted),
				testCondition(CondProgrammed, ReasonProgrammed, testZoneMsgProgrammed),
			},
		}
	}
}

func testDomain(nameservers []networkingv1alpha.Nameserver, conditions ...metav1.Condition) *networkingv1alpha.Domain {
	return &networkingv1alpha.Domain{
		ObjectMeta: metav1.ObjectMeta{Name: "example.com", Namespace: "default"},
		Spec:       networkingv1alpha.DomainSpec{DomainName: "example.com"},
		Status:     networkingv1alpha.DomainStatus{Nameservers: nameservers, Conditions: conditions},
	}
}

func testZoneDefaultRecordSets(zone *dnsv1alpha1.DNSZone) []client.Object {
	return []client.Object{
		&dnsv1alpha1.DNSRecordSet{
			ObjectMeta: metav1.ObjectMeta{Name: zone.Name + "-soa", Namespace: zone.Namespace},
			Spec: dnsv1alpha1.DNSRecordSetSpec{
				DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
				RecordType: dnsv1alpha1.RRTypeSOA,
				Records:    []dnsv1alpha1.RecordEntry{{Name: "@", SOA: &dnsv1alpha1.SOARecordSpec{MName: "ns1.example.com", RName: "hostmaster.example.com."}}},
			},
		},
		&dnsv1alpha1.DNSRecordSet{
			ObjectMeta: metav1.ObjectMeta{Name: zone.Name + "-ns", Namespace: zone.Namespace},
			Spec: dnsv1alpha1.DNSRecordSetSpec{
				DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
				RecordType: dnsv1alpha1.RRTypeNS,
				Records:    []dnsv1alpha1.RecordEntry{{Name: "@", NS: &dnsv1alpha1.NSRecordSpec{Content: "ns1.example.com"}}},
			},
		},
	}
}

func newZoneUpstreamClient(scheme *runtime.Scheme, objs ...client.Object) client.Client {
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.DNSZone{}).
		WithObjects(objs...).
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
}

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
	return &allTrackingStatusWriter{StatusWriter: c.Client.Status(), patchCount: &c.statusPatch}
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
