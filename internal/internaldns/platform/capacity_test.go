// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"fmt"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func capacityBinding(index int) model.Binding {
	return model.Binding{BindingUID: fmt.Sprintf("binding-%d", index), ProjectUID: "project", ContextUID: fmt.Sprintf("context-%d", index), BindingGeneration: 1, ConfigurationRevision: 1, ConsumerAddress: fmt.Sprintf("fd53::%d", index), ClusterAddress: fmt.Sprintf("fd54::%d", index), Port: 53, Transports: []model.Transport{model.TransportUDP, model.TransportTCP}, Authorization: model.Authorization{IssuerEpoch: 1, Revision: 1}}
}

func TestCapacityOverflowDoesNotBlockWithdrawal(t *testing.T) {
	prior := model.ServingSnapshot{Region: "east", Shard: "a", ConfigurationEpoch: 1, ConfigurationRevision: 1, GeneratedAt: time.Now(), Bindings: []model.Binding{capacityBinding(1), capacityBinding(2)}}
	candidate := prior
	candidate.ConfigurationRevision++
	candidate.Bindings = []model.Binding{capacityBinding(2), capacityBinding(3), capacityBinding(4)}
	bounded, rejected := fitSnapshot(candidate, prior, 2, 512*1024)
	if len(bounded.Bindings) != 2 || len(rejected) != 1 {
		t.Fatalf("capacity result: %#v, rejected %#v", bounded.Bindings, rejected)
	}
	for _, binding := range bounded.Bindings {
		if binding.BindingUID == "binding-1" {
			t.Fatal("overflow prevented withdrawal")
		}
	}
	if bounded.Bindings[0].BindingUID != "binding-2" {
		t.Fatal("new allocations displaced an existing context")
	}
}

func TestByteOverflowPreservesExistingContext(t *testing.T) {
	prior := model.ServingSnapshot{Region: "east", Shard: "a", ConfigurationEpoch: 1, ConfigurationRevision: 1, GeneratedAt: time.Now(), Bindings: []model.Binding{capacityBinding(1)}}
	candidate := prior
	binding := capacityBinding(1)
	for i := range 256 {
		binding.Zones = append(binding.Zones, model.ZoneAttachment{ZoneUID: fmt.Sprintf("zone-%d", i), Apex: fmt.Sprintf("zone-%d.internal", i), RequiredPublicationEpoch: 1, RequiredPublicationRevision: 1})
	}
	binding.ConfigurationRevision++
	candidate.Bindings = []model.Binding{binding, capacityBinding(2)}
	bounded, rejected := fitSnapshot(candidate, prior, 2, 4096)
	if !rejected["binding-1"] || len(bounded.Bindings) != 2 || bounded.Bindings[0].ConfigurationRevision != 1 {
		t.Fatalf("overflow interrupted an established context: %#v", bounded)
	}
}
