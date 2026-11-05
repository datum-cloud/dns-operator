// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type blockingListClient struct {
	client.Client
	started chan struct{}
	once    sync.Once
}

func (c *blockingListClient) List(ctx context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	c.once.Do(func() { close(c.started) })
	<-ctx.Done()
	return ctx.Err()
}

func TestProjectPublicationContinuesAfterSourceFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	plan := model.PublicationPlan{ZoneUID: "zone-healthy", Apex: "healthy.internal", Registrations: []model.RegistrationFence{{UID: "registration-healthy", Generation: 2}}, RRSets: []model.RRSet{}}
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	chunk := &dnsv1alpha1.DNSPublicationChunk{ObjectMeta: metav1.ObjectMeta{Name: "chunk", Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationChunkSpec{WriterEpoch: 1, Revision: 3, Index: 0, SHA256: model.Hash(payload), Payload: payload}}
	manifest := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: "manifest", Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationManifestSpec{
		ZoneRef: dnsv1alpha1.DNSObjectReference{UID: "zone-healthy"}, ZoneApex: "healthy.internal", WriterEpoch: 1, Revision: 3,
		Chunks: []dnsv1alpha1.DNSPublicationChunkReference{{Name: chunk.Name, SHA256: chunk.Spec.SHA256, Size: int32(len(payload))}}, ContentHash: model.Hash(payload), GeneratedAt: metav1.NewTime(now),
	}}
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "healthy", Namespace: "healthy-project", UID: "zone-healthy", Generation: 4}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "healthy.internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	registration := &dnsv1alpha1.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: zone.Namespace, UID: "registration-healthy", Generation: 2}, Spec: dnsv1alpha1.DNSRegistrationSpec{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: zone.Name}, Name: "api"}}
	healthy := fake.NewClientBuilder().WithScheme(scheme).WithObjects(zone, registration).WithStatusSubresource(&dnsv1alpha1.DNSZone{}, &dnsv1alpha1.DNSRegistration{}).Build()
	blocked := &blockingListClient{Client: healthy, started: make(chan struct{})}
	sink := AckSink{Client: testClient(t, chunk), Namespace: "platform", Projects: []ProjectScope{{Client: blocked, Namespace: "offline-project", RequestTimeout: 200 * time.Millisecond}, {Client: healthy, Namespace: zone.Namespace}}}
	projectionDone := make(chan error, 1)
	go func() { projectionDone <- sink.projectPublication(ctx, manifest, true, now) }()
	<-blocked.started
	deadline := time.Now().Add(150 * time.Millisecond)
	var updated dnsv1alpha1.DNSZone
	for {
		if err := healthy.Get(ctx, client.ObjectKeyFromObject(zone), &updated); err != nil {
			t.Fatal(err)
		}
		condition := apimeta.FindStatusCondition(updated.Status.Conditions, "Published")
		if condition != nil && condition.Status == metav1.ConditionTrue {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("healthy source status waited for the partitioned source timeout")
		}
		time.Sleep(time.Millisecond)
	}
	if err := <-projectionDone; err == nil {
		t.Fatal("offline source timeout was not reported")
	}
	condition := apimeta.FindStatusCondition(updated.Status.Conditions, "Published")
	if condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("healthy source status was blocked by offline project: %#v", updated.Status.Conditions)
	}
	zoneResourceVersion := updated.ResourceVersion
	var updatedRegistration dnsv1alpha1.DNSRegistration
	if err := healthy.Get(ctx, client.ObjectKeyFromObject(registration), &updatedRegistration); err != nil {
		t.Fatal(err)
	}
	registrationResourceVersion := updatedRegistration.ResourceVersion
	if err := sink.projectPublication(ctx, manifest, true, now.Add(time.Minute)); err == nil {
		t.Fatal("offline source failure was not reported on repeated projection")
	}
	if err := healthy.Get(ctx, client.ObjectKeyFromObject(zone), &updated); err != nil {
		t.Fatal(err)
	}
	if err := healthy.Get(ctx, client.ObjectKeyFromObject(registration), &updatedRegistration); err != nil {
		t.Fatal(err)
	}
	if updated.ResourceVersion != zoneResourceVersion || updatedRegistration.ResourceVersion != registrationResourceVersion {
		t.Fatalf("converged project status churned: zone %s→%s registration %s→%s", zoneResourceVersion, updated.ResourceVersion, registrationResourceVersion, updatedRegistration.ResourceVersion)
	}
}

