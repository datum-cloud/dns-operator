package controller

import (
	"context"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/dns"
	dnsfake "go.miloapis.com/dns-operator/internal/dns/fake"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

type recordSetDeleteCall struct {
	ZoneName   string
	DomainName string
	RecordSet  string
}

type recordSetDNSClientSpy struct {
	*dnsfake.FakeDNSClient
	EnsureRecordSetCalls int
	DeleteRecordSetCalls []recordSetDeleteCall
}

func (s *recordSetDNSClientSpy) EnsureRecordSet(ctx context.Context, zone dnsv1alpha1.DNSZone, recordSet dnsv1alpha1.DNSRecordSet) ([]dnsv1alpha1.RecordSetStatus, error) {
	s.EnsureRecordSetCalls++
	return nil, nil
}

func (s *recordSetDNSClientSpy) DeleteRecordSet(ctx context.Context, zone dnsv1alpha1.DNSZone, recordSet dnsv1alpha1.DNSRecordSet) error {
	s.DeleteRecordSetCalls = append(s.DeleteRecordSetCalls, recordSetDeleteCall{
		ZoneName:   zone.Name,
		DomainName: zone.Spec.DomainName,
		RecordSet:  recordSet.Name,
	})
	return nil
}

func newDownstreamRecordSetTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add dns api to scheme: %v", err)
	}

	return s
}

func newDownstreamRecordSetReconciler(t *testing.T, objs ...client.Object) (*DNSRecordSetReconciler, *recordSetDNSClientSpy, client.Client) {
	t.Helper()

	scheme := newDownstreamRecordSetTestScheme(t)
	k8sClient := ctrlfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&dnsv1alpha1.DNSRecordSet{}).
		Build()

	spy := &recordSetDNSClientSpy{FakeDNSClient: dnsfake.NewFakeDNSClient()}
	r := &DNSRecordSetReconciler{
		Client: k8sClient,
		Scheme: scheme,
		DNSHandler: &dns.DNSHandler{
			Client: &dns.DNSClient{
				Name:          "downstream-class",
				Type:          "fake",
				DNSController: spy,
			},
		},
	}

	return r, spy, k8sClient
}

func TestDNSRecordSetReconcile_AddsFinalizerOwnerReferenceAndPrograms(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "zone-a", Namespace: "default", UID: types.UID("zone-uid")},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "downstream-class",
		},
	}
	zoneClass := &dnsv1alpha1.DNSZoneClass{
		ObjectMeta: metav1.ObjectMeta{Name: "downstream-class"},
		Spec:       dnsv1alpha1.DNSZoneClassSpec{ControllerName: "fake"},
	}
	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "record-a", Namespace: "default"},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone-a"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{{
				Name: "www",
				A:    &dnsv1alpha1.ARecordSpec{Content: "192.0.2.10"},
			}},
		},
	}

	r, spy, k8sClient := newDownstreamRecordSetReconciler(t, zone, zoneClass, rs)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "record-a"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	var afterFirst dnsv1alpha1.DNSRecordSet
	if err := k8sClient.Get(context.Background(), req.NamespacedName, &afterFirst); err != nil {
		t.Fatalf("get after first reconcile: %v", err)
	}
	if len(afterFirst.Finalizers) != 1 || afterFirst.Finalizers[0] != downstreamRSFinalizer {
		t.Fatalf("expected finalizer %q after first reconcile, got %v", downstreamRSFinalizer, afterFirst.Finalizers)
	}
	if len(afterFirst.OwnerReferences) != 0 {
		t.Fatalf("expected no owner references after first reconcile, got %v", afterFirst.OwnerReferences)
	}
	if spy.EnsureRecordSetCalls != 0 {
		t.Fatalf("expected EnsureRecordSet not to run before owner reference is established, got %d", spy.EnsureRecordSetCalls)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	var afterSecond dnsv1alpha1.DNSRecordSet
	if err := k8sClient.Get(context.Background(), req.NamespacedName, &afterSecond); err != nil {
		t.Fatalf("get after second reconcile: %v", err)
	}
	if len(afterSecond.OwnerReferences) != 1 {
		t.Fatalf("expected a single owner reference after second reconcile, got %v", afterSecond.OwnerReferences)
	}
	if afterSecond.OwnerReferences[0].UID != zone.UID {
		t.Fatalf("expected owner reference UID %q, got %q", zone.UID, afterSecond.OwnerReferences[0].UID)
	}
	if afterSecond.OwnerReferences[0].Controller != nil && *afterSecond.OwnerReferences[0].Controller {
		t.Fatalf("expected owner reference to be non-controller, got %v", afterSecond.OwnerReferences[0])
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("third reconcile: %v", err)
	}

	var afterThird dnsv1alpha1.DNSRecordSet
	if err := k8sClient.Get(context.Background(), req.NamespacedName, &afterThird); err != nil {
		t.Fatalf("get after third reconcile: %v", err)
	}
	accepted := apimeta.FindStatusCondition(afterThird.Status.Conditions, CondAccepted)
	if accepted == nil {
		t.Fatal("expected Accepted condition after third reconcile")
	}
	if accepted.Status != metav1.ConditionTrue || accepted.Reason != ReasonAccepted {
		t.Fatalf("expected Accepted=True with reason %q, got status=%q reason=%q", ReasonAccepted, accepted.Status, accepted.Reason)
	}
	if spy.EnsureRecordSetCalls != 0 {
		t.Fatalf("expected EnsureRecordSet to wait until Accepted is persisted, got %d", spy.EnsureRecordSetCalls)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("fourth reconcile: %v", err)
	}

	if spy.EnsureRecordSetCalls != 1 {
		t.Fatalf("expected EnsureRecordSet to run once, got %d", spy.EnsureRecordSetCalls)
	}

	var afterFourth dnsv1alpha1.DNSRecordSet
	if err := k8sClient.Get(context.Background(), req.NamespacedName, &afterFourth); err != nil {
		t.Fatalf("get after fourth reconcile: %v", err)
	}
	programmed := apimeta.FindStatusCondition(afterFourth.Status.Conditions, CondProgrammed)
	if programmed == nil {
		t.Fatal("expected Programmed condition after fourth reconcile")
	}
	if programmed.Status != metav1.ConditionTrue || programmed.Reason != ReasonProgrammed {
		t.Fatalf("expected Programmed=True with reason %q, got status=%q reason=%q", ReasonProgrammed, programmed.Status, programmed.Reason)
	}
}

