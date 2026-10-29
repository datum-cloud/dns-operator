package admission

import (
	"context"
	"strings"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
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
			h := Handler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(managed, tenant, public).Build(), ReservedScopes: model.ReservedScopes{{Name: scope, Publishers: []string{"publisher"}}}}
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
			h := Handler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(resolver, shadow).Build(), ReservedScopes: model.ReservedScopes{{Name: scope}}}
			a := &dnsv1alpha1.DNSZoneAssociation{Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: shadow.Name}, ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: resolver.Name}}}
			if got := h.Handle(context.Background(), request("dnszoneassociations", "tenant", a, nil)); got.Allowed || !strings.Contains(got.Result.Message, "ReservedScope") {
				t.Fatalf("shadow allowed before managed zone creation: %v", got)
			}
		})
	}
}

func TestUnconfiguredManagedAdmissionFailsClosed(t *testing.T) {
	scheme, _, zone := managedZoneFixture(t)
	h := Handler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone).Build()}
	reg := &dnsv1alpha1.DNSRegistration{Spec: dnsv1alpha1.DNSRegistrationSpec{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: zone.Name}, Name: "web.instances"}}
	if got := h.Handle(context.Background(), request("dnsregistrations", "tenant", reg, nil)); got.Allowed || !strings.Contains(got.Result.Message, "not configured") {
		t.Fatalf("missing policy failed open: %v", got)
	}
	h.ReservedScopes = model.ReservedScopes{}
	if got := h.Handle(context.Background(), request("dnsregistrations", "tenant", reg, nil)); !got.Allowed {
		t.Fatalf("explicit empty policy rejected: %v", got)
	}
}
