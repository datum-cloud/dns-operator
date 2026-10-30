package model

import (
	"fmt"
	"sort"
	"strings"

	dns "go.miloapis.com/dns-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ReservedScope is derived from a service-owned registration, never from DNS
// deployment configuration. Its lifetime is the registration's lifetime.
type ReservedScope struct {
	Name            string
	RegistrationUID types.UID
}
type ReservedScopes []ReservedScope

func Within(name, subtree string) bool {
	name, subtree = scopeName(name), scopeName(subtree)
	return name == subtree || strings.HasSuffix(name, "."+subtree)
}
func scopeName(s string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
}

func ValidateReservation(reg *dns.DNSRegistration, apex string) error {
	for _, name := range reg.Spec.ReservedDescendants {
		if len(validation.IsDNS1123Subdomain(ZoneRelativeOwner(name, apex))) != 0 {
			return fmt.Errorf("reserved descendant %q must name a DNS subtree below the zone apex", name)
		}
	}
	return nil
}

// RegistrationScopes derives all declared subtrees, including terminating
// reservations until actual deletion. Conflicting declarations remain protected:
// Owner permits publication only when every matching declaration has that UID.
// This fails closed even if concurrent admissions both observed no conflict.
func RegistrationScopes(zoneName, apex string, regs []dns.DNSRegistration) ReservedScopes {
	var scopes ReservedScopes
	for _, reg := range regs {
		if reg.Spec.DNSZoneRef.Name != zoneName || reg.UID == "" {
			continue
		}
		for _, name := range reg.Spec.ReservedDescendants {
			if len(validation.IsDNS1123Subdomain(ZoneRelativeOwner(name, apex))) != 0 {
				continue
			}
			scopes = append(scopes, ReservedScope{Name: ZoneRelativeOwner(name, apex), RegistrationUID: reg.UID})
		}
	}
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].Name != scopes[j].Name {
			return scopes[i].Name < scopes[j].Name
		}
		return scopes[i].RegistrationUID < scopes[j].RegistrationUID
	})
	return scopes
}

func (scopes ReservedScopes) Match(owner, apex string) bool {
	name := ZoneRelativeOwner(owner, apex)
	for _, scope := range scopes {
		if Within(name, scope.Name) {
			return true
		}
	}
	return false
}

// Owner enforces exclusive ownership across record types and descendant claims.
func (scopes ReservedScopes) Owner(reg *dns.DNSRegistration, apex string) bool {
	for _, scope := range scopes {
		if scope.RegistrationUID == reg.UID {
			continue
		}
		if Within(ZoneRelativeOwner(reg.Spec.Name, apex), scope.Name) {
			return false
		}
		for _, name := range reg.Spec.ReservedDescendants {
			relative := ZoneRelativeOwner(name, apex)
			if Within(relative, scope.Name) || Within(scope.Name, relative) {
				return false
			}
		}
	}
	return true
}

func (scopes ReservedScopes) Shadows(domain, apex string) bool {
	for _, scope := range scopes {
		root := scope.Name + "." + apex
		if Within(domain, root) || (Within(root, domain) && Within(domain, apex)) {
			return true
		}
	}
	return false
}

// ShadowsContext consults declarations for this resolver's deterministic managed
// zone. No endpoint records or Accepted status are needed to activate a policy.
func ShadowsContext(zone *dns.DNSZone, resolver *dns.DNSResolverContext, regs []dns.DNSRegistration) bool {
	if zone.Spec.Visibility != dns.DNSZoneVisibilityPrivate || !resolver.Spec.ManagedNamespace.Enabled {
		return false
	}
	managedName := ManagedZoneName(string(resolver.UID))
	if zone.Name == managedName && IsManagedZone(zone) {
		return false
	}
	for _, apex := range []string{resolver.Spec.ManagedNamespace.DomainSuffix, resolver.Status.ManagedNamespace.Suffix} {
		if apex != "" && RegistrationScopes(managedName, apex, regs).Shadows(zone.Spec.DomainName, apex) {
			return true
		}
	}
	return false
}