func TestDNSRecordSetReconcile_DeleteFlowCallsDeleteAndRemovesFinalizer(t *testing.T) {
	t.Parallel()

	now := metav1.NewTime(time.Now())
	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "zone-a", Namespace: "default", UID: types.UID("zone-uid")},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "downstream-class",
		},
	}
	zoneClass := &dnsv1alpha1.DNSZoneClass{
		ObjectMeta: metav1.ObjectMeta{Name: "downstream-class"},
		Spec:       dnsv1alpha1.DNSZoneClassSpec{ControllerName: "fake"},
	}
	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "record-a",
			Namespace:         "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{downstreamRSFinalizer},
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone-a"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{{
				Name: "www",
				A:    &dnsv1alpha1.ARecordSpec{Content: "192.0.2.10"},
			}},
		},
	}

	r, spy, k8sClient := newDownstreamRecordSetReconciler(t, zone, zoneClass, rs)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "record-a"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(spy.DeleteRecordSetCalls) != 1 {
		t.Fatalf("expected DeleteRecordSet to be called once, got %d", len(spy.DeleteRecordSetCalls))
	}
	if spy.DeleteRecordSetCalls[0].ZoneName != zone.Name {
		t.Fatalf("expected DeleteRecordSet to target zone %q, got %q", zone.Name, spy.DeleteRecordSetCalls[0].ZoneName)
	}
	if spy.DeleteRecordSetCalls[0].RecordSet != rs.Name {
		t.Fatalf("expected DeleteRecordSet for recordset %q, got %q", rs.Name, spy.DeleteRecordSetCalls[0].RecordSet)
	}

	var stored dnsv1alpha1.DNSRecordSet
	err := k8sClient.Get(context.Background(), req.NamespacedName, &stored)
	if err == nil {
		if containsStringRS(stored.Finalizers, downstreamRSFinalizer) {
			t.Fatalf("expected finalizer to be removed, finalizers=%v", stored.Finalizers)
		}
	} else if !apierrors.IsNotFound(err) {
		t.Fatalf("get recordset after delete reconcile: %v", err)
	}
}

func containsStringRS(items []string, want string) bool {
	for i := range items {
		if items[i] == want {
			return true
		}
	}
	return false
}