func TestSharedPlannerPreservesOtherProjects(t *testing.T) {
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	region, shard, ns := "east", "shared-01", "platform"
	state := shardState{Holder: "planner", Epoch: 1, NextRevision: 1, LeaseUntil: now.Add(time.Minute)}
	b, _ := json.Marshal(state)
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "dns-shard-owner-" + model.OpaqueToken(region+"/"+shard), Namespace: ns, UID: "shard-owner-uid"}, Data: map[string]string{"state": string(b)}}
	objects := []client.Object{owner}
	for i := 1; i <= 2; i++ {
		zuid := types.UID(fmt.Sprintf("zone-%d", i))
		vuid := types.UID(fmt.Sprintf("vpc-%d", i))
		mname := fmt.Sprintf("manifest-%d", i)
		objects = append(objects, &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-" + model.OpaqueToken(string(zuid))[:20], Namespace: ns}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ZoneUID: zuid, WriterEpoch: 1, ActiveManifestName: mname}})
		objects = append(objects, &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: mname, Namespace: ns}, Spec: dnsv1alpha1.DNSPublicationManifestSpec{ZoneRef: dnsv1alpha1.DNSObjectReference{UID: zuid}, ZoneApex: "prod.internal", WriterEpoch: 1, Revision: 1, ServingTargets: []dnsv1alpha1.DNSPublicationTarget{{Region: region, Shard: shard}}}})
		objects = append(objects, &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("binding-%d", i), Namespace: ns, UID: types.UID(fmt.Sprintf("binding-uid-%d", i)), Labels: map[string]string{"internal-dns.miloapis.com/region": region, "internal-dns.miloapis.com/shard": shard}}, Spec: dnsv1alpha1.DNSResolverBindingSpec{Source: dnsv1alpha1.DNSResolverBindingSource{ProjectUID: types.UID(fmt.Sprintf("project-%d", i)), ResolverContextRef: dnsv1alpha1.DNSObjectReference{Name: string(vuid), UID: vuid}}, Placement: dnsv1alpha1.DNSResolverBindingPlacement{Region: region, Shard: shard}, Configuration: dnsv1alpha1.DNSResolverBindingConfiguration{Generation: 1, Revision: 1, Query: dnsv1alpha1.DNSResolverBindingQuery{NodeAddress: fmt.Sprintf("fd53::%d", i), RegionalAddress: fmt.Sprintf("fd54::%d", i)}, ZoneRefs: zoneReferences([]types.UID{zuid})}, Version: dnsv1alpha1.DNSResolverAccessVersion{Epoch: 1, Sequence: 1}}})
	}
	cl := testClient(t, objects...)
	p := Planner{Client: cl, Allocator: &Allocator{Client: cl, Namespace: ns}, Config: PlannerConfig{Namespace: ns, Region: region, Shard: shard, Identity: "planner", ClusterBackends: []model.Backend{{MemberID: "cluster-1", Address: "fd00::20", Port: 5300}}}, Now: func() time.Time { return now }}
	if err := p.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	var out dnsv1alpha1.DNSTransportOutboxList
	if err := cl.List(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("expected one shared snapshot, got %d", len(out.Items))
	}
	var env model.Envelope
	var snapshot model.ServingSnapshot
	_ = json.Unmarshal(out.Items[0].Spec.Payload, &env)
	_ = json.Unmarshal(env.Payload, &snapshot)
	if len(snapshot.Bindings) != 2 {
		t.Fatal("one project's update removed another project")
	}
	if err := p.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = cl.List(context.Background(), &out)
	if len(out.Items) != 1 {
		t.Fatal("unchanged desired state generated a new configuration revision")
	}
	var changing dnsv1alpha1.DNSResolverBinding
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "binding-1"}, &changing); err != nil {
		t.Fatal(err)
	}
	originalAuthorization := changing.Spec.Version
	changing.Spec.Configuration.Revision++
	changing.Spec.Configuration.ZoneRefs = append(changing.Spec.Configuration.ZoneRefs, dnsv1alpha1.DNSObjectReference{UID: "zone-extra"})
	if err := cl.Update(context.Background(), &changing); err != nil {
		t.Fatal(err)
	}
	if err := p.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	pending := latestPlannerSnapshot(t, &p)
	if len(pending.Bindings) != 2 || !pending.Bindings[0].ConfigurationPending || pending.Bindings[0].Tombstone || len(pending.Bindings[0].Zones) != 0 || pending.Bindings[1].ConfigurationPending {
		t.Fatal("incomplete zone addition withdrew authorization, exposed partial zones, or gated the healthy project")
	}
	if pending.Bindings[0].Version.Sequence != uint64(originalAuthorization.Sequence) {
		t.Fatal("pending configuration renewed the original authorization")
	}
	changing.Spec.Configuration.ZoneApices = map[string]string{"zone-extra": "apps.internal"}
	if err := cl.Update(context.Background(), &changing); err != nil {
		t.Fatal(err)
	}
	if err := p.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	isolated := latestPlannerSnapshot(t, &p)
	if isolated.Bindings[0].ConfigurationPending || len(isolated.Bindings[0].Zones) != 2 || !isolated.Bindings[0].Zones[1].Pending {
		t.Fatalf("a known pending apex suppressed the ready zone: %#v", isolated.Bindings[0])
	}
	for _, obj := range []client.Object{
		&dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-" + model.OpaqueToken("zone-extra")[:20], Namespace: ns}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ZoneUID: "zone-extra", ActiveManifestName: "manifest-extra"}},
		&dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: "manifest-extra", Namespace: ns}, Spec: dnsv1alpha1.DNSPublicationManifestSpec{ZoneRef: dnsv1alpha1.DNSObjectReference{UID: "zone-extra"}, ZoneApex: "apps.internal", WriterEpoch: 1, Revision: 1, ServingTargets: []dnsv1alpha1.DNSPublicationTarget{{Region: region, Shard: shard}}}},
	} {
		if err := cl.Create(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	ready := latestPlannerSnapshot(t, &p)
	if ready.Bindings[0].ConfigurationPending || len(ready.Bindings[0].Zones) != 2 || ready.Bindings[0].Version != pending.Bindings[0].Version {
		t.Fatal("completed zone addition did not recover with the original authorization")
	}
	var untargeted dnsv1alpha1.DNSPublicationManifest
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "manifest-1"}, &untargeted); err != nil {
		t.Fatal(err)
	}
	untargeted.Spec.ServingTargets = nil
	if err := cl.Update(context.Background(), &untargeted); err != nil {
		t.Fatal(err)
	}
	if err := p.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = cl.List(context.Background(), &out)
	if len(out.Items) != 5 {
		t.Fatal("regional export removal did not change the shared snapshot")
	}
	var updatedOwner corev1.ConfigMap
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(owner), &updatedOwner); err != nil {
		t.Fatal(err)
	}
	var updatedState shardState
	_ = json.Unmarshal([]byte(updatedOwner.Data["state"]), &updatedState)
	var current dnsv1alpha1.DNSTransportOutbox
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: updatedState.ActiveOutbox}, &current); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(current.Spec.Payload, &env)
	_ = json.Unmarshal(env.Payload, &snapshot)
	if len(snapshot.Bindings) != 2 || !snapshot.Bindings[0].ConfigurationPending || len(snapshot.Bindings[0].Zones) != 0 || snapshot.Bindings[1].ContextUID != "vpc-2" || snapshot.Bindings[1].ConfigurationPending {
		t.Fatal("untargeted zone remained attached, withdrew access, or gated another project")
	}
	p.Config.Identity = "retired-other-owner"
	if err := p.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = cl.List(context.Background(), &out)
	if len(out.Items) != 5 {
		t.Fatal("another holder published during a current lease")
	}
}

func latestPlannerSnapshot(t *testing.T, p *Planner) model.ServingSnapshot {
	t.Helper()
	ctx := context.Background()
	var owner corev1.ConfigMap
	key := client.ObjectKey{Namespace: p.Config.Namespace, Name: "dns-shard-owner-" + model.OpaqueToken(p.Config.Region+"/"+p.Config.Shard)}
	if err := p.Client.Get(ctx, key, &owner); err != nil {
		t.Fatal(err)
	}
	var state shardState
	if err := json.Unmarshal([]byte(owner.Data["state"]), &state); err != nil {
		t.Fatal(err)
	}
	var out dnsv1alpha1.DNSTransportOutbox
	if err := p.Client.Get(ctx, client.ObjectKey{Namespace: p.Config.Namespace, Name: state.ActiveOutbox}, &out); err != nil {
		t.Fatal(err)
	}
	var env model.Envelope
	if err := json.Unmarshal(out.Spec.Payload, &env); err != nil {
		t.Fatal(err)
	}
	var snapshot model.ServingSnapshot
	if err := json.Unmarshal(env.Payload, &snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

type recordingPublisher struct{ events []model.Envelope }

func (p *recordingPublisher) Publish(_ context.Context, _ string, e model.Envelope) (transport.PublishAck, error) {
	p.events = append(p.events, e)
	return transport.PublishAck{Sequence: uint64(len(p.events))}, nil
}

func TestOutboxDoesNotPublishAnUncommittedRevision(t *testing.T) {
	now := time.Now().UTC()
	state := shardState{Holder: "planner", Epoch: 1, NextRevision: 2, LeaseUntil: now.Add(time.Minute)}
	b, _ := json.Marshal(state)
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "shard-owner", Namespace: "platform", UID: "owner-uid"}, Data: map[string]string{"state": string(b)}}
	snapshot := model.ServingSnapshot{Region: "east", Shard: "shared", ConfigurationEpoch: 1, ConfigurationRevision: 1, GeneratedAt: now}
	env, err := model.NewEnvelope(model.KindServingSnapshot, "event-1", "east", "shared", "owner-uid", 1, 1, now, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(env)
	out := &dnsv1alpha1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: "event-1", Namespace: "platform", UID: "outbox-uid", Labels: map[string]string{shardOwnerLabel: owner.Name}}, Spec: dnsv1alpha1.DNSTransportOutboxSpec{Subject: model.ServingSubject("east", "shared"), ResourceUID: owner.UID, WriterEpoch: 1, Revision: 1, Activation: true, Payload: payload, PayloadHash: model.Hash(payload)}}
	cl := testClient(t, owner, out)
	pub := &recordingPublisher{}
	export := Outbox{Client: cl, Namespace: "platform", Publisher: pub}
	if err := export.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pub.events) != 0 {
		t.Fatal("uncommitted staged revision escaped through NATS")
	}
	var current corev1.ConfigMap
	_ = cl.Get(context.Background(), client.ObjectKeyFromObject(owner), &current)
	state.ActiveOutbox = out.Name
	b, _ = json.Marshal(state)
	current.Data["state"] = string(b)
	if err := cl.Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	if err := export.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pub.events) != 1 {
		t.Fatal("committed revision was not exported")
	}
	if err := export.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(pub.events) != 1 {
		t.Fatal("durable acknowledged outbox was exported again")
	}
}

