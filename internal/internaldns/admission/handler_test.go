// SPDX-License-Identifier: AGPL-3.0-only

package admission

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	kubeadmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestResourceAccessUsesAPIServerAuthorization(t *testing.T) {
	h := Handler{}
	for _, resource := range []string{"dnsresolvercontexts", "dnsresolverbindings", "dnspublicationmanifests", "dnspublicationchunks", "dnstransportoutboxes", "dnspublicationownerships"} {
		if !h.Handle(context.Background(), request(resource, "authorized-user", map[string]any{}, nil)).Allowed {
			t.Fatalf("admission duplicates reaccess version for %s", resource)
		}
	}
	req := request("dnsrecordcontributions", "authorized-user", map[string]any{}, nil)
	req.SubResource = "status"
	if !h.Handle(context.Background(), req).Allowed {
		t.Fatal("admission duplicates status authorization")
	}
}

func TestUnavailableProjectContextFailsClosed(t *testing.T) {
	h := Handler{}
	res := h.Handle(context.Background(), request("dnsrecordcontributions", "publisher", map[string]any{}, nil))
	if res.Allowed || res.Result == nil || res.Result.Code != 503 {
		t.Fatalf("must fail closed: %#v", res)
	}
}

func request(resource, username string, obj, old any) kubeadmission.Request {
	raw, _ := json.Marshal(obj)
	var oldRaw []byte
	if old != nil {
		oldRaw, _ = json.Marshal(old)
	}
	return kubeadmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: types.UID("request-uid"), Namespace: "project", Operation: admissionv1.Update,
		Resource: metav1.GroupVersionResource{Group: dnsv1alpha1.GroupVersion.Group, Version: "v1alpha1", Resource: resource},
		Kind:     metav1.GroupVersionKind{Group: dnsv1alpha1.GroupVersion.Group, Version: "v1alpha1"},
		UserInfo: authenticationv1.UserInfo{Username: username}, Object: runtime.RawExtension{Raw: raw}, OldObject: runtime.RawExtension{Raw: oldRaw},
	}}
}

func TestInternalAdmissionPreservesPublicZoneControllerStatus(t *testing.T) {
	h := Handler{}
	req := kubeadmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Update, Resource: metav1.GroupVersionResource{Group: dnsv1alpha1.GroupVersion.Group, Version: "v1alpha1", Resource: "dnszones"}, SubResource: "status", UserInfo: authenticationv1.UserInfo{Username: "system:serviceaccount:dns-system:public-controller"}}}
	zone := dnsv1alpha1.DNSZone{Spec: dnsv1alpha1.DNSZoneSpec{Visibility: dnsv1alpha1.DNSZoneVisibilityPublic}}
	req.Object.Raw, _ = json.Marshal(zone)
	if !h.Handle(context.Background(), req).Allowed {
		t.Fatal("internal admission blocked legacy public-zone status")
	}
	zone.Spec.Visibility = dnsv1alpha1.DNSZoneVisibilityPrivate
	req.Object.Raw, _ = json.Marshal(zone)
	if !h.Handle(context.Background(), req).Allowed {
		t.Fatal("status authorization must use the API server")
	}
}

func TestProductIntentCanBeDeletedWithoutLiveReferences(t *testing.T) {
	h := Handler{}
	req := kubeadmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Delete, Resource: metav1.GroupVersionResource{Group: dnsv1alpha1.GroupVersion.Group, Version: "v1alpha1", Resource: "dnsrecordcontributions"}, UserInfo: authenticationv1.UserInfo{Username: "system:serviceaccount:compute:publisher"}}}
	if !h.Handle(context.Background(), req).Allowed {
		t.Fatal("contribution cleanup required live references")
	}
	req.Resource.Resource = "dnspublicationownerships"
	if !h.Handle(context.Background(), req).Allowed {
		t.Fatal("deletion authorization must use the API server")
	}
}

func TestResolverAccessRequiresCurrentContext(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	resolverContext := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "ctx", Namespace: "project", UID: "ctx-uid", Generation: 1}, Status: dnsv1alpha1.DNSResolverContextStatus{AccessEpoch: 3}}
	setAdmissionCondition(&resolverContext.Status.Conditions, "Ready", metav1.ConditionTrue, 1, now)
	h := Handler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(resolverContext).Build()}
	old := &dnsv1alpha1.DNSResolverAccessBinding{Spec: dnsv1alpha1.DNSResolverAccessBindingSpec{ContextRef: dnsv1alpha1.DNSObjectReference{Name: "ctx", UID: "ctx-uid"}, Region: "central", QueryIdentity: dnsv1alpha1.DNSResolverQueryIdentity{Type: dnsv1alpha1.DNSResolverQueryIdentityDestinationAddress, Value: "fd70:100::10"}, Port: 53, Transports: []string{"UDP", "TCP"}, Version: dnsv1alpha1.DNSResolverAccessVersion{Epoch: 3, Sequence: 9}}}
	current := old.DeepCopy()
	current.Spec.Version.Sequence = 10
	req := request("dnsresolveraccessbindings", "system:serviceaccount:network-system:integration", current, old)
	if res := h.Handle(context.Background(), req); !res.Allowed {
		t.Fatalf("current integration update denied: %v", res.Result)
	}
	forged := current.DeepCopy()
	forged.Spec.ContextRef.UID = "replacement"
	req = request("dnsresolveraccessbindings", "system:serviceaccount:network-system:integration", forged, old)
	if h.Handle(context.Background(), req).Allowed {
		t.Fatal("context lifetime changed in place")
	}
	staleContext := resolverContext.DeepCopy()
	staleContext.Generation = 2
	h.Client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(staleContext).Build()
	req = request("dnsresolveraccessbindings", "system:serviceaccount:network-system:integration", current, old)
	if h.Handle(context.Background(), req).Allowed {
		t.Fatal("stale context Ready condition authorized access")
	}
}

