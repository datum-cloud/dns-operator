// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	dns "go.miloapis.com/dns-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func inputCache(t *testing.T, h *perZoneHarness) client.Client {
	t.Helper()
	objects := []client.Object{}
	for _, list := range []client.ObjectList{&dns.DNSRegistrationList{}, &dns.DNSRecordSetList{}, &dns.DNSRecordContributionList{}} {
		if err := h.c.List(h.ctx, list); err != nil {
			t.Fatal(err)
		}
		switch v := list.(type) {
		case *dns.DNSRegistrationList:
			for i := range v.Items {
				objects = append(objects, &v.Items[i])
			}
		case *dns.DNSRecordSetList:
			for i := range v.Items {
				objects = append(objects, &v.Items[i])
			}
		case *dns.DNSRecordContributionList:
			for i := range v.Items {
				objects = append(objects, &v.Items[i])
			}
		}
	}
	return fake.NewClientBuilder().WithScheme(h.r.Scheme).WithObjects(objects...).
		WithIndex(&dns.DNSRegistration{}, inputZoneIndex, func(o client.Object) []string { return []string{o.(*dns.DNSRegistration).Spec.DNSZoneRef.Name} }).
		WithIndex(&dns.DNSRecordSet{}, inputZoneIndex, func(o client.Object) []string { return []string{o.(*dns.DNSRecordSet).Spec.DNSZoneRef.Name} }).
		WithIndex(&dns.DNSRecordContribution{}, inputRegistrationIndex, func(o client.Object) []string {
			return []string{o.(*dns.DNSRecordContribution).Spec.RegistrationRef.Name}
		}).Build()
}

type countInputLists struct {
	client.Reader
	lists  int
	forbid bool
}

func (c *countInputLists) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	switch list.(type) {
	case *dns.DNSRegistrationList, *dns.DNSRecordContributionList, *dns.DNSRecordSetList:
		c.lists++
		if c.forbid {
			return fmt.Errorf("unexpected live input list: %T", list)
		}
	}
	return c.Reader.List(ctx, list, opts...)
}

func TestCachedZoneInputsSteadyStateAvoidsLiveLists(t *testing.T) {
	h, z := newPerZoneHarness(t)
	h.settle()
	h.r.Options.CachedZoneInputs = true
	h.r.InputReader = inputCache(t, h)
	reader := &countInputLists{Reader: h.c}
	h.r.APIReader = reader
	h.run(zoneQueuePrefix + z.Name) // live baseline
	h.run(zoneQueuePrefix + z.Name) // initial shadow
	h.r.InputReader = inputCache(t, h)
	reader.forbid = true
	h.run(zoneQueuePrefix + z.Name)
	if len(h.publishedNames()) != 1 {
		t.Fatal("cached compile withdrew records")
	}
}

func TestCachedZoneInputsNewEpochUsesLiveBaseline(t *testing.T) {
	h, z := newPerZoneHarness(t)
	h.settle()
	h.r.Options.CachedZoneInputs = true
	cache := inputCache(t, h)
	h.r.InputReader = cache
	reader := &countInputLists{Reader: h.c}
	h.r.APIReader = reader
	h.run(zoneQueuePrefix + z.Name)
	h.run(zoneQueuePrefix + z.Name)
	var regs dns.DNSRegistrationList
	if err := h.c.List(h.ctx, &regs); err != nil {
		t.Fatal(err)
	}
	reg := &regs.Items[0]
	reg.Generation++
	if err := h.c.Update(h.ctx, reg); err != nil {
		t.Fatal(err)
	}
	var own dns.DNSPublicationOwnership
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: h.r.Options.PlatformNamespace, Name: ownershipName(z.UID)}, &own); err != nil {
		t.Fatal(err)
	}
	own.Spec.WriterEpoch++
	if err := h.c.Update(h.ctx, &own); err != nil {
		t.Fatal(err)
	}
	before := reader.lists
	h.run(zoneQueuePrefix + z.Name)
	if reader.lists == before {
		t.Fatal("new writer epoch reused stale cached inputs")
	}
	value, _ := h.r.zoneInputs.Load(z.UID)
	if value.(*zoneInputState).ready {
		t.Fatal("stale cache passed new epoch baseline")
	}
}

