// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResolverBindingUsesNestedContractAndIndependentCopies(t *testing.T) {
	original := &DNSResolverBinding{Spec: DNSResolverBindingSpec{
		Source:        DNSResolverBindingSource{ProjectUID: "project", ResolverContextRef: DNSObjectReference{Name: "context", UID: "context-uid"}, AccessBindingRef: DNSObjectReference{Name: "access", UID: "access-uid"}},
		Placement:     DNSResolverBindingPlacement{Region: "central", Shard: "shared"},
		Configuration: DNSResolverBindingConfiguration{Generation: 1, Revision: 2, Listeners: DNSResolverBindingListeners{Node: DNSResolverBindingListener{Address: "fd53::1", Port: 53, Transports: []string{"UDP", "TCP"}}, Regional: DNSResolverBindingListener{Address: "fd54::1", Port: 53, Transports: []string{"UDP", "TCP"}}}, ZoneRefs: []DNSObjectReference{{Name: "private", UID: "zone-uid"}}},
		Authorization: DNSResolverAccessAuthorization{WriterEpoch: 3, Sequence: 4, ValidUntil: metav1.Now()},
	}}
	copy := original.DeepCopy()
	copy.Spec.Configuration.ZoneRefs[0].Name = "changed"
	copy.Spec.Configuration.Listeners.Node.Transports[0] = "changed"
	copy.Spec.Configuration.Listeners.Regional.Transports[0] = "changed"
	if original.Spec.Configuration.ZoneRefs[0].Name != "private" || original.Spec.Configuration.Listeners.Node.Transports[0] != "UDP" || original.Spec.Configuration.Listeners.Regional.Transports[0] != "UDP" {
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
	if len(fields) != 4 || fields["source"] == nil || fields["placement"] == nil || fields["configuration"] == nil || fields["authorization"] == nil {
		t.Fatalf("unexpected binding wire structure: %s", raw)
	}
}
