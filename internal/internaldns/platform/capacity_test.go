// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"fmt"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

func TestCapacityReportDoesNotOverwriteAnotherShard(t *testing.T) {
	ctx := context.Background()
	binding := &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding", Namespace: "platform", UID: "binding-uid"}, Spec: dnsv1alpha1.DNSResolverBindingSpec{Placement: dnsv1alpha1.DNSResolverBindingPlacement{Region: "west", Shard: "b"}}, Status: dnsv1alpha1.DNSResolverBindingStatus{Conditions: []metav1.Condition{{Type: "CapacityAvailable", Status: metav1.ConditionFalse, Reason: "ShardCapacity", LastTransitionTime: metav1.Now()}}}}
	cl := testClient(t, binding)
	planner := Planner{Client: cl, Config: PlannerConfig{Region: "east", Shard: "a"}}
	if err := planner.reportCapacity(ctx, []dnsv1alpha1.DNSResolverBinding{*binding}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	var updated dnsv1alpha1.DNSResolverBinding
	if err := cl.Get(ctx, client.ObjectKeyFromObject(binding), &updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Status.Conditions) != 1 || updated.Status.Conditions[0].Status != metav1.ConditionFalse {
		t.Fatal("capacity from another shard was overwritten")
	}
}