func TestInputsCaughtUpUsesUIDGenerationAndDeletion(t *testing.T) {
	live := zoneInputs{registrations: []dns.DNSRegistration{{ObjectMeta: metav1.ObjectMeta{Name: "a", UID: "uid", Generation: 4, ResourceVersion: "opaque-z"}}}}
	for _, tc := range []struct {
		name  string
		uid   types.UID
		gen   int64
		extra bool
		want  bool
	}{
		{"same", "uid", 4, false, true}, {"ahead", "uid", 5, false, true}, {"behind", "uid", 3, false, false}, {"replacement", "new", 4, false, false}, {"extra", "uid", 4, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cached := zoneInputs{registrations: []dns.DNSRegistration{*live.registrations[0].DeepCopy()}}
			cached.registrations[0].UID, cached.registrations[0].Generation, cached.registrations[0].ResourceVersion = tc.uid, tc.gen, "opaque-a"
			if tc.extra {
				cached.records = []dns.DNSRecordSet{{ObjectMeta: metav1.ObjectMeta{Name: "extra"}}}
			}
			if got := inputsCaughtUp(cached, live); got != tc.want {
				t.Fatalf("caught up = %v", got)
			}
		})
	}
	cached := zoneInputs{registrations: []dns.DNSRegistration{*live.registrations[0].DeepCopy()}}
	now := metav1.Now()
	cached.registrations[0].DeletionTimestamp = &now
	if inputsCaughtUp(cached, live) {
		t.Fatal("unobserved deletion passed baseline")
	}
}

func TestCachedMissingContributionConfirmedLive(t *testing.T) {
	h, z := newPerZoneHarness(t)
	h.settle()
	var cs dns.DNSRecordContributionList
	if err := h.c.List(h.ctx, &cs); err != nil {
		t.Fatal(err)
	}
	c := cs.Items[0]
	in := CompileInput{Previous: &PublicationPlan{Contributions: []ContributionFence{{UID: c.UID, Generation: c.Generation, SourceName: c.Name}}}}
	ok, err := h.r.cachedFencesCurrent(h.ctx, z, &in)
	if err != nil || !ok || len(in.Contributions) != 1 {
		t.Fatalf("live fenced object not restored: %v %v %#v", ok, err, in.Contributions)
	}
	if err := h.c.Delete(h.ctx, &c); err != nil {
		t.Fatal(err)
	}
	in.Contributions = nil
	ok, err = h.r.cachedFencesCurrent(h.ctx, z, &in)
	if err != nil || !ok || len(in.Contributions) != 0 {
		t.Fatalf("confirmed deletion not retired: %v %v", ok, err)
	}
}

func TestCachedNewRejectionRecompilesLive(t *testing.T) {
	h, z := newPerZoneHarness(t)
	h.settle()
	h.r.Options.CachedZoneInputs = true
	h.r.InputReader = inputCache(t, h)
	h.run(zoneQueuePrefix + z.Name)
	h.run(zoneQueuePrefix + z.Name)
	value, _ := h.r.zoneInputs.Load(z.UID)
	state := value.(*zoneInputState)
	if len(state.accepted) == 0 {
		t.Fatal("accepted inputs were not remembered")
	}
	before := h.publishedNames()
	var cs dns.DNSRecordContributionList
	if err := h.r.InputReader.List(h.ctx, &cs); err != nil {
		t.Fatal(err)
	}
	c := cs.Items[0].DeepCopy()
	// A newer cached registration pin while the contribution watch still lags.
	c.Spec.RegistrationRef.Generation++
	c.Generation++
	if err := h.r.InputReader.(client.Client).Update(h.ctx, c); err != nil {
		t.Fatal(err)
	}
	reader := &countInputLists{Reader: h.c}
	h.r.APIReader = reader
	h.run(zoneQueuePrefix + z.Name)
	if reader.lists == 0 {
		t.Fatal("new rejection was not confirmed live")
	}
	if got := h.publishedNames(); !reflect.DeepEqual(got, before) {
		t.Fatalf("cache skew withdrew records: %v -> %v", before, got)
	}
}