func TestLoadCommittedServingSnapshotSurvivesLeaseHandoff(t *testing.T) {
	now := time.Now().UTC()
	const (
		namespace = "platform"
		region    = "east"
		shard     = "shared"
	)
	snapshot := model.ServingSnapshot{Region: region, Shard: shard, ConfigurationEpoch: 1, ConfigurationRevision: 7, GeneratedAt: now}
	envelope, err := model.NewEnvelope(model.KindServingSnapshot, "serving-1-7", region, shard, "owner-uid", 1, 7, now, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	out := &dnsv1alpha1.DNSTransportOutbox{
		ObjectMeta: metav1.ObjectMeta{Name: envelope.EventID, Namespace: namespace},
		Spec: dnsv1alpha1.DNSTransportOutboxSpec{
			WriterEpoch: 1,
			Revision:    7,
			Payload:     payload,
			PayloadHash: model.Hash(payload),
		},
	}
	state, err := json.Marshal(shardState{Epoch: 2, ActiveOutbox: out.Name})
	if err != nil {
		t.Fatal(err)
	}
	owner := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "dns-shard-owner-" + model.OpaqueToken(region+"/"+shard), Namespace: namespace},
		Data:       map[string]string{"state": string(state)},
	}

	gotEnvelope, gotSnapshot, err := LoadCommittedServingSnapshot(context.Background(), testClient(t, owner, out), namespace, region, shard)
	if err != nil {
		t.Fatal(err)
	}
	if gotEnvelope == nil || gotSnapshot == nil || gotEnvelope.Epoch != 1 || gotSnapshot.ConfigurationRevision != 7 {
		t.Fatalf("unexpected committed snapshot after handoff: envelope=%#v snapshot=%#v", gotEnvelope, gotSnapshot)
	}
}

func TestAckCannotForgeAnotherMemberSubject(t *testing.T) {
	now := time.Now().UTC()
	a := model.MemberAck{MemberID: "auth-a", ReplicaID: "replica-a", ResourceUID: "zone-uid", Kind: model.KindPublicationManifest, Epoch: 1, Revision: 1, Phase: model.AckVerified, ObservedAt: now, ValidUntil: now.Add(30 * time.Second)}
	env, err := model.NewEnvelope(model.KindMemberAck, "ack-1", "east", "shared", "zone-uid", 1, 1, now, a)
	if err != nil {
		t.Fatal(err)
	}
	sink := AckSink{Client: testClient(t), Namespace: "platform", Region: "east", Shard: "shared", Members: []model.Member{{MemberID: "auth-a", ReplicaID: "replica-a", Role: "regional"}}}
	if err := sink.Apply(context.Background(), model.AckSubject("east", "shared", "auth-b"), env); err == nil {
		t.Fatal("forged member acknowledgement was accepted")
	}
	if err := sink.Apply(context.Background(), model.AckSubject("east", "shared", "auth-a"), env); err != nil {
		t.Fatal(err)
	}
}

func TestAckFromUnassignedMemberIsDrained(t *testing.T) {
	now := time.Now().UTC()
	sink := AckSink{Client: testClient(t), Namespace: "platform", Region: "east", Shard: "shared", Members: []model.Member{{MemberID: "auth-a", ReplicaID: "replica-a", Role: "regional"}}}
	for _, a := range []model.MemberAck{
		{MemberID: "auth-b", ReplicaID: "replica-b", ResourceUID: "zone-uid", Kind: model.KindPublicationManifest, Epoch: 1, Revision: 1, Phase: model.AckVerified, ObservedAt: now, ValidUntil: now.Add(30 * time.Second)},
		{MemberID: "auth-a", ReplicaID: "replica-b", ResourceUID: "zone-uid", Kind: model.KindPublicationManifest, Epoch: 1, Revision: 1, Phase: model.AckVerified, ObservedAt: now, ValidUntil: now.Add(30 * time.Second)},
	} {
		env, err := model.NewEnvelope(model.KindMemberAck, "ack-1", "east", "shared", "zone-uid", 1, 1, now, a)
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.Apply(context.Background(), model.AckSubject("east", "shared", a.MemberID), env); err != nil {
			t.Fatalf("acknowledgement from unassigned %s/%s was left for redelivery: %v", a.MemberID, a.ReplicaID, err)
		}
	}
}

func TestPublicationAckDoesNotWaitForSourceProjection(t *testing.T) {
	now := time.Now().UTC()
	zoneUID := types.UID("zone-uid")
	manifest := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: "manifest", Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationManifestSpec{
		ZoneRef: dnsv1alpha1.DNSObjectReference{UID: zoneUID}, WriterEpoch: 1, Revision: 1,
		ServingTargets: []dnsv1alpha1.DNSPublicationTarget{{Region: "east", Shard: "shared"}},
	}}
	owner := &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-" + model.OpaqueToken(string(zoneUID))[:20], Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ZoneUID: zoneUID, WriterEpoch: 1, ActiveManifestName: manifest.Name}}
	platformClient := testClient(t, owner, manifest)
	blocked := &blockingListClient{Client: platformClient, started: make(chan struct{})}
	sink := AckSink{Client: platformClient, Namespace: "platform", Region: "east", Shard: "shared",
		Projects: []ProjectScope{{Client: blocked, Namespace: "offline", RequestTimeout: time.Second}},
		Members:  []model.Member{{MemberID: "auth", ReplicaID: "replica", Role: "regional"}},
		PublicationRegions: map[string][]model.Member{
			"east/shared": {{MemberID: "auth", ReplicaID: "replica", Role: "regional"}},
		},
		Now: func() time.Time { return now },
	}
	ack := model.MemberAck{MemberID: "auth", ReplicaID: "replica", ResourceUID: string(zoneUID), Kind: model.KindPublicationManifest, Epoch: 1, Revision: 1, Phase: model.AckVerified, ObservedAt: now, ValidUntil: now.Add(30 * time.Second)}
	env, err := model.NewEnvelope(model.KindMemberAck, "ack", "east", "shared", string(zoneUID), 1, 1, now, ack)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- sink.Apply(context.Background(), model.AckSubject("east", "shared", "auth"), env) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("platform acknowledgement waited for a partitioned source API")
	}
	select {
	case <-blocked.started:
		t.Fatal("ACK path attempted source status projection")
	default:
	}
	var updated dnsv1alpha1.DNSPublicationManifest
	if err := platformClient.Get(context.Background(), client.ObjectKeyFromObject(manifest), &updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Status.ReplicaAcknowledgements) != 1 {
		t.Fatalf("platform ACK was not persisted: %#v", updated.Status)
	}
}

func TestReadinessCannotMixAuthorizationRenewals(t *testing.T) {
	now := time.Now().UTC()
	until := metav1.NewTime(now.Add(time.Minute))
	s := AckSink{Members: []model.Member{{MemberID: "node", Role: "node"}, {MemberID: "cluster", Role: "cluster"}}}
	rows := []dnsv1alpha1.DNSApplyAcknowledgement{
		{MemberID: "node", WriterEpoch: 2, Revision: 7, AccessEpoch: 3, AccessSequence: 5, Phase: string(model.AckVerified), ValidUntil: &until},
		{MemberID: "cluster", WriterEpoch: 2, Revision: 7, AccessEpoch: 3, AccessSequence: 4, Phase: string(model.AckVerified), ValidUntil: &until},
	}
	if s.verified(rows, now, "resolver", 7, 2, 3, 5) {
		t.Fatal("old cluster authorization acknowledgement satisfied a renewed lease")
	}
	rows[1].AccessSequence = 5
	if !s.verified(rows, now, "resolver", 7, 2, 3, 5) {
		t.Fatal("fresh complete member set was not ready")
	}
	rows[1].WriterEpoch = 1
	if s.verified(rows, now, "resolver", 7, 2, 3, 5) {
		t.Fatal("retired planner epoch satisfied current readiness")
	}
}

