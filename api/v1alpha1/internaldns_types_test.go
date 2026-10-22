package v1alpha1

import (
	"encoding/json"
	"testing"
)

func TestDNSPoliciesSelectResolverContexts(t *testing.T) {
	for name, value := range map[string]any{
		"association context": DNSZoneAssociationSpec{ResolverContextRef: DNSNameReference{Name: "ctx"}},
		"naming context":      DNSNamingPolicySpec{ResolverContextRef: DNSNameReference{Name: "ctx"}},
	} {
		t.Run(name, func(t *testing.T) {
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			var object map[string]any
			if err := json.Unmarshal(data, &object); err != nil {
				t.Fatal(err)
			}
			if _, hasVPC := object["vpcRef"]; hasVPC {
				t.Fatal("DNS policies must not expose VPC references")
			}
			if _, hasContext := object["resolverContextRef"]; !hasContext {
				t.Fatal("expected value-typed resolverContextRef property")
			}
		})
	}
}