func TestCachedAccessChangeBlocksPublication(t *testing.T) {
	h, z := newPerZoneHarness(t)
	h.settle()
	var assocs dns.DNSZoneAssociationList
	if err := h.c.List(h.ctx, &assocs); err != nil {
		t.Fatal(err)
	}
	var current dns.DNSZone
	if err := h.c.Get(h.ctx, client.ObjectKeyFromObject(z), &current); err != nil {
		t.Fatal(err)
	}
	guard, err := h.r.guardZoneAccess(h.ctx, &current, assocs.Items)
	if err != nil {
		t.Fatal(err)
	}
	a := &assocs.Items[0]
	a.Generation++
	if err := h.c.Update(h.ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := guard(CompileInput{}); err == nil {
		t.Fatal("changed access snapshot allowed publication")
	}
}

func TestCachedZoneRecordChangesPublishWithoutLiveLists(t *testing.T) {
	h, z := newPerZoneHarness(t)
	h.settle()
	h.r.Options.CachedZoneInputs = true
	h.r.InputReader = inputCache(t, h)
	h.run(zoneQueuePrefix + z.Name)
	h.run(zoneQueuePrefix + z.Name)
	h.run(zoneQueuePrefix + z.Name)
	ttl := int64(30)
	record := &dns.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "added", Namespace: z.Namespace, UID: "static-added", Generation: 1}, Spec: dns.DNSRecordSetSpec{DNSZoneRef: corev1.LocalObjectReference{Name: z.Name}, RecordType: dns.RRTypeA, Records: []dns.RecordEntry{{Name: "cached", TTL: &ttl, A: &dns.ARecordSpec{Content: "192.0.2.20"}}}}}
	if err := h.c.Create(h.ctx, record); err != nil {
		t.Fatal(err)
	}
	h.r.InputReader = inputCache(t, h)
	h.r.APIReader = &countInputLists{Reader: h.c, forbid: true}
	h.run(zoneQueuePrefix + z.Name)
	if len(h.publishedNames()) != 2 {
		t.Fatal("cached addition did not publish")
	}
	// Deletions are confirmed with a live compile before withdrawing accepted input.
	h.r.APIReader = h.c
	if err := h.c.Delete(h.ctx, record); err != nil {
		t.Fatal(err)
	}
	h.r.InputReader = inputCache(t, h)
	h.run(zoneQueuePrefix + z.Name)
	if len(h.publishedNames()) != 1 {
		t.Fatal("confirmed deletion did not publish")
	}
}

type countChunkGets struct {
	client.Client
	chunks int
}

func (c *countChunkGets) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*dns.DNSPublicationChunk); ok {
		c.chunks++
	}
	return c.Client.Get(ctx, key, obj, opts...)
}
func TestCachedPreviousPlanByManifest(t *testing.T) {
	h, z := newPerZoneHarness(t)
	h.settle()
	h.r.Options.CachedZoneInputs = true
	counted := &countChunkGets{Client: h.c}
	h.r.Client = counted
	var own dns.DNSPublicationOwnership
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: h.r.Options.PlatformNamespace, Name: ownershipName(z.UID)}, &own); err != nil {
		t.Fatal(err)
	}
	first, err := h.r.loadPrevious(h.ctx, &own)
	if err != nil {
		t.Fatal(err)
	}
	if counted.chunks == 0 || len(first.Registrations) == 0 {
		t.Fatal("previous plan lost registration fences")
	}
	count := counted.chunks
	second, err := h.r.loadPrevious(h.ctx, &own)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) || counted.chunks != count {
		t.Fatal("unchanged manifest reloaded chunks")
	}
	own.Spec.ActiveManifestName = "missing"
	if _, err := h.r.loadPrevious(h.ctx, &own); err == nil {
		t.Fatal("changed manifest reused previous cache")
	}
}

func TestCachedStatusPreservesLivePublishedCondition(t *testing.T) {
	h, _ := newPerZoneHarness(t)
	h.settle()
	var regs dns.DNSRegistrationList
	if err := h.c.List(h.ctx, &regs); err != nil {
		t.Fatal(err)
	}
	stale := regs.Items[0].DeepCopy()
	live := stale.DeepCopy()
	live.Status.PublicationRevision = 91
	if err := h.c.Status().Update(h.ctx, live); err != nil {
		t.Fatal(err)
	}
	if err := h.r.patchInputStatus(h.ctx, stale, func(o client.Object) { o.(*dns.DNSRegistration).Status.CanonicalFQDN = "changed.example." }); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Get(h.ctx, client.ObjectKeyFromObject(live), live); err != nil {
		t.Fatal(err)
	}
	if live.Status.PublicationRevision != 91 {
		t.Fatal("cached status overwrote live publication")
	}
	stale.Generation--
	if err := h.r.patchInputStatus(h.ctx, stale, func(o client.Object) { o.(*dns.DNSRegistration).Status.CanonicalFQDN = "rejected.example." }); err == nil {
		t.Fatal("stale generation allowed status write")
	}
}

