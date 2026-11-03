package admission

import (
	"context"
	"strings"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRecordSetReservedScopes(t *testing.T) {
	for _, scope := range []string{"instances", "databases"} {
		t.Run(scope, func(t *testing.T) {
			scheme, _, managed := managedZoneFixture(t)
			tenant := managed.DeepCopy()
			tenant.Name = "tenant"
			public := managed.DeepCopy()
			public.Name = "public"
			public.Spec.Visibility = dnsv1alpha1.DNSZoneVisibilityPublic
			h := Handler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(managed, tenant, public, reservationFixture(managed.Name, scope)).Build()}
			for _, tc := range []struct {
				zone, owner string
				allowed     bool
			}{
				{managed.Name, scope, false}, {managed.Name, "web." + scope, false},
				{managed.Name, "WEB." + strings.ToUpper(scope) + ".DATUM.INTERNAL.", false},
				{managed.Name, "web." + scope + ".datum.internal", false},
				{managed.Name, "my" + scope, true}, {managed.Name, scope + ".web", true}, {managed.Name, "@", true},
				{tenant.Name, "web." + scope, true}, {public.Name, "web." + scope, true},
			} {
				for _, user := range []string{"tenant", "publisher"} {
					rs := &dnsv1alpha1.DNSRecordSet{Spec: dnsv1alpha1.DNSRecordSetSpec{DNSZoneRef: corev1.LocalObjectReference{Name: tc.zone}, Records: []dnsv1alpha1.RecordEntry{{Name: tc.owner}}}}
					if got := h.Handle(context.Background(), request("dnsrecordsets", user, rs, nil)); got.Allowed != tc.allowed {
						t.Fatalf("%s %s %s: %v", tc.zone, tc.owner, user, got.Result)
					}
				}
			}
		})
	}
}

func TestShadowingWithoutManagedZoneOrRecords(t *testing.T) {
	for _, scope := range []string{"instances", "databases"} {
		t.Run(scope, func(t *testing.T) {
			scheme, resolver, _ := managedZoneFixture(t)
			shadow := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "shadow", Namespace: "project", UID: "shadow"}, Spec: dnsv1alpha1.DNSZoneSpec{Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate, DomainName: "web." + scope + ".datum.internal"}}
			h := Handler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(resolver, shadow, reservationFixture(model.ManagedZoneName(string(resolver.UID)), scope)).Build()}
			a := &dnsv1alpha1.DNSZoneAssociation{Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: shadow.Name}, ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: resolver.Name}}}
			if got := h.Handle(context.Background(), request("dnszoneassociations", "tenant", a, nil)); got.Allowed || !strings.Contains(got.Result.Message, "ReservedScope") {
				t.Fatalf("shadow allowed before managed zone creation: %v", got)
			}
		})
	}
}

func TestNoServiceDeclarationMeansNoReservation(t *testing.T) {
	scheme, _, zone := managedZoneFixture(t)
	h := Handler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone).Build()}
	reg := reservationFixture(zone.Name, "instances")
	reg.Spec.ReservedDescendants = nil
	if got := h.Handle(context.Background(), request("dnsregistrations", "tenant", reg, nil)); !got.Allowed {
		t.Fatalf("undeclared name is reserved: %v", got.Result)
	}
}

func reservationFixture(zone, scope string) *dnsv1alpha1.DNSRegistration {
	return &dnsv1alpha1.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: "service-policy", Namespace: "project", UID: "service-policy", Generation: 1}, Spec: dnsv1alpha1.DNSRegistrationSpec{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: zone}, Name: scope, ReservedDescendants: []string{scope}, RecordTypes: []dnsv1alpha1.RRType{dnsv1alpha1.RRTypeA}, TTLSeconds: 30}}
}

func TestReservedRecordSetMetadataCleanup(t *testing.T) {
	for _, scope := range []string{"instances", "databases"} {
		t.Run(scope, func(t *testing.T) {
			scheme, _, zone := managedZoneFixture(t)
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone, reservationFixture(zone.Name, scope)).Build()
			h := Handler{Client: cl}
			old := &dnsv1alpha1.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "legacy", Finalizers: []string{"cleanup"}}, Spec: dnsv1alpha1.DNSRecordSetSpec{DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name}, RecordType: dnsv1alpha1.RRTypeA, Records: []dnsv1alpha1.RecordEntry{{Name: "web." + scope, A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.1"}}}}}
			next := old.DeepCopy()
			next.Finalizers = nil
			next.Labels = map[string]string{"cleanup": "true"}
			if got := h.Handle(context.Background(), request("dnsrecordsets", "tenant", next, old)); !got.Allowed {
				t.Fatalf("metadata cleanup denied: %v", got.Result)
			}
			changed := next.DeepCopy()
			changed.Spec.Records[0].A.Content = "192.0.2.2"
			if got := h.Handle(context.Background(), request("dnsrecordsets", "tenant", changed, old)); got.Allowed {
				t.Fatal("spec mutation bypassed reservation")
			}
			req := request("dnsrecordsets", "tenant", nil, old)
			req.Operation = admissionv1.Delete
			if got := h.Handle(context.Background(), req); !got.Allowed {
				t.Fatal("cleanup deletion denied")
			}
			if err := cl.Delete(context.Background(), zone); err != nil {
				t.Fatal(err)
			}
			if got := h.Handle(context.Background(), request("dnsrecordsets", "tenant", next, old)); !got.Allowed {
				t.Fatal("metadata cleanup required a live zone")
			}
		})
	}
}
