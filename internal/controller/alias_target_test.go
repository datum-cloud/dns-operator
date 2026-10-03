// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][]net.IPAddr
	errs    map[string]error
	lookups []string
}

func (f *fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups = append(f.lookups, host)
	if err, ok := f.errs[host]; ok {
		return nil, err
	}
	return f.answers[host], nil
}

func aliasRecordSet(name string, targets ...string) *dnsv1alpha1.DNSRecordSet {
	records := make([]dnsv1alpha1.RecordEntry, 0, len(targets))
	for _, target := range targets {
		records = append(records, dnsv1alpha1.RecordEntry{
			Name:  "@",
			ALIAS: &dnsv1alpha1.ALIASRecordSpec{Content: target},
		})
	}
	return &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Generation: 1},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: "zone-a"},
			RecordType: dnsv1alpha1.RRTypeALIAS,
			Records:    records,
		},
	}
}

var (
	resolvedAddr = []net.IPAddr{{IP: net.ParseIP("192.0.2.10")}}
	nxdomain     = &net.DNSError{Err: "no such host", Name: "gone.example.net.", IsNotFound: true}
	lookupTimout = &net.DNSError{Err: "i/o timeout", Name: "slow.example.net.", IsTimeout: true}
)

func TestALIASTargetCondition(t *testing.T) {
	t.Parallel()

	resolver := &fakeResolver{
		answers: map[string][]net.IPAddr{"proxy.example.net.": resolvedAddr},
		errs: map[string]error{
			"gone.example.net.": nxdomain,
			"slow.example.net.": lookupTimout,
		},
	}

	tests := []struct {
		name       string
		targets    []string
		wantStatus metav1.ConditionStatus
		wantReason string
		wantIn     string
	}{
		{"a target that resolves", []string{"proxy.example.net."}, metav1.ConditionTrue, ReasonTargetResolved, ""},
		{"a relative spelling is looked up as absolute", []string{"proxy.example.net"}, metav1.ConditionTrue, ReasonTargetResolved, ""},
		{"NXDOMAIN", []string{"gone.example.net."}, metav1.ConditionFalse, ReasonTargetUnresolved, "gone.example.net."},
		{"a name with no address", []string{"empty.example.net."}, metav1.ConditionFalse, ReasonTargetUnresolved, "empty.example.net."},
		{"a failed lookup is not called dangling", []string{"slow.example.net."}, metav1.ConditionUnknown, ReasonTargetLookupFailed, "slow.example.net."},
		{"a dangling target outweighs a failed lookup", []string{"slow.example.net.", "gone.example.net."}, metav1.ConditionFalse, ReasonTargetUnresolved, "gone.example.net."},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cond := aliasTargetCondition(context.Background(), resolver, aliasRecordSet("rs", tc.targets...))
			if cond.Type != CondTargetResolved || cond.Status != tc.wantStatus || cond.Reason != tc.wantReason {
				t.Fatalf("got %s=%s reason %s, want %s reason %s", cond.Type, cond.Status, cond.Reason, tc.wantStatus, tc.wantReason)
			}
			if !strings.Contains(cond.Message, tc.wantIn) {
				t.Fatalf("message %q does not name %q", cond.Message, tc.wantIn)
			}
		})
	}
}

func programmedALIASRecordSet(t *testing.T, name, target string) (*dnsv1alpha1.DNSZone, *dnsv1alpha1.DNSRecordSet) {
	t.Helper()
	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "zone-a", Namespace: "default", UID: types.UID("zone-uid")},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "example.com", DNSZoneClassName: "downstream-class"},
	}
	rs := aliasRecordSet(name, target)
	rs.Finalizers = []string{downstreamRSFinalizer}
	rs.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: dnsv1alpha1.GroupVersion.String(), Kind: "DNSZone", Name: zone.Name, UID: zone.UID,
	}}
	now := metav1.Now()
	rs.Status.Conditions = []metav1.Condition{
		{Type: CondAccepted, Status: metav1.ConditionTrue, Reason: ReasonAccepted, Message: "RecordSet is accepted for processing", ObservedGeneration: 1, LastTransitionTime: now},
		{Type: CondProgrammed, Status: metav1.ConditionTrue, Reason: ReasonProgrammed, ObservedGeneration: 1, LastTransitionTime: now},
	}
	return zone, rs
}

