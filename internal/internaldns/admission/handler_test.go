// SPDX-License-Identifier: AGPL-3.0-only

package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	kubeadmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestContributionAuthenticatedWriter(t *testing.T) {
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	producer := "system:serviceaccount:compute-system:dns-publisher"
	registration := &dnsv1alpha1.DNSRegistration{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "project", UID: "reg-uid", Generation: 3},
		Spec:       dnsv1alpha1.DNSRegistrationSpec{DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: "private"}, RecordTypes: []dnsv1alpha1.RRType{"AAAA"}},
	}
	grant := &dnsv1alpha1.DNSContributionGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "compute", Namespace: "project", UID: "grant-uid"},
		Spec: dnsv1alpha1.DNSContributionGrantSpec{
			RegistrationRef: dnsv1alpha1.DNSObjectReference{Name: "api", UID: "reg-uid"},
			Principal:       dnsv1alpha1.DNSProducerPrincipal{ClusterUID: "cluster-uid", Subject: producer},
			RecordTypes:     []dnsv1alpha1.RRType{"AAAA"},
		},
		Status: dnsv1alpha1.DNSContributionGrantStatus{ActiveWriterEpoch: 8, ObservedRegistrationGeneration: 3},
	}
	contribution := dnsv1alpha1.DNSRecordContribution{
		ObjectMeta: metav1.ObjectMeta{Name: "api-east", Namespace: "project", UID: "contribution-uid", Generation: 2},
		Spec: dnsv1alpha1.DNSRecordContributionSpec{
			RegistrationRef: dnsv1alpha1.DNSObjectReference{Name: "api", UID: "reg-uid", Generation: 3},
			GrantRef:        dnsv1alpha1.DNSObjectReference{Name: "compute", UID: "grant-uid"},
		},
		Status: dnsv1alpha1.DNSRecordContributionStatus{ObservedGeneration: 2, WriterEpoch: 8, Sequence: 18, Eligible: true, ValidUntil: ptrTime(now.Add(30 * time.Second))},
	}
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	h := Handler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(registration, grant, &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "project"}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "corp.internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}).Build(), TrustedClusterUID: "cluster-uid", Now: func() time.Time { return now }}
	cases := []struct {
		name    string
		user    string
		mutate  func(*dnsv1alpha1.DNSRecordContribution)
		allowed bool
	}{
		{"scoped fresh producer", producer, nil, true},
		{"another producer", "system:serviceaccount:connector-system:dns-publisher", nil, false},
		{"controller binds issued epoch", "rbac-authorized-controller", func(c *dnsv1alpha1.DNSRecordContribution) {
			c.Status.Conditions = []metav1.Condition{{Type: "AuthorityBound", Status: metav1.ConditionTrue}}
		}, true},
		{"controller cannot bind another epoch", "rbac-authorized-controller", func(c *dnsv1alpha1.DNSRecordContribution) { c.Status.WriterEpoch = 99 }, false},
		{"epoch binding cannot assert health", "rbac-authorized-controller", func(c *dnsv1alpha1.DNSRecordContribution) { c.Status.Eligible = true }, false},

		{"retired epoch", producer, func(c *dnsv1alpha1.DNSRecordContribution) { c.Status.WriterEpoch = 7 }, false},
		{"unchanged observation", producer, func(c *dnsv1alpha1.DNSRecordContribution) { c.Status.Sequence = 17 }, true},
		{"decreasing sequence", producer, func(c *dnsv1alpha1.DNSRecordContribution) { c.Status.Sequence = 16 }, false},
		{"stale generation", producer, func(c *dnsv1alpha1.DNSRecordContribution) { c.Status.ObservedGeneration = 1 }, false},
		{"forged grant UID", producer, func(c *dnsv1alpha1.DNSRecordContribution) { c.Spec.GrantRef.UID = "other-grant" }, false},
		{"retired registration UID", producer, func(c *dnsv1alpha1.DNSRecordContribution) { c.Spec.RegistrationRef.UID = "old-reg" }, false},
		{"publication fields", producer, func(c *dnsv1alpha1.DNSRecordContribution) { c.Status.PublishedRevision = 1000 }, false},
		{"overlong lease", producer, func(c *dnsv1alpha1.DNSRecordContribution) { c.Status.ValidUntil = ptrTime(now.Add(time.Hour)) }, false},
		{"expired eligible lease", producer, func(c *dnsv1alpha1.DNSRecordContribution) { c.Status.ValidUntil = ptrTime(now.Add(-time.Second)) }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := contribution.DeepCopy()
			old := contribution.DeepCopy()
			old.Status.Sequence = 17
			if tc.name == "controller binds issued epoch" || tc.name == "controller cannot bind another epoch" || tc.name == "epoch binding cannot assert health" {
				old.Status = dnsv1alpha1.DNSRecordContributionStatus{}
				c.Status = dnsv1alpha1.DNSRecordContributionStatus{WriterEpoch: 8}
			}
			if tc.mutate != nil {
				tc.mutate(c)
			}
			req := request("dnsrecordcontributions", tc.user, c, old)
			req.SubResource = "status"
			res := h.Handle(context.Background(), req)
			if res.Allowed != tc.allowed {
				t.Fatalf("allowed=%v want=%v: %v", res.Allowed, tc.allowed, res.Result)
			}
		})
	}
}

