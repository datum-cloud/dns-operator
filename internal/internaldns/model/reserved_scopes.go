package model

import (
	"fmt"
	"slices"
	"strings"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ReservedScope reserves a relative subtree of every resolver managed zone.
// Publishers are authenticated Kubernetes usernames, subject to existing registration
// and API authorization. An empty list reserves the subtree without a publisher.
type ReservedScope struct {
	Name       string   `json:"name"`
	Publishers []string `json:"publishers"`
}

type ReservedScopes []ReservedScope

func (scopes ReservedScopes) Validate() error {
	if scopes == nil {
		return fmt.Errorf("reservedScopes is required; use [] only to explicitly disable reservations")
	}
	for i, scope := range scopes {
		if len(validation.IsDNS1123Subdomain(scope.Name)) != 0 {
			return fmt.Errorf("reservedScopes[%d].name must be a lowercase relative DNS subtree", i)
		}
		for _, subject := range scope.Publishers {
			if strings.TrimSpace(subject) == "" || subject != strings.TrimSpace(subject) {
				return fmt.Errorf("reservedScopes[%d] has an invalid publisher username", i)
			}
		}
		for _, other := range scopes[:i] {
			if Within(scope.Name, other.Name) || Within(other.Name, scope.Name) {
				return fmt.Errorf("reserved scopes %q and %q overlap", scope.Name, other.Name)
			}
		}
	}
	return nil
}

// Within compares DNS labels, including the subtree root, case insensitively.
func Within(name, subtree string) bool {
	name, subtree = scopeName(name), scopeName(subtree)
	return name == subtree || strings.HasSuffix(name, "."+subtree)
}
func scopeName(s string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
}

func (scopes ReservedScopes) Match(zone *dnsv1alpha1.DNSZone, owner string) *ReservedScope {
	if zone.Spec.Visibility != dnsv1alpha1.DNSZoneVisibilityPrivate || !IsManagedZone(zone) {
		return nil
	}
	name := ZoneRelativeOwner(owner, zone.Spec.DomainName)
	for i := range scopes {
		if Within(name, scopes[i].Name) {
			return &scopes[i]
		}
	}
	return nil
}

func (scopes ReservedScopes) Authorize(zone *dnsv1alpha1.DNSZone, owner, username string) error {
	if scope := scopes.Match(zone, owner); scope != nil && !slices.Contains(scope.Publishers, username) {
		return fmt.Errorf("ReservedScope: %q is reserved and %q is not an authorized publisher", scope.Name+"."+zone.Spec.DomainName, username)
	}
	return nil
}

// Shadows rejects a tenant zone that would capture any reserved name in this
// context. It uses declared namespace intent, so no zone or record must exist.
func (scopes ReservedScopes) Shadows(zone *dnsv1alpha1.DNSZone, resolver *dnsv1alpha1.DNSResolverContext) bool {
	if zone.Spec.Visibility != dnsv1alpha1.DNSZoneVisibilityPrivate || !resolver.Spec.ManagedNamespace.Enabled {
		return false
	}
	if zone.Name == ManagedZoneName(string(resolver.UID)) && IsManagedZone(zone) {
		return false
	}
	apexes := []string{resolver.Spec.ManagedNamespace.DomainSuffix, resolver.Status.ManagedNamespace.Suffix}
	for _, apex := range apexes {
		if apex == "" {
			continue
		}
		for _, scope := range scopes {
			root := scope.Name + "." + apex
			if Within(zone.Spec.DomainName, root) || (Within(root, zone.Spec.DomainName) && Within(zone.Spec.DomainName, apex)) {
				return true
			}
		}
	}
	return false
}
