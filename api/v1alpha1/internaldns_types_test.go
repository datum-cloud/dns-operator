package v1alpha1

import (
	"encoding/json"
	"testing"
)

// Value-typed references with omitempty encode their unselected peer as {}.
// The CRD CEL tests a nonempty selected name so Go clients can omit its peer.
func TestConsumerSelectorJSONUsesEmptyUnselectedReference(t *testing.T) {
	for name, value := range map[string]any{
		"association context": DNSZoneAssociationSpec{ResolverContextRef: DNSNameReference{Name: "ctx"}},
		"naming legacy":       DNSNamingPolicySpec{VPCRef: DNSNameReference{Name: "vpc"}},
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