func setAdmissionCondition(conditions *[]metav1.Condition, conditionType string, status metav1.ConditionStatus, generation int64, now time.Time) {
	apimeta.SetStatusCondition(conditions, metav1.Condition{Type: conditionType, Status: status, Reason: "Test", ObservedGeneration: generation, LastTransitionTime: metav1.NewTime(now)})
}

func TestPrivateRegistrationRejectsAuthorityOverrides(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "project", UID: "zone-uid"}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "corp.internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	h := Handler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone).Build()}
	for _, tt := range []struct {
		owner   string
		rt      dnsv1alpha1.RRType
		allowed bool
	}{{"api", dnsv1alpha1.RRTypeA, true}, {"api", dnsv1alpha1.RRTypeCNAME, true}, {"@", dnsv1alpha1.RRTypeCNAME, false}, {"CORP.INTERNAL.", dnsv1alpha1.RRTypeCNAME, false}, {"@", dnsv1alpha1.RRTypeSOA, false}} {
		registration := dnsv1alpha1.DNSRegistration{Spec: dnsv1alpha1.DNSRegistrationSpec{DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: zone.Name, UID: zone.UID}, TTLSeconds: 5, Name: tt.owner, RecordTypes: []dnsv1alpha1.RRType{tt.rt}}}
		response := h.Handle(context.Background(), request("dnsregistrations", "tenant", registration, nil))
		if response.Allowed != tt.allowed {
			t.Fatalf("owner %s type %s allowed=%v expected=%v", tt.owner, tt.rt, response.Allowed, tt.allowed)
		}
	}
}

func TestRegistrationRequiresCurrentZone(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "project", UID: "current"}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "corp.internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	h := Handler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone).Build()}
	for _, tt := range []struct {
		uid     types.UID
		ttl     int32
		allowed bool
	}{
		{"current", 1, true}, {"current", 30, true}, {"", 5, false}, {"retired", 5, false},
	} {
		reg := dnsv1alpha1.DNSRegistration{Spec: dnsv1alpha1.DNSRegistrationSpec{DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: zone.Name, UID: tt.uid}, TTLSeconds: tt.ttl, Name: "api", RecordTypes: []dnsv1alpha1.RRType{dnsv1alpha1.RRTypeA}}}
		if got := h.Handle(context.Background(), request("dnsregistrations", "tenant", reg, nil)); got.Allowed != tt.allowed {
			t.Errorf("UID=%q TTL=%d: allowed=%v: %v", tt.uid, tt.ttl, got.Allowed, got.Result)
		}
	}
}

func TestContributionValidatesReferencesForRBACAuthorizedCallers(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "project", UID: "zone-uid"}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "datum.internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	registration := &dnsv1alpha1.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "project", UID: "registration", Generation: 1}, Spec: dnsv1alpha1.DNSRegistrationSpec{DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: zone.Name, UID: zone.UID}, Name: "web", RecordTypes: []dnsv1alpha1.RRType{dnsv1alpha1.RRTypeA}}}
	handler := Handler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone, registration).Build()}
	contribution := &dnsv1alpha1.DNSRecordContribution{Spec: dnsv1alpha1.DNSRecordContributionSpec{RegistrationRef: dnsv1alpha1.DNSObjectReference{Name: registration.Name, UID: registration.UID, Generation: registration.Generation}, RecordSets: []dnsv1alpha1.DNSContributionRecordSet{{RecordType: dnsv1alpha1.RRTypeA, Records: []dnsv1alpha1.RecordEntry{{Name: "web", A: &dnsv1alpha1.ARecordSpec{Content: "10.0.0.8"}}}}}}}
	for _, caller := range []string{"compute-publisher", "connect-publisher"} {
		if result := handler.Handle(context.Background(), request("dnsrecordcontributions", caller, contribution, nil)); !result.Allowed {
			t.Fatalf("valid contribution rejected for %s: %v", caller, result.Result)
		}
	}
	contribution.Spec.RegistrationRef.UID = "replacement"
	if result := handler.Handle(context.Background(), request("dnsrecordcontributions", "compute-publisher", contribution, nil)); result.Allowed {
		t.Fatal("contribution accepted a mismatched registration lifetime")
	}
}
