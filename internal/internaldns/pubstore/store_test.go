// SPDX-License-Identifier: AGPL-3.0-only

package pubstore

import (
	"context"
	"errors"
	"testing"
	"time"

	dnsv1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
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

func seedPublications(t *testing.T, store *PublicationStore, dependsOn ...string) {
	t.Helper()
	now := time.Now()
	exported := map[string]time.Time{"r1": now.Add(-2 * time.Hour), "r2": now.Add(-time.Hour), "r3": now.Add(-time.Minute)}
	objects := []client.Object{&dnsv1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-a", Namespace: "platform"}, Spec: dnsv1.DNSPublicationOwnershipSpec{ZoneUID: "zone-a", ActiveManifestName: "r3"}}}
	for i, name := range []string{"r1", "r2", "r3"} {
		at := metav1.NewTime(exported[name])
		objects = append(objects,
			&dnsv1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "platform"}, Spec: dnsv1.DNSPublicationManifestSpec{
				ZoneRef: dnsv1.DNSObjectReference{UID: "zone-a"}, WriterEpoch: 1, Revision: int64(i + 1), GeneratedAt: at,
				Chunks: []dnsv1.DNSPublicationChunkReference{{Name: name + "-000"}},
			}, Status: dnsv1.DNSPublicationManifestStatus{ExportedAt: &at}},
			&dnsv1.DNSPublicationChunk{ObjectMeta: metav1.ObjectMeta{Name: name + "-000", Namespace: "platform"}},
			&dnsv1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: name + "-activation", Namespace: "platform"}, Spec: dnsv1.DNSTransportOutboxSpec{ManifestRef: dnsv1.DNSObjectReference{Name: name}}, Status: dnsv1.DNSTransportOutboxStatus{State: "Acknowledged"}})
	}
	objects = append(objects, &dnsv1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: "serving", Namespace: "platform"}, Spec: dnsv1.DNSTransportOutboxSpec{DependsOn: dependsOn}})
	for _, obj := range objects {
		if _, err := store.Seed(t.Context(), obj); err != nil {
			t.Fatal(err)
		}
	}
}

func assertKept(t *testing.T, kv *MemoryKV, want map[string]bool) {
	t.Helper()
	for key, kept := range want {
		_, _, err := kv.Get(t.Context(), key)
		if (err == nil) != kept {
			t.Errorf("%s kept=%v, want %v", key, err == nil, kept)
		}
	}
}

func TestCollectRetainsFromWhenAPublicationWasSuperseded(t *testing.T) {
	store, _, kv := newStore(t)
	seedPublications(t, store)
	if err := store.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertKept(t, kv, map[string]bool{
		"man.platform.r1": false, "chk.platform.r1-000": false, "out.platform.r1-activation": false,
		"man.platform.r2": true, "chk.platform.r2-000": true, "out.platform.r2-activation": true,
		"man.platform.r3": true, "chk.platform.r3-000": true, "out.platform.r3-activation": true,
	})
}

func TestCollectKeepsWhatAPendingOutboxDependsOn(t *testing.T) {
	store, _, kv := newStore(t)
	seedPublications(t, store, "r1-activation")
	if err := store.Collect(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertKept(t, kv, map[string]bool{"man.platform.r1": true, "chk.platform.r1-000": true, "out.platform.r1-activation": true})
}

func TestCollectDeletesOnlyServingOutboxesOlderThanTheActiveOne(t *testing.T) {
	ctx := t.Context()
	store, _, kv := newStore(t)
	old := metav1.NewTime(time.Now().Add(-time.Hour))
	lease := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: shardOwnerPrefix + "east", Namespace: "platform", UID: "shard"}, Data: map[string]string{"state": `{"epoch":7,"activeOutbox":"serving-r5"}`}}
	objects := []client.Object{lease}
	for _, r := range []struct {
		name     string
		epoch    int64
		revision int64
	}{{"serving-r4", 7, 4}, {"serving-r5", 7, 5}, {"serving-r6", 7, 6}} {
		objects = append(objects, &dnsv1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: r.name, Namespace: "platform", CreationTimestamp: old, Labels: map[string]string{shardOwnerLabel: lease.Name}},
			Spec: dnsv1.DNSTransportOutboxSpec{ResourceUID: "shard", WriterEpoch: r.epoch, Revision: r.revision}, Status: dnsv1.DNSTransportOutboxStatus{State: "Acknowledged"}})
	}
	for _, obj := range objects {
		if _, err := store.Seed(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Collect(ctx); err != nil {
		t.Fatal(err)
	}
	assertKept(t, kv, map[string]bool{"out.platform.serving-r4": false, "out.platform.serving-r5": true, "out.platform.serving-r6": true})
}

func TestStoresSharingABucketSeeEachOthersWrites(t *testing.T) {
	ctx := t.Context()
	a, _, kv := newStore(t)
	b := New(fake.NewClientBuilder().Build(), kv)
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	own := &dnsv1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-a", Namespace: "platform"}, Spec: dnsv1.DNSPublicationOwnershipSpec{ZoneUID: "zone-a"}}
	if err := a.Create(ctx, own); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var list dnsv1.DNSPublicationOwnershipList
		if err := b.List(ctx, &list); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second store never saw the first store's write")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := a.Delete(ctx, own); err != nil {
		t.Fatal(err)
	}
	for {
		var list dnsv1.DNSPublicationOwnershipList
		if err := b.List(ctx, &list); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("second store never saw the delete")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRegionalRecordsLiveInTheirRegionsBucket(t *testing.T) {
	ctx := t.Context()
	home, east := NewMemoryKV(), NewMemoryKV()
	store := NewRegional(fake.NewClientBuilder().Build(), "central", map[string]KV{"central": home, "east": east}, map[string]string{"central": "central", "east": "east"})
	if err := store.Start(ctx); err != nil {
		t.Fatal(err)
	}
	out := &dnsv1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: "activation-east", Namespace: "platform", Labels: map[string]string{regionLabel: "east"}}}
	own := &dnsv1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-a", Namespace: "platform"}}
	for _, obj := range []client.Object{out, own} {
		if err := store.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := east.Get(ctx, "out.platform.activation-east"); err != nil {
		t.Fatalf("east outbox not in the east bucket: %v", err)
	}
	if _, _, err := home.Get(ctx, "own.platform.owner-a"); err != nil {
		t.Fatalf("zone lease not in the home bucket: %v", err)
	}
	base := out.DeepCopy()
	out.Status.State = "Acknowledged"
	if err := store.Status().Patch(ctx, out, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		t.Fatalf("status patch through the regional bucket: %v", err)
	}
	var got dnsv1.DNSTransportOutbox
	if err := store.Get(ctx, client.ObjectKeyFromObject(out), &got); err != nil || got.Status.State != "Acknowledged" {
		t.Fatalf("regional read: %v %#v", err, got.Status)
	}
	var list dnsv1.DNSTransportOutboxList
	if err := store.List(ctx, &list); err != nil || len(list.Items) != 1 {
		t.Fatalf("list across buckets: %v %d", err, len(list.Items))
	}
	relabelled := got.DeepCopy()
	relabelled.Labels[regionLabel] = "central"
	if err := store.Update(ctx, relabelled); err != nil {
		t.Fatalf("update of a relabelled record: %v", err)
	}
	if _, _, err := east.Get(ctx, "out.platform.activation-east"); err != nil {
		t.Fatalf("relabelled record left the bucket that holds it: %v", err)
	}
	duplicate := &dnsv1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: "activation-east", Namespace: "platform"}}
	if err := store.Create(ctx, duplicate); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("same name created in a second bucket: %v", err)
	}
}