func TestCachedEpochRequiresSuccessfulLiveCompile(t *testing.T) {
	h, z := newPerZoneHarness(t)
	h.settle()
	h.r.Options.CachedZoneInputs = true
	cache := inputCache(t, h)
	h.r.InputReader = cache
	for _, list := range []client.ObjectList{&dns.DNSRegistrationList{}, &dns.DNSRecordSetList{}, &dns.DNSRecordContributionList{}} {
		if err := cache.List(h.ctx, list); err != nil {
			t.Fatal(err)
		}
		objects, err := apimeta.ExtractList(list)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range objects {
			if err := cache.Delete(h.ctx, o.(client.Object)); err != nil {
				t.Fatal(err)
			}
		}
	}
	reader := &countInputLists{Reader: h.c, forbid: true}
	h.r.APIReader = reader
	var own dns.DNSPublicationOwnership
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: h.r.Options.PlatformNamespace, Name: ownershipName(z.UID)}, &own); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := h.r.selectZoneInputs(h.ctx, z, &own); err == nil {
			t.Fatalf("attempt %d accepted an uninitialized empty cache", i)
		}
	}
	reader.forbid = false
	_, session, err := h.r.selectZoneInputs(h.ctx, z, &own)
	if err != nil || session.cached {
		t.Fatalf("recovery did not start live: %v", err)
	}
}

func TestCachedRecreatedContributionNotDuplicated(t *testing.T) {
	h, z := newPerZoneHarness(t)
	h.settle()
	var cs dns.DNSRecordContributionList
	if err := h.c.List(h.ctx, &cs); err != nil {
		t.Fatal(err)
	}
	c := cs.Items[0]
	in := CompileInput{Contributions: []dns.DNSRecordContribution{c}, Previous: &PublicationPlan{Contributions: []ContributionFence{{UID: "old-uid", Generation: 1, SourceName: c.Name}}}}
	ok, err := h.r.cachedFencesCurrent(h.ctx, z, &in)
	if err != nil || !ok || len(in.Contributions) != 1 {
		t.Fatalf("recreated contribution duplicated: %v %v %d", ok, err, len(in.Contributions))
	}
}

func TestCachedRemovingLastAssociationPublishes(t *testing.T) {
	h, z := newPerZoneHarness(t)
	h.settle()
	h.r.Options.CachedZoneInputs = true
	h.r.InputReader = inputCache(t, h)
	h.run(zoneQueuePrefix + z.Name)
	h.run(zoneQueuePrefix + z.Name)
	var assocs dns.DNSZoneAssociationList
	if err := h.c.List(h.ctx, &assocs); err != nil {
		t.Fatal(err)
	}
	for i := range assocs.Items {
		if err := h.c.Delete(h.ctx, &assocs.Items[i]); err != nil {
			t.Fatal(err)
		}
	}
	if result := h.run(zoneQueuePrefix + z.Name); result.RequeueAfter == zoneConflictRetry {
		t.Fatal("empty associations caused permanent conflict")
	}
	var own dns.DNSPublicationOwnership
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: h.r.Options.PlatformNamespace, Name: ownershipName(z.UID)}, &own); err != nil {
		t.Fatal(err)
	}
	var manifest dns.DNSPublicationManifest
	if err := h.c.Get(h.ctx, client.ObjectKey{Namespace: h.r.Options.PlatformNamespace, Name: own.Spec.ActiveManifestName}, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Spec.ContextUIDs) != 0 {
		t.Fatal("removed association retained published context")
	}
}

func TestCachedAccessRejectsAlreadyRevokedContext(t *testing.T) {
	for _, kind := range []string{"deleted", "recreated", "not-ready"} {
		t.Run(kind, func(t *testing.T) {
			h, z := newPerZoneHarness(t)
			h.settle()
			var assocs dns.DNSZoneAssociationList
			if err := h.c.List(h.ctx, &assocs); err != nil {
				t.Fatal(err)
			}
			var current dns.DNSZone
			if err := h.c.Get(h.ctx, client.ObjectKeyFromObject(z), &current); err != nil {
				t.Fatal(err)
			}
			var c dns.DNSResolverContext
			key := client.ObjectKey{Namespace: z.Namespace, Name: assocs.Items[0].Spec.ResolverContextRef.Name}
			if err := h.c.Get(h.ctx, key, &c); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "deleted":
				if err := h.c.Delete(h.ctx, &c); err != nil {
					t.Fatal(err)
				}
			case "recreated":
				c.UID = "replacement"
				if err := h.c.Update(h.ctx, &c); err != nil {
					t.Fatal(err)
				}
			case "not-ready":
				c.Status.Conditions = nil
				if err := h.c.Status().Update(h.ctx, &c); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.r.guardZoneAccess(h.ctx, &current, assocs.Items); err == nil {
				t.Fatal("revoked context accepted")
			}
		})
	}
}

type countAccessReads struct {
	client.Client
	references, associations int
}

