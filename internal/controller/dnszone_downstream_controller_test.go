// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/dns"
	dnsfake "go.miloapis.com/dns-operator/internal/dns/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newDownstreamZoneTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add dns api to scheme: %v", err)
	}

	return s
}

func newDownstreamZoneReconciler(t *testing.T, objs ...client.Object) (*DNSZoneReconciler, *dnsfake.FakeDNSClient, client.Client) {
	t.Helper()

	scheme := newDownstreamZoneTestScheme(t)
	k8sClient := ctrlfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithIndex(&dnsv1alpha1.DNSRecordSet{}, "spec.DNSZoneRef.Name", func(obj client.Object) []string {
			rs := obj.(*dnsv1alpha1.DNSRecordSet)
			if rs.Spec.DNSZoneRef.Name == "" {
				return nil
			}
			return []string{rs.Spec.DNSZoneRef.Name}
		}).
		WithStatusSubresource(&dnsv1alpha1.DNSZone{}).
		Build()

	fakeDNS := dnsfake.NewFakeDNSClient()
	r := &DNSZoneReconciler{
		Client: k8sClient,
		Scheme: scheme,
		DNSHandler: &dns.DNSHandler{
			Client: &dns.DNSClient{
				Name:          "downstream-class",
				Type:          "fake",
				DNSController: fakeDNS,
			},
		},
	}

	return r, fakeDNS, k8sClient
}

func TestDNSZoneDownstreamReconcile_IgnoresDifferentClass(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "zone-a", Namespace: "default"},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "some-other-class",
		},
	}

	r, fakeDNS, k8sClient := newDownstreamZoneReconciler(t, zone)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "zone-a"},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(fakeDNS.EnsureZoneCalls) != 0 {
		t.Fatalf("expected EnsureZone to not be called, got %d", len(fakeDNS.EnsureZoneCalls))
	}
	if len(fakeDNS.DeleteZoneCalls) != 0 {
		t.Fatalf("expected DeleteZone to not be called, got %d", len(fakeDNS.DeleteZoneCalls))
	}

	var stored dnsv1alpha1.DNSZone
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "zone-a"}, &stored); err != nil {
		t.Fatalf("get zone: %v", err)
	}
	if len(stored.Finalizers) != 0 {
		t.Fatalf("expected no finalizers to be added, got %v", stored.Finalizers)
	}
}

func TestDNSZoneDownstreamReconcile_AddsFinalizerThenEnsures(t *testing.T) {
	t.Parallel()

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "zone-a", Namespace: "default"},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "downstream-class",
		},
	}
	zoneClass := &dnsv1alpha1.DNSZoneClass{
		ObjectMeta: metav1.ObjectMeta{Name: "downstream-class"},
		Spec:       dnsv1alpha1.DNSZoneClassSpec{ControllerName: "fake"},
	}

	r, fakeDNS, k8sClient := newDownstreamZoneReconciler(t, zone, zoneClass)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "zone-a"}}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	if len(fakeDNS.EnsureZoneCalls) != 0 {
		t.Fatalf("expected first reconcile to only add finalizer, EnsureZone calls=%d", len(fakeDNS.EnsureZoneCalls))
	}

	var afterFirst dnsv1alpha1.DNSZone
	if err := k8sClient.Get(context.Background(), req.NamespacedName, &afterFirst); err != nil {
		t.Fatalf("get zone after first reconcile: %v", err)
	}
	if !containsString(afterFirst.Finalizers, downstreamZoneFinalizer) {
		t.Fatalf("expected finalizer %q after first reconcile, got %v", downstreamZoneFinalizer, afterFirst.Finalizers)
	}

	_, err = r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	var afterSecond dnsv1alpha1.DNSZone
	if err := k8sClient.Get(context.Background(), req.NamespacedName, &afterSecond); err != nil {
		t.Fatalf("get zone after second reconcile: %v", err)
	}
	accepted := apimeta.FindStatusCondition(afterSecond.Status.Conditions, CondAccepted)
	if accepted == nil {
		t.Fatal("expected Accepted condition after second reconcile")
	}
	if accepted.Status != metav1.ConditionTrue || accepted.Reason != ReasonAccepted {
		t.Fatalf("expected Accepted=True with reason %q, got status=%q reason=%q", ReasonAccepted, accepted.Status, accepted.Reason)
	}

	_, err = r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("third reconcile: %v", err)
	}

	if len(fakeDNS.EnsureZoneCalls) != 1 {
		t.Fatalf("expected EnsureZone to be called once by third reconcile, got %d", len(fakeDNS.EnsureZoneCalls))
	}
	if fakeDNS.EnsureZoneCalls[0].Zone != "zone-a" {
		t.Fatalf("expected EnsureZone call for zone-a, got %q", fakeDNS.EnsureZoneCalls[0].Zone)
	}
	if fakeDNS.EnsureZoneCalls[0].Class != "downstream-class" {
		t.Fatalf("expected EnsureZone class downstream-class, got %q", fakeDNS.EnsureZoneCalls[0].Class)
	}

	var afterThird dnsv1alpha1.DNSZone
	if err := k8sClient.Get(context.Background(), req.NamespacedName, &afterThird); err != nil {
		t.Fatalf("get zone after third reconcile: %v", err)
	}
	programmed := apimeta.FindStatusCondition(afterThird.Status.Conditions, CondProgrammed)
	if programmed == nil {
		t.Fatal("expected Programmed condition after third reconcile")
	}
	if programmed.Status != metav1.ConditionTrue || programmed.Reason != ReasonProgrammed {
		t.Fatalf("expected Programmed=True with reason %q, got status=%q reason=%q", ReasonProgrammed, programmed.Status, programmed.Reason)
	}
}