func TestRegionsSharingABrokerShareOneBucket(t *testing.T) {
	ctx := t.Context()
	shared := NewMemoryKV()
	store := NewRegional(fake.NewClientBuilder().Build(), "broker", map[string]KV{"broker": shared}, map[string]string{"central": "broker", "east": "broker"})
	if err := store.Start(ctx); err != nil {
		t.Fatal(err)
	}
	out := &dnsv1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: "activation-east", Namespace: "platform", Labels: map[string]string{regionLabel: "east"}}}
	if err := store.Create(ctx, out); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		base := out.DeepCopy()
		out.Status.Attempts = int32(i + 1)
		if err := store.Status().Patch(ctx, out, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			t.Fatalf("write %d on a shared broker: %v", i, err)
		}
	}
}

type unreachableKV struct{ *MemoryKV }

func (unreachableKV) Get(context.Context, string) ([]byte, uint64, error) {
	return nil, 0, errors.New("broker unreachable")
}

func (unreachableKV) Watch(context.Context) (<-chan *transport.KVEvent, error) {
	return nil, errors.New("broker unreachable")
}

func TestOneRegionsBrokerOutageLeavesTheOthersWorking(t *testing.T) {
	ctx := t.Context()
	home := NewMemoryKV()
	store := NewRegional(fake.NewClientBuilder().Build(), "central", map[string]KV{"central": home, "east": unreachableKV{NewMemoryKV()}}, map[string]string{"central": "central", "east": "east"})
	if err := store.Start(ctx); err != nil {
		t.Fatalf("start waited on an unreachable regional broker: %v", err)
	}
	own := &dnsv1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: "owner-a", Namespace: "platform"}}
	if err := store.Get(ctx, client.ObjectKeyFromObject(own), own); !apierrors.IsNotFound(err) {
		t.Fatalf("zone lease lookup depended on the regional broker: %v", err)
	}
	central := &dnsv1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: "activation-central", Namespace: "platform", Labels: map[string]string{regionLabel: "central"}}}
	if err := store.Create(ctx, central); err != nil {
		t.Fatal(err)
	}
	var found dnsv1.DNSTransportOutbox
	if err := store.Get(ctx, client.ObjectKeyFromObject(central), &found); err != nil {
		t.Fatalf("central record lookup depended on the regional broker: %v", err)
	}
	if err := store.Collect(ctx); err == nil {
		t.Fatal("collected without every region's records loaded")
	}
}

func TestReplayDropsRecordsDeletedWhileTheWatchWasDown(t *testing.T) {
	store, _, kv := newStore(t)
	store.apply("", "out.platform.gone", 7, []byte(`{"metadata":{"name":"gone","namespace":"platform"}}`))
	store.dropUnseen("", map[string]bool{}, kv.revision)
	store.mu.RLock()
	_, kept := store.index["out.platform.gone"]
	store.mu.RUnlock()
	if !kept {
		t.Fatal("dropped a record newer than the replay")
	}
	store.dropUnseen("", map[string]bool{}, 7)
	store.mu.RLock()
	_, kept = store.index["out.platform.gone"]
	store.mu.RUnlock()
	if kept {
		t.Fatal("kept a record the replay up to its revision did not carry")
	}
}
