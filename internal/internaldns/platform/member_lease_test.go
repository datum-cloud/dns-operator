// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

type leaseFixture struct {
	t        *testing.T
	now      time.Time
	client   client.Client
	sink     *AckSink
	manifest client.ObjectKey
}

func newLeaseFixture(t *testing.T) *leaseFixture {
	t.Helper()
	zoneUID := types.UID("zone-uid")
	manifest := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: "manifest", Namespace: "platform", Generation: 1}, Spec: dnsv1alpha1.DNSPublicationManifestSpec{
		ZoneRef: dnsv1alpha1.DNSObjectReference{UID: zoneUID}, WriterEpoch: 1, Revision: 1,
		ServingTargets: []dnsv1alpha1.DNSPublicationTarget{{Region: "east", Shard: "shared"}},
	}}
	owner := &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-" + model.OpaqueToken(string(zoneUID))[:20], Namespace: "platform"}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ZoneUID: zoneUID, WriterEpoch: 1, ActiveManifestName: manifest.Name}}
	f := &leaseFixture{t: t, now: time.Date(2026, 11, 5, 20, 0, 0, 0, time.UTC), client: testClient(t, owner, manifest), manifest: client.ObjectKeyFromObject(manifest)}
	member := model.Member{MemberID: "regional-0", ReplicaID: "replica", Role: "regional"}
	f.sink = &AckSink{Client: f.client, Namespace: "platform", Region: "east", Shard: "shared",
		Members: []model.Member{member}, PublicationRegions: map[string][]model.Member{"east/shared": {member}},
		Now: func() time.Time { return f.now }}
	return f
}

func (f *leaseFixture) ack(phase model.AckPhase) {
	f.t.Helper()
	a := model.MemberAck{MemberID: "regional-0", ReplicaID: "replica", ResourceUID: "zone-uid", Kind: model.KindPublicationManifest, Epoch: 1, Revision: 1, Phase: phase, ObservedAt: f.now, ValidUntil: f.now.Add(90 * time.Second)}
	env, err := model.NewEnvelope(model.KindMemberAck, "ack-"+f.now.String(), "east", "shared", "zone-uid", 1, 1, f.now, a)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.sink.Apply(context.Background(), model.AckSubject("east", "shared", "regional-0"), env); err != nil {
		f.t.Fatal(err)
	}
}

func (f *leaseFixture) get() dnsv1alpha1.DNSPublicationManifest {
	f.t.Helper()
	var m dnsv1alpha1.DNSPublicationManifest
	if err := f.client.Get(context.Background(), f.manifest, &m); err != nil {
		f.t.Fatal(err)
	}
	return m
}

func published(m dnsv1alpha1.DNSPublicationManifest) bool {
	return apimeta.IsStatusConditionTrue(m.Status.Conditions, "Published")
}

func TestVerifiedRenewalRenewsTheMemberLeaseNotTheManifest(t *testing.T) {
	f := newLeaseFixture(t)
	f.ack(model.AckVerified)
	first := f.get()
	if !published(first) {
		t.Fatalf("first verification did not publish: %#v", first.Status)
	}

	for range 4 {
		f.now = f.now.Add(30 * time.Second)
		f.ack(model.AckVerified)
	}
	renewed := f.get()
	if renewed.ResourceVersion != first.ResourceVersion {
		t.Fatalf("a renewal rewrote the manifest: version %s -> %s", first.ResourceVersion, renewed.ResourceVersion)
	}
	var lease corev1.ConfigMap
	if err := f.client.Get(context.Background(), client.ObjectKey{Namespace: "platform", Name: memberLeaseName("east", "shared", "regional-0")}, &lease); err != nil {
		t.Fatal(err)
	}
	if want := f.now.Add(90 * time.Second).Format(time.RFC3339Nano); lease.Data[memberLeaseKey] != want {
		t.Fatalf("member lease = %q, want %q", lease.Data[memberLeaseKey], want)
	}

	f.now = f.now.Add(publicationRowLease - publicationRowRenewBefore)
	f.ack(model.AckVerified)
	if got := f.get(); got.ResourceVersion == first.ResourceVersion || !got.Status.ReplicaAcknowledgements[0].ValidUntil.Time.Equal(f.now.Add(publicationRowLease)) {
		t.Fatalf("a row near lapsing was not renewed: %#v", got.Status.ReplicaAcknowledgements)
	}
}

func TestLapsedMemberLeaseWithdrawsPublication(t *testing.T) {
	f := newLeaseFixture(t)
	f.ack(model.AckVerified)
	f.now = f.now.Add(91 * time.Second)
	if err := f.sink.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := f.get()
	if published(m) {
		t.Fatal("a member that stopped renewing still counts as verified")
	}
	if !m.Status.ReplicaAcknowledgements[0].ValidUntil.After(f.now) {
		t.Fatal("the withdrawal must come from the member lease, not the row")
	}

	f.now = f.now.Add(time.Second)
	f.ack(model.AckVerified)
	if !published(f.get()) {
		t.Fatal("a member renewing again did not restore publication")
	}
}

func TestRowsDecideForAMemberWithoutALease(t *testing.T) {
	f := newLeaseFixture(t)
	until := metav1.NewTime(f.now.Add(90 * time.Second))
	m := f.get()
	m.Status.ReplicaAcknowledgements = []dnsv1alpha1.DNSApplyAcknowledgement{{MemberID: "regional-0", Phase: string(model.AckVerified), WriterEpoch: 1, Revision: 1, ObservedAt: metav1.NewTime(f.now), ValidUntil: &until}}
	if err := f.client.Status().Update(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	if err := f.sink.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !published(f.get()) {
		t.Fatal("a row written before member leases existed must still publish")
	}
	f.now = f.now.Add(91 * time.Second)
	if err := f.sink.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if published(f.get()) {
		t.Fatal("a lapsed row without a member lease must withdraw publication")
	}
}

func TestRejectionIsRecordedAtOnce(t *testing.T) {
	f := newLeaseFixture(t)
	f.ack(model.AckVerified)
	f.now = f.now.Add(time.Second)
	f.ack(model.AckRejected)
	m := f.get()
	if published(m) || m.Status.ReplicaAcknowledgements[0].Phase != string(model.AckRejected) {
		t.Fatalf("rejection not recorded: %#v", m.Status)
	}
}
