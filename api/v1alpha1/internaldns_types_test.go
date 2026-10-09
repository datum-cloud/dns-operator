package v1alpha1

import (
	"encoding/json"
	"testing"
)

// Value-typed references with omitempty encode their unselected peer as {}.
// The CRD CEL deliberately tests a complete name/UID pin, rather than mere
// property presence, so this normal Go client representation remains valid.
func TestConsumerSelectorJSONUsesEmptyUnselectedReference(t *testing.T) {
	for name, value := range map[string]any{
		"association context": DNSZoneAssociationSpec{ResolverContextRef: DNSObjectReference{Name: "ctx", UID: "ctx-uid"}},
		"naming legacy":       DNSNamingPolicySpec{VPCRef: DNSObjectReference{Name: "vpc", UID: "vpc-uid"}},
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
			if _, hasVPC := object["vpcRef"]; !hasVPC {
				t.Fatal("expected value-typed vpcRef property")
			}
			if _, hasContext := object["resolverContextRef"]; !hasContext {
				t.Fatal("expected value-typed resolverContextRef property")
			}
		})
	}
}
