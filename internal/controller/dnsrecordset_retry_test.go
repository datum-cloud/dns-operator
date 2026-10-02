// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	pdnsclient "go.miloapis.com/dns-operator/internal/dns/pdns"
)

func retryTestZone() *dnsv1alpha1.DNSZone {
	return &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "zone-a", Namespace: "default", UID: types.UID("zone-uid")},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "downstream-class",
		},
	}
}

func retryTestRecordSet(name string, recordType dnsv1alpha1.RRType, owners ...string) *dnsv1alpha1.DNSRecordSet {
	records := make([]dnsv1alpha1.RecordEntry, 0, len(owners))
	for _, owner := range owners {
		entry := dnsv1alpha1.RecordEntry{Name: owner}
		switch recordType {
		case dnsv1alpha1.RRTypeCNAME:
			entry.CNAME = &dnsv1alpha1.CNAMERecordSpec{Content: "target.example.net."}
		default:
			entry.A = &dnsv1alpha1.ARecordSpec{Content: "192.0.2.10"}
		}
		records = append(records, entry)
	}
	return &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone-a"},
			RecordType: recordType,
			Records:    records,
		},
	}
}

func ownerStatus(name string, status metav1.ConditionStatus, reason string, since time.Time) dnsv1alpha1.RecordSetStatus {
	return dnsv1alpha1.RecordSetStatus{
		Name: name,
		Conditions: []metav1.Condition{{
			Type:               CondProgrammed,
			Status:             status,
			Reason:             reason,
			Message:            reason,
			LastTransitionTime: metav1.NewTime(since),
		}},
	}
}

func reconcileToProgramming(t *testing.T, r *DNSRecordSetReconciler, req ctrl.Request) {
	t.Helper()
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("setup reconcile %d: %v", i+1, err)
		}
	}
}

func TestDNSRecordSetReconcile_RetriesARefusedRecordWithoutAResync(t *testing.T) {
	t.Parallel()

	rs := retryTestRecordSet("record-a", dnsv1alpha1.RRTypeA, "www")
	r, spy, k8sClient := newDownstreamRecordSetReconciler(t, retryTestZone(), rs)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(rs)}
	reconcileToProgramming(t, r, req)

	blocked := true
	spy.EnsureRecordSetFunc = func(dnsv1alpha1.DNSRecordSet) ([]dnsv1alpha1.RecordSetStatus, error) {
		if blocked {
			return []dnsv1alpha1.RecordSetStatus{ownerStatus("www", metav1.ConditionFalse, ReasonConflict, time.Now())}, nil
		}
		return []dnsv1alpha1.RecordSetStatus{ownerStatus("www", metav1.ConditionTrue, ReasonProgrammed, time.Now())}, nil
	}

	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("a refused record must not be reported as a reconcile error: %v", err)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > unprogrammedRetryMaxDelay {
		t.Fatalf("expected a bounded retry for a refused record, got %+v", res)
	}

	var refused dnsv1alpha1.DNSRecordSet
	if err := k8sClient.Get(context.Background(), req.NamespacedName, &refused); err != nil {
		t.Fatalf("get record set: %v", err)
	}
	programmed := apimeta.FindStatusCondition(refused.Status.Conditions, CondProgrammed)
	if programmed == nil || programmed.Status != metav1.ConditionFalse || programmed.Reason != ReasonConflict {
		t.Fatalf("expected Programmed=False with reason %q while refused, got %+v", ReasonConflict, programmed)
	}

	blocked = false
	res, err = r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("retry reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("expected no further retry once published, got %+v", res)
	}
	if err := k8sClient.Get(context.Background(), req.NamespacedName, &refused); err != nil {
		t.Fatalf("get record set: %v", err)
	}
	programmed = apimeta.FindStatusCondition(refused.Status.Conditions, CondProgrammed)
	if programmed == nil || programmed.Status != metav1.ConditionTrue {
		t.Fatalf("expected Programmed=True once the blocker is gone, got %+v", programmed)
	}
}

