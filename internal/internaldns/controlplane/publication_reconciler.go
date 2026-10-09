// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/projectstatus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const privateZoneFinalizer = "internal-dns.dns.networking.miloapis.com/publication"

func (r *Reconciler) reconcileProject(ctx context.Context, ns string) (ctrl.Result, error) {
	now := r.Options.Now().UTC()

	if err := r.reconcileResolverContexts(ctx, ns, now); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileManagedNamespaces(ctx, ns, now); err != nil {
		return ctrl.Result{}, err
	}
	var zones dnsv1alpha1.DNSZoneList
	var assocs dnsv1alpha1.DNSZoneAssociationList
	var regs dnsv1alpha1.DNSRegistrationList
	var grants dnsv1alpha1.DNSContributionGrantList
	var contributions dnsv1alpha1.DNSRecordContributionList
	var records dnsv1alpha1.DNSRecordSetList
	var policies dnsv1alpha1.DNSNamingPolicyList
	for _, item := range []client.ObjectList{&zones, &assocs, &regs, &grants, &contributions, &records, &policies} {
		if err := r.List(ctx, item, client.InNamespace(ns)); err != nil {
			return ctrl.Result{}, err
		}
	}
	zoneByName := map[string]*dnsv1alpha1.DNSZone{}
	for i := range zones.Items {
		z := &zones.Items[i]
		zoneByName[z.Name] = z
	}
	if err := r.reconcileAssociations(ctx, assocs.Items, zoneByName, now); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileNamingPolicies(ctx, policies.Items, assocs.Items, zoneByName, now); err != nil {
		return ctrl.Result{}, err
	}
	if changed, err := r.issueGrants(ctx, grants.Items, regs.Items, now); err != nil || changed {
		return ctrl.Result{Requeue: changed}, err
	}
	if changed, err := r.bindContributionEpochs(ctx, contributions.Items, grants.Items, now); err != nil || changed {
		return ctrl.Result{Requeue: changed}, err
	}
	if err := r.reconcileBindings(ctx, ns, assocs.Items, zoneByName, now); err != nil {
		return ctrl.Result{}, err
	}
	var earliest time.Time
	for i := range zones.Items {
		z := &zones.Items[i]
		if z.Spec.Visibility != dnsv1alpha1.DNSZoneVisibilityPrivate {
			continue
		}
		if z.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(z, privateZoneFinalizer) {
			base := z.DeepCopy()
			controllerutil.AddFinalizer(z, privateZoneFinalizer)
			if err := r.Patch(ctx, z, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
		result, next, err := r.compileZone(ctx, z, assocs.Items, regs.Items, grants.Items, contributions.Items, records.Items, now)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !next.IsZero() && (earliest.IsZero() || next.Before(earliest)) {
			earliest = next
		}
		if err := r.aggregatePublicationStatus(ctx, z, regs.Items, contributions.Items, now); err != nil {
			return ctrl.Result{}, err
		}
		if !z.DeletionTimestamp.IsZero() && result {
			base := z.DeepCopy()
			controllerutil.RemoveFinalizer(z, privateZoneFinalizer)
			if err := r.Patch(ctx, z, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	return ctrl.Result{RequeueAfter: nextProjectRequeue(now, earliest, r.Options.LeaseDuration)}, nil
}

// nextProjectRequeue never lets a later contribution or authorization expiry
// postpone ownership renewal. A standby polls more frequently while another
// holder owns a zone, but the active holder must still run within half a lease
// even when the compiled publication is otherwise quiescent.
func nextProjectRequeue(now, earliest time.Time, lease time.Duration) time.Duration {
	delay := lease / 2
	if delay <= 0 {
		delay = time.Second
	}
	if !earliest.IsZero() {
		until := earliest.Sub(now)
		if until < delay {
			delay = until
		}
	}
	if delay < time.Second {
		return time.Second
	}
	return delay
}

func (r *Reconciler) aggregatePublicationStatus(ctx context.Context, z *dnsv1alpha1.DNSZone, regs []dnsv1alpha1.DNSRegistration, cs []dnsv1alpha1.DNSRecordContribution, now time.Time) error {
	ownerName := "owner-" + model.OpaqueToken(string(z.UID))[:20]
	var owner dnsv1alpha1.DNSPublicationOwnership
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.Options.PlatformNamespace, Name: ownerName}, &owner); err != nil {
		return client.IgnoreNotFound(err)
	}
	if owner.Spec.ActiveManifestName == "" {
		return nil
	}
	var manifest dnsv1alpha1.DNSPublicationManifest
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.Options.PlatformNamespace, Name: owner.Spec.ActiveManifestName}, &manifest); err != nil {
		return client.IgnoreNotFound(err)
	}
	published := false
	if c := apimeta.FindStatusCondition(manifest.Status.Conditions, "Published"); c != nil && c.Status == metav1.ConditionTrue {
		published = true
	}
	regUIDs := map[types.UID]bool{}
	registrationFences := map[types.UID]int64{}
	for _, fence := range manifest.Spec.RegistrationFences {
		registrationFences[fence.UID] = fence.Generation
	}
	for i := range regs {
		reg := &regs[i]
		if !refMatches(reg.Spec.DNSZoneRef, z.Name, z.UID, z.Generation) || registrationFences[reg.UID] != reg.Generation {
			continue
		}
		regUIDs[reg.UID] = true
		base := reg.DeepCopy()
		reg.Status.PublicationWriterEpoch = manifest.Spec.WriterEpoch
		reg.Status.PublicationRevision = manifest.Spec.Revision
		projectstatus.Published(&reg.Status.Conditions, published, reg.Generation, now)
		if !jsonEqual(base.Status, reg.Status) {
			if err := r.Status().Patch(ctx, reg, client.MergeFrom(base)); err != nil {
				return err
			}
		}
	}
	fenced := map[types.UID]bool{}
	for _, f := range manifest.Spec.ContributionFences {
		fenced[f.UID] = true
	}
	for i := range cs {
		c := &cs[i]
		if !regUIDs[c.Spec.RegistrationRef.UID] || !fenced[c.UID] {
			continue
		}
		base := c.DeepCopy()
		status := metav1.ConditionFalse
		reason, msg := "PendingReplicaVerification", "publication awaits required replica verification"
		if published {
			c.Status.PublishedRevision = manifest.Spec.Revision
			status = metav1.ConditionTrue
			reason = "Published"
			msg = "contribution revision is verified by required serving replicas"
		}
		setCondition(&c.Status.Conditions, "Published", status, reason, msg, c.Generation, now)
		if !jsonEqual(base.Status, c.Status) {
			if err := r.Status().Patch(ctx, c, client.MergeFrom(base)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Reconciler) issueGrants(ctx context.Context, grants []dnsv1alpha1.DNSContributionGrant, regs []dnsv1alpha1.DNSRegistration, now time.Time) (bool, error) {
	byUID := map[types.UID]*dnsv1alpha1.DNSRegistration{}
	for i := range regs {
		byUID[regs[i].UID] = &regs[i]
	}
	for i := range grants {
		g := &grants[i]
		reg := byUID[g.Spec.RegistrationRef.UID]
		valid := reg != nil && g.Spec.RegistrationRef.Name == reg.Name && g.Spec.RegistrationRef.Generation == reg.Generation
		base := g.DeepCopy()
		if valid && (g.Status.ActiveWriterEpoch == 0 || g.Status.ObservedGrantGeneration != g.Generation || g.Status.ObservedRegistrationGeneration != reg.Generation) {
			g.Status.ActiveWriterEpoch++
			if g.Status.ActiveWriterEpoch == 0 {
				g.Status.ActiveWriterEpoch = 1
			}
			g.Status.ObservedGrantGeneration = g.Generation
			g.Status.ObservedRegistrationGeneration = reg.Generation
		}
		status := metav1.ConditionFalse
		reason, msg := "StaleRegistrationGeneration", "grant registration reference is stale"
		if valid {
			status = metav1.ConditionTrue
			reason = "Active"
			msg = "writer epoch is active"
		}
		setCondition(&g.Status.Conditions, "Active", status, reason, msg, g.Generation, now)
		if !statusEqualGrant(base, g) {
			if err := r.Status().Patch(ctx, g, client.MergeFrom(base)); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

func (r *Reconciler) bindContributionEpochs(ctx context.Context, cs []dnsv1alpha1.DNSRecordContribution, gs []dnsv1alpha1.DNSContributionGrant, now time.Time) (bool, error) {
	byUID := map[types.UID]*dnsv1alpha1.DNSContributionGrant{}
	for i := range gs {
		byUID[gs[i].UID] = &gs[i]
	}
	for i := range cs {
		c := &cs[i]
		g := byUID[c.Spec.GrantRef.UID]
		if g == nil || g.Status.ActiveWriterEpoch == 0 {
			continue
		}
		if c.Status.WriterEpoch == 0 {
			base := c.DeepCopy()
			c.Status.WriterEpoch = g.Status.ActiveWriterEpoch
			setCondition(&c.Status.Conditions, "AuthorityBound", metav1.ConditionTrue, "WriterEpochIssued", "DNS bound the contribution to its grant epoch", c.Generation, now)
			if err := r.Status().Patch(ctx, c, client.MergeFrom(base)); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

func (r *Reconciler) compileZone(ctx context.Context, z *dnsv1alpha1.DNSZone, assocs []dnsv1alpha1.DNSZoneAssociation, regs []dnsv1alpha1.DNSRegistration, grants []dnsv1alpha1.DNSContributionGrant, cs []dnsv1alpha1.DNSRecordContribution, records []dnsv1alpha1.DNSRecordSet, now time.Time) (bool, time.Time, error) {
	own, err := r.acquireOwnership(ctx, z, now)
	if err != nil {
		return false, time.Time{}, err
	}
	if own == nil {
		return false, now.Add(100 * time.Millisecond), nil
	}
	previous, err := r.loadPrevious(ctx, own)
	if err != nil {
		return false, time.Time{}, err
	}
	input := CompileInput{Zone: z, Associations: assocs, Registrations: regs, Grants: grants, Contributions: cs, StaticRecords: records, Previous: previous, Now: now}
	tombstone := !z.DeletionTimestamp.IsZero()
	var payload []byte
	var ownPlan PublicationPlan
	if !tombstone {
		compiled, err := Compile(input)
		if err != nil {
			return false, time.Time{}, err
		}
		ownPlan = compiled.Plan
		wire, err := WirePlan(compiled.Plan)
		if err != nil {
			return false, time.Time{}, err
		}
		payload, err = json.Marshal(wire)
		if err != nil {
			return false, time.Time{}, err
		}
		if err := r.patchInputStatuses(ctx, z, compiled, regs, cs, now); err != nil {
			return false, time.Time{}, err
		}
	}
	hash := model.Hash(payload)
	servingTargets, err := r.publicationTargets(ctx, own, ownPlan.ContextUIDs, tombstone)
	if err != nil {
		return false, time.Time{}, err
	}
	stateHash := hash
	if !tombstone {
		fenceBytes, _ := json.Marshal(ownPlan.Contributions)
		stateHash = model.Hash(append(append([]byte(nil), payload...), fenceBytes...))
		targetBytes, _ := json.Marshal(servingTargets)
		stateHash = model.Hash(append([]byte(stateHash), targetBytes...))
	}
	if tombstone {
		hash = model.EmptyContentHash()
		stateHash = hash
	}
	if own.Spec.LastContentHash == stateHash {
		if !own.Spec.LeaseUntil.Time.After(now.Add(r.Options.LeaseDuration / 2)) {
			base := own.DeepCopy()
			own.Spec.LeaseUntil = metav1.NewTime(now.Add(r.Options.LeaseDuration))
			if err := r.Patch(ctx, own, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return false, time.Time{}, err
			}
		}
		return r.tombstoneAcknowledged(ctx, own), nextExpiry(ownPlan, now), nil
	}
	previousRevision, err := r.activeRevision(ctx, own)
	if err != nil {
		return false, time.Time{}, err
	}
	revision := own.Spec.NextRevision
	if revision <= 0 {
		revision = 1
	}
	// Reserve the identity before staging immutable artifacts. A crash after this
	// CAS leaves a harmless gap; a retry can never reuse (epoch, revision) with
	// different generated bytes or timestamps.
	reservationBase := own.DeepCopy()
	own.Spec.NextRevision = revision + 1
	own.Spec.LeaseUntil = metav1.NewTime(now.Add(r.Options.LeaseDuration))
	if err := r.Patch(ctx, own, client.MergeFromWithOptions(reservationBase, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, time.Time{}, err
	}
	manifestName := artifactName("pub", z.UID, own.Spec.WriterEpoch, revision)
	manifest, chunkNames, err := r.persistPublication(ctx, z, manifestName, own.Spec.WriterEpoch, revision, previousRevision, tombstone, payload, hash, ownPlan.ContextUIDs, ownPlan.Contributions, ownPlan.Registrations, servingTargets, now)
	if err != nil {
		return false, time.Time{}, err
	}
	base := own.DeepCopy()
	own.Spec.LastContentHash = stateHash
	own.Spec.ActiveManifestName = manifest.Name
	own.Spec.LeaseUntil = metav1.NewTime(now.Add(r.Options.LeaseDuration))
	if err := r.Patch(ctx, own, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, time.Time{}, err
	}
	_ = chunkNames
	return false, nextExpiry(ownPlan, now), nil
}

func (r *Reconciler) activeRevision(ctx context.Context, own *dnsv1alpha1.DNSPublicationOwnership) (int64, error) {
	if own.Spec.ActiveManifestName == "" {
		return 0, nil
	}
	var manifest dnsv1alpha1.DNSPublicationManifest
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.Options.PlatformNamespace, Name: own.Spec.ActiveManifestName}, &manifest); err != nil {
		return 0, err
	}
	if manifest.Spec.ZoneRef.UID != own.Spec.ZoneUID || manifest.Spec.WriterEpoch > own.Spec.WriterEpoch {
		return 0, fmt.Errorf("active publication does not match ownership fence")
	}
	if manifest.Spec.WriterEpoch < own.Spec.WriterEpoch {
		return 0, nil
	}
	return manifest.Spec.Revision, nil
}

func (r *Reconciler) publicationTargets(ctx context.Context, own *dnsv1alpha1.DNSPublicationOwnership, vpcs []types.UID, includePrevious bool) ([]ServingRegion, error) {
	var bindings dnsv1alpha1.DNSResolverBindingList
	if err := r.List(ctx, &bindings, client.InNamespace(r.Options.PlatformNamespace)); err != nil {
		return nil, err
	}
	byKey := map[string]ServingRegion{}
	for _, vpc := range vpcs {
		for _, target := range r.selectServingTargets(vpc, bindings.Items) {
			byKey[target.Region+"\x00"+target.Shard] = target
		}
	}
	if includePrevious && own.Spec.ActiveManifestName != "" {
		var previous dnsv1alpha1.DNSPublicationManifest
		if err := r.Get(ctx, client.ObjectKey{Namespace: r.Options.PlatformNamespace, Name: own.Spec.ActiveManifestName}, &previous); err != nil {
			return nil, err
		}
		for _, target := range previous.Spec.ServingTargets {
			v := ServingRegion{Region: target.Region, Shard: target.Shard}
			byKey[v.Region+"\x00"+v.Shard] = v
		}
	}
	result := make([]ServingRegion, 0, len(byKey))
	for _, target := range byKey {
		result = append(result, target)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Region == result[j].Region {
			return result[i].Shard < result[j].Shard
		}
		return result[i].Region < result[j].Region
	})
	return result, nil
}

func (r *Reconciler) persistPublication(ctx context.Context, z *dnsv1alpha1.DNSZone, name string, epoch, revision, previous int64, tombstone bool, payload []byte, hash string, vpcUIDs []types.UID, fences []ContributionFence, registrations []RegistrationFence, servingTargets []ServingRegion, now time.Time) (*dnsv1alpha1.DNSPublicationManifest, []string, error) {
	vpcUIDs = append([]types.UID{}, vpcUIDs...)
	refs := []dnsv1alpha1.DNSPublicationChunkReference{}
	depsByRegion := map[string][]string{}
	allOutboxes := []string{}
	wireRefs := []model.ChunkRef{}
	parts := chunk(payload, r.Options.ChunkSize)
	for i, p := range parts {
		chunkName := fmt.Sprintf("%s-%03d", name, i)
		sum := model.Hash(p)
		obj := &dnsv1alpha1.DNSPublicationChunk{ObjectMeta: metav1.ObjectMeta{Name: chunkName, Namespace: r.Options.PlatformNamespace}, Spec: dnsv1alpha1.DNSPublicationChunkSpec{WriterEpoch: epoch, Revision: revision, Index: int32(i), SHA256: sum, Payload: p}}
		if err := r.createImmutable(ctx, obj); err != nil {
			return nil, nil, err
		}
		refs = append(refs, dnsv1alpha1.DNSPublicationChunkReference{Name: chunkName, SHA256: sum, Size: int32(len(p))})
		wireRefs = append(wireRefs, model.ChunkRef{Index: i, SHA256: sum, Size: len(p)})
		wire := model.PublicationChunk{ManifestUID: name, ZoneUID: string(z.UID), WriterEpoch: uint64(epoch), Revision: uint64(revision), Index: i, SHA256: sum, Payload: p}
		for _, target := range servingTargets {
			token := regionToken(target)
			eventID := token + ":" + chunkName
			env, err := model.NewEnvelope(model.KindPublicationChunk, eventID, target.Region, target.Shard, string(z.UID), uint64(epoch), uint64(revision), now, wire)
			if err != nil {
				return nil, nil, err
			}
			b, _ := json.Marshal(env)
			outName := boundedName(chunkName + "-" + token + "-export")
			out := r.outbox(outName, target.Region, target.Shard, model.RecordChunkSubject(target.Region, target.Shard, string(z.UID)), z.UID, epoch, revision, name, b, false, nil)
			if err := r.createImmutable(ctx, out); err != nil {
				return nil, nil, err
			}
			depsByRegion[token] = append(depsByRegion[token], out.Name)
			allOutboxes = append(allOutboxes, out.Name)
		}
	}
	apiFences := make([]dnsv1alpha1.DNSContributionFence, 0, len(fences))
	for _, f := range fences {
		apiFences = append(apiFences, dnsv1alpha1.DNSContributionFence{UID: f.UID, GrantUID: f.GrantUID, Epoch: f.Epoch, Sequence: f.Sequence, ValidUntil: metav1.NewTime(f.ValidUntil)})
	}
	apiRegistrations := make([]dnsv1alpha1.DNSRegistrationFence, 0, len(registrations))
	for _, f := range registrations {
		apiRegistrations = append(apiRegistrations, dnsv1alpha1.DNSRegistrationFence{UID: f.UID, Generation: f.Generation})
	}
	apiTargets := make([]dnsv1alpha1.DNSPublicationTarget, 0, len(servingTargets))
	for _, target := range servingTargets {
		apiTargets = append(apiTargets, dnsv1alpha1.DNSPublicationTarget{Region: target.Region, Shard: target.Shard})
	}
	m := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Options.PlatformNamespace, Labels: map[string]string{"internal-dns.miloapis.com/zone-uid": string(z.UID)}}, Spec: dnsv1alpha1.DNSPublicationManifestSpec{ZoneRef: dnsv1alpha1.DNSObjectReference{Name: z.Name, UID: z.UID}, ZoneApex: z.Spec.DomainName, ContextUIDs: vpcUIDs, WriterEpoch: epoch, Revision: revision, PreviousRevision: previous, Tombstone: tombstone, Chunks: refs, ContentHash: hash, GeneratedAt: metav1.NewTime(now), ContributionFences: apiFences, RegistrationFences: apiRegistrations, ServingTargets: apiTargets}}
	if err := r.createImmutable(ctx, m); err != nil {
		return nil, nil, err
	}
	wmanifest := model.PublicationManifest{ManifestUID: name, ZoneUID: string(z.UID), Apex: z.Spec.DomainName, WriterEpoch: uint64(epoch), Revision: uint64(revision), PreviousRevision: uint64(previous), Tombstone: tombstone, Chunks: wireRefs, ContentHash: hash, GeneratedAt: now}
	if err := wmanifest.Validate(); err != nil {
		return nil, nil, err
	}
	for _, target := range servingTargets {
		token := regionToken(target)
		eventID := token + ":" + name
		env, err := model.NewEnvelope(model.KindPublicationManifest, eventID, target.Region, target.Shard, string(z.UID), uint64(epoch), uint64(revision), now, wmanifest)
		if err != nil {
			return nil, nil, err
		}
		b, _ := json.Marshal(env)
		out := r.outbox(model.PublicationActivationName(name, target.Region, target.Shard), target.Region, target.Shard, model.RecordManifestSubject(target.Region, target.Shard, string(z.UID)), z.UID, epoch, revision, name, b, true, depsByRegion[token])
		if err := r.createImmutable(ctx, out); err != nil {
			return nil, nil, err
		}
		allOutboxes = append(allOutboxes, out.Name)
	}
	return m, allOutboxes, nil
}

func (r *Reconciler) outbox(name, region, shard, subject string, uid types.UID, epoch, revision int64, manifest string, payload []byte, activation bool, deps []string) *dnsv1alpha1.DNSTransportOutbox {
	ownerName := ""
	if activation {
		ownerName = "owner-" + model.OpaqueToken(string(uid))[:20]
	}
	return &dnsv1alpha1.DNSTransportOutbox{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Options.PlatformNamespace, Labels: map[string]string{
		"internal-dns.miloapis.com/region": model.SafeToken(region),
		"internal-dns.miloapis.com/shard":  model.SafeToken(shard),
	}}, Spec: dnsv1alpha1.DNSTransportOutboxSpec{Subject: subject, ResourceUID: uid, WriterEpoch: epoch, Revision: revision, ManifestRef: dnsv1alpha1.DNSObjectReference{Name: manifest}, OwnershipRef: dnsv1alpha1.DNSObjectReference{Name: ownerName}, PayloadHash: model.Hash(payload), Payload: payload, Activation: activation, DependsOn: deps}}
}

func (r *Reconciler) acquireOwnership(ctx context.Context, z *dnsv1alpha1.DNSZone, now time.Time) (*dnsv1alpha1.DNSPublicationOwnership, error) {
	name := "owner-" + model.OpaqueToken(string(z.UID))[:20]
	o := &dnsv1alpha1.DNSPublicationOwnership{}
	key := client.ObjectKey{Namespace: r.Options.PlatformNamespace, Name: name}
	err := r.Get(ctx, key, o)
	if apierrors.IsNotFound(err) {
		o = &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: key.Namespace}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ZoneUID: z.UID, HolderIdentity: r.Options.Identity, WriterEpoch: 1, NextRevision: 1, LeaseUntil: metav1.NewTime(now.Add(r.Options.LeaseDuration))}}
		if err := r.Create(ctx, o); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if o.Spec.ZoneUID != z.UID {
		return nil, fmt.Errorf("ownership collision for %s", z.UID)
	}
	if o.Spec.HolderIdentity != r.Options.Identity {
		if o.Spec.LeaseUntil.Time.After(now) {
			return nil, nil
		}
		base := o.DeepCopy()
		o.Spec.HolderIdentity = r.Options.Identity
		o.Spec.WriterEpoch++
		o.Spec.LeaseUntil = metav1.NewTime(now.Add(r.Options.LeaseDuration))
		if err := r.Patch(ctx, o, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return nil, err
		}
	}
	return o, nil
}

func (r *Reconciler) loadPrevious(ctx context.Context, o *dnsv1alpha1.DNSPublicationOwnership) (*PublicationPlan, error) {
	if o.Spec.ActiveManifestName == "" {
		return nil, nil
	}
	var m dnsv1alpha1.DNSPublicationManifest
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.Options.PlatformNamespace, Name: o.Spec.ActiveManifestName}, &m); err != nil {
		return nil, fmt.Errorf("load active publication manifest %s: %w", o.Spec.ActiveManifestName, err)
	}
	var b []byte
	for i, ref := range m.Spec.Chunks {
		var c dnsv1alpha1.DNSPublicationChunk
		if err := r.Get(ctx, client.ObjectKey{Namespace: r.Options.PlatformNamespace, Name: ref.Name}, &c); err != nil {
			return nil, fmt.Errorf("load active publication chunk %s: %w", ref.Name, err)
		}
		if int(c.Spec.Index) != i || c.Spec.WriterEpoch != m.Spec.WriterEpoch || c.Spec.Revision != m.Spec.Revision || c.Spec.SHA256 != ref.SHA256 || model.Hash(c.Spec.Payload) != ref.SHA256 || len(c.Spec.Payload) != int(ref.Size) {
			return nil, fmt.Errorf("publication %s chunk %d failed verification", m.Name, i)
		}
		b = append(b, c.Spec.Payload...)
	}
	if model.Hash(b) != m.Spec.ContentHash {
		return nil, fmt.Errorf("publication %s content hash mismatch", m.Name)
	}
	if len(b) == 0 {
		if !m.Spec.Tombstone {
			return nil, fmt.Errorf("publication %s has an empty non-tombstone payload", m.Name)
		}
		return nil, nil
	}
	var wire model.PublicationPlan
	if err := json.Unmarshal(b, &wire); err != nil {
		return nil, fmt.Errorf("decode active publication %s: %w", m.Name, err)
	}
	if err := wire.Validate(); err != nil {
		return nil, fmt.Errorf("validate active publication %s: %w", m.Name, err)
	}
	p := &PublicationPlan{ZoneUID: types.UID(wire.ZoneUID), ZoneApex: wire.Apex}
	// The chunk payload is the canonical immutable fence. Kubernetes date-time
	// fields may lose fractional seconds during API round trips, so rebuilding
	// from manifest metadata can make an unchanged observation appear mutated.
	for _, f := range wire.ObservationFences {
		p.Contributions = append(p.Contributions, ContributionFence{UID: types.UID(f.ContributionUID), GrantUID: types.UID(f.GrantUID), Epoch: int64(f.WriterEpoch), Sequence: int64(f.Sequence), ValidUntil: f.ValidUntil.UTC()})
	}
	return p, nil
}

func (r *Reconciler) patchInputStatuses(ctx context.Context, zone *dnsv1alpha1.DNSZone, result CompileResult, regs []dnsv1alpha1.DNSRegistration, cs []dnsv1alpha1.DNSRecordContribution, now time.Time) error {
	relevant := map[types.UID]bool{}
	for i := range regs {
		reg := &regs[i]
		if !refMatches(reg.Spec.DNSZoneRef, zone.Name, zone.UID, zone.Generation) {
			continue
		}
		relevant[reg.UID] = true
		accepted := result.Accepted[reg.UID]
		reason := result.Reasons[reg.UID]
		if reason == "" {
			reason = "Accepted"
		}
		base := reg.DeepCopy()
		reg.Status.CanonicalFQDN = absoluteOwner(reg.Spec.Name, result.Plan.ZoneApex)
		reg.Status.ObservedGeneration = reg.Generation
		setCondition(&reg.Status.Conditions, "Accepted", boolCondition(accepted), reason, reason, reg.Generation, now)
		if err := r.Status().Patch(ctx, reg, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	for i := range cs {
		c := &cs[i]
		if !relevant[c.Spec.RegistrationRef.UID] {
			continue
		}
		accepted := result.Accepted[c.UID]
		reason := result.Reasons[c.UID]
		if reason == "" {
			reason = "Accepted"
		}
		base := c.DeepCopy()
		setCondition(&c.Status.Conditions, "Accepted", boolCondition(accepted), reason, reason, c.Generation, now)
		if err := r.Status().Patch(ctx, c, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *Reconciler) tombstoneAcknowledged(ctx context.Context, o *dnsv1alpha1.DNSPublicationOwnership) bool {
	if o.Spec.ActiveManifestName == "" {
		return false
	}
	var manifest dnsv1alpha1.DNSPublicationManifest
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.Options.PlatformNamespace, Name: o.Spec.ActiveManifestName}, &manifest); err != nil {
		return false
	}
	c := apimeta.FindStatusCondition(manifest.Status.Conditions, "Published")
	return manifest.Spec.Tombstone && c != nil && c.Status == metav1.ConditionTrue
}

func (r *Reconciler) createImmutable(ctx context.Context, desired client.Object) error {
	if err := r.Create(ctx, desired); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return err
	}
	switch want := desired.(type) {
	case *dnsv1alpha1.DNSPublicationChunk:
		var have dnsv1alpha1.DNSPublicationChunk
		if err := r.Get(ctx, client.ObjectKeyFromObject(want), &have); err != nil {
			return err
		}
		if !reflect.DeepEqual(have.Spec, want.Spec) {
			return fmt.Errorf("immutable publication chunk %s has conflicting content", want.Name)
		}
	case *dnsv1alpha1.DNSPublicationManifest:
		var have dnsv1alpha1.DNSPublicationManifest
		if err := r.Get(ctx, client.ObjectKeyFromObject(want), &have); err != nil {
			return err
		}
		if !reflect.DeepEqual(have.Spec, want.Spec) {
			return fmt.Errorf("immutable publication manifest %s has conflicting content", want.Name)
		}
	case *dnsv1alpha1.DNSTransportOutbox:
		var have dnsv1alpha1.DNSTransportOutbox
		if err := r.Get(ctx, client.ObjectKeyFromObject(want), &have); err != nil {
			return err
		}
		if !reflect.DeepEqual(have.Spec, want.Spec) {
			return fmt.Errorf("immutable transport outbox %s has conflicting content", want.Name)
		}
	default:
		return fmt.Errorf("immutable create is unsupported for %T", desired)
	}
	return nil
}

func statusEqualGrant(a, b *dnsv1alpha1.DNSContributionGrant) bool {
	x, _ := json.Marshal(a.Status)
	y, _ := json.Marshal(b.Status)
	return string(x) == string(y)
}

func artifactName(prefix string, uid types.UID, epoch, rev int64) string {
	return fmt.Sprintf("%s-%s-e%d-r%d", prefix, model.OpaqueToken(string(uid))[:16], epoch, rev)
}

func chunk(b []byte, size int) [][]byte {
	if len(b) == 0 {
		return nil
	}
	var out [][]byte
	for len(b) > 0 {
		n := size
		if n > len(b) {
			n = len(b)
		}
		out = append(out, append([]byte(nil), b[:n]...))
		b = b[n:]
	}
	return out
}

func nextExpiry(p PublicationPlan, now time.Time) time.Time {
	var next time.Time
	for _, f := range p.Contributions {
		if f.ValidUntil.After(now) && (next.IsZero() || f.ValidUntil.Before(next)) {
			next = f.ValidUntil
		}
	}
	return next
}
