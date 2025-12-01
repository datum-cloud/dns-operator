// controller/dnsrecordset_powerdns_reconciler_test.go
package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/controller"
	pdnsclient "go.miloapis.com/dns-operator/internal/pdns"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const ns = "default"

type fakePDNSClient struct {
	replaceCalls []replaceCall
	deleteCalls  []deleteCall

	replaceErr error
	deleteErr  error
}

type replaceCall struct {
	Zone       string
	RecordType string
	OwnerName  string
	TTL        int
	Values     []string
}

type deleteCall struct {
	Zone       string
	RecordType string
	OwnerName  string
}

var _ pdnsclient.Interface = (*fakePDNSClient)(nil)

func (f *fakePDNSClient) ReplaceRRSet(
	_ context.Context,
	zone, recordType, ownerName string,
	ttl int,
	values []string,
) error {
	f.replaceCalls = append(f.replaceCalls, replaceCall{
		Zone:       zone,
		RecordType: recordType,
		OwnerName:  ownerName,
		TTL:        ttl,
		Values:     values,
	})
	return f.replaceErr
}

func (f *fakePDNSClient) DeleteRRSet(
	_ context.Context,
	zone, recordType, ownerName string,
) error {
	f.deleteCalls = append(f.deleteCalls, deleteCall{
		Zone:       zone,
		RecordType: recordType,
		OwnerName:  ownerName,
	})
	return f.deleteErr
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add dns scheme: %v", err)
	}
	return scheme
}

func newZoneAndClass(zoneName string) (*dnsv1alpha1.DNSZone, *dnsv1alpha1.DNSZoneClass) {
	zc := &dnsv1alpha1.DNSZoneClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pdns-class",
		},
		Spec: dnsv1alpha1.DNSZoneClassSpec{
			ControllerName: controller.ControllerNamePowerDNS,
		},
	}
	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:      zoneName,
			Namespace: "default",
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: zc.Name,
		},
	}
	return zone, zc
}

// helper to make a simple A record DNSRecordSet with a single owner name/value
func newARecordSet(name, zoneName, ownerName, value string) *dnsv1alpha1.DNSRecordSet {
	ttl := int64(300)
	return &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			// CreationTimestamp is zero; name ordering will decide owner when equal
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{
				Name: zoneName,
			},
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{
				{
					Name: ownerName,
					TTL:  &ttl,
					A: &dnsv1alpha1.ARecordSpec{
						Content: value,
					},
				},
			},
		},
		Status: dnsv1alpha1.DNSRecordSetStatus{},
	}
}

func newFakeClient(t *testing.T, scheme *runtime.Scheme, objs ...client.Object) client.Client {
	t.Helper()

	b := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dnsv1alpha1.DNSRecordSet{}).
		WithObjects(objs...)

	// Index for spec.DNSZoneRef.Name
	b = b.WithIndex(
		&dnsv1alpha1.DNSRecordSet{},
		"spec.DNSZoneRef.Name",
		func(obj client.Object) []string {
			rs := obj.(*dnsv1alpha1.DNSRecordSet)
			if rs.Spec.DNSZoneRef.Name == "" {
				return nil
			}
			return []string{rs.Spec.DNSZoneRef.Name}
		},
	)

	// Index for spec.recordType
	b = b.WithIndex(
		&dnsv1alpha1.DNSRecordSet{},
		"spec.recordType",
		func(obj client.Object) []string {
			rs := obj.(*dnsv1alpha1.DNSRecordSet)
			if rs.Spec.RecordType == "" {
				return nil
			}
			return []string{string(rs.Spec.RecordType)}
		},
	)

	return b.Build()
}