func TestReadinessExpiresWithoutAnotherAck(t *testing.T) {
	now := time.Now().UTC()
	until := metav1.NewTime(now.Add(-time.Second))
	b := &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding", Namespace: "platform", Labels: map[string]string{"internal-dns.miloapis.com/region": "east", "internal-dns.miloapis.com/shard": "shared"}}, Spec: dnsv1alpha1.DNSResolverBindingSpec{Configuration: dnsv1alpha1.DNSResolverBindingConfiguration{Revision: 1}, Version: dnsv1alpha1.DNSResolverAccessVersion{Epoch: 1, Sequence: 1}}, Status: dnsv1alpha1.DNSResolverBindingStatus{Phase: "Serving", MemberAcknowledgements: []dnsv1alpha1.DNSApplyAcknowledgement{{MemberID: "resolver", WriterEpoch: 1, Revision: 1, AccessEpoch: 1, AccessSequence: 1, Phase: string(model.AckVerified), ValidUntil: &until}}}}
	cl := testClient(t, b)
	s := AckSink{Client: cl, Namespace: "platform", Region: "east", Shard: "shared", Members: []model.Member{{MemberID: "resolver", Role: "resolver"}}, Now: func() time.Time { return now }}
	if err := s.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	var actual dnsv1alpha1.DNSResolverBinding
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(b), &actual); err != nil {
		t.Fatal(err)
	}
	if actual.Status.Phase == "Serving" {
		t.Fatal("dead member retained ready status beyond its verification lease")
	}
}

func TestRefreshContinuesPastConflictingBinding(t *testing.T) {
	now := time.Now().UTC()
	until := metav1.NewTime(now.Add(-time.Second))
	labels := map[string]string{"internal-dns.miloapis.com/region": "east", "internal-dns.miloapis.com/shard": "shared"}
	binding := func(name string) *dnsv1alpha1.DNSResolverBinding {
		return &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "platform", Labels: labels}, Spec: dnsv1alpha1.DNSResolverBindingSpec{Configuration: dnsv1alpha1.DNSResolverBindingConfiguration{Revision: 1}, Version: dnsv1alpha1.DNSResolverAccessVersion{Epoch: 1, Sequence: 1}}, Status: dnsv1alpha1.DNSResolverBindingStatus{Phase: "Serving", MemberAcknowledgements: []dnsv1alpha1.DNSApplyAcknowledgement{{MemberID: "resolver", WriterEpoch: 1, Revision: 1, AccessEpoch: 1, AccessSequence: 1, Phase: string(model.AckVerified), ValidUntil: &until}}}}
	}
	contended, later := binding("binding-a"), binding("binding-b")
	zoneUID := types.UID("zone-uid")
	manifest := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: "manifest", Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationManifestSpec{ZoneRef: dnsv1alpha1.DNSObjectReference{UID: zoneUID}, WriterEpoch: 1, Revision: 1}}
	owner := &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-" + model.OpaqueToken(string(zoneUID))[:20], Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ZoneUID: zoneUID, WriterEpoch: 1, ActiveManifestName: manifest.Name}}
	base := testClient(t, contended, later, manifest, owner)
	// An ack write lands on the first binding between this pass's read and write.
	cl := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if obj.GetName() == contended.Name {
				return apierrors.NewConflict(dnsv1alpha1.GroupVersion.WithResource("dnsresolverbindings").GroupResource(), obj.GetName(), fmt.Errorf("the object has been modified"))
			}
			return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
		},
	})
	s := AckSink{Client: cl, Namespace: "platform", Region: "east", Shard: "shared", Members: []model.Member{{MemberID: "resolver", Role: "resolver"}}, Now: func() time.Time { return now }}
	if err := s.Refresh(context.Background()); !apierrors.IsConflict(err) {
		t.Fatalf("expected the binding conflict to be reported, got %v", err)
	}
	var refreshed dnsv1alpha1.DNSResolverBinding
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(later), &refreshed); err != nil {
		t.Fatal(err)
	}
	if refreshed.Status.Phase == "Serving" {
		t.Fatal("a conflict on one binding stopped the refresh of later bindings")
	}
	var published dnsv1alpha1.DNSPublicationManifest
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(manifest), &published); err != nil {
		t.Fatal(err)
	}
	if apimeta.FindStatusCondition(published.Status.Conditions, "Published") == nil {
		t.Fatal("a binding conflict stopped manifest publication status from refreshing")
	}
}

func TestServingMinimumPublicationFenceSurvivesSupersededActivation(t *testing.T) {
	ns := "platform"
	old := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: "old-manifest", Namespace: ns}, Spec: dnsv1alpha1.DNSPublicationManifestSpec{ZoneRef: dnsv1alpha1.DNSObjectReference{UID: "zone"}, WriterEpoch: 1, Revision: 1}}
	current := old.DeepCopy()
	current.Name = "current-manifest"
	current.Spec.Revision = 2
	own := &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-" + model.OpaqueToken("zone")[:20], Namespace: ns}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ZoneUID: "zone", WriterEpoch: 1, ActiveManifestName: current.Name}}
	dep := &dnsv1alpha1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: "old-activation", Namespace: ns}, Spec: dnsv1alpha1.DNSTransportOutboxSpec{ManifestRef: dnsv1alpha1.DNSObjectReference{Name: old.Name}, WriterEpoch: 1, Revision: 1}}
	newer := &dnsv1alpha1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: model.PublicationActivationName(current.Name, "east", "shared"), Namespace: ns}, Status: dnsv1alpha1.DNSTransportOutboxStatus{State: "Acknowledged"}}
	o := Outbox{Client: testClient(t, old, current, own, dep, newer), Namespace: ns}
	env := model.Envelope{Kind: model.KindServingSnapshot, Region: "east", Shard: "shared"}
	ready, err := o.dependencyReady(context.Background(), env, dep.Name, map[string]string{})
	if err != nil || !ready {
		t.Fatalf("newer committed publication did not satisfy the minimum fence: %v", err)
	}
	env.Kind = model.KindPublicationManifest
	ready, err = o.dependencyReady(context.Background(), env, dep.Name, map[string]string{})
	if err != nil || ready {
		t.Fatal("manifest allowed mixing chunk dependencies from other revisions")
	}
}

func TestPublicationReadinessRequiresEveryRegionalMember(t *testing.T) {
	now := time.Now().UTC()
	until := metav1.NewTime(now.Add(time.Minute))
	s := AckSink{PublicationRegions: map[string][]model.Member{"east": {{MemberID: "east-a", Role: "regional"}, {MemberID: "east-b", Role: "regional"}}, "west": {{MemberID: "west-a", Role: "regional"}, {MemberID: "west-b", Role: "regional"}}}}
	row := func(member string) dnsv1alpha1.DNSApplyAcknowledgement {
		return dnsv1alpha1.DNSApplyAcknowledgement{MemberID: member, WriterEpoch: 1, Revision: 2, Phase: string(model.AckVerified), ValidUntil: &until}
	}
	rows := make([]dnsv1alpha1.DNSApplyAcknowledgement, 0, 4)
	rows = append(rows, row("east-a"), row("east-b"))
	if s.verified(rows, now, "publication", 2, 1, 0, 0) {
		t.Fatal("east replicas satisfied missing west region")
	}
	rows = append(rows, row("west-a"))
	if s.verified(rows, now, "publication", 2, 1, 0, 0) {
		t.Fatal("missing regional member acknowledged publication")
	}
	rows = append(rows, row("west-b"))
	if !s.verified(rows, now, "publication", 2, 1, 0, 0) {
		t.Fatal("all regional members did not satisfy publication")
	}
}

func TestOutOfOrderAckDoesNotReplaceWithdrawalInSameSecond(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	expired := dnsv1alpha1.DNSApplyAcknowledgement{MemberID: "auth", Phase: string(model.AckExpired), ObservedAt: metav1.NewTime(now), ObservedUnixNano: now.Add(900 * time.Millisecond).UnixNano()}
	late := expired
	late.Phase = string(model.AckVerified)
	late.ObservedUnixNano = now.Add(100 * time.Millisecond).UnixNano()
	rows := upsertAck([]dnsv1alpha1.DNSApplyAcknowledgement{expired}, late)
	if rows[0].Phase != string(model.AckExpired) {
		t.Fatal("reordered positive acknowledgement overwrote a newer withdrawal")
	}
}

