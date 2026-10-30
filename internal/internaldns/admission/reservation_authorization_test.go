package admission

import (
	"context"
	"errors"
	"testing"

	dns "go.miloapis.com/dns-operator/api/v1alpha1"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestServiceReservationAuthorization(t *testing.T) {
	for _, scope := range []string{"instances", "databases"} {
		t.Run(scope, func(t *testing.T) {
			scheme, resolver, zone := managedZoneFixture(t)
			if err := authorizationv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			root := reservationFixture(zone.Name, scope)
			calls := 0
			authorizationUnavailable := false
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone, root, resolver).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				review, ok := obj.(*authorizationv1.SubjectAccessReview)
				if !ok {
					return cl.Create(ctx, obj, opts...)
				}
				calls++
				a := review.Spec.ResourceAttributes
				if a == nil || a.Name != root.Name || a.Namespace != "project" || a.Group != dns.GroupVersion.Group || a.Resource != "dnsregistrations" {
					t.Fatalf("incorrect authorization target: %#v", review.Spec)
				}
				if authorizationUnavailable {
					return errors.New("authorization unavailable")
				}
				review.Status.Allowed = review.Spec.User == "owner" || (review.Spec.User == "publisher" && a.Verb == "publish")
				return nil
			}}).Build()
			h := Handler{Client: cl}
			for _, user := range []string{"tenant", "publisher", "owner"} {
				for _, operation := range []admissionv1.Operation{admissionv1.Create, admissionv1.Update, admissionv1.Delete} {
					req := request("dnsregistrations", user, root, root)
					req.Operation = operation
					if got := h.Handle(context.Background(), req); got.Allowed != (user == "owner") {
						t.Errorf("%s %s reservation: %v", user, operation, got.Result)
					}
				}
				// Clearing the policy must authorize the old object too.
				stripped := root.DeepCopy()
				stripped.Spec.ReservedDescendants = nil
				if got := h.Handle(context.Background(), request("dnsregistrations", user, stripped, root)); got.Allowed != (user == "owner") {
					t.Errorf("%s stripped reservation: %v", user, got.Result)
				}
				terminating := root.DeepCopy()
				now := metav1.Now()
				terminating.DeletionTimestamp = &now
				if got := h.Handle(context.Background(), request("dnsregistrations", user, terminating, root)); got.Allowed != (user == "owner") {
					t.Errorf("%s finalized reservation: %v", user, got.Result)
				}
				contribution := &dns.DNSRecordContribution{Spec: dns.DNSRecordContributionSpec{RegistrationRef: dns.DNSObjectReference{Name: root.Name, UID: root.UID, Generation: root.Generation}, RecordSets: []dns.DNSContributionRecordSet{{RecordType: dns.RRTypeA, Records: []dns.RecordEntry{{Name: "web." + scope, A: &dns.ARecordSpec{Content: "192.0.2.1"}}}}}}}
				if got := h.Handle(context.Background(), request("dnsrecordcontributions", user, contribution, nil)); got.Allowed != (user != "tenant") {
					t.Errorf("%s contribution: %v", user, got.Result)
				}
				contribution.Spec.RecordSets[0].Records[0].Name = "outside"
				if got := h.Handle(context.Background(), request("dnsrecordcontributions", user, contribution, nil)); got.Allowed {
					t.Errorf("%s published outside owned subtree", user)
				}
				for _, name := range []string{scope, "web." + scope, "web." + scope + ".datum.internal."} {
					leaf := root.DeepCopy()
					leaf.Name = "leaf"
					leaf.UID = "leaf"
					leaf.Spec.Name = name
					leaf.Spec.ReservedDescendants = nil
					if got := h.Handle(context.Background(), request("dnsregistrations", user, leaf, nil)); got.Allowed {
						t.Errorf("%s claimed reserved name %s via another registration", user, name)
					}
				}
			}
			if calls == 0 {
				t.Fatal("no authorization checks made")
			}
			authorizationUnavailable = true
			if got := h.Handle(context.Background(), request("dnsregistrations", "owner", root, nil)); got.Allowed {
				t.Fatal("authorization failure opened reservation writes")
			}
			// Tenant-owned private-zone registrations retain their existing semantics.
			tenant := zone.DeepCopy()
			tenant.Name = "tenant"
			tenant.ResourceVersion = ""
			tenant.UID = "tenant"
			if err := cl.Create(context.Background(), tenant); err != nil {
				t.Fatal(err)
			}
			tenantReg := root.DeepCopy()
			tenantReg.Spec.DNSZoneRef.Name = tenant.Name
			before := calls
			if got := h.Handle(context.Background(), request("dnsregistrations", "tenant", tenantReg, nil)); !got.Allowed {
				t.Fatalf("tenant-owned policy rejected: %v", got.Result)
			}
			if calls != before {
				t.Fatal("tenant-owned zone required platform reservation permissions")
			}
			if err := cl.Delete(context.Background(), tenant); err != nil {
				t.Fatal(err)
			}
			orphanRequest := request("dnsregistrations", "tenant", nil, tenantReg)
			orphanRequest.Operation = admissionv1.Delete
			if got := h.Handle(context.Background(), orphanRequest); !got.Allowed {
				t.Fatalf("tenant orphan cleanup rejected: %v", got.Result)
			}
			authorizationUnavailable = false
			if err := cl.Delete(context.Background(), zone); err != nil {
				t.Fatal(err)
			}
			for _, user := range []string{"tenant", "owner"} {
				req := request("dnsregistrations", user, nil, root)
				req.Operation = admissionv1.Delete
				if got := h.Handle(context.Background(), req); got.Allowed != (user == "owner") {
					t.Errorf("%s retirement after zone loss: %v", user, got.Result)
				}
			}

		})
	}
}

func TestReservationAuthorizationPreservesIdentity(t *testing.T) {
	scheme, _, zone := managedZoneFixture(t)
	if err := authorizationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reg := reservationFixture(zone.Name, "databases")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone).WithInterceptorFuncs(interceptor.Funcs{Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
		sar := obj.(*authorizationv1.SubjectAccessReview)
		if sar.Spec.User != "service" || sar.Spec.UID != "principal" || len(sar.Spec.Groups) != 1 || sar.Spec.Groups[0] != "service-group" || sar.Spec.Extra["project"][0] != "project-id" {
			t.Fatalf("identity lost: %#v", sar.Spec)
		}
		sar.Status.Allowed = true
		return nil
	}}).Build()
	req := request("dnsregistrations", "service", reg, nil)
	req.UserInfo = authenticationv1.UserInfo{Username: "service", UID: "principal", Groups: []string{"service-group"}, Extra: map[string]authenticationv1.ExtraValue{"project": {"project-id"}}}
	if got := (&Handler{Client: cl}).Handle(context.Background(), req); !got.Allowed {
		t.Fatalf("service declaration rejected: %v", got.Result)
	}
}