func TestResourceAccessUsesAPIServerAuthorization(t *testing.T) {
	h := Handler{}
	for _, resource := range []string{"dnsresolvercontexts", "dnsresolverbindings", "dnspublicationmanifests", "dnspublicationchunks", "dnstransportoutboxes", "dnspublicationownerships"} {
		if !h.Handle(context.Background(), request(resource, "authorized-user", map[string]any{}, nil)).Allowed {
			t.Fatalf("admission duplicates resource authorization for %s", resource)
		}
	}
	req := request("dnscontributiongrants", "authorized-user", map[string]any{}, nil)
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

func ptrTime(t time.Time) *metav1.Time { v := metav1.NewTime(t); return &v }

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

func TestRetiredProductIntentCanBeDeletedWithoutLiveGrant(t *testing.T) {
	h := Handler{}
	req := kubeadmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Operation: admissionv1.Delete, Resource: metav1.GroupVersionResource{Group: dnsv1alpha1.GroupVersion.Group, Version: "v1alpha1", Resource: "dnsrecordcontributions"}, UserInfo: authenticationv1.UserInfo{Username: "system:serviceaccount:compute:publisher"}}}
	if !h.Handle(context.Background(), req).Allowed {
		t.Fatal("retired contribution cleanup required a live deleted grant")
	}
	req.Resource.Resource = "dnspublicationownerships"
	if !h.Handle(context.Background(), req).Allowed {
		t.Fatal("deletion authorization must use the API server")
	}
}

func TestResolverAccessRequiresCurrentContextAndBoundedLease(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	resolverContext := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "ctx", Namespace: "project", UID: "ctx-uid", Generation: 1}, Status: dnsv1alpha1.DNSResolverContextStatus{AccessWriterEpoch: 3}}
	setAdmissionCondition(&resolverContext.Status.Conditions, "Ready", metav1.ConditionTrue, 1, now)
	h := Handler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(resolverContext).Build(), MaxAccessLease: 5 * time.Minute, Now: func() time.Time { return now }}
	old := &dnsv1alpha1.DNSResolverAccessBinding{Spec: dnsv1alpha1.DNSResolverAccessBindingSpec{ContextRef: dnsv1alpha1.DNSObjectReference{Name: "ctx", UID: "ctx-uid"}, Region: "central", QueryIdentity: dnsv1alpha1.DNSResolverQueryIdentity{Type: dnsv1alpha1.DNSResolverQueryIdentityDestinationAddress, Value: "fd70:100::10"}, Port: 53, Transports: []string{"UDP", "TCP"}, Authorization: dnsv1alpha1.DNSResolverAccessAuthorization{WriterEpoch: 3, Sequence: 9, ValidUntil: metav1.NewTime(now.Add(2 * time.Minute))}}}
	current := old.DeepCopy()
	current.Spec.Authorization.Sequence = 10
	current.Spec.Authorization.ValidUntil = metav1.NewTime(now.Add(3 * time.Minute))
	req := request("dnsresolveraccessbindings", "system:serviceaccount:network-system:integration", current, old)
	if res := h.Handle(context.Background(), req); !res.Allowed {
		t.Fatalf("fresh integration renewal denied: %v", res.Result)
	}
	overlong := current.DeepCopy()
	overlong.Spec.Authorization.ValidUntil = metav1.NewTime(now.Add(6 * time.Minute))
	req = request("dnsresolveraccessbindings", "system:serviceaccount:network-system:integration", overlong, old)
	if h.Handle(context.Background(), req).Allowed {
		t.Fatal("overlong access lease accepted")
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

func TestConsumerSelectorUsesNamesAndRejectsAmbiguity(t *testing.T) {
	vpc := dnsv1alpha1.DNSNameReference{Name: "vpc"}
	resolverContext := dnsv1alpha1.DNSNameReference{Name: "ctx"}
	for _, contextMode := range []bool{false, true} {
		for _, tc := range []struct {
			name         string
			vpc, context dnsv1alpha1.DNSNameReference
		}{{"empty", dnsv1alpha1.DNSNameReference{}, dnsv1alpha1.DNSNameReference{}}, {"both", vpc, resolverContext}} {
			t.Run(fmt.Sprintf("context=%v/%s", contextMode, tc.name), func(t *testing.T) {
				if err := validateConsumerSelector(tc.vpc, tc.context, contextMode); err == nil {
					t.Fatal("ambiguous selector accepted")
				}
			})
		}
	}
	if err := validateConsumerSelector(vpc, dnsv1alpha1.DNSNameReference{}, false); err != nil {
		t.Fatalf("network selector rejected: %v", err)
	}
	if err := validateConsumerSelector(dnsv1alpha1.DNSNameReference{}, resolverContext, true); err != nil {
		t.Fatalf("context selector rejected: %v", err)
	}
	if validateConsumerSelector(vpc, dnsv1alpha1.DNSNameReference{}, true) == nil || validateConsumerSelector(dnsv1alpha1.DNSNameReference{}, resolverContext, false) == nil {
		t.Fatal("selector accepted in the wrong integration mode")
	}
}

func TestCustomerNamedReferencesRequireZoneAndContextAuthorization(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "project", UID: "zone"}, Spec: dnsv1alpha1.DNSZoneSpec{Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	resolver := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{Name: "application", Namespace: "project", UID: "context", Generation: 1}}
	setAdmissionCondition(&resolver.Status.Conditions, "Ready", metav1.ConditionTrue, resolver.Generation, time.Now())
	deniedResource := ""
	reviewed := map[string]string{}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone, resolver).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if review, ok := obj.(*authorizationv1.SubjectAccessReview); ok {
			attributes := review.Spec.ResourceAttributes
			reviewed[attributes.Resource] = attributes.Verb + "/" + attributes.Name
			review.Status.Allowed = attributes.Resource != deniedResource
			return nil
		}
		return cl.Create(ctx, obj, opts...)
	}}).Build()
	h := Handler{Client: cl, ResolverContextsEnabled: true}
	zoneRef := dnsv1alpha1.DNSNameReference{Name: zone.Name}
	contextRef := dnsv1alpha1.DNSNameReference{Name: resolver.Name}
	for resource, object := range map[string]any{
		"dnszoneassociations": &dnsv1alpha1.DNSZoneAssociation{Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: zoneRef, ResolverContextRef: contextRef}},
		"dnsnamingpolicies":   &dnsv1alpha1.DNSNamingPolicy{Spec: dnsv1alpha1.DNSNamingPolicySpec{ResolverContextRef: contextRef, AdditionalNames: []dnsv1alpha1.DNSAdditionalNameRule{{DNSZoneRef: zoneRef, NamePrefix: "hosts"}}}},
	} {
		t.Run(resource, func(t *testing.T) {
			for _, denied := range []string{"", "dnszones", "dnsresolvercontexts"} {
				deniedResource = denied
				reviewed = map[string]string{}
				response := h.Handle(context.Background(), request(resource, "customer", object, nil))
				if response.Allowed != (denied == "") {
					t.Fatalf("denied=%q allowed=%v: %v", denied, response.Allowed, response.Result)
				}
				if denied == "" && (reviewed["dnszones"] != "update/production" || reviewed["dnsresolvercontexts"] != "use/application") {
					t.Fatalf("references did not authorize both named resources: %v", reviewed)
				}
			}
			req := request(resource, "customer", object, nil)
			req.SubResource = "status"
			if h.Handle(context.Background(), req).Allowed {
				t.Fatal("customer can modify DNS-owned lifetime pins")
			}
		})
	}
}