func TestReconcile_SingleOwner_Success(t *testing.T) {
	t.Parallel()

	scheme := newScheme(t)
	zoneName := "my-zone-a"
	ownerName := "@"

	zone, zc := newZoneAndClass(zoneName)
	rs := newARecordSet("rs-1", zoneName, ownerName, "1.2.3.4")

	pdns := &fakePDNSClient{}
	cl := newFakeClient(t, scheme, zone, zc, rs)

	r := &controller.DNSRecordSetPowerDNSReconciler{
		Client: cl,
		Scheme: scheme,
		PDNS:   pdns,
	}

	ctx := context.Background()
	req := controller.PowerDNSRecordSetReconcileRequest{
		Request: ctrl.Request{
			NamespacedName: client.ObjectKey{
				Namespace: ns,
				Name:      zoneName,
			},
		},
		RecordSetType: string(dnsv1alpha1.RRTypeA),
		RecordSetName: ownerName,
	}

	res, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter > 0 {
		t.Fatalf("expected no requeue, got %+v", res)
	}

	// PDNS should have been called with a REPLACE
	if len(pdns.replaceCalls) != 1 {
		t.Fatalf("expected 1 ReplaceRRSet call, got %d", len(pdns.replaceCalls))
	}
	call := pdns.replaceCalls[0]
	if call.Zone != zone.Spec.DomainName || call.OwnerName != ownerName {
		t.Fatalf("unexpected PDNS call: %+v", call)
	}

	// Reload DNSRecordSet and check status
	var updated dnsv1alpha1.DNSRecordSet
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: rs.Name}, &updated); err != nil {
		t.Fatalf("get updated: %v", err)
	}

	// per-owner status
	var st *dnsv1alpha1.RecordSetStatus
	for i := range updated.Status.RecordSets {
		if updated.Status.RecordSets[i].Name == ownerName {
			st = &updated.Status.RecordSets[i]
			break
		}
	}
	if st == nil {
		t.Fatalf("expected RecordSetStatus for %q", ownerName)
	}
	cond := apimeta.FindStatusCondition(st.Conditions, controller.CondProgrammed)
	if cond == nil {
		t.Fatalf("expected per-record CondProgrammed condition")
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != controller.ReasonProgrammed {
		t.Fatalf("unexpected per-record condition: %+v", cond)
	}

	// aggregate status
	prog := apimeta.FindStatusCondition(updated.Status.Conditions, controller.CondProgrammed)
	if prog == nil {
		t.Fatalf("expected aggregate CondProgrammed condition")
	}
	if prog.Status != metav1.ConditionTrue || prog.Reason != controller.ReasonProgrammed {
		t.Fatalf("unexpected aggregate condition: %+v", prog)
	}
}

func TestReconcile_SingleOwner_PDNSError(t *testing.T) {
	t.Parallel()

	scheme := newScheme(t)
	zoneName := "my-zone-b"
	ownerName := "www"

	zone, zc := newZoneAndClass(zoneName)
	rs := newARecordSet("rs-1", zoneName, ownerName, "1.2.3.4")

	pdns := &fakePDNSClient{
		replaceErr: errors.New("boom"),
	}
	cl := newFakeClient(t, scheme, zone, zc, rs)

	r := &controller.DNSRecordSetPowerDNSReconciler{
		Client: cl,
		Scheme: scheme,
		PDNS:   pdns,
	}

	ctx := context.Background()
	req := controller.PowerDNSRecordSetReconcileRequest{
		Request: ctrl.Request{
			NamespacedName: client.ObjectKey{
				Namespace: ns,
				Name:      zoneName,
			},
		},
		RecordSetType: string(dnsv1alpha1.RRTypeA),
		RecordSetName: ownerName,
	}

	_, err := r.Reconcile(ctx, req)
	if err == nil {
		t.Fatalf("expected PDNS error to be returned")
	}

	var updated dnsv1alpha1.DNSRecordSet
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: rs.Name}, &updated); err != nil {
		t.Fatalf("get updated: %v", err)
	}

	var st *dnsv1alpha1.RecordSetStatus
	for i := range updated.Status.RecordSets {
		if updated.Status.RecordSets[i].Name == ownerName {
			st = &updated.Status.RecordSets[i]
			break
		}
	}
	if st == nil {
		t.Fatalf("expected RecordSetStatus for %q", ownerName)
	}
	cond := apimeta.FindStatusCondition(st.Conditions, controller.CondProgrammed)
	if cond == nil {
		t.Fatalf("expected per-record CondProgrammed condition")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != controller.ReasonPDNSError {
		t.Fatalf("unexpected per-record condition: %+v", cond)
	}

	prog := apimeta.FindStatusCondition(updated.Status.Conditions, controller.CondProgrammed)
	if prog == nil {
		t.Fatalf("expected aggregate CondProgrammed condition")
	}
	if prog.Status != metav1.ConditionFalse || prog.Reason != controller.ReasonPDNSError {
		t.Fatalf("unexpected aggregate condition: %+v", prog)
	}
}

