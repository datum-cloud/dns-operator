// SPDX-License-Identifier: AGPL-3.0-only

package plugin_test

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// setEntitlement creates or updates the DNS ServiceEntitlement in the given
// phase. An empty phase deletes it, which is how the pre-flight's "not
// entitled" path is reached.
func setEntitlement(ctx context.Context, phase string) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(serviceEntitlementGVK)
	obj.SetName(dnsServiceRef)

	if phase == "" {
		if err := h.k8s.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting entitlement: %w", err)
		}
		return nil
	}

	if err := unstructured.SetNestedField(obj.Object, dnsServiceRef, "spec", "serviceRef", "name"); err != nil {
		return err
	}
	if err := unstructured.SetNestedField(obj.Object, phase, "status", "phase"); err != nil {
		return err
	}

	err := h.k8s.Create(ctx, obj)
	if apierrors.IsAlreadyExists(err) {
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(serviceEntitlementGVK)
		if err := h.k8s.Get(ctx, client.ObjectKey{Name: dnsServiceRef}, existing); err != nil {
			return fmt.Errorf("reading entitlement: %w", err)
		}
		obj.SetResourceVersion(existing.GetResourceVersion())
		return h.k8s.Update(ctx, obj)
	}
	return err
}

// withoutEntitlement removes the DNS entitlement for the duration of a test and
// restores it afterwards, so the pre-flight's refusal path can be exercised
// without leaking that state into other tests.
func withoutEntitlement(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	if err := setEntitlement(ctx, ""); err != nil {
		t.Fatalf("removing the entitlement: %v", err)
	}
	t.Cleanup(func() {
		if err := setEntitlement(context.Background(), "Active"); err != nil {
			t.Fatalf("restoring the entitlement: %v", err)
		}
	})
}

// withEntitlementPhase puts the entitlement into a specific phase for the
// duration of a test.
func withEntitlementPhase(t *testing.T, phase string) {
	t.Helper()
	if err := setEntitlement(t.Context(), phase); err != nil {
		t.Fatalf("setting the entitlement phase to %q: %v", phase, err)
	}
	t.Cleanup(func() {
		if err := setEntitlement(context.Background(), "Active"); err != nil {
			t.Fatalf("restoring the entitlement: %v", err)
		}
	})
}

// ensureNamespace creates the namespace the plugin operates in. envtest starts
// with only "default", which happens to be the one util uses, but asserting it
// rather than assuming keeps the harness honest if that constant changes.
func ensureNamespace(t *testing.T) {
	t.Helper()
	ns := &unstructured.Unstructured{}
	ns.SetAPIVersion("v1")
	ns.SetKind("Namespace")
	ns.SetName(util.ResourceNamespace)
	if err := h.k8s.Create(t.Context(), ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("creating namespace %q: %v", util.ResourceNamespace, err)
	}
}

// createZone creates a DNSZone through the admin client and registers cleanup.
// It returns the object as the server stored it, defaults and all.
func createZone(t *testing.T, name, domain string) *dnsv1alpha1.DNSZone {
	t.Helper()
	ensureNamespace(t)

	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: util.ResourceNamespace},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       domain,
			DNSZoneClassName: "datum-external-global-dns",
		},
	}
	if err := h.k8s.Create(t.Context(), zone); err != nil {
		t.Fatalf("creating zone %q: %v", domain, err)
	}
	t.Cleanup(func() {
		if err := h.k8s.Delete(context.Background(), zone); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("deleting zone %q: %v", domain, err)
		}
	})
	return zone
}

// createRecordSet creates a DNSRecordSet through the admin client. The returned
// object carries whatever the API server defaulted onto it, which is the point:
// the status conditions come back stamped at the Unix epoch.
func createRecordSet(
	t *testing.T,
	name, zoneName string,
	recordType dnsv1alpha1.RRType,
	entries ...dnsv1alpha1.RecordEntry,
) *dnsv1alpha1.DNSRecordSet {
	t.Helper()
	ensureNamespace(t)

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: util.ResourceNamespace},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1LocalRef(zoneName),
			RecordType: recordType,
			Records:    entries,
		},
	}
	if err := h.k8s.Create(t.Context(), rs); err != nil {
		t.Fatalf("creating record set %q: %v", name, err)
	}
	t.Cleanup(func() {
		if err := h.k8s.Delete(context.Background(), rs); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("deleting record set %q: %v", name, err)
		}
	})
	return rs
}

// corev1LocalRef is a tiny helper so the fixture builders read cleanly.
func corev1LocalRef(name string) corev1.LocalObjectReference {
	return corev1.LocalObjectReference{Name: name}
}

// recordEntry builds a single A-record entry, the shape most fixtures need.
func recordEntry(name, ip string) dnsv1alpha1.RecordEntry {
	return dnsv1alpha1.RecordEntry{
		Name: name,
		A:    &dnsv1alpha1.ARecordSpec{Content: ip},
	}
}
