// SPDX-License-Identifier: AGPL-3.0-only

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	kubeadmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestManagedZoneIsKeptWhileItsContextUsesIt(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	const contextUID = types.UID("context-uid")
	live := func() *dnsv1alpha1.DNSResolverContext {
		return &dnsv1alpha1.DNSResolverContext{
			ObjectMeta: metav1.ObjectMeta{Name: "vpc", Namespace: "project", UID: contextUID},
			Spec:       dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "project-uid/vpc", ManagedNamespace: dnsv1alpha1.DNSResolverContextManagedNamespace{Enabled: true, DomainSuffix: "datum.internal"}},
		}
	}
	zoneFor := func(uid types.UID) *dnsv1alpha1.DNSZone {
		return &dnsv1alpha1.DNSZone{
			ObjectMeta: metav1.ObjectMeta{Name: model.ManagedZoneName(string(uid)), Namespace: "project", OwnerReferences: []metav1.OwnerReference{{
				APIVersion: dnsv1alpha1.GroupVersion.String(), Kind: "DNSResolverContext", Name: "vpc", UID: uid, Controller: ptr.To(true),
			}}},
			Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "datum.internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate},
		}
	}
	terminating := live()
	terminating.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	terminating.Finalizers = []string{"example.com/hold"}
	disabled := live()
	disabled.Spec.ManagedNamespace.Enabled = false
	replaced := live()
	replaced.UID = "replacement-uid"
	deletingZone := zoneFor(contextUID)
	deletingZone.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	tenantZone := zoneFor(contextUID)
	tenantZone.Name = "corp"
	refused := live()
	refused.Generation = 2
	refused.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: model.InvalidManagedDomainReason, ObservedGeneration: 2}}
	staleRefusal := refused.DeepCopy()
	staleRefusal.Generation = 3
	readyRefusal := refused.DeepCopy()
	readyRefusal.Status.Conditions[0].Status = metav1.ConditionTrue

	for _, tc := range []struct {
		name    string
		context *dnsv1alpha1.DNSResolverContext
		zone    *dnsv1alpha1.DNSZone
		allowed bool
	}{
		{"context uses the zone", live(), zoneFor(contextUID), false},
		{"context is gone", nil, zoneFor(contextUID), true},
		{"context is being deleted", terminating, zoneFor(contextUID), true},
		{"context turned off its managed namespace", disabled, zoneFor(contextUID), false},
		{"context was replaced", replaced, zoneFor(contextUID), true},
		{"zone is already being deleted", live(), deletingZone, true},
		{"zone is not a managed zone", live(), tenantZone, true},
		{"service refused the managed domain", refused, zoneFor(contextUID), true},
		{"refusal is for an older generation", staleRefusal, zoneFor(contextUID), false},
		{"refusal reason on a ready context", readyRefusal, zoneFor(contextUID), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := []client.Object{}
			if tc.context != nil {
				objects = append(objects, tc.context)
			}
			h := Handler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
			res := h.Handle(context.Background(), zoneDelete(t, tc.zone))
			if res.Allowed != tc.allowed {
				t.Fatalf("allowed = %v, want %v: %#v", res.Allowed, tc.allowed, res.Result)
			}
		})
	}

	t.Run("project unavailable", func(t *testing.T) {
		res := (&Handler{}).Handle(context.Background(), zoneDelete(t, zoneFor(contextUID)))
		if res.Allowed || res.Result == nil || res.Result.Code != 503 {
			t.Fatalf("must fail closed: %#v", res)
		}
	})
	t.Run("project is being deleted", func(t *testing.T) {
		h := &Handler{ClientForRequest: func(context.Context, kubeadmission.Request) (client.Client, error) {
			return nil, ErrProjectDeleting
		}}
		if res := h.Handle(context.Background(), zoneDelete(t, zoneFor(contextUID))); !res.Allowed {
			t.Fatalf("teardown delete was refused: %#v", res.Result)
		}
	})
	t.Run("project is not ready", func(t *testing.T) {
		h := &Handler{ClientForRequest: func(context.Context, kubeadmission.Request) (client.Client, error) {
			return nil, errors.New("authenticated project is unavailable")
		}}
		if res := h.Handle(context.Background(), zoneDelete(t, zoneFor(contextUID))); res.Allowed || res.Result == nil || res.Result.Code != 503 {
			t.Fatalf("must fail closed: %#v", res)
		}
	})
	t.Run("tenant zone needs no project lookup", func(t *testing.T) {
		if res := (&Handler{}).Handle(context.Background(), zoneDelete(t, tenantZone)); !res.Allowed {
			t.Fatalf("tenant zone delete was refused: %#v", res.Result)
		}
	})
}