func TestReconcile_StatusCleanup_WhenOwnerRemoved(t *testing.T) {
	t.Parallel()

	scheme := newScheme(t)
	zoneName := "my-zone-c"

	zone, zc := newZoneAndClass(zoneName)

	// RecordSet had a record "old", but spec no longer has it; status still has it => stale.
	rs := newARecordSet("rs-1", zoneName, "new", "1.2.3.4")
	rs.Status.RecordSets = []dnsv1alpha1.RecordSetStatus{
		{
			Name: "old",
			Conditions: []metav1.Condition{
				{
					Type:   controller.CondProgrammed,
					Status: metav1.ConditionTrue,
					Reason: controller.ReasonProgrammed,
				},
			},
		},
	}

	pdns := &fakePDNSClient{}
	cl := newFakeClient(t, scheme, zone, zc, rs)

	r := &controller.DNSRecordSetPowerDNSReconciler{
		Client: cl,
		Scheme: scheme,
		PDNS:   pdns,
	}

	ctx := context.Background()
	// Reconcile for the *stale* owner; since it's not in spec but has status,
	// splitRecordSetsByName should put it into staleStatusRecordSets and remove it,
	// and since owner == nil, it should call DeleteRRSet.
	req := controller.PowerDNSRecordSetReconcileRequest{
		Request: ctrl.Request{
			NamespacedName: client.ObjectKey{
				Namespace: ns,
				Name:      zoneName,
			},
		},
		RecordSetType: string(dnsv1alpha1.RRTypeA),
		RecordSetName: "old",
	}

	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// PDNS should have been asked to delete this RRSet, not replace it.
	if len(pdns.deleteCalls) != 1 {
		t.Fatalf("expected 1 DeleteRRSet call, got %d", len(pdns.deleteCalls))
	}
	if len(pdns.replaceCalls) != 0 {
		t.Fatalf("expected 0 ReplaceRRSet calls, got %d", len(pdns.replaceCalls))
	}
	del := pdns.deleteCalls[0]
	if del.Zone != zone.Spec.DomainName || del.OwnerName != "old" {
		t.Fatalf("unexpected DeleteRRSet call: %+v", del)
	}

	var updated dnsv1alpha1.DNSRecordSet
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: rs.Name}, &updated); err != nil {
		t.Fatalf("get updated: %v", err)
	}

	for _, st := range updated.Status.RecordSets {
		if st.Name == "old" {
			t.Fatalf("expected stale status for %q to be removed, still present: %+v", "old", st)
		}
	}
}

func TestReconcile_OwnerConflict_NotOwnerCondition(t *testing.T) {
	t.Parallel()

	scheme := newScheme(t)
	zoneName := "my-zone"
	ownerName := "www"

	zone, zc := newZoneAndClass(zoneName)

	rsOwner := newARecordSet("aaa-owner", zoneName, ownerName, "1.2.3.4")
	rsOther := newARecordSet("zzz-other", zoneName, ownerName, "5.6.7.8")

	// Make timestamps identical so name ordering decides owner ("aaa-owner" < "zzz-other")
	now := metav1.NewTime(time.Now())
	rsOwner.CreationTimestamp = now
	rsOther.CreationTimestamp = now

	pdns := &fakePDNSClient{}
	cl := newFakeClient(t, scheme, zone, zc, rsOwner, rsOther)

	r := &controller.DNSRecordSetPowerDNSReconciler{
		Client: cl,
		Scheme: scheme,
		PDNS:   pdns,
	}

	ctx := context.Background()
	req := controller.PowerDNSRecordSetReconcileRequest{
		Request: ctrl.Request{
			NamespacedName: client.ObjectKey{
				Namespace: ns,
				Name:      zoneName,
			},
		},
		RecordSetType: string(dnsv1alpha1.RRTypeA),
		RecordSetName: ownerName,
	}

	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// PDNS should be called once, with values from the chosen owner (rsOwner)
	if len(pdns.replaceCalls) != 1 {
		t.Fatalf("expected 1 ReplaceRRSet call, got %d", len(pdns.replaceCalls))
	}
	call := pdns.replaceCalls[0]
	if call.OwnerName != ownerName {
		t.Fatalf("unexpected PDNS call: %+v", call)
	}

	// Check owner status: True / Programmed
	var ownerUpdated dnsv1alpha1.DNSRecordSet
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: rsOwner.Name}, &ownerUpdated); err != nil {
		t.Fatalf("get owner: %v", err)
	}
	var ownerSt *dnsv1alpha1.RecordSetStatus
	for i := range ownerUpdated.Status.RecordSets {
		if ownerUpdated.Status.RecordSets[i].Name == ownerName {
			ownerSt = &ownerUpdated.Status.RecordSets[i]
			break
		}
	}
	if ownerSt == nil {
		t.Fatalf("expected owner status entry for %q", ownerName)
	}
	ownerCond := apimeta.FindStatusCondition(ownerSt.Conditions, controller.CondProgrammed)
	if ownerCond == nil || ownerCond.Status != metav1.ConditionTrue || ownerCond.Reason != controller.ReasonProgrammed {
		t.Fatalf("unexpected owner condition: %+v", ownerCond)
	}

	// Check non-owner status: False / NotOwner
	var otherUpdated dnsv1alpha1.DNSRecordSet
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: rsOther.Name}, &otherUpdated); err != nil {
		t.Fatalf("get other: %v", err)
	}
	var otherSt *dnsv1alpha1.RecordSetStatus
	for i := range otherUpdated.Status.RecordSets {
		if otherUpdated.Status.RecordSets[i].Name == ownerName {
			otherSt = &otherUpdated.Status.RecordSets[i]
			break
		}
	}
	if otherSt == nil {
		t.Fatalf("expected other status entry for %q", ownerName)
	}
	otherCond := apimeta.FindStatusCondition(otherSt.Conditions, controller.CondProgrammed)
	if otherCond == nil || otherCond.Status != metav1.ConditionFalse || otherCond.Reason != controller.ReasonNotOwner {
		t.Fatalf("unexpected other condition: %+v", otherCond)
	}
}

