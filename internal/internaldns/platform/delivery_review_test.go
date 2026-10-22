// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCapacityReductionCannotDropEstablishedBindings(t *testing.T) {
	prior := model.ServingSnapshot{Region: "east", Shard: "a", ConfigurationEpoch: 1, ConfigurationRevision: 1, GeneratedAt: time.Now(), Bindings: []model.Binding{capacityBinding(1), capacityBinding(2)}}
	for _, limits := range [][2]int{{1, 512 * 1024}, {2, 1}} {
		_, _, err := fitSnapshot(prior, prior, limits[0], limits[1])
		if err == nil {
			t.Fatalf("accepted limits %v that drop an established context", limits)
		}
	}
}

func TestPlannerWaitsForPublicationActivation(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	ns, region, shard := "platform", "east", "a"
	state := shardState{Holder: "planner", Epoch: 1, NextRevision: 1, LeaseUntil: now.Add(time.Minute)}
	data, _ := json.Marshal(state)
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "dns-shard-owner-" + model.OpaqueToken(region+"/"+shard), Namespace: ns, UID: "owner"}, Data: map[string]string{"state": string(data)}}
	zone := &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-" + model.OpaqueToken("zone")[:20], Namespace: ns}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ZoneUID: "zone", WriterEpoch: 1, ActiveManifestName: "manifest"}}
	manifest := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: "manifest", Namespace: ns}, Spec: dnsv1alpha1.DNSPublicationManifestSpec{ZoneRef: dnsv1alpha1.DNSObjectReference{UID: "zone"}, ZoneApex: "datum.internal", WriterEpoch: 1, Revision: 2, ServingTargets: []dnsv1alpha1.DNSPublicationTarget{{Region: region, Shard: shard}}}}
	binding := &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding", Namespace: ns, UID: "binding"}, Spec: dnsv1alpha1.DNSResolverBindingSpec{Source: dnsv1alpha1.DNSResolverBindingSource{ProjectUID: "project", ResolverContextRef: dnsv1alpha1.DNSObjectReference{UID: "context"}}, Placement: dnsv1alpha1.DNSResolverBindingPlacement{Region: region, Shard: shard}, Version: dnsv1alpha1.DNSResolverAccessVersion{Epoch: 1, Sequence: 1}, Configuration: dnsv1alpha1.DNSResolverBindingConfiguration{Generation: 1, Revision: 1, Query: dnsv1alpha1.DNSResolverBindingQuery{NodeAddress: "fd53::1", RegionalAddress: "fd54::1"}, ZoneRefs: []dnsv1alpha1.DNSObjectReference{{UID: "zone"}}}}}
	binding.Labels = map[string]string{"internal-dns.miloapis.com/region": model.SafeToken(region), "internal-dns.miloapis.com/shard": model.SafeToken(shard)}
	cl := testClient(t, owner, zone, manifest, binding)
	p := Planner{Client: capacityConflictClient{Client: cl}, Allocator: &Allocator{Client: cl, Namespace: ns}, Config: PlannerConfig{Namespace: ns, Region: region, Shard: shard, Identity: "planner"}, Now: func() time.Time { return now }}
	if err := p.Step(ctx); err != nil {
		t.Fatal(err)
	}
	var outs dnsv1alpha1.DNSTransportOutboxList
	if err := cl.List(ctx, &outs); err != nil {
		t.Fatal(err)
	}
	if len(outs.Items) != 1 {
		t.Fatalf("outboxes=%d", len(outs.Items))
	}
	out := outs.Items[0]
	activation := model.PublicationActivationName(manifest.Name, region, shard)
	if len(out.Spec.DependsOn) != 1 || out.Spec.DependsOn[0] != activation {
		t.Fatalf("activation fence missing: %v", out.Spec.DependsOn)
	}
	var envelope model.Envelope
	var snapshot model.ServingSnapshot
	_ = json.Unmarshal(out.Spec.Payload, &envelope)
	_ = json.Unmarshal(envelope.Payload, &snapshot)
	if len(snapshot.RequiredPublications) != 1 || snapshot.RequiredPublications[0].Revision != 2 {
		t.Fatalf("publication fence missing: %+v", snapshot.RequiredPublications)
	}
	worker := Outbox{Client: cl, Namespace: ns}
	ready, err := worker.dependencyReady(ctx, envelope, activation, map[string]string{})
	if err != nil || ready {
		t.Fatalf("undelivered activation accepted: %v %v", ready, err)
	}
	dependency := &dnsv1alpha1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: activation, Namespace: ns}, Spec: dnsv1alpha1.DNSTransportOutboxSpec{ManifestRef: dnsv1alpha1.DNSObjectReference{Name: manifest.Name}}}
	if err := cl.Create(ctx, dependency); err != nil {
		t.Fatal(err)
	}
	ready, err = worker.dependencyReady(ctx, envelope, activation, map[string]string{})
	if err != nil || ready {
		t.Fatalf("pending activation accepted: %v %v", ready, err)
	}
	dependency.Status.State = dnsValueAcknowledged
	if err := cl.Status().Update(ctx, dependency); err != nil {
		t.Fatal(err)
	}
	ready, err = worker.dependencyReady(ctx, envelope, activation, map[string]string{})
	if err != nil || !ready {
		t.Fatalf("delivered activation rejected: %v %v", ready, err)
	}
	var before corev1.ConfigMap
	if err := cl.Get(ctx, client.ObjectKeyFromObject(owner), &before); err != nil {
		t.Fatal(err)
	}
	p.Config.MaxSnapshotBytes = 1
	if err := p.Step(ctx); err == nil {
		t.Fatal("oversized envelope accepted")
	}
	var after corev1.ConfigMap
	if err := cl.Get(ctx, client.ObjectKeyFromObject(owner), &after); err != nil {
		t.Fatal(err)
	}
	if before.Data["state"] != after.Data["state"] {
		t.Fatal("size rejection consumed a revision")
	}

}

type capacityConflictClient struct{ client.Client }
type capacityConflictStatus struct{ client.SubResourceWriter }

func (c capacityConflictClient) Status() client.SubResourceWriter {
	return capacityConflictStatus{SubResourceWriter: c.Client.Status()}
}

func (c capacityConflictStatus) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	if _, ok := obj.(*dnsv1alpha1.DNSResolverBinding); ok {
		return fmt.Errorf("injected binding status conflict")
	}
	return c.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}
