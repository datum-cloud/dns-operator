// SPDX-License-Identifier: AGPL-3.0-only

package pubstore

import (
	"testing"
	"time"

	dnsv1 "go.miloapis.com/dns-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newStore(t *testing.T) (*PublicationStore, client.Client, *MemoryKV) {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := dnsv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	api := fake.NewClientBuilder().WithScheme(s).Build()
	kv := NewMemoryKV()
	store := New(api, kv)
	if err := store.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return store, api, kv
}

func TestStoreKeepsCompareAndSwapContract(t *testing.T) {
	ctx := t.Context()
	store, _, _ := newStore(t)
	own := &dnsv1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-a", Namespace: "platform"}, Spec: dnsv1.DNSPublicationOwnershipSpec{ZoneUID: "zone-a", HolderIdentity: "a", WriterEpoch: 1}}
	if err := store.Create(ctx, own); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, own.DeepCopy()); !apierrors.IsBadRequest(err) {
		t.Fatalf("create with a resource version: %v", err)
	}
	again := own.DeepCopy()
	again.ResourceVersion = ""
	if err := store.Create(ctx, again); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("duplicate create: %v", err)
	}

	stale := own.DeepCopy()
	base := own.DeepCopy()
	own.Spec.NextRevision = 5
	if err := store.Patch(ctx, own, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		t.Fatal(err)
	}
	staleBase := stale.DeepCopy()
	stale.Spec.HolderIdentity = "b"
	if err := store.Patch(ctx, stale, client.MergeFromWithOptions(staleBase, client.MergeFromWithOptimisticLock{})); !apierrors.IsConflict(err) {
		t.Fatalf("stale optimistic patch: %v", err)
	}
	plainBase := stale.DeepCopy()
	stale.Spec.LeaseUntil = metav1.NewTime(time.Unix(1_800_000_000, 0).UTC())
	if err := store.Patch(ctx, stale, client.MergeFrom(plainBase)); err != nil {
		t.Fatalf("unconditional patch on a moved object: %v", err)
	}
	var got dnsv1.DNSPublicationOwnership
	if err := store.Get(ctx, client.ObjectKeyFromObject(own), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.NextRevision != 5 || got.Spec.HolderIdentity != "a" || got.Spec.LeaseUntil.Unix() != 1_800_000_000 || got.UID != own.UID {
		t.Fatalf("writes merged wrongly: %#v", got)
	}
}

func TestStoreSeparatesStatusFromSpec(t *testing.T) {
	ctx := t.Context()
	store, _, _ := newStore(t)
	m := &dnsv1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: "pub", Namespace: "platform"}, Spec: dnsv1.DNSPublicationManifestSpec{Revision: 1}, Status: dnsv1.DNSPublicationManifestStatus{ExportState: "Ignored"}}
	if err := store.Create(ctx, m); err != nil {
		t.Fatal(err)
	}
	base := m.DeepCopy()
	m.Status.ExportState = "Acknowledged"
	m.Spec.Revision = 9
	if err := store.Status().Patch(ctx, m, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	if m.Spec.Revision != 1 || m.Status.ExportState != "Acknowledged" {
		t.Fatalf("status patch changed spec or lost status: %#v", m)
	}
	base = m.DeepCopy()
	m.Status.ExportState = "Pending"
	m.Spec.Revision = 2
	if err := store.Patch(ctx, m, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
	if m.Spec.Revision != 2 || m.Status.ExportState != "Acknowledged" {
		t.Fatalf("main patch changed status or lost spec: %#v", m)
	}
}

func TestStoreListsItsOwnWritesAndPassesOtherKindsThrough(t *testing.T) {
	ctx := t.Context()
	store, api, _ := newStore(t)
	for _, state := range []string{"Pending", "Acknowledged"} {
		out := &dnsv1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: "out-" + state, Namespace: "platform", Labels: map[string]string{"region": "east"}}}
		if err := store.Create(ctx, out); err != nil {
			t.Fatal(err)
		}
		out.Status.State = state
		if err := store.Status().Update(ctx, out); err != nil {
			t.Fatal(err)
		}
	}
	var pending dnsv1.DNSTransportOutboxList
	if err := store.List(ctx, &pending, client.InNamespace("platform"), client.MatchingLabels{"region": "east"}, client.MatchingFieldsSelector{Selector: fields.OneTermNotEqualSelector("status.state", "Acknowledged")}); err != nil {
		t.Fatal(err)
	}
	if len(pending.Items) != 1 || pending.Items[0].Name != "out-Pending" {
		t.Fatalf("pending list: %#v", pending.Items)
	}
	claim := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "platform"}}
	shard := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: shardOwnerPrefix + "east", Namespace: "platform"}, Data: map[string]string{"state": `{"holder":"a","epoch":1,"nextRevision":1}`}}
	for _, cm := range []*corev1.ConfigMap{claim, shard} {
		if err := store.Create(ctx, cm); err != nil {
			t.Fatal(err)
		}
	}
	var onAPI corev1.ConfigMapList
	if err := api.List(ctx, &onAPI); err != nil {
		t.Fatal(err)
	}
	if len(onAPI.Items) != 1 || onAPI.Items[0].Name != "claim" {
		t.Fatalf("only the address claim belongs on the API server: %#v", onAPI.Items)
	}
}