func TestReconcile_OwnerWithNoRecords_DeletesRRSet(t *testing.T) {
	t.Parallel()

	scheme := newScheme(t)
	zoneName := "my-zone"
	ownerName := "empty"

	zone, zc := newZoneAndClass(zoneName)

	// RecordSet that owns "empty" but has no usable A record content.
	ttl := int64(300)
	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rs-empty",
			Namespace: ns,
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{
				Name: zoneName,
			},
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{
				{
					Name: ownerName,
					TTL:  &ttl,
					A: &dnsv1alpha1.ARecordSpec{
						Content: "", // <- empty => buildRRSets will not create any PDNS records
					},
				},
			},
		},
	}

	pdns := &fakePDNSClient{}
	cl := newFakeClient(t, scheme, zone, zc, rs)

	r := &controller.DNSRecordSetPowerDNSReconciler{
		Client: cl,
		Scheme: scheme,
		PDNS:   pdns,
	}

	ctx := context.Background()
	req := controller.PowerDNSRecordSetReconcileRequest{
		Request: ctrl.Request{
			NamespacedName: client.ObjectKey{
				Namespace: ns,
				Name:      zoneName,
			},
		},
		RecordSetType: string(dnsv1alpha1.RRTypeA),
		RecordSetName: ownerName,
	}

	_, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Since BuildOwnerRRSet returned an OwnerRRSet with zero Records,
	// reconciler should call DeleteRRSet, not ReplaceRRSet.
	if len(pdns.deleteCalls) != 1 {
		t.Fatalf("expected 1 DeleteRRSet call, got %d", len(pdns.deleteCalls))
	}
	if len(pdns.replaceCalls) != 0 {
		t.Fatalf("expected 0 ReplaceRRSet calls, got %d", len(pdns.replaceCalls))
	}
	del := pdns.deleteCalls[0]
	if del.Zone != zone.Spec.DomainName || del.OwnerName != ownerName {
		t.Fatalf("unexpected DeleteRRSet call: %+v", del)
	}

	// Status should still show the record as "programmed" (successfully applied to PDNS as a deletion).
	var updated dnsv1alpha1.DNSRecordSet
	if err := cl.Get(ctx, client.ObjectKey{Namespace: ns, Name: rs.Name}, &updated); err != nil {
		t.Fatalf("get updated: %v", err)
	}

	var st *dnsv1alpha1.RecordSetStatus
	for i := range updated.Status.RecordSets {
		if updated.Status.RecordSets[i].Name == ownerName {
			st = &updated.Status.RecordSets[i]
			break
		}
	}
	if st == nil {
		t.Fatalf("expected RecordSetStatus for %q", ownerName)
	}

	cond := apimeta.FindStatusCondition(st.Conditions, controller.CondProgrammed)
	if cond == nil {
		t.Fatalf("expected per-record CondProgrammed condition")
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != controller.ReasonProgrammed {
		t.Fatalf("unexpected per-record condition on delete-only owner: %+v", cond)
	}
}