func TestBindingReadinessRequiresCurrentCommittedSnapshot(t *testing.T) {
	now := time.Now().UTC()
	until := metav1.NewTime(now.Add(time.Minute))
	b := &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{UID: "binding-uid"}, Spec: dnsv1alpha1.DNSResolverBindingSpec{Configuration: dnsv1alpha1.DNSResolverBindingConfiguration{Generation: 1, Revision: 7}, Version: dnsv1alpha1.DNSResolverAccessVersion{Epoch: 3, Sequence: 5}}, Status: dnsv1alpha1.DNSResolverBindingStatus{MemberAcknowledgements: []dnsv1alpha1.DNSApplyAcknowledgement{
		{MemberID: "node", WriterEpoch: 2, SnapshotRevision: 8, Revision: 7, AccessEpoch: 3, AccessSequence: 5, Phase: string(model.AckVerified), ValidUntil: &until},
		{MemberID: "cluster", WriterEpoch: 2, SnapshotRevision: 8, Revision: 7, AccessEpoch: 3, AccessSequence: 5, Phase: string(model.AckVerified), ValidUntil: &until},
	}}}
	s := AckSink{Members: []model.Member{{MemberID: "node", Role: "node"}, {MemberID: "cluster", Role: "cluster"}}}
	envelope := &model.Envelope{Epoch: 2, Revision: 9}
	snapshot := &model.ServingSnapshot{Bindings: []model.Binding{{BindingUID: "binding-uid", BindingGeneration: 1, ConfigurationRevision: 7, Version: model.AccessVersion{Epoch: 3, Sequence: 5}}}}
	if s.bindingVerified(b, envelope, snapshot, now) {
		t.Fatal("retired snapshot remained ready")
	}
	b.Status.MemberAcknowledgements[0].SnapshotRevision = 9
	if s.bindingVerified(b, envelope, snapshot, now) {
		t.Fatal("mixed snapshot acknowledgements remained ready")
	}
	b.Status.MemberAcknowledgements[1].SnapshotRevision = 9
	if !s.bindingVerified(b, envelope, snapshot, now) {
		t.Fatal("current complete member set was not ready")
	}
	snapshot.Bindings = nil
	if s.bindingVerified(b, envelope, snapshot, now) {
		t.Fatal("omitted binding remained ready")
	}
}

func TestBindingWithdrawalRequiresEveryMemberAtExactTombstone(t *testing.T) {
	now := time.Now().UTC()
	until := metav1.NewTime(now.Add(time.Minute))
	binding := &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{UID: "binding-uid", Generation: 2}, Spec: dnsv1alpha1.DNSResolverBindingSpec{Tombstone: true, Configuration: dnsv1alpha1.DNSResolverBindingConfiguration{Generation: 1, Revision: 2}, Version: dnsv1alpha1.DNSResolverAccessVersion{Epoch: 1, Sequence: 2}}}
	sink := AckSink{Members: []model.Member{{MemberID: "node-a", Role: "node"}, {MemberID: "node-b", Role: "node"}, {MemberID: "regional-a", Role: "regional"}, {MemberID: "regional-b", Role: "regional"}}}
	envelope := &model.Envelope{Epoch: 7, Revision: 11}
	snapshot := &model.ServingSnapshot{Bindings: []model.Binding{{BindingUID: "binding-uid", BindingGeneration: 1, ConfigurationRevision: 2, Version: model.AccessVersion{Epoch: 1, Sequence: 2}, Tombstone: true}}}
	ack := func(member string) dnsv1alpha1.DNSApplyAcknowledgement {
		return dnsv1alpha1.DNSApplyAcknowledgement{MemberID: member, WriterEpoch: 7, SnapshotRevision: 11, Revision: 2, AccessEpoch: 1, AccessSequence: 2, Phase: string(model.AckVerified), ValidUntil: &until}
	}
	binding.Status.MemberAcknowledgements = []dnsv1alpha1.DNSApplyAcknowledgement{ack("node-a"), ack("regional-a")}
	if !sink.bindingVerified(binding, envelope, snapshot, now) {
		t.Fatal("one healthy member per tier did not report the applied withdrawal")
	}
	sink.bindingConditions(binding, envelope, snapshot, now, true)
	if apimeta.IsStatusConditionTrue(binding.Status.Conditions, "FullyReplicated") {
		t.Fatal("partial withdrawal acknowledgements claimed every member removed access")
	}
	if apimeta.IsStatusConditionTrue(binding.Status.Conditions, "Serving") {
		t.Fatal("a verified tombstone was reported as serving")
	}
	binding.Status.MemberAcknowledgements = append(binding.Status.MemberAcknowledgements, ack("node-b"), ack("regional-b"))
	sink.bindingConditions(binding, envelope, snapshot, now, true)
	if !apimeta.IsStatusConditionTrue(binding.Status.Conditions, "FullyReplicated") {
		t.Fatal("the exact tombstone was not proven on every member")
	}
	stale := snapshot.Bindings[0]
	stale.Tombstone = false
	snapshot.Bindings[0] = stale
	if sink.bindingVerified(binding, envelope, snapshot, now) {
		t.Fatal("an active snapshot satisfied a withdrawal proof")
	}
}

func TestExpiredAcknowledgementReplayIsConsumedWithoutStatus(t *testing.T) {
	now := time.Now().UTC()
	s := AckSink{Namespace: "platform", Region: "east", Shard: "shared", Members: []model.Member{{MemberID: "auth", ReplicaID: "replica", Role: "regional"}}, Now: func() time.Time { return now }}
	ack := model.MemberAck{MemberID: "auth", ReplicaID: "replica", ResourceUID: "zone", Kind: model.KindPublicationManifest, Epoch: 1, Revision: 1, Phase: model.AckVerified, ObservedAt: now.Add(-time.Minute), ValidUntil: now.Add(-time.Second)}
	env, err := model.NewEnvelope(model.KindMemberAck, "old-health", "east", "shared", "zone", 1, 1, now.Add(-time.Minute), ack)
	if err != nil {
		t.Fatal(err)
	}
	// No API client is provided: expired replay must be consumed without any
	// attempted status write, even when project APIs are unavailable.
	if err := s.Apply(context.Background(), model.AckSubject("east", "shared", "auth"), env); err != nil {
		t.Fatalf("expired replay became permanently retryable: %v", err)
	}
}

func zoneReferences(uids []types.UID) []dnsv1alpha1.DNSObjectReference {
	refs := make([]dnsv1alpha1.DNSObjectReference, 0, len(uids))
	for _, uid := range uids {
		refs = append(refs, dnsv1alpha1.DNSObjectReference{Name: string(uid), UID: uid})
	}
	return refs
}

func TestZeroTargetTombstoneNeedsNoReplicaACK(t *testing.T) {
	sink := &AckSink{}
	manifest := &dnsv1alpha1.DNSPublicationManifest{}
	if sink.publicationVerified(manifest, time.Now()) {
		t.Fatal("active zero-target publication must not be declared served")
	}
	manifest.Spec.Tombstone = true
	if !sink.publicationVerified(manifest, time.Now()) {
		t.Fatal("zero-target tombstone must complete")
	}
	manifest.Spec.ServingTargets = []dnsv1alpha1.DNSPublicationTarget{{Region: "retired-region", Shard: "retired-shard"}}
	if sink.publicationVerified(manifest, time.Now()) {
		t.Fatal("prior-target withdrawal must await its ACKs")
	}
}