func (c *countAccessReads) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.ConfigMap); ok {
		c.references++
	}
	return c.Client.Get(ctx, key, obj, opts...)
}
func (c *countAccessReads) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*dns.DNSZoneAssociationList); ok {
		c.associations++
	}
	return c.Client.List(ctx, list, opts...)
}
func TestCachedAccessReadCountBound(t *testing.T) {
	h, z := newPerZoneHarness(t)
	var regs dns.DNSRegistrationList
	if err := h.c.List(h.ctx, &regs); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		reg := regs.Items[0].DeepCopy()
		reg.Name = fmt.Sprintf("extra-%d", i)
		reg.UID = types.UID(reg.Name)
		reg.ResourceVersion = ""
		reg.Spec.Name = reg.Name
		if err := h.c.Create(h.ctx, reg); err != nil {
			t.Fatal(err)
		}
	}
	h.settle()
	h.r.Options.CachedZoneInputs = true
	h.r.InputReader = inputCache(t, h)
	h.run(zoneQueuePrefix + z.Name)
	h.run(zoneQueuePrefix + z.Name)
	h.run(zoneQueuePrefix + z.Name)
	h.r.InputReader = inputCache(t, h)
	counted := &countAccessReads{Client: h.c}
	h.r.Client = counted
	h.r.APIReader = counted
	h.run(zoneQueuePrefix + z.Name)
	if counted.associations > 3 {
		t.Fatalf("access snapshots reread associations %d times", counted.associations)
	}
	if counted.references > 2*5+4 {
		t.Fatalf("reference reads scale above two per registration: %d", counted.references)
	}
}

func TestCachedFailedShadowKeepsRetryingLive(t *testing.T) {
	h, z := newPerZoneHarness(t)
	h.settle()
	h.r.Options.CachedZoneInputs = true
	h.r.InputReader = inputCache(t, h)
	h.run(zoneQueuePrefix + z.Name)
	h.r.APIReader = &countInputLists{Reader: h.c, forbid: true}
	for i := 0; i < 2; i++ {
		_, err := h.r.Reconcile(h.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: z.Namespace, Name: zoneQueuePrefix + z.Name}})
		if err == nil {
			t.Fatal("failed shadow was bypassed")
		}
	}
	value, _ := h.r.zoneInputs.Load(z.UID)
	if !value.(*zoneInputState).shadowAt.IsZero() {
		t.Fatal("failed shadow advanced successful comparison time")
	}
}

type accessRaceReader struct {
	client.Reader
	client    client.Client
	snapshots int
}

func (c *accessRaceReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*dns.DNSZoneAssociationList); ok {
		c.snapshots++
		if c.snapshots == 2 {
			var assocs dns.DNSZoneAssociationList
			if err := c.client.List(ctx, &assocs); err != nil {
				return err
			}
			a := &assocs.Items[0]
			a.Generation++
			if err := c.client.Update(ctx, a); err != nil {
				return err
			}
		}
	}
	return c.Reader.List(ctx, list, opts...)
}
func TestCachedAccessRacePreventsNewManifest(t *testing.T) {
	h, z := newPerZoneHarness(t)
	h.settle()
	h.r.Options.CachedZoneInputs = true
	h.r.InputReader = inputCache(t, h)
	h.run(zoneQueuePrefix + z.Name)
	h.run(zoneQueuePrefix + z.Name)
	h.run(zoneQueuePrefix + z.Name)
	key := client.ObjectKey{Namespace: h.r.Options.PlatformNamespace, Name: ownershipName(z.UID)}
	var before dns.DNSPublicationOwnership
	if err := h.c.Get(h.ctx, key, &before); err != nil {
		t.Fatal(err)
	}
	ttl := int64(30)
	record := &dns.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "racing", Namespace: z.Namespace, UID: "racing", Generation: 1}, Spec: dns.DNSRecordSetSpec{DNSZoneRef: corev1.LocalObjectReference{Name: z.Name}, RecordType: dns.RRTypeA, Records: []dns.RecordEntry{{Name: "racing", TTL: &ttl, A: &dns.ARecordSpec{Content: "192.0.2.30"}}}}}
	if err := h.c.Create(h.ctx, record); err != nil {
		t.Fatal(err)
	}
	h.r.InputReader = inputCache(t, h)
	h.r.APIReader = &accessRaceReader{Reader: h.c, client: h.c}
	result := h.run(zoneQueuePrefix + z.Name)
	if result.RequeueAfter != zoneConflictRetry {
		t.Fatal("raced access did not requeue")
	}
	var after dns.DNSPublicationOwnership
	if err := h.c.Get(h.ctx, key, &after); err != nil {
		t.Fatal(err)
	}
	if after.Spec.ActiveManifestName != before.Spec.ActiveManifestName {
		t.Fatal("raced access published new manifest")
	}
}