func TestWriterEpochsMoveToWallClock(t *testing.T) {
	now := time.Unix(1_790_000_000, 0)
	own := &dnsv1.DNSPublicationOwnership{Spec: dnsv1.DNSPublicationOwnershipSpec{WriterEpoch: 3}}
	RaiseWriterEpoch(own, false, now)
	if own.Spec.WriterEpoch != now.Unix() {
		t.Fatalf("counter epoch kept: %d", own.Spec.WriterEpoch)
	}
	own.Spec.WriterEpoch++
	RaiseWriterEpoch(own, false, now.Add(time.Hour))
	if own.Spec.WriterEpoch != now.Unix()+1 {
		t.Fatalf("wall-clock epoch moved again: %d", own.Spec.WriterEpoch)
	}
	shard := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: shardOwnerPrefix + "east"}, Data: map[string]string{"state": `{"epoch":2}`}}
	RaiseWriterEpoch(shard, false, now)
	if shard.Data["state"] != `{"epoch":2}` {
		t.Fatalf("shard epoch raised on update: %s", shard.Data["state"])
	}
	RaiseWriterEpoch(shard, true, now)
	if shard.Data["state"] != `{"epoch":1790000000}` {
		t.Fatalf("shard epoch not raised on create: %s", shard.Data["state"])
	}
}

func TestCollectKeepsActiveAndDependedOnPublications(t *testing.T) {
	ctx := t.Context()
	store, _, kv := newStore(t)
	old := time.Now().Add(-time.Hour)
	manifest := func(name string, revision int64) *dnsv1.DNSPublicationManifest {
		return &dnsv1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "platform"}, Spec: dnsv1.DNSPublicationManifestSpec{
			ZoneRef: dnsv1.DNSObjectReference{UID: "zone-a"}, WriterEpoch: 1, Revision: revision, GeneratedAt: metav1.NewTime(old),
			Chunks: []dnsv1.DNSPublicationChunkReference{{Name: name + "-000"}},
		}}
	}
	objects := []client.Object{
		&dnsv1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-a", Namespace: "platform"}, Spec: dnsv1.DNSPublicationOwnershipSpec{ZoneUID: "zone-a", ActiveManifestName: "r3"}},
		manifest("r1", 1), manifest("r2", 2), manifest("r3", 3),
	}
	for _, name := range []string{"r1", "r2", "r3"} {
		objects = append(objects,
			&dnsv1.DNSPublicationChunk{ObjectMeta: metav1.ObjectMeta{Name: name + "-000", Namespace: "platform"}},
			&dnsv1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: name + "-activation", Namespace: "platform"}, Spec: dnsv1.DNSTransportOutboxSpec{ManifestRef: dnsv1.DNSObjectReference{Name: name}}, Status: dnsv1.DNSTransportOutboxStatus{State: "Acknowledged"}})
	}
	objects = append(objects, &dnsv1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: "serving", Namespace: "platform"}, Spec: dnsv1.DNSTransportOutboxSpec{DependsOn: []string{"r2-activation"}}})
	for _, obj := range objects {
		if _, err := store.Seed(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]bool{
		"man.platform.r1": false, "chk.platform.r1-000": false, "out.platform.r1-activation": false,
		"man.platform.r2": true, "chk.platform.r2-000": true, "out.platform.r2-activation": true,
		"man.platform.r3": true, "chk.platform.r3-000": true, "out.platform.r3-activation": true,
	} {
		_, _, err := kv.Get(ctx, key)
		if (err == nil) != want {
			t.Errorf("%s kept=%v, want %v", key, err == nil, want)
		}
	}
}