func TestServingAvailabilityDoesNotRequireEveryReplica(t *testing.T) {
	now := time.Now().UTC()
	until := metav1.NewTime(now.Add(time.Minute))
	binding := &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{UID: "binding"}}
	binding.Spec.Configuration.Generation = 1
	binding.Spec.Configuration.Revision = 1
	binding.Spec.Version = dnsv1alpha1.DNSResolverAccessVersion{Epoch: 1, Sequence: 1}
	for _, member := range []string{"node-a", "regional-a"} {
		binding.Status.MemberAcknowledgements = append(binding.Status.MemberAcknowledgements, dnsv1alpha1.DNSApplyAcknowledgement{MemberID: member, WriterEpoch: 1, SnapshotRevision: 1, Revision: 1, AccessEpoch: 1, AccessSequence: 1, Phase: string(model.AckVerified), ValidUntil: &until})
	}
	sink := AckSink{Members: []model.Member{{MemberID: "node-a", Role: "node"}, {MemberID: "node-b", Role: "node"}, {MemberID: "regional-a", Role: "regional"}, {MemberID: "regional-b", Role: "regional"}}}
	envelope := &model.Envelope{Epoch: 1, Revision: 1}
	snapshot := &model.ServingSnapshot{Bindings: []model.Binding{{BindingUID: "binding", BindingGeneration: 1, ConfigurationRevision: 1, Version: model.AccessVersion{Epoch: 1, Sequence: 1}}}}
	if !sink.bindingVerified(binding, envelope, snapshot, now) {
		t.Fatal("a missing replica report suppressed healthy capacity")
	}
	sink.bindingConditions(binding, envelope, snapshot, now, true)
	if apimeta.IsStatusConditionTrue(binding.Status.Conditions, "FullyReplicated") {
		t.Fatal("degraded replication was reported complete")
	}
	binding.Status.ObservedConfigurationRevision = 1
	sink.bindingConditions(binding, envelope, snapshot, now.Add(2*time.Minute), false)
	if c := apimeta.FindStatusCondition(binding.Status.Conditions, "Serving"); c == nil || c.Status != metav1.ConditionUnknown {
		t.Fatal("stale observations claimed a serving outage")
	}
}

func TestPublicationProjectionOnlyReadsOwningProject(t *testing.T) {
	cl := testClient(t)
	unrelated := &blockingListClient{Client: cl, started: make(chan struct{})}
	manifest := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{"internal-dns.miloapis.com/project-uid": "owner"},
	}}
	sink := AckSink{Client: cl, Projects: []ProjectScope{
		{ProjectUID: "unrelated", Client: unrelated, RequestTimeout: 20 * time.Millisecond},
		{ProjectUID: "owner", Client: cl},
	}}
	if err := sink.projectPublication(context.Background(), manifest, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-unrelated.started:
		t.Fatal("publication contacted an unrelated project")
	default:
	}
}

type heldManifestRead struct {
	client.Client
	once    sync.Once
	read    chan struct{}
	release chan struct{}
}

func (c *heldManifestRead) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, object, opts...); err != nil {
		return err
	}
	if _, ok := object.(*dnsv1alpha1.DNSPublicationManifest); ok {
		c.once.Do(func() { close(c.read) })
		select {
		case <-c.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func TestConcurrentAckProjectionPreservesNewerObservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	now := time.Now().UTC()
	manifest := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: "manifest", Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationManifestSpec{ZoneRef: dnsv1alpha1.DNSObjectReference{UID: "zone"}, WriterEpoch: 1, Revision: 2, ServingTargets: []dnsv1alpha1.DNSPublicationTarget{{Region: "east", Shard: "shared"}}}}
	owner := &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-" + model.OpaqueToken("zone")[:20], Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ZoneUID: "zone", WriterEpoch: 1, ActiveManifestName: manifest.Name}}
	cl := testClient(t, owner, manifest)
	held := &heldManifestRead{Client: cl, read: make(chan struct{}), release: make(chan struct{})}
	sink := AckSink{Client: cl, Namespace: "platform", Region: "east", Shard: "shared", Members: []model.Member{{MemberID: "auth", ReplicaID: "replica", Role: "regional"}}, Now: func() time.Time { return now }}
	olderSink := sink
	olderSink.Client = held
	ack := model.MemberAck{MemberID: "auth", ReplicaID: "replica", ResourceUID: "zone", Kind: model.KindPublicationManifest, Epoch: 1, Revision: 2, Phase: model.AckVerified, ObservedAt: now.Add(-time.Second), ValidUntil: now.Add(time.Minute)}
	older, err := model.NewEnvelope(model.KindMemberAck, "older", "east", "shared", "zone", 1, 2, now, ack)
	if err != nil {
		t.Fatal(err)
	}
	ack.Phase = model.AckRejected
	ack.ObservedAt = now
	newer, err := model.NewEnvelope(model.KindMemberAck, "newer", "east", "shared", "zone", 1, 2, now, ack)
	if err != nil {
		t.Fatal(err)
	}
	subject := model.AckSubject("east", "shared", "auth")
	done := make(chan error, 1)
	go func() { done <- olderSink.Apply(ctx, subject, older) }()
	select {
	case <-held.read:
	case <-ctx.Done():
		t.Fatal("older acknowledgement did not reach manifest")
	}
	if err := sink.Apply(ctx, subject, newer); err != nil {
		close(held.release)
		t.Fatal(err)
	}
	close(held.release)
	if err := <-done; err != nil {
		t.Fatalf("racing older projection did not retry the optimistic conflict: %v", err)
	}
	if err := sink.Apply(ctx, subject, older); err != nil {
		t.Fatalf("older acknowledgement redelivery failed: %v", err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(manifest), manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Status.ReplicaAcknowledgements) != 1 || manifest.Status.ReplicaAcknowledgements[0].Phase != string(model.AckRejected) || manifest.Status.ReplicaAcknowledgements[0].ObservedUnixNano != now.UnixNano() {
		t.Fatalf("late verified acknowledgement overwrote newer rejection: %+v", manifest.Status.ReplicaAcknowledgements)
	}
}

func TestWithdrawnShardAckIsNotRecordedOnTheManifest(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	manifest := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: "manifest", Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationManifestSpec{ZoneRef: dnsv1alpha1.DNSObjectReference{UID: "zone"}, WriterEpoch: 1, Revision: 2,
		ServingTargets: []dnsv1alpha1.DNSPublicationTarget{{Region: "east", Shard: "shared"}}, WithdrawnTargets: []dnsv1alpha1.DNSPublicationTarget{{Region: "west", Shard: "shared"}}}}
	owner := &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-" + model.OpaqueToken("zone")[:20], Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ZoneUID: "zone", WriterEpoch: 1, ActiveManifestName: manifest.Name}}
	cl := testClient(t, owner, manifest)
	sink := AckSink{Client: cl, Namespace: "platform", Region: "west", Shard: "shared", Members: []model.Member{{MemberID: "west-auth", ReplicaID: "replica", Role: "regional"}}, Now: func() time.Time { return now }}
	ack := model.MemberAck{MemberID: "west-auth", ReplicaID: "replica", ResourceUID: "zone", Kind: model.KindPublicationManifest, Epoch: 1, Revision: 2, Phase: model.AckVerified, ObservedAt: now, ValidUntil: now.Add(time.Minute)}
	env, err := model.NewEnvelope(model.KindMemberAck, "withdrawn", "west", "shared", "zone", 1, 2, now, ack)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Apply(ctx, model.AckSubject("west", "shared", "west-auth"), env); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(manifest), manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Status.ReplicaAcknowledgements) != 0 || hasApplied(manifest.Status.ReplicaAcknowledgements, 1, 2) {
		t.Fatalf("a withdrawn shard's acknowledgement was recorded with the serving shards' acknowledgements: %+v", manifest.Status.ReplicaAcknowledgements)
	}
}

func TestPublicationProjectionReportsMissingProjectIdentity(t *testing.T) {
	manifest := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"internal-dns.miloapis.com/project-uid": "project-owner"}}}
	sink := AckSink{Projects: []ProjectScope{{Namespace: "default"}}}
	if err := sink.projectPublication(context.Background(), manifest, true, time.Now()); err == nil {
		t.Fatal("missing projection identity was silently skipped")
	}
}