// Every status write comes back as an update event. Reprogramming a record set
// that is already programmed at its current generation turns those writes into a
// loop, which for a record set holding thousands of names means the provider is
// never idle.
func TestDNSRecordSetReconcile_SkipsWhenAlreadyProgrammedAtGeneration(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "zone-a", Namespace: "default", UID: types.UID("zone-uid")},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "downstream-class",
		},
	}
	zoneClass := &dnsv1alpha1.DNSZoneClass{
		ObjectMeta: metav1.ObjectMeta{Name: "downstream-class"},
		Spec:       dnsv1alpha1.DNSZoneClassSpec{ControllerName: "fake"},
	}
	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "record-a", Namespace: "default"},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone-a"},
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{{
				Name: "www",
				A:    &dnsv1alpha1.ARecordSpec{Content: "192.0.2.10"},
			}},
		},
	}

	r, spy, k8sClient := newDownstreamRecordSetReconciler(t, zone, zoneClass, rs)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "record-a"}}

	// Finalizer, owner reference, Accepted, then the programming reconcile.
	for i := 0; i < 4; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile %d: %v", i+1, err)
		}
	}
	if spy.EnsureRecordSetCalls != 1 {
		t.Fatalf("expected EnsureRecordSet to run once while programming, got %d", spy.EnsureRecordSetCalls)
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile after programming: %v", err)
	}
	if spy.EnsureRecordSetCalls != 1 {
		t.Fatalf("expected no reprogramming at an unchanged generation, got %d calls", spy.EnsureRecordSetCalls)
	}

	// A spec change must still be programmed.
	var current dnsv1alpha1.DNSRecordSet
	if err := k8sClient.Get(context.Background(), req.NamespacedName, &current); err != nil {
		t.Fatalf("get record set: %v", err)
	}
	current.Spec.Records[0].A.Content = "192.0.2.11"
	// The fake client does not maintain metadata.generation, so stand in for the
	// API server bumping it on a spec write.
	current.Generation++
	if err := k8sClient.Update(context.Background(), &current); err != nil {
		t.Fatalf("update record set: %v", err)
	}

	// One reconcile restamps Accepted at the new generation, the next programs.
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile %d after spec change: %v", i+1, err)
		}
	}
	if spy.EnsureRecordSetCalls != 2 {
		t.Fatalf("expected a spec change to be programmed, got %d calls", spy.EnsureRecordSetCalls)
	}
}

func TestZoneBecameProgrammed(t *testing.T) {
	t.Parallel()

	zoneWith := func(status metav1.ConditionStatus) *dnsv1alpha1.DNSZone {
		return &dnsv1alpha1.DNSZone{
			ObjectMeta: metav1.ObjectMeta{Name: "zone-a", Namespace: "default"},
			Status: dnsv1alpha1.DNSZoneStatus{
				Conditions: []metav1.Condition{{
					Type:               CondProgrammed,
					Status:             status,
					Reason:             ReasonProgrammed,
					LastTransitionTime: metav1.Now(),
				}},
			},
		}
	}
	pending := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "zone-a", Namespace: "default"}}

	p := zoneBecameProgrammed()

	if !p.Create(event.CreateEvent{Object: zoneWith(metav1.ConditionTrue)}) {
		t.Fatal("a programmed zone observed at startup should fan out")
	}
	if p.Create(event.CreateEvent{Object: pending}) {
		t.Fatal("a zone that is not programmed has nothing to tell its record sets")
	}
	if !p.Update(event.UpdateEvent{ObjectOld: pending, ObjectNew: zoneWith(metav1.ConditionTrue)}) {
		t.Fatal("a zone becoming programmed must fan out to its record sets")
	}
	// The fan-out enqueues every record set in the zone, so a zone that was
	// already programmed must not repeat it on every status write.
	if p.Update(event.UpdateEvent{ObjectOld: zoneWith(metav1.ConditionTrue), ObjectNew: zoneWith(metav1.ConditionTrue)}) {
		t.Fatal("an already programmed zone should not re-enqueue its record sets")
	}
	if p.Update(event.UpdateEvent{ObjectOld: zoneWith(metav1.ConditionTrue), ObjectNew: zoneWith(metav1.ConditionFalse)}) {
		t.Fatal("a zone losing Programmed should not fan out")
	}
	if p.Delete(event.DeleteEvent{Object: zoneWith(metav1.ConditionTrue)}) {
		t.Fatal("zone deletion is handled by ownership, not by fan-out")
	}
	if p.Generic(event.GenericEvent{Object: zoneWith(metav1.ConditionTrue)}) {
		t.Fatal("generic zone events should not fan out")
	}
}