func TestManagedZoneKeepsItsController(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	resolver := &dnsv1alpha1.DNSResolverContext{
		ObjectMeta: metav1.ObjectMeta{Name: "vpc", Namespace: "project", UID: "context-uid"},
		Spec:       dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "project-uid/vpc", ManagedNamespace: dnsv1alpha1.DNSResolverContextManagedNamespace{Enabled: true, DomainSuffix: "datum.internal"}},
	}
	old := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: model.ManagedZoneName("context-uid"), Namespace: "project", OwnerReferences: []metav1.OwnerReference{{
			APIVersion: dnsv1alpha1.GroupVersion.String(), Kind: "DNSResolverContext", Name: "vpc", UID: "context-uid", Controller: ptr.To(true),
		}}},
		Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "datum.internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate},
	}
	old.Labels = map[string]string{model.ManagedObjectLabel: "true"}
	released := old.DeepCopy()
	released.OwnerReferences[0].Controller = ptr.To(false)
	orphaned := old.DeepCopy()
	orphaned.OwnerReferences = nil
	relabelled := old.DeepCopy()
	relabelled.Labels["team"] = "network"
	unlabelled := old.DeepCopy()
	delete(unlabelled.Labels, model.ManagedObjectLabel)
	renamedOwner := old.DeepCopy()
	renamedOwner.OwnerReferences[0].Name = "bogus"
	extraOwner := old.DeepCopy()
	extraOwner.OwnerReferences = append(extraOwner.OwnerReferences, metav1.OwnerReference{APIVersion: "v1", Kind: "ConfigMap", Name: "other", UID: "other-uid"})
	for _, tc := range []struct {
		name    string
		context *dnsv1alpha1.DNSResolverContext
		zone    *dnsv1alpha1.DNSZone
		allowed bool
	}{
		{"metadata change", resolver, relabelled, true},
		{"controller released", resolver, released, false},
		{"owner removed", resolver, orphaned, false},
		{"owner renamed", resolver, renamedOwner, false},
		{"owner added", resolver, extraOwner, false},
		{"managed label removed", resolver, unlabelled, false},
		{"owner removed after the context is gone", nil, orphaned, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := []client.Object{}
			if tc.context != nil {
				objects = append(objects, tc.context)
			}
			h := Handler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
			req := zoneDelete(t, old)
			req.Operation = admissionv1.Update
			req.Object.Raw, _ = json.Marshal(tc.zone)
			if res := h.Handle(context.Background(), req); res.Allowed != tc.allowed {
				t.Fatalf("allowed = %v, want %v: %#v", res.Allowed, tc.allowed, res.Result)
			}
		})
	}
	t.Run("metadata change needs no project lookup", func(t *testing.T) {
		req := zoneDelete(t, old)
		req.Operation = admissionv1.Update
		req.Object.Raw, _ = json.Marshal(relabelled)
		if res := (&Handler{}).Handle(context.Background(), req); !res.Allowed {
			t.Fatalf("metadata change was refused: %#v", res.Result)
		}
	})
}

func zoneDelete(t *testing.T, zone *dnsv1alpha1.DNSZone) kubeadmission.Request {
	t.Helper()
	raw, err := json.Marshal(zone)
	if err != nil {
		t.Fatal(err)
	}
	return kubeadmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "request-uid", Namespace: "project", Name: zone.Name, Operation: admissionv1.Delete,
		Resource: metav1.GroupVersionResource{Group: dnsv1alpha1.GroupVersion.Group, Version: "v1alpha1", Resource: "dnszones"},
		UserInfo: authenticationv1.UserInfo{Username: "dns-admin"}, OldObject: runtime.RawExtension{Raw: raw},
	}}
}