func TestDNSRecordSetReconcile_ReturnsTheErrorWhenPowerDNSIsUnreachable(t *testing.T) {
	t.Parallel()

	rs := retryTestRecordSet("record-a", dnsv1alpha1.RRTypeA, "www")
	r, spy, k8sClient := newDownstreamRecordSetReconciler(t, retryTestZone(), rs)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(rs)}
	reconcileToProgramming(t, r, req)

	outage := errors.New("dial tcp 10.0.0.1:8081: connect: connection refused")
	down := true
	spy.EnsureRecordSetFunc = func(dnsv1alpha1.DNSRecordSet) ([]dnsv1alpha1.RecordSetStatus, error) {
		if down {
			return nil, outage
		}
		return []dnsv1alpha1.RecordSetStatus{ownerStatus("www", metav1.ConditionTrue, ReasonProgrammed, time.Now())}, nil
	}

	if _, err := r.Reconcile(context.Background(), req); !errors.Is(err, outage) {
		t.Fatalf("expected the outage to be returned so the rate limiter retries, got %v", err)
	}
	var current dnsv1alpha1.DNSRecordSet
	if err := k8sClient.Get(context.Background(), req.NamespacedName, &current); err != nil {
		t.Fatalf("get record set: %v", err)
	}
	if programmed := apimeta.FindStatusCondition(current.Status.Conditions, CondProgrammed); programmed == nil || programmed.Status != metav1.ConditionFalse {
		t.Fatalf("expected Programmed=False during the outage, got %+v", programmed)
	}

	down = false
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile after recovery: %v", err)
	}
	if err := k8sClient.Get(context.Background(), req.NamespacedName, &current); err != nil {
		t.Fatalf("get record set: %v", err)
	}
	if programmed := apimeta.FindStatusCondition(current.Status.Conditions, CondProgrammed); programmed == nil || programmed.Status != metav1.ConditionTrue {
		t.Fatalf("expected Programmed=True after PowerDNS recovers, got %+v", programmed)
	}
}

func TestUnprogrammedRetryDelay(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		statuses []dnsv1alpha1.RecordSetStatus
		want     time.Duration
		waiting  bool
	}{
		{name: "nothing to program", statuses: nil, waiting: false},
		{
			name:     "everything programmed",
			statuses: []dnsv1alpha1.RecordSetStatus{ownerStatus("www", metav1.ConditionTrue, ReasonProgrammed, now.Add(-time.Hour))},
			waiting:  false,
		},
		{
			name:     "fresh refusal waits the base delay",
			statuses: []dnsv1alpha1.RecordSetStatus{ownerStatus("www", metav1.ConditionFalse, ReasonConflict, now)},
			want:     unprogrammedRetryBaseDelay,
			waiting:  true,
		},
		{
			name:     "delay grows with how long the record has waited",
			statuses: []dnsv1alpha1.RecordSetStatus{ownerStatus("www", metav1.ConditionFalse, ReasonConflict, now.Add(-40*time.Second))},
			want:     40 * time.Second,
			waiting:  true,
		},
		{
			name:     "delay is capped",
			statuses: []dnsv1alpha1.RecordSetStatus{ownerStatus("www", metav1.ConditionFalse, ReasonPDNSError, now.Add(-21*time.Hour))},
			want:     unprogrammedRetryMaxDelay,
			waiting:  true,
		},
		{
			name: "the newest failure sets the pace",
			statuses: []dnsv1alpha1.RecordSetStatus{
				ownerStatus("old", metav1.ConditionFalse, ReasonConflict, now.Add(-time.Hour)),
				ownerStatus("new", metav1.ConditionFalse, ReasonConflict, now.Add(-10*time.Second)),
			},
			want:    10 * time.Second,
			waiting: true,
		},
		{
			name:     "an owner with no condition is retried soon",
			statuses: []dnsv1alpha1.RecordSetStatus{{Name: "www"}},
			want:     unprogrammedRetryBaseDelay,
			waiting:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, waiting := unprogrammedRetryDelay(tt.statuses, now)
			if waiting != tt.waiting || got != tt.want {
				t.Fatalf("unprogrammedRetryDelay() = %v, %v; want %v, %v", got, waiting, tt.want, tt.waiting)
			}
		})
	}
}

