package model

import (
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReservedScopes(t *testing.T) {
	controller := true
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: ManagedZoneName("ctx"), OwnerReferences: []metav1.OwnerReference{{APIVersion: dnsv1alpha1.GroupVersion.String(), Kind: "DNSResolverContext", Name: "ctx", UID: "ctx", Controller: &controller}}}, Spec: dnsv1alpha1.DNSZoneSpec{Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate, DomainName: "corp.internal"}}
	for _, name := range []string{"instances", "databases"} {
		scopes := ReservedScopes{{Name: name, Publishers: []string{"publisher"}}}
		for _, owner := range []string{name, "web." + name, "a.b." + name, "WEB." + name + ".CORP.INTERNAL.", "web." + name + ".corp.internal"} {
			if scopes.Match(zone, owner) == nil || scopes.Authorize(zone, owner, "tenant") == nil || scopes.Authorize(zone, owner, "publisher") != nil {
				t.Errorf("scope %s owner %s: incorrect authorization", name, owner)
			}
		}
		for _, owner := range []string{"@", "corp.internal.", "my" + name, name + ".web", "web", "web." + name + ".other.internal."} {
			if scopes.Match(zone, owner) != nil {
				t.Errorf("scope %s owner %s: false match", name, owner)
			}
		}
		copy := zone.DeepCopy()
		copy.Name = "tenant"
		if scopes.Match(copy, name) != nil {
			t.Fatal("copied owner reference reserved tenant zone")
		}
		copy = zone.DeepCopy()
		copy.Spec.Visibility = dnsv1alpha1.DNSZoneVisibilityPublic
		if scopes.Match(copy, name) != nil {
			t.Fatal("reserved public zone")
		}
		scopes[0].Publishers = nil
		if scopes.Authorize(zone, name, "publisher") == nil {
			t.Fatal("empty publishers failed open")
		}
	}
}

func TestReservedScopeValidation(t *testing.T) {
	for _, scopes := range []ReservedScopes{nil, {{Name: ""}}, {{Name: "@"}}, {{Name: "*.svc"}}, {{Name: "svc."}}, {{Name: "SVC"}}, {{Name: "svc"}, {Name: "a.svc"}}, {{Name: "svc"}, {Name: "svc"}}, {{Name: "svc", Publishers: []string{""}}}} {
		if scopes.Validate() == nil {
			t.Errorf("invalid scopes accepted: %#v", scopes)
		}
	}
	for _, scopes := range []ReservedScopes{{}, {{Name: "instances"}, {Name: "databases"}}, {{Name: "a.svc"}, {Name: "b.svc"}}} {
		if err := scopes.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReservedScopeShadowingBeforeZoneCreation(t *testing.T) {
	resolver := &dnsv1alpha1.DNSResolverContext{ObjectMeta: metav1.ObjectMeta{UID: "ctx"}, Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "consumer", ManagedNamespace: dnsv1alpha1.DNSResolverContextManagedNamespace{Enabled: true, DomainSuffix: OpaqueToken("consumer") + ".internal"}}}
	apex := OpaqueToken("consumer") + ".internal"
	for _, name := range []string{"instances", "databases", "nested.services"} {
		scopes := ReservedScopes{{Name: name}}
		for _, domain := range []string{name + "." + apex, "web." + name + "." + apex + ".", apex} {
			zone := &dnsv1alpha1.DNSZone{Spec: dnsv1alpha1.DNSZoneSpec{Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate, DomainName: domain}}
			if !scopes.Shadows(zone, resolver) {
				t.Errorf("allowed shadow %s", domain)
			}
		}
		for _, domain := range []string{"other." + apex, "my" + name + "." + apex, name + ".other.internal", "internal"} {
			zone := &dnsv1alpha1.DNSZone{Spec: dnsv1alpha1.DNSZoneSpec{Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate, DomainName: domain}}
			if scopes.Shadows(zone, resolver) {
				t.Errorf("rejected unrelated zone %s", domain)
			}
		}
	}
	scopes := ReservedScopes{{Name: "nested.services"}}
	zone := &dnsv1alpha1.DNSZone{Spec: dnsv1alpha1.DNSZoneSpec{Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate, DomainName: "services." + apex}}
	if !scopes.Shadows(zone, resolver) {
		t.Fatal("intermediate zone can shadow reserved subtree")
	}
	resolver.Spec.ManagedNamespace.Enabled = false
	if scopes.Shadows(zone, resolver) {
		t.Fatal("reserved unmanaged context")
	}
}
