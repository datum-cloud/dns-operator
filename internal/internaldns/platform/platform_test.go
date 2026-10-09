// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
	registration := &dnsv1alpha1.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: zone.Namespace, UID: "registration-healthy", Generation: 2}, Spec: dnsv1alpha1.DNSRegistrationSpec{DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: zone.Name, UID: zone.UID}, Name: "api"}}
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
		objects = append(objects, &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("binding-%d", i), Namespace: ns, UID: types.UID(fmt.Sprintf("binding-uid-%d", i)), Labels: map[string]string{"internal-dns.miloapis.com/region": region, "internal-dns.miloapis.com/shard": shard}}, Spec: dnsv1alpha1.DNSResolverBindingSpec{Source: dnsv1alpha1.DNSResolverBindingSource{ProjectUID: types.UID(fmt.Sprintf("project-%d", i)), ResolverContextRef: dnsv1alpha1.DNSObjectReference{Name: string(vuid), UID: vuid}}, Placement: dnsv1alpha1.DNSResolverBindingPlacement{Region: region, Shard: shard}, Configuration: dnsv1alpha1.DNSResolverBindingConfiguration{Generation: 1, Revision: 1, Listeners: dnsv1alpha1.DNSResolverBindingListeners{Node: dnsv1alpha1.DNSResolverBindingListener{Address: fmt.Sprintf("fd53::%d", i), Port: 53, Transports: []string{"UDP", "TCP"}}, Regional: dnsv1alpha1.DNSResolverBindingListener{Address: fmt.Sprintf("fd54::%d", i), Port: 53, Transports: []string{"UDP", "TCP"}}}, ZoneRefs: zoneReferences([]types.UID{zuid})}, Authorization: dnsv1alpha1.DNSResolverAccessAuthorization{WriterEpoch: 1, Sequence: 1, ValidUntil: metav1.NewTime(now.Add(time.Minute))}}})
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
	if len(out.Items) != 2 {
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
	if len(snapshot.Bindings) != 1 || snapshot.Bindings[0].ContextUID != "vpc-2" {
		t.Fatal("untargeted zone entered snapshot or removed another project")
	}
	p.Config.Identity = "retired-other-owner"
	if err := p.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = cl.List(context.Background(), &out)
	if len(out.Items) != 2 {
		t.Fatal("another holder published during a current lease")
	}
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
		{MemberID: "node", WriterEpoch: 2, Revision: 7, AuthorizationIssuerEpoch: 3, AuthorizationRevision: 5, Phase: string(model.AckVerified), ValidUntil: &until},
		{MemberID: "cluster", WriterEpoch: 2, Revision: 7, AuthorizationIssuerEpoch: 3, AuthorizationRevision: 4, Phase: string(model.AckVerified), ValidUntil: &until},
	}
	if s.verified(rows, now, "resolver", 7, 2, 3, 5) {
		t.Fatal("old cluster authorization acknowledgement satisfied a renewed lease")
	}
	rows[1].AuthorizationRevision = 5
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
	b := &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{Name: "binding", Namespace: "platform", Labels: map[string]string{"internal-dns.miloapis.com/region": "east", "internal-dns.miloapis.com/shard": "shared"}}, Spec: dnsv1alpha1.DNSResolverBindingSpec{Configuration: dnsv1alpha1.DNSResolverBindingConfiguration{Revision: 1}, Authorization: dnsv1alpha1.DNSResolverAccessAuthorization{WriterEpoch: 1, Sequence: 1, ValidUntil: metav1.NewTime(now.Add(time.Minute))}}, Status: dnsv1alpha1.DNSResolverBindingStatus{Phase: "Serving", MemberAcknowledgements: []dnsv1alpha1.DNSApplyAcknowledgement{{MemberID: "resolver", WriterEpoch: 1, Revision: 1, AuthorizationIssuerEpoch: 1, AuthorizationRevision: 1, Phase: string(model.AckVerified), ValidUntil: &until}}}}
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
	b := &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{UID: "binding-uid"}, Spec: dnsv1alpha1.DNSResolverBindingSpec{Configuration: dnsv1alpha1.DNSResolverBindingConfiguration{Generation: 1, Revision: 7}, Authorization: dnsv1alpha1.DNSResolverAccessAuthorization{WriterEpoch: 3, Sequence: 5, ValidUntil: until}}, Status: dnsv1alpha1.DNSResolverBindingStatus{MemberAcknowledgements: []dnsv1alpha1.DNSApplyAcknowledgement{
		{MemberID: "node", WriterEpoch: 2, SnapshotRevision: 8, Revision: 7, AuthorizationIssuerEpoch: 3, AuthorizationRevision: 5, Phase: string(model.AckVerified), ValidUntil: &until},
		{MemberID: "cluster", WriterEpoch: 2, SnapshotRevision: 8, Revision: 7, AuthorizationIssuerEpoch: 3, AuthorizationRevision: 5, Phase: string(model.AckVerified), ValidUntil: &until},
	}}}
	s := AckSink{Members: []model.Member{{MemberID: "node", Role: "node"}, {MemberID: "cluster", Role: "cluster"}}}
	envelope := &model.Envelope{Epoch: 2, Revision: 9}
	snapshot := &model.ServingSnapshot{Bindings: []model.Binding{{BindingUID: "binding-uid", BindingGeneration: 1, ConfigurationRevision: 7, Authorization: model.Authorization{IssuerEpoch: 3, Revision: 5}}}}
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
