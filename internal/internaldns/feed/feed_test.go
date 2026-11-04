// SPDX-License-Identifier: AGPL-3.0-only

package feed

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/pubstore"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// mappedNamespace is a project namespace the hub placed in a region.
func mappedNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns-uid", Labels: map[string]string{replicatedLabel: "true"}}}
}

func contribution(origin string) *dnsv1alpha1.DNSRecordContribution {
	return &dnsv1alpha1.DNSRecordContribution{ObjectMeta: metav1.ObjectMeta{Name: "instance-a", Namespace: "ns-uid", Generation: 4, UID: "11111111-1111-1111-1111-111111111111", Labels: map[string]string{OriginLabel: origin, "app": "web"}}, Spec: dnsv1alpha1.DNSRecordContributionSpec{RegistrationRef: dnsv1alpha1.DNSObjectReference{Name: "web"}}}
}

// replay applies every key the bucket holds, as a mirror's watch would.
func replay(t *testing.T, kv *pubstore.MemoryKV, a *Applier) map[string]bool {
	t.Helper()
	keys, err := snapshot(t.Context(), kv)
	if err != nil {
		t.Fatal(err)
	}
	for key := range keys {
		value, _, err := kv.Get(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Apply(t.Context(), key, value); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func TestARegionsWriteReachesTheOtherRegionAndTheHub(t *testing.T) {
	ctx := context.Background()
	s := scheme(t)
	written := contribution("us-east-1")
	east := fake.NewClientBuilder().WithScheme(s).WithObjects(written).Build()
	hub := fake.NewClientBuilder().WithScheme(s).Build()
	central := fake.NewClientBuilder().WithScheme(s).WithObjects(mappedNamespace()).Build()
	kv := pubstore.NewMemoryKV()
	p := &Publisher{Region: "us-east-1", Local: east, KV: kv, Hub: hub}
	a := &Applier{Region: "us-east-1", Local: central, Mirror: kv}
	key := types.NamespacedName{Namespace: "ns-uid", Name: "instance-a"}
	gvk := Kinds[1]

	if err := p.Sync(ctx, gvk, key); err != nil {
		t.Fatal(err)
	}
	replay(t, kv, a)
	var copied dnsv1alpha1.DNSRecordContribution
	if err := central.Get(ctx, key, &copied); err != nil {
		t.Fatal(err)
	}
	if copied.Labels[OriginLabel] != "us-east-1" || copied.Labels["app"] != "web" || copied.Annotations[OriginGenerationAnnotation] != "4" || copied.Annotations[OriginUIDAnnotation] != "11111111-1111-1111-1111-111111111111" || copied.Spec.RegistrationRef.Name != "web" {
		t.Fatalf("central copy = %#v", copied)
	}
	var inHub dnsv1alpha1.DNSRecordContribution
	if err := hub.Get(ctx, key, &inHub); err != nil {
		t.Fatal(err)
	}
	if inHub.Labels[OriginLabel] != "us-east-1" || inHub.Labels["internal-dns.miloapis.com/replicated"] != "" {
		t.Fatalf("hub copy = %#v", inHub)
	}

	if err := east.Delete(ctx, written); err != nil {
		t.Fatal(err)
	}
	if err := p.Sync(ctx, gvk, key); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(ctx, Key(gvk.Kind, key.Namespace, key.Name), nil); err != nil {
		t.Fatal(err)
	}
	if err := central.Get(ctx, key, &copied); !apierrors.IsNotFound(err) {
		t.Fatalf("central copy outlived the writer's delete: %v", err)
	}
	if err := hub.Get(ctx, key, &inHub); !apierrors.IsNotFound(err) {
		t.Fatalf("hub copy outlived the writer's delete: %v", err)
	}
	if _, _, err := kv.Get(ctx, Key(gvk.Kind, key.Namespace, key.Name)); err == nil {
		t.Fatal("the feed kept a deleted object's key")
	}
}

func TestOnlyTheWritingRegionsObjectsArePublishedOrReplaced(t *testing.T) {
	ctx := context.Background()
	s := scheme(t)
	fromProject := contribution("project")
	east := fake.NewClientBuilder().WithScheme(s).WithObjects(fromProject).Build()
	kv := pubstore.NewMemoryKV()
	p := &Publisher{Region: "us-east-1", Local: east, KV: kv}
	key := types.NamespacedName{Namespace: "ns-uid", Name: "instance-a"}
	if err := p.Sync(ctx, Kinds[1], key); err != nil {
		t.Fatal(err)
	}
	if keys, _ := snapshot(ctx, kv); len(keys) != 0 {
		t.Fatalf("published an object the region did not write: %v", keys)
	}

	central := fake.NewClientBuilder().WithScheme(s).WithObjects(mappedNamespace(), contribution("project")).Build()
	other := fake.NewClientBuilder().WithScheme(s).WithObjects(contribution("us-east-1")).Build()
	otherKV := pubstore.NewMemoryKV()
	if err := (&Publisher{Region: "us-east-1", Local: other, KV: otherKV}).Sync(ctx, Kinds[1], key); err != nil {
		t.Fatal(err)
	}
	value, _, _ := otherKV.Get(ctx, Key("DNSRecordContribution", "ns-uid", "instance-a"))
	if err := (&Applier{Region: "us-east-1", Local: central, Mirror: otherKV}).Apply(ctx, Key("DNSRecordContribution", "ns-uid", "instance-a"), value); err == nil {
		t.Fatal("another region's write replaced an object the project owns")
	}
}

func TestApplierRemovesCopiesTheWriterNoLongerHolds(t *testing.T) {
	s := scheme(t)
	stale := contribution("us-east-1")
	mine := contribution("project")
	mine.Name = "project-owned"
	central := fake.NewClientBuilder().WithScheme(s).WithObjects(stale, mine).Build()
	a := &Applier{Region: "us-east-1", Local: central}
	if err := a.prune(t.Context(), map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if err := central.Get(t.Context(), client.ObjectKeyFromObject(stale), stale); !apierrors.IsNotFound(err) {
		t.Fatalf("a copy the writer withdrew survived resync: %v", err)
	}
	if err := central.Get(t.Context(), client.ObjectKeyFromObject(mine), mine); err != nil {
		t.Fatalf("resync removed an object of another origin: %v", err)
	}
}

func TestResyncWithdrawsKeysWhoseObjectIsGone(t *testing.T) {
	ctx := context.Background()
	s := scheme(t)
	east := fake.NewClientBuilder().WithScheme(s).Build()
	kv := pubstore.NewMemoryKV()
	if _, err := kv.Create(ctx, Key("DNSRecordContribution", "ns-uid", "gone"), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := (&Publisher{Region: "us-east-1", Local: east, KV: kv}).Resync(ctx); err != nil {
		t.Fatal(err)
	}
	if keys, _ := snapshot(ctx, kv); len(keys) != 0 {
		t.Fatalf("keys after resync = %v", keys)
	}
}

func TestAPeerCannotWriteOutsideProjectNamespaces(t *testing.T) {
	ctx := context.Background()
	s := scheme(t)
	other := contribution("us-east-1")
	other.Namespace = "kube-system"
	east := fake.NewClientBuilder().WithScheme(s).WithObjects(other).Build()
	kv := pubstore.NewMemoryKV()
	if err := (&Publisher{Region: "us-east-1", Local: east, KV: kv}).Sync(ctx, Kinds[1], types.NamespacedName{Namespace: "kube-system", Name: "instance-a"}); err != nil {
		t.Fatal(err)
	}
	central := fake.NewClientBuilder().WithScheme(s).WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}}).Build()
	key := Key("DNSRecordContribution", "kube-system", "instance-a")
	value, _, _ := kv.Get(ctx, key)
	if err := (&Applier{Region: "us-east-1", Local: central}).Apply(ctx, key, value); err == nil {
		t.Fatal("a peer wrote into a namespace the hub did not place")
	}
}

func TestARecreatedWriterObjectReplacesTheStaleCopy(t *testing.T) {
	ctx := context.Background()
	s := scheme(t)
	stale := contribution("us-east-1")
	stale.Annotations = map[string]string{OriginUIDAnnotation: "33333333-3333-3333-3333-333333333333", OriginGenerationAnnotation: "9"}
	central := fake.NewClientBuilder().WithScheme(s).WithObjects(mappedNamespace(), stale).Build()
	recreated := contribution("us-east-1")
	recreated.Generation = 1
	recreated.UID = "22222222-2222-2222-2222-222222222222"
	east := fake.NewClientBuilder().WithScheme(s).WithObjects(recreated).Build()
	kv := pubstore.NewMemoryKV()
	key := types.NamespacedName{Namespace: "ns-uid", Name: "instance-a"}
	if err := (&Publisher{Region: "us-east-1", Local: east, KV: kv}).Sync(ctx, Kinds[1], key); err != nil {
		t.Fatal(err)
	}
	a := &Applier{Region: "us-east-1", Local: central}
	feedKey := Key("DNSRecordContribution", key.Namespace, key.Name)
	value, _, _ := kv.Get(ctx, feedKey)
	if err := a.Apply(ctx, feedKey, value); err == nil {
		t.Fatal("the stale copy was kept without asking for another pass")
	}
	if err := a.Apply(ctx, feedKey, value); err != nil {
		t.Fatal(err)
	}
	var copied dnsv1alpha1.DNSRecordContribution
	if err := central.Get(ctx, key, &copied); err != nil {
		t.Fatal(err)
	}
	if copied.Annotations[OriginUIDAnnotation] != "22222222-2222-2222-2222-222222222222" || copied.Annotations[OriginGenerationAnnotation] != "1" {
		t.Fatalf("copy after recreate = %#v", copied.Annotations)
	}
}

func TestApplierPrunesOnlyOnceTheMirrorIsCaughtUp(t *testing.T) {
	s := scheme(t)
	central := fake.NewClientBuilder().WithScheme(s).WithObjects(mappedNamespace(), contribution("us-east-1")).Build()
	var caughtUp atomic.Bool
	a := &Applier{Region: "us-east-1", Local: central, Mirror: pubstore.NewMemoryKV(), Interval: 10 * time.Millisecond, CaughtUp: func(context.Context) (bool, error) { return caughtUp.Load(), nil }}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- a.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	var copied dnsv1alpha1.DNSRecordContribution
	if err := central.Get(t.Context(), types.NamespacedName{Namespace: "ns-uid", Name: "instance-a"}, &copied); err != nil {
		t.Fatalf("an empty mirror that is not caught up withdrew a copy: %v", err)
	}
	caughtUp.Store(true)
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := central.Get(t.Context(), types.NamespacedName{Namespace: "ns-uid", Name: "instance-a"}, &copied)
		if apierrors.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a caught-up mirror never removed the withdrawn copy")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAPeerCannotClaimAnotherObjectsUID(t *testing.T) {
	ctx := context.Background()
	s := scheme(t)
	victim := &dnsv1alpha1.DNSRecordContribution{ObjectMeta: metav1.ObjectMeta{Name: "victim", Namespace: "ns-uid", UID: "44444444-4444-4444-4444-444444444444"}}
	central := fake.NewClientBuilder().WithScheme(s).WithObjects(mappedNamespace(), victim).Build()
	impostor := contribution("us-east-1")
	impostor.Annotations = map[string]string{OriginUIDAnnotation: "44444444-4444-4444-4444-444444444444"}
	east := fake.NewClientBuilder().WithScheme(s).WithObjects(impostor).Build()
	kv := pubstore.NewMemoryKV()
	key := types.NamespacedName{Namespace: "ns-uid", Name: "instance-a"}
	if err := (&Publisher{Region: "us-east-1", Local: east, KV: kv}).Sync(ctx, Kinds[1], key); err != nil {
		t.Fatal(err)
	}
	feedKey := Key("DNSRecordContribution", key.Namespace, key.Name)
	value, _, _ := kv.Get(ctx, feedKey)
	err := (&Applier{Region: "us-east-1", Local: central}).Apply(ctx, feedKey, value)
	var refused *refusedError
	if !errors.As(err, &refused) {
		t.Fatalf("a peer claimed another object's UID: %v", err)
	}
}