func TestRecordSetsWaitingOn_EnqueuesRecordSetsRefusedOnTheHoldersNames(t *testing.T) {
	t.Parallel()

	now := time.Now()
	holder := retryTestRecordSet("holder", dnsv1alpha1.RRTypeA, "www")
	refused := retryTestRecordSet("refused", dnsv1alpha1.RRTypeCNAME, "WWW.example.com.")
	refused.Status.RecordSets = []dnsv1alpha1.RecordSetStatus{
		ownerStatus("WWW.example.com.", metav1.ConditionFalse, ReasonConflict, now),
	}
	elsewhere := retryTestRecordSet("elsewhere", dnsv1alpha1.RRTypeCNAME, "api")
	elsewhere.Status.RecordSets = []dnsv1alpha1.RecordSetStatus{
		ownerStatus("api", metav1.ConditionFalse, ReasonConflict, now),
	}
	published := retryTestRecordSet("published", dnsv1alpha1.RRTypeCNAME, "www")
	published.Status.RecordSets = []dnsv1alpha1.RecordSetStatus{
		ownerStatus("www", metav1.ConditionTrue, ReasonProgrammed, now),
	}

	r, _, _ := newDownstreamRecordSetReconciler(t, retryTestZone(), holder, refused, elsewhere, published)

	released := holder.DeepCopy()
	released.Spec.Records = nil
	holder.Status.RecordSets = []dnsv1alpha1.RecordSetStatus{ownerStatus("www", metav1.ConditionTrue, ReasonProgrammed, now)}

	tests := []struct {
		name    string
		holders []client.Object
		want    []string
	}{
		{name: "holder deleted", holders: []client.Object{holder}, want: []string{"refused"}},
		{name: "holder dropped the name", holders: []client.Object{holder, released}, want: []string{"refused"}},
		{name: "unrelated names", holders: []client.Object{retryTestRecordSet("other", dnsv1alpha1.RRTypeA, "mail")}, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqs := r.recordSetsWaitingOn(context.Background(), tt.holders...)
			got := make([]string, 0, len(reqs))
			for _, req := range reqs {
				got = append(got, req.Name)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("enqueued %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("enqueued %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestDNSRecordSetReconcile_APartialPublishIsNotProgrammed(t *testing.T) {
	t.Parallel()

	rs := retryTestRecordSet("record-a", dnsv1alpha1.RRTypeCNAME, "api", "www")
	r, spy, k8sClient := newDownstreamRecordSetReconciler(t, retryTestZone(), rs)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(rs)}
	reconcileToProgramming(t, r, req)

	spy.EnsureRecordSetFunc = func(dnsv1alpha1.DNSRecordSet) ([]dnsv1alpha1.RecordSetStatus, error) {
		refused := ownerStatus("www", metav1.ConditionFalse, ReasonConflict, time.Now())
		refused.Conditions[0].Message = "www.example.com cannot be published because the A record of DNSRecordSet web already holds the name."
		return []dnsv1alpha1.RecordSetStatus{
			ownerStatus("api", metav1.ConditionTrue, ReasonProgrammed, time.Now()),
			refused,
		}, nil
	}

	res, err := r.Reconcile(context.Background(), req)
	if err != nil || res.RequeueAfter <= 0 {
		t.Fatalf("expected a timed retry, got %+v, %v", res, err)
	}
	var current dnsv1alpha1.DNSRecordSet
	if err := k8sClient.Get(context.Background(), req.NamespacedName, &current); err != nil {
		t.Fatalf("get record set: %v", err)
	}
	programmed := apimeta.FindStatusCondition(current.Status.Conditions, CondProgrammed)
	if programmed == nil || programmed.Status != metav1.ConditionFalse || programmed.Reason != ReasonConflict {
		t.Fatalf("a record set with a refused name must not read Programmed, got %+v", programmed)
	}
	if !strings.Contains(programmed.Message, "www: ") || strings.Contains(programmed.Message, "api") {
		t.Fatalf("expected the message to name only the refused name, got %q", programmed.Message)
	}
}

func TestDNSRecordSetReconcile_APermanentRejectionWaitsInsteadOfErroring(t *testing.T) {
	t.Parallel()

	rs := retryTestRecordSet("record-a", dnsv1alpha1.RRTypeA, "www")
	r, spy, k8sClient := newDownstreamRecordSetReconciler(t, retryTestZone(), rs)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(rs)}
	reconcileToProgramming(t, r, req)

	spy.EnsureRecordSetFunc = func(dnsv1alpha1.DNSRecordSet) ([]dnsv1alpha1.RecordSetStatus, error) {
		return nil, pdnsclient.NewAPIError(404, `{"error": "Could not find domain"}`)
	}

	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("a permanent rejection must not be returned as an error, got %v", err)
	}
	if res.RequeueAfter < unprogrammedRetryBaseDelay || res.RequeueAfter > unprogrammedRetryMaxDelay {
		t.Fatalf("expected a capped retry, got %+v", res)
	}
	var current dnsv1alpha1.DNSRecordSet
	if err := k8sClient.Get(context.Background(), req.NamespacedName, &current); err != nil {
		t.Fatalf("get record set: %v", err)
	}
	if programmed := apimeta.FindStatusCondition(current.Status.Conditions, CondProgrammed); programmed == nil || programmed.Reason != ReasonPDNSError {
		t.Fatalf("expected Programmed=False with reason %q, got %+v", ReasonPDNSError, programmed)
	}
}

func TestDNSRecordSetReconcile_ReturnsATransientPatchFailure(t *testing.T) {
	t.Parallel()

	rs := retryTestRecordSet("record-a", dnsv1alpha1.RRTypeA, "www")
	r, spy, _ := newDownstreamRecordSetReconciler(t, retryTestZone(), rs)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(rs)}
	reconcileToProgramming(t, r, req)

	unavailable := pdnsclient.NewAPIError(503, "")
	spy.EnsureRecordSetFunc = func(dnsv1alpha1.DNSRecordSet) ([]dnsv1alpha1.RecordSetStatus, error) {
		return []dnsv1alpha1.RecordSetStatus{ownerStatus("www", metav1.ConditionFalse, ReasonPDNSError, time.Now())}, unavailable
	}

	if _, err := r.Reconcile(context.Background(), req); !errors.Is(err, unavailable) {
		t.Fatalf("expected a transient PowerDNS failure to be returned for the rate limiter, got %v", err)
	}
}

func TestRecordSetsWaitingOn_IgnoresADeletedZone(t *testing.T) {
	t.Parallel()

	refused := retryTestRecordSet("refused", dnsv1alpha1.RRTypeCNAME, "www")
	refused.Status.RecordSets = []dnsv1alpha1.RecordSetStatus{ownerStatus("www", metav1.ConditionFalse, ReasonConflict, time.Now())}
	r, _, _ := newDownstreamRecordSetReconciler(t, refused)

	if reqs := r.recordSetsWaitingOn(context.Background(), retryTestRecordSet("holder", dnsv1alpha1.RRTypeA, "mail")); len(reqs) != 0 {
		t.Fatalf("expected nothing to be queued for a zone that is gone, got %v", reqs)
	}
}

func TestDNSRecordSetReconcile_ARefusalBesideATransientFailureIsRetried(t *testing.T) {
	t.Parallel()

	rs := retryTestRecordSet("record-a", dnsv1alpha1.RRTypeCNAME, "www")
	r, spy, k8sClient := newDownstreamRecordSetReconciler(t, retryTestZone(), rs)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(rs)}
	reconcileToProgramming(t, r, req)

	joined := errors.Join(
		pdnsclient.NewAPIError(422, `{"error": "RRset www.example.com. IN CNAME: Conflicts with pre-existing RRset"}`),
		pdnsclient.NewAPIError(503, ""),
	)
	spy.EnsureRecordSetFunc = func(dnsv1alpha1.DNSRecordSet) ([]dnsv1alpha1.RecordSetStatus, error) {
		return []dnsv1alpha1.RecordSetStatus{ownerStatus("www", metav1.ConditionFalse, ReasonConflict, time.Now())}, joined
	}

	if _, err := r.Reconcile(context.Background(), req); !errors.Is(err, joined) {
		t.Fatalf("expected the transient part to be returned for the rate limiter, got %v", err)
	}
	var current dnsv1alpha1.DNSRecordSet
	if err := k8sClient.Get(context.Background(), req.NamespacedName, &current); err != nil {
		t.Fatalf("get record set: %v", err)
	}
	if programmed := apimeta.FindStatusCondition(current.Status.Conditions, CondProgrammed); programmed == nil || programmed.Status != metav1.ConditionFalse || programmed.Reason != ReasonPending {
		t.Fatalf("expected Programmed=False pending a retry, got %+v", programmed)
	}
	if len(current.Status.RecordSets) != 1 || current.Status.RecordSets[0].Conditions[0].Reason != ReasonConflict {
		t.Fatalf("expected the refused name to stay marked Conflict, got %+v", current.Status.RecordSets)
	}
}
