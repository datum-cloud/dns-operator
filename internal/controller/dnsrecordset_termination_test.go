// SPDX-License-Identifier: AGPL-3.0-only
package controller

import (
	"context"
	"errors"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRecordSetReplicatorDoesNotMutateTerminatingShadow(t *testing.T) {
	now := metav1.Now()
	shadow := &dnsv1alpha1.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "record", Namespace: "ns-downstream", DeletionTimestamp: &now, Finalizers: []string{downstreamRSFinalizer}}}
	cl := fake.NewClientBuilder().WithScheme(newTestScheme(t)).WithObjects(shadow).Build()
	r := &DNSRecordSetReplicator{DownstreamClient: cl}
	upstream := shadow.DeepCopy()
	upstream.Namespace = "default"
	upstream.DeletionTimestamp = nil
	strategy := annotatingStrategy{fakeStrategy{namespace: "ns-downstream", client: cl}}
	_, err := r.ensureDownstreamRecordSet(context.Background(), strategy, upstream)
	if !errors.Is(err, errDownstreamTerminating) {
		t.Fatalf("got %v", err)
	}
	var got dnsv1alpha1.DNSRecordSet
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(shadow), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Annotations) != 0 {
		t.Fatal("terminating shadow was mutated")
	}
}

func TestDeletingRecordSetWithoutZoneReleasesFinalizer(t *testing.T) {
	now := metav1.Now()
	rs := &dnsv1alpha1.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "record", Namespace: "default", DeletionTimestamp: &now, Finalizers: []string{downstreamRSFinalizer}}, Spec: dnsv1alpha1.DNSRecordSetSpec{DNSZoneRef: corev1.LocalObjectReference{Name: "gone"}}}
	r, spy, cl := newDownstreamRecordSetReconciler(t, rs)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(rs)})
	if err != nil {
		t.Fatal(err)
	}
	var got dnsv1alpha1.DNSRecordSet
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(rs), &got); !apierrors.IsNotFound(err) {
		t.Fatalf("orphan remains: %v", err)
	}
	if len(spy.DeleteRecordSetCalls) != 0 {
		t.Fatal("called provider with missing zone")
	}
}

func TestDeletingRecordSetRetainsFinalizerOnZoneCacheMiss(t *testing.T) {
	now := metav1.Now()
	rs := &dnsv1alpha1.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "record", Namespace: "default", DeletionTimestamp: &now, Finalizers: []string{downstreamRSFinalizer}}, Spec: dnsv1alpha1.DNSRecordSetSpec{DNSZoneRef: corev1.LocalObjectReference{Name: "zone"}}}
	r, _, cl := newDownstreamRecordSetReconciler(t, rs)
	r.APIReader = fake.NewClientBuilder().WithScheme(newDownstreamRecordSetTestScheme(t)).WithObjects(&dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "zone", Namespace: "default"}}).Build()
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(rs)})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("result=%v err=%v", result, err)
	}
	var got dnsv1alpha1.DNSRecordSet
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(rs), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Finalizers) != 1 {
		t.Fatal("finalizer removed while zone exists")
	}
}

func TestRecordSetReplicatorWaitsForTerminatingNamespace(t *testing.T) {
	now := metav1.Now()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns-downstream", DeletionTimestamp: &now, Finalizers: []string{"test/finalizer"}}}
	cl := fake.NewClientBuilder().WithScheme(newTestScheme(t)).WithObjects(ns).Build()
	r := &DNSRecordSetReplicator{DownstreamClient: cl}
	upstream := &dnsv1alpha1.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "record", Namespace: "default"}}
	strategy := annotatingStrategy{fakeStrategy{namespace: "ns-downstream", client: cl}}
	_, err := r.ensureDownstreamRecordSet(context.Background(), strategy, upstream)
	if !errors.Is(err, errDownstreamTerminating) {
		t.Fatalf("got %v", err)
	}
	var records dnsv1alpha1.DNSRecordSetList
	if err := cl.List(context.Background(), &records); err != nil {
		t.Fatal(err)
	}
	if len(records.Items) != 0 {
		t.Fatal("created a record in a terminating namespace")
	}
}

func TestZoneReplicatorWaitsForTerminatingNamespace(t *testing.T) {
	now := metav1.Now()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns-downstream", DeletionTimestamp: &now, Finalizers: []string{"test/finalizer"}}}
	cl := fake.NewClientBuilder().WithScheme(newTestScheme(t)).WithObjects(ns).Build()
	r := &DNSZoneReplicator{DownstreamClient: cl}
	strategy := annotatingStrategy{fakeStrategy{namespace: "ns-downstream", client: cl}}
	_, err := r.ensureDownstreamZone(context.Background(), strategy, &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "zone", Namespace: "default"}})
	if !errors.Is(err, errDownstreamTerminating) {
		t.Fatalf("got %v", err)
	}
}

func TestDownstreamTerminationAdmissionRace(t *testing.T) {
	for _, cause := range []metav1.CauseType{corev1.NamespaceTerminatingCause, metav1.CauseTypeUnexpectedServerResponse} {
		err := &apierrors.StatusError{ErrStatus: metav1.Status{Reason: metav1.StatusReasonForbidden, Details: &metav1.StatusDetails{Causes: []metav1.StatusCause{{Type: cause}}}}}
		got := downstreamTerminationError(err)
		if cause == corev1.NamespaceTerminatingCause {
			if !errors.Is(got, errDownstreamTerminating) {
				t.Fatalf("got %v", got)
			}
		} else if got != err {
			t.Fatal("unrelated forbidden error swallowed")
		}
	}
}