func TestWithdrawalToAShardTheManifestDoesNotServeIsExported(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	manifest := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: "manifest", Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationManifestSpec{ZoneRef: dnsv1alpha1.DNSObjectReference{UID: "zone"}, ZoneApex: "prod.internal", WriterEpoch: 1, Revision: 2,
		ServingTargets: []dnsv1alpha1.DNSPublicationTarget{{Region: "east", Shard: "shared"}}, WithdrawnTargets: []dnsv1alpha1.DNSPublicationTarget{{Region: "west", Shard: "shared"}}}}
	owner := &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-" + model.OpaqueToken("zone")[:20], Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ZoneUID: "zone", WriterEpoch: 1, ActiveManifestName: manifest.Name}}
	withdrawal := model.PublicationManifest{ManifestUID: manifest.Name, ZoneUID: "zone", Apex: "prod.internal", WriterEpoch: 1, Revision: 2, PreviousRevision: 1, Tombstone: true, Chunks: []model.ChunkRef{}, ContentHash: model.EmptyContentHash(), GeneratedAt: now}
	env, err := model.NewEnvelope(model.KindPublicationManifest, "west:manifest", "west", "shared", "zone", 1, 2, now, withdrawal)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(env)
	out := &dnsv1alpha1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: model.PublicationActivationName(manifest.Name, "west", "shared"), Namespace: "platform"}, Spec: dnsv1alpha1.DNSTransportOutboxSpec{Subject: model.RecordManifestSubject("west", "shared", "zone"), ResourceUID: "zone", WriterEpoch: 1, Revision: 2, ManifestRef: dnsv1alpha1.DNSObjectReference{Name: manifest.Name}, Activation: true, Payload: payload, PayloadHash: model.Hash(payload)}}
	cl := testClient(t, owner, manifest, out)
	worker := Outbox{Client: cl, Namespace: "platform"}
	if out.Spec.Subject != expectedSubject(env) {
		t.Fatalf("withdrawal subject %q does not match its payload", out.Spec.Subject)
	}
	committed, err := worker.committed(ctx, out, env)
	if err != nil || !committed {
		t.Fatalf("a withdrawal of the active revision must be exportable: %v %v", committed, err)
	}
}

func TestNodePublicationAckIsDroppedNotRetried(t *testing.T) {
	now := time.Now().UTC()
	a := model.MemberAck{MemberID: "node-a", ReplicaID: "node-a", ResourceUID: "zone-uid", Kind: model.KindPublicationManifest, Epoch: 1, Revision: 1, Phase: model.AckRejected, Error: "local verification failed", ObservedAt: now}
	env, err := model.NewEnvelope(model.KindMemberAck, "ack-node", "east", "shared", "zone-uid", 1, 1, now, a)
	if err != nil {
		t.Fatal(err)
	}
	sink := AckSink{Client: testClient(t), Namespace: "platform", Region: "east", Shard: "shared", Members: []model.Member{{MemberID: "node-a", ReplicaID: "node-a", Role: "node"}}}
	if err := sink.Apply(context.Background(), model.AckSubject("east", "shared", "node-a"), env); err != nil {
		t.Fatalf("a node member's publication acknowledgement must be settled, not retried: %v", err)
	}
}

func TestPatternMemberAckIsAccepted(t *testing.T) {
	now := time.Now().UTC()
	patterns := []model.MemberPattern{{Role: "node", IDPrefix: "node-east-"}}
	sink := AckSink{Client: testClient(t), Namespace: "platform", Region: "east", Shard: "shared",
		Members:        []model.Member{{MemberID: "cluster", Role: "cluster"}},
		MemberPatterns: patterns,
	}
	for _, a := range []model.MemberAck{
		{MemberID: "node-east-3", ReplicaID: "node-east-3", ResourceUID: "zone-uid", Kind: model.KindPublicationManifest, Epoch: 1, Revision: 1, Phase: model.AckVerified, ObservedAt: now, ValidUntil: now.Add(30 * time.Second)},
		{MemberID: "node-east-3", ReplicaID: "other", ResourceUID: "zone-uid", Kind: model.KindPublicationManifest, Epoch: 1, Revision: 1, Phase: model.AckVerified, ObservedAt: now, ValidUntil: now.Add(30 * time.Second)},
	} {
		env, err := model.NewEnvelope(model.KindMemberAck, "ack-1", "east", "shared", "zone-uid", 1, 1, now, a)
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.Apply(context.Background(), model.AckSubject("east", "shared", a.MemberID), env); err != nil {
			t.Fatalf("acknowledgement from %s/%s: %v", a.MemberID, a.ReplicaID, err)
		}
	}

}

func TestPatternMembersHoldWithdrawalUntilRetired(t *testing.T) {
	now := time.Now().UTC()
	fresh := metav1.NewTime(now.Add(time.Minute))
	binding := &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{UID: "binding-uid", Generation: 2}, Spec: dnsv1alpha1.DNSResolverBindingSpec{Tombstone: true, Configuration: dnsv1alpha1.DNSResolverBindingConfiguration{Generation: 1, Revision: 2}, Version: dnsv1alpha1.DNSResolverAccessVersion{Epoch: 1, Sequence: 2}}}
	sink := AckSink{Members: []model.Member{{MemberID: "regional-a", Role: "regional"}}, MemberPatterns: []model.MemberPattern{{Role: "node", IDPrefix: "node-east-"}}}
	envelope := &model.Envelope{Epoch: 7, Revision: 11}
	snapshot := &model.ServingSnapshot{Bindings: []model.Binding{{BindingUID: "binding-uid", BindingGeneration: 1, ConfigurationRevision: 2, Version: model.AccessVersion{Epoch: 1, Sequence: 2}, Tombstone: true}}}
	ack := func(member string, snapshotRevision int64, until metav1.Time) dnsv1alpha1.DNSApplyAcknowledgement {
		return dnsv1alpha1.DNSApplyAcknowledgement{MemberID: member, WriterEpoch: 7, SnapshotRevision: snapshotRevision, Revision: 2, AccessEpoch: 1, AccessSequence: 2, Phase: string(model.AckVerified), ValidUntil: &until}
	}
	fullyReplicated := func() bool {
		sink.bindingConditions(binding, envelope, snapshot, now, true)
		return apimeta.IsStatusConditionTrue(binding.Status.Conditions, "FullyReplicated")
	}

	binding.Status.MemberAcknowledgements = []dnsv1alpha1.DNSApplyAcknowledgement{ack("regional-a", 11, fresh)}
	if sink.bindingVerified(binding, envelope, snapshot, now) || fullyReplicated() {
		t.Fatal("a withdrawal was proven before any node member applied it")
	}

	binding.Status.MemberAcknowledgements = append(binding.Status.MemberAcknowledgements, ack("node-east-2", 11, fresh), ack("node-east-3", 10, fresh))
	if !sink.bindingVerified(binding, envelope, snapshot, now) {
		t.Fatal("one verified node member did not report the applied withdrawal while another lagged")
	}
	if fullyReplicated() {
		t.Fatal("a pattern member on an older snapshot did not hold back full replication")
	}

	expired := metav1.NewTime(now.Add(-time.Hour))
	binding.Status.MemberAcknowledgements[2] = ack("node-east-3", 10, expired)
	if fullyReplicated() {
		t.Fatal("a pattern member silent for an hour stopped holding back full replication")
	}

	retired := metav1.NewTime(now.Add(-patternMemberRetireAfter - time.Minute))
	binding.Status.MemberAcknowledgements[2] = ack("node-east-3", 10, retired)
	if !fullyReplicated() {
		t.Fatal("a retired pattern member still held back full replication")
	}
}