// A programmed ALIAS record set whose target is gone must say so, and the
// status write that reports it must not set off another lookup.
func TestDNSRecordSetReconcile_ReportsDanglingALIASOncePerInterval(t *testing.T) {
	t.Parallel()

	zone, rs := programmedALIASRecordSet(t, "dangling-apex", "gone.example.net.")
	r, spy, k8sClient := newDownstreamRecordSetReconciler(t, zone, rs)
	resolver := &fakeResolver{errs: map[string]error{"gone.example.net.": nxdomain}}
	r.TargetResolver = resolver
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: rs.Name}}

	res, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter != aliasTargetCheckInterval {
		t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, aliasTargetCheckInterval)
	}
	if spy.EnsureRecordSetCalls != 0 {
		t.Fatalf("an already programmed record set was written again (%d calls)", spy.EnsureRecordSetCalls)
	}

	var got dnsv1alpha1.DNSRecordSet
	if err := k8sClient.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, CondTargetResolved)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != ReasonTargetUnresolved {
		t.Fatalf("TargetResolved = %+v, want False/%s", cond, ReasonTargetUnresolved)
	}
	if programmed := apimeta.FindStatusCondition(got.Status.Conditions, CondProgrammed); programmed.Status != metav1.ConditionTrue {
		t.Fatalf("a dangling target flipped Programmed to %s", programmed.Status)
	}
	var m dto.Metric
	if err := aliasTargetUnresolved.WithLabelValues("default", rs.Name).Write(&m); err != nil || m.GetGauge().GetValue() != 1 {
		t.Fatalf("unresolved gauge = %v (err %v), want 1", m.GetGauge().GetValue(), err)
	}

	res, err = r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if len(resolver.lookups) != 1 {
		t.Fatalf("looked up %d times within one interval, want 1", len(resolver.lookups))
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > aliasTargetCheckInterval {
		t.Fatalf("second RequeueAfter = %v, want the rest of the interval", res.RequeueAfter)
	}
}

func TestDNSRecordSetReconcile_ClearsGaugeWhenALIASTargetResolves(t *testing.T) {
	t.Parallel()

	zone, rs := programmedALIASRecordSet(t, "healthy-apex", "proxy.example.net.")
	r, _, k8sClient := newDownstreamRecordSetReconciler(t, zone, rs)
	r.TargetResolver = &fakeResolver{answers: map[string][]net.IPAddr{"proxy.example.net.": resolvedAddr}}
	aliasTargetUnresolved.WithLabelValues("default", rs.Name).Set(1)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: rs.Name}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got dnsv1alpha1.DNSRecordSet
	if err := k8sClient.Get(context.Background(), req.NamespacedName, &got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if !apimeta.IsStatusConditionTrue(got.Status.Conditions, CondTargetResolved) {
		t.Fatalf("TargetResolved is not True: %+v", got.Status.Conditions)
	}
	if aliasTargetUnresolved.DeleteLabelValues("default", rs.Name) {
		t.Fatal("unresolved gauge still has a series for a resolving target")
	}
}

func TestDNSRecordSetReconcile_SkipsTargetCheckForOtherTypes(t *testing.T) {
	t.Parallel()

	zone, rs := programmedALIASRecordSet(t, "plain-a", "proxy.example.net.")
	rs.Spec.RecordType = dnsv1alpha1.RRTypeA
	rs.Spec.Records = []dnsv1alpha1.RecordEntry{{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.10"}}}
	r, _, _ := newDownstreamRecordSetReconciler(t, zone, rs)
	resolver := &fakeResolver{}
	r.TargetResolver = resolver

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: rs.Name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(resolver.lookups) != 0 || res.RequeueAfter != 0 {
		t.Fatalf("an A record set was checked as ALIAS: lookups %v, RequeueAfter %v", resolver.lookups, res.RequeueAfter)
	}
}

func TestALIASTargetChecks_DueAgainAfterIntervalOrNewGeneration(t *testing.T) {
	t.Parallel()

	var checks aliasTargetChecks
	key := types.NamespacedName{Namespace: "default", Name: "rs"}
	start := time.Now()
	checks.record(key, 1, start)

	if _, due := checks.due(key, 1, start.Add(time.Minute)); due {
		t.Fatal("due one minute after a check")
	}
	if _, due := checks.due(key, 2, start.Add(time.Minute)); !due {
		t.Fatal("not due after the spec changed")
	}
	if _, due := checks.due(key, 1, start.Add(aliasTargetCheckInterval)); !due {
		t.Fatal("not due once the interval passed")
	}
}