func TestDNSZoneDownstreamReconcile_DeleteFlowRemovesFinalizer(t *testing.T) {
	t.Parallel()

	now := metav1.NewTime(time.Now())
	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "zone-a",
			Namespace:         "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{downstreamZoneFinalizer},
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "downstream-class",
		},
	}
	zoneClass := &dnsv1alpha1.DNSZoneClass{
		ObjectMeta: metav1.ObjectMeta{Name: "downstream-class"},
		Spec:       dnsv1alpha1.DNSZoneClassSpec{ControllerName: "fake"},
	}

	r, fakeDNS, k8sClient := newDownstreamZoneReconciler(t, zone, zoneClass)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "zone-a"},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(fakeDNS.DeleteZoneCalls) != 1 {
		t.Fatalf("expected DeleteZone to be called once, got %d", len(fakeDNS.DeleteZoneCalls))
	}
	if fakeDNS.DeleteZoneCalls[0].Zone != "zone-a" {
		t.Fatalf("expected DeleteZone call for zone-a, got %q", fakeDNS.DeleteZoneCalls[0].Zone)
	}

	var stored dnsv1alpha1.DNSZone
	err = k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "zone-a"}, &stored)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			t.Fatalf("get zone: %v", err)
		}
		return
	}
	if containsString(stored.Finalizers, downstreamZoneFinalizer) {
		t.Fatalf("expected finalizer to be removed, finalizers=%v", stored.Finalizers)
	}
}

func TestDNSZoneDownstreamReconcile_DeleteFailureKeepsFinalizer(t *testing.T) {
	t.Parallel()

	now := metav1.NewTime(time.Now())
	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "zone-a",
			Namespace:         "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{downstreamZoneFinalizer},
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       "example.com",
			DNSZoneClassName: "downstream-class",
		},
	}
	zoneClass := &dnsv1alpha1.DNSZoneClass{
		ObjectMeta: metav1.ObjectMeta{Name: "downstream-class"},
		Spec:       dnsv1alpha1.DNSZoneClassSpec{ControllerName: "fake"},
	}

	r, fakeDNS, k8sClient := newDownstreamZoneReconciler(t, zone, zoneClass)
	fakeDNS.DeleteZoneErr = errors.New("downstream delete failed")

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "zone-a"},
	})
	if err == nil {
		t.Fatal("expected reconcile error when downstream delete fails")
	}

	if len(fakeDNS.DeleteZoneCalls) != 1 {
		t.Fatalf("expected DeleteZone to be called once, got %d", len(fakeDNS.DeleteZoneCalls))
	}

	var stored dnsv1alpha1.DNSZone
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "zone-a"}, &stored); err != nil {
		t.Fatalf("get zone: %v", err)
	}
	if !containsString(stored.Finalizers, downstreamZoneFinalizer) {
		t.Fatalf("expected finalizer to remain after delete failure, finalizers=%v", stored.Finalizers)
	}
}

func containsString(items []string, want string) bool {
	for i := range items {
		if items[i] == want {
			return true
		}
	}
	return false
}