func TestNamingPolicyRejectsMalformedSelectors(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	h := Handler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), ResolverContextsEnabled: true}
	for _, selector := range []*metav1.LabelSelector{
		{MatchLabels: map[string]string{"bad key": "value"}},
		{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: "Unsupported"}}},
		{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: metav1.LabelSelectorOpIn}}},
	} {
		policy := &dnsv1alpha1.DNSNamingPolicy{Spec: dnsv1alpha1.DNSNamingPolicySpec{ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: "application"}, AdditionalNames: []dnsv1alpha1.DNSAdditionalNameRule{{Selector: selector, DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: "production"}, NamePrefix: "application"}}}}
		response := h.Handle(context.Background(), request("dnsnamingpolicies", "customer", policy, nil))
		if response.Allowed || response.Result == nil || !strings.Contains(response.Result.Message, "invalid naming selector") {
			t.Fatalf("malformed selector was not rejected by validation: %v", response.Result)
		}
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

func TestPublicationStatusDoesNotRenewPublisherObservation(t *testing.T) {
	h := Handler{Client: fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()}
	old := &dnsv1alpha1.DNSRecordContribution{Status: dnsv1alpha1.DNSRecordContributionStatus{WriterEpoch: 4, Sequence: 10, Eligible: true, ValidUntil: ptrTime(time.Now().Add(-time.Minute))}}
	current := old.DeepCopy()
	current.Status.PublishedRevision = 12
	current.Status.Conditions = []metav1.Condition{{Type: "Published", Status: metav1.ConditionTrue}}
	req := request("dnsrecordcontributions", "rbac-authorized-controller", current, old)
	req.SubResource = "status"
	if result := h.Handle(context.Background(), req); !result.Allowed {
		t.Fatalf("publication result needs no producer grant: %v", result.Result)
	}
	current.Status.ValidUntil = ptrTime(time.Now().Add(time.Minute))
	req = request("dnsrecordcontributions", "rbac-authorized-controller", current, old)
	req.SubResource = "status"
	if h.Handle(context.Background(), req).Allowed {
		t.Fatal("publication update renewed producer freshness without a grant")
	}
}
