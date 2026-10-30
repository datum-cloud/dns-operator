package model

import (
	"testing"

	dns "go.miloapis.com/dns-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestServiceDeclarations(t *testing.T) {
	for _, scope := range []string{"instances", "databases"} {
		t.Run(scope, func(t *testing.T) {
			root := dns.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: "service", UID: "service"}, Spec: dns.DNSRegistrationSpec{DNSZoneRef: dns.DNSNameReference{Name: "managed"}, Name: scope, ReservedDescendants: []string{scope + ".datum.internal."}}}
			scopes := RegistrationScopes("managed", "datum.internal", []dns.DNSRegistration{root})
			for _, name := range []string{scope, "web." + scope, "WEB." + scope + ".DATUM.INTERNAL."} {
				if !scopes.Match(name, "datum.internal") {
					t.Errorf("unprotected %s", name)
				}
			}
			for _, name := range []string{"my" + scope, scope + ".web", "@", "web.other"} {
				if scopes.Match(name, "datum.internal") {
					t.Errorf("reserved sibling %s", name)
				}
			}
			if !scopes.Owner(&root, "datum.internal") {
				t.Fatal("owner rejected")
			}
			other := root.DeepCopy()
			other.UID = "other"
			if scopes.Owner(other, "datum.internal") {
				t.Fatal("overlapping owner accepted")
			}
			other.Spec.ReservedDescendants = nil
			other.Spec.Name = "web." + scope
			if scopes.Owner(other, "datum.internal") {
				t.Fatal("child registration accepted")
			}
			for _, domain := range []string{scope + ".datum.internal", "web." + scope + ".datum.internal", "datum.internal"} {
				if !scopes.Shadows(domain, "datum.internal") {
					t.Errorf("shadow accepted: %s", domain)
				}
			}
			if scopes.Shadows("my"+scope+".datum.internal", "datum.internal") || scopes.Shadows(scope+".other.internal", "datum.internal") {
				t.Fatal("unrelated zone blocked")
			}
			root.DeletionTimestamp = &metav1.Time{Time: metav1.Now().Time}
			if len(RegistrationScopes("managed", "datum.internal", []dns.DNSRegistration{root})) != 1 {
				t.Fatal("terminating policy released before deletion")
			}
			if len(RegistrationScopes("other", "datum.internal", []dns.DNSRegistration{root})) != 0 {
				t.Fatal("policy crossed zone boundary")
			}
		})
	}
}

func TestInvalidReservations(t *testing.T) {
	for _, scope := range []string{"@", "", "*.instances", "datum.internal.", "bad..label"} {
		reg := dns.DNSRegistration{Spec: dns.DNSRegistrationSpec{ReservedDescendants: []string{scope}}}
		if err := ValidateReservation(&reg, "datum.internal"); err == nil {
			t.Errorf("invalid subtree accepted: %q", scope)
		}
	}
}