func TestPatternMemberAckAtStaleRevisionStillRequiresIt(t *testing.T) {
	now := time.Now().UTC()
	const namespace, region, shard = "platform", "east", "shared"
	snapshot := model.ServingSnapshot{Region: region, Shard: shard, ConfigurationEpoch: 1, ConfigurationRevision: 7, GeneratedAt: now,
		Bindings: []model.Binding{{BindingUID: "binding-uid", BindingGeneration: 1, ConfigurationRevision: 2, Version: model.AccessVersion{Epoch: 1, Sequence: 2}}}}
	envelope, err := model.NewEnvelope(model.KindServingSnapshot, "serving-1-7", region, shard, "owner-uid", 1, 7, now, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	out := &dnsv1alpha1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: envelope.EventID, Namespace: namespace}, Spec: dnsv1alpha1.DNSTransportOutboxSpec{WriterEpoch: 1, Revision: 7, Payload: payload, PayloadHash: model.Hash(payload)}}
	state, err := json.Marshal(shardState{Epoch: 1, ActiveOutbox: out.Name})
	if err != nil {
		t.Fatal(err)
	}
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "dns-shard-owner-" + model.OpaqueToken(region+"/"+shard), Namespace: namespace}, Data: map[string]string{"state": string(state)}}
	retired := metav1.NewTime(now.Add(-patternMemberRetireAfter - time.Minute))
	binding := &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding", Namespace: namespace, UID: "binding-uid", Labels: map[string]string{"internal-dns.miloapis.com/region": model.SafeToken(region), "internal-dns.miloapis.com/shard": model.SafeToken(shard)}},
		Spec:   dnsv1alpha1.DNSResolverBindingSpec{Configuration: dnsv1alpha1.DNSResolverBindingConfiguration{Generation: 1, Revision: 2}, Version: dnsv1alpha1.DNSResolverAccessVersion{Epoch: 1, Sequence: 2}},
		Status: dnsv1alpha1.DNSResolverBindingStatus{MemberAcknowledgements: []dnsv1alpha1.DNSApplyAcknowledgement{{MemberID: "node-east-9", Phase: string(model.AckVerified), ValidUntil: &retired}}}}
	c := testClient(t, owner, out, binding)
	sink := AckSink{Client: c, Namespace: namespace, Region: region, Shard: shard,
		Members: []model.Member{{MemberID: "regional-a", Role: "regional"}}, MemberPatterns: []model.MemberPattern{{Role: "node", IDPrefix: "node-east-"}}}

	ack := model.MemberAck{MemberID: "node-east-4", ReplicaID: "node-east-4", ResourceUID: "binding-uid", Kind: model.KindServingSnapshot, Epoch: 1, Revision: 6, Phase: model.AckVerified, ObservedAt: now, ValidUntil: now.Add(time.Minute)}
	env, err := model.NewEnvelope(model.KindMemberAck, "ack-1", region, shard, "binding-uid", 1, 6, now, ack)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Apply(context.Background(), model.AckSubject(region, shard, ack.MemberID), env); err != nil {
		t.Fatal(err)
	}
	var got dnsv1alpha1.DNSResolverBinding
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(binding), &got); err != nil {
		t.Fatal(err)
	}
	rows := got.Status.MemberAcknowledgements
	if !slices.ContainsFunc(rows, func(r dnsv1alpha1.DNSApplyAcknowledgement) bool {
		return r.MemberID == "node-east-4" && r.Revision == 0 && r.SnapshotRevision == 6
	}) {
		t.Fatalf("a stale acknowledgement left the pattern member without a presence row: %#v", rows)
	}
	members := sink.bindingMembers(rows, now)
	if !slices.ContainsFunc(members, func(m model.Member) bool { return m.MemberID == "node-east-4" }) {
		t.Fatalf("the present pattern member is not required: %#v", members)
	}

	if err := sink.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(binding), &got); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(got.Status.MemberAcknowledgements, func(r dnsv1alpha1.DNSApplyAcknowledgement) bool { return r.MemberID == "node-east-9" }) {
		t.Fatal("a retired pattern member's row was kept")
	}
}

type countingListClient struct {
	client.Client
	mu    sync.Mutex
	lists int
	fail  bool
}

func (c *countingListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.mu.Lock()
	c.lists++
	fail := c.fail
	c.mu.Unlock()
	if fail {
		return errors.New("project API unavailable")
	}
	return c.Client.List(ctx, list, opts...)
}

func (c *countingListClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lists
}

// The refresh runs every few seconds. A manifest whose projection inputs have
// not changed must not list its project's zones and registrations again until
// the resync interval passes.
func TestRefreshSkipsUnchangedProjectionUntilResync(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC)
	plan := model.PublicationPlan{ZoneUID: "zone-a", Apex: "a.internal", RRSets: []model.RRSet{}}
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	chunk := &dnsv1alpha1.DNSPublicationChunk{ObjectMeta: metav1.ObjectMeta{Name: "chunk", Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationChunkSpec{WriterEpoch: 1, Revision: 3, Index: 0, SHA256: model.Hash(payload), Payload: payload}}
	manifest := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: "manifest", Namespace: "platform", UID: "manifest-uid", Generation: 1}, Spec: dnsv1alpha1.DNSPublicationManifestSpec{
		ZoneRef: dnsv1alpha1.DNSObjectReference{UID: "zone-a"}, ZoneApex: "a.internal", WriterEpoch: 1, Revision: 3, Tombstone: true,
		Chunks: []dnsv1alpha1.DNSPublicationChunkReference{{Name: chunk.Name, SHA256: chunk.Spec.SHA256, Size: int32(len(payload))}}, ContentHash: model.Hash(payload), GeneratedAt: metav1.NewTime(now),
	}}
	owner := &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "zone-a", Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ZoneUID: "zone-a", ActiveManifestName: manifest.Name}}
	platformClient := testClient(t, chunk, manifest, owner)
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	project := &countingListClient{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	clock := now
	var scopes []ProjectScope
	sink := &AckSink{Client: platformClient, Namespace: "platform", Region: "central", Shard: "s", ProjectScopes: func() []ProjectScope { return scopes }, Now: func() time.Time { return clock }}

	refresh := func() int {
		t.Helper()
		before := project.count()
		_ = sink.Refresh(ctx)
		return project.count() - before
	}
	// Before discovery finds the owning project nothing is written, and that
	// must not count as projected.
	refresh()
	scopes = []ProjectScope{{Client: project, Namespace: "project"}}
	clock = clock.Add(2 * time.Second)
	if n := refresh(); n == 0 {
		t.Fatal("the manifest was not projected once its project was discovered")
	}
	clock = clock.Add(2 * time.Second)
	if n := refresh(); n != 0 {
		t.Fatalf("unchanged manifest listed project objects %d times", n)
	}
	var current dnsv1alpha1.DNSPublicationManifest
	if err := platformClient.Get(ctx, client.ObjectKeyFromObject(manifest), &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Revision = 4
	current.Generation = 2
	if err := platformClient.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Second)
	if n := refresh(); n == 0 {
		t.Fatal("a new manifest revision was not projected")
	}
	clock = clock.Add(2 * time.Second)
	if n := refresh(); n != 0 {
		t.Fatalf("unchanged manifest listed project objects %d times", n)
	}
	clock = clock.Add(projectionResync)
	if n := refresh(); n == 0 {
		t.Fatal("the resync interval did not project the manifest again")
	}

	// A failed projection is retried on the next pass.
	project.fail = true
	clock = clock.Add(2 * time.Second)
	current.Spec.Revision = 5
	if err := platformClient.Get(ctx, client.ObjectKeyFromObject(manifest), &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Revision = 5
	if err := platformClient.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	refresh()
	project.fail = false
	clock = clock.Add(2 * time.Second)
	if n := refresh(); n == 0 {
		t.Fatal("a failed projection was remembered as done")
	}

	// A pod that stopped refreshing, as after losing its region lease, starts
	// again from nothing.
	clock = clock.Add(projectionGap + time.Second)
	if n := refresh(); n == 0 {
		t.Fatal("memos survived a gap in refreshing")
	}
}
