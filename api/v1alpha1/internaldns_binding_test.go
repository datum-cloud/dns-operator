// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"encoding/json"
	"testing"
)

func TestResolverBindingUsesNestedContractAndIndependentCopies(t *testing.T) {
	original := &DNSResolverBinding{Spec: DNSResolverBindingSpec{
		Source:        DNSResolverBindingSource{ProjectUID: "project", ResolverContextRef: DNSObjectReference{Name: "context", UID: "context-uid"}, AccessBindingRef: DNSObjectReference{Name: "access", UID: "access-uid"}},
		Placement:     DNSResolverBindingPlacement{Region: "central", Shard: "shared"},
		Configuration: DNSResolverBindingConfiguration{Generation: 1, Revision: 2, Query: DNSResolverBindingQuery{NodeAddress: "fd53::1", RegionalAddress: "fd54::1", Transports: []string{"UDP", "TCP"}}, ZoneRefs: []DNSObjectReference{{Name: "private", UID: "zone-uid"}}},
		Version:       DNSResolverAccessVersion{Epoch: 3, Sequence: 4},
	}}
	copy := original.DeepCopy()
	copy.Spec.Configuration.ZoneRefs[0].Name = "changed"
	copy.Spec.Configuration.Query.Transports[0] = "changed"
	if original.Spec.Configuration.ZoneRefs[0].Name != "private" || original.Spec.Configuration.Query.Transports[0] != "UDP" {
		t.Fatal("deep copy aliases nested API collections")
	}
	raw, err := json.Marshal(original.Spec)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 4 || fields["source"] == nil || fields["placement"] == nil || fields["configuration"] == nil || fields["version"] == nil {
		t.Fatalf("unexpected binding wire structure: %s", raw)
	}
}
