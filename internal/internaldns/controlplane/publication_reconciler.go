// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
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
	started := time.Now()
	totals := passTotals{}
	ctx = context.WithValue(ctx, passTotalsKey{}, totals)
	defer func() {
		elapsed := time.Since(started)
		passDuration.Observe(elapsed.Seconds())
		if elapsed > slowPass {
			ctrl.LoggerFrom(ctx).Info("slow project pass", "projectUID", r.Options.ProjectUID, "namespace", ns, "seconds", elapsed.Seconds(), "phases", totals.seconds())
		}
	}()
	timer := newPhaseTimer(ctx)

	if err := r.reconcileResolverContexts(ctx, ns, now); err != nil {
		return ctrl.Result{}, err
	}
	timer.done("contexts")

	var zones dnsv1alpha1.DNSZoneList
	var assocs dnsv1alpha1.DNSZoneAssociationList
	var regs dnsv1alpha1.DNSRegistrationList
	var contributions dnsv1alpha1.DNSRecordContributionList
	var records dnsv1alpha1.DNSRecordSetList
	var policies dnsv1alpha1.DNSNamingPolicyList
	for _, item := range []client.ObjectList{&zones, &assocs, &regs, &contributions, &records, &policies} {
		if err := r.List(ctx, item, client.InNamespace(ns)); err != nil {
			return ctrl.Result{}, err
		}
	}
	timer.done("list")
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
	timer.done("associations")
	var sources []client.Object
	for i := range assocs.Items {
		sources = append(sources, &assocs.Items[i])
	}
	for i := range policies.Items {
		sources = append(sources, &policies.Items[i])
	}
	registrationZones := make(map[types.UID]dnsv1alpha1.DNSObjectReference, len(regs.Items))
	for i := range regs.Items {
		registration := &regs.Items[i]
		ref := dnsv1alpha1.DNSObjectReference{Name: registration.Spec.DNSZoneRef.Name}
		if zone := zoneByName[ref.Name]; zone != nil && zone.DeletionTimestamp.IsZero() && zone.Spec.Visibility == dnsv1alpha1.DNSZoneVisibilityPrivate {
			ref.UID, ref.Generation = zone.UID, zone.Generation
		}
		pinned, _, err := r.pinReferences(ctx, registration, map[string]dnsv1alpha1.DNSObjectReference{"zone": ref})
		if err != nil {
			return ctrl.Result{}, err
		}
		registrationZones[registration.UID] = pinned["zone"]
		sources = append(sources, registration)
	}
	if err := r.collectReferenceStates(ctx, sources); err != nil {
		return ctrl.Result{}, err
	}
	timer.done("references")
	if err := r.reconcileBindings(ctx, ns, assocs.Items, zoneByName, now); err != nil {
		return ctrl.Result{}, err
	}
	timer.done("bindings")
	var earliest time.Time
	var zoneErrors []error
	held := map[types.UID]heldOwnership{}
	for i := range zones.Items {
		z := &zones.Items[i]
		if z.Spec.Visibility != dnsv1alpha1.DNSZoneVisibilityPrivate || (z.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(z, privateZoneFinalizer)) {
			continue
		}
		own, retryAt, err := r.holdOwnership(ctx, z)
		held[z.UID] = heldOwnership{own: own, retryAt: retryAt, err: err}
	}
	timer.done("ownership")
	for i := range zones.Items {
		z := &zones.Items[i]
		if z.Spec.Visibility != dnsv1alpha1.DNSZoneVisibilityPrivate {
			continue
		}
		if z.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(z, privateZoneFinalizer) {
			base := z.DeepCopy()
			controllerutil.AddFinalizer(z, privateZoneFinalizer)
			if err := r.Patch(ctx, z, client.MergeFrom(base)); err != nil {
				zoneErrors = append(zoneErrors, fmt.Errorf("zone %s: %w", z.Name, err))
			}
			continue
		}
		lease := held[z.UID]
		if lease.err != nil {
			zoneErrors = append(zoneErrors, fmt.Errorf("zone %s: %w", z.Name, lease.err))
			continue
		}
		if lease.own == nil {
			if earliest.IsZero() || lease.retryAt.Before(earliest) {
				earliest = lease.retryAt
			}
			continue
		}
		result, err := r.compileZone(ctx, z, lease.own, assocs.Items, regs.Items, registrationZones, contributions.Items, records.Items, now)
		timer.done("compile")
		if err != nil {
			zoneErrors = append(zoneErrors, fmt.Errorf("zone %s: %w", z.Name, err))
			continue
		}
		pending, err := r.aggregatePublicationStatus(ctx, z, regs.Items, contributions.Items, now)
		timer.done("status")
		if err != nil {
			zoneErrors = append(zoneErrors, fmt.Errorf("zone %s: %w", z.Name, err))
			continue
		}
		if !pending.IsZero() && (earliest.IsZero() || pending.Before(earliest)) {
			earliest = pending
		}
		if !z.DeletionTimestamp.IsZero() && result {
			base := z.DeepCopy()
			controllerutil.RemoveFinalizer(z, privateZoneFinalizer)
			if err := r.Patch(ctx, z, client.MergeFrom(base)); err != nil {
				zoneErrors = append(zoneErrors, fmt.Errorf("zone %s: %w", z.Name, err))
			}
		}
	}
	renewBy := now.Add(r.Options.LeaseDuration / 2)
	if earliest.IsZero() || renewBy.Before(earliest) {
		earliest = renewBy
	}
	return ctrl.Result{RequeueAfter: nextProjectRequeue(r.Options.Now().UTC(), earliest, r.Options.LeaseDuration)}, errors.Join(zoneErrors...)
}

// nextProjectRequeue never lets a later scheduled reconciliation
// postpone ownership renewal. A standby wakes when the holder's lease could
// expire, but the active holder must still run within half a lease even when
// the compiled publication is otherwise quiescent.
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

func (r *Reconciler) aggregatePublicationStatus(ctx context.Context, z *dnsv1alpha1.DNSZone, regs []dnsv1alpha1.DNSRegistration, cs []dnsv1alpha1.DNSRecordContribution, now time.Time) (time.Time, error) {
	ownerName := "owner-" + model.OpaqueToken(string(z.UID))[:20]
	var owner dnsv1alpha1.DNSPublicationOwnership
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.Options.PlatformNamespace, Name: ownerName}, &owner); err != nil {
		return time.Time{}, client.IgnoreNotFound(err)
	}
	if owner.Spec.ActiveManifestName == "" {
		return time.Time{}, nil
	}
	var manifest dnsv1alpha1.DNSPublicationManifest
	if err := r.Get(ctx, client.ObjectKey{Namespace: r.Options.PlatformNamespace, Name: owner.Spec.ActiveManifestName}, &manifest); err != nil {
		return time.Time{}, client.IgnoreNotFound(err)
	}
	published := false
	if c := apimeta.FindStatusCondition(manifest.Status.Conditions, "Published"); c != nil && c.Status == metav1.ConditionTrue {
		published = true
	}
	var recheck time.Time
	if !published && owner.Spec.HolderIdentity == r.Options.Identity {
		recheck = now.Add(publishedRecheckDelay(now, manifest.Spec.GeneratedAt.Time))
	}
	regUIDs := map[types.UID]bool{}
	registrationFences := map[types.UID]int64{}
	for _, fence := range manifest.Spec.RegistrationFences {
		registrationFences[fence.UID] = fence.Generation
	}
	for i := range regs {
		reg := &regs[i]
		if reg.Spec.DNSZoneRef.Name != z.Name || registrationFences[reg.UID] != reg.Generation {
			continue
		}
		regUIDs[reg.UID] = true
		base := reg.DeepCopy()
		reg.Status.PublicationWriterEpoch = manifest.Spec.WriterEpoch
		reg.Status.PublicationRevision = manifest.Spec.Revision
		projectstatus.Published(&reg.Status.Conditions, published, reg.Generation, now)
		if !jsonEqual(base.Status, reg.Status) {
			if err := r.Status().Patch(ctx, reg, client.MergeFrom(base)); err != nil {
				return time.Time{}, err
			}
		}
	}
	fenced := map[types.UID]int64{}
	for _, f := range manifest.Spec.ContributionFences {
		fenced[f.UID] = f.Generation
	}
	for i := range cs {
		c := &cs[i]
		if !regUIDs[c.Spec.RegistrationRef.UID] || fenced[c.UID] != c.Generation {
			continue
		}
		var current dnsv1alpha1.DNSRecordContribution
		if err := r.sourceReader().Get(ctx, client.ObjectKeyFromObject(c), &current); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return time.Time{}, err
		}
		if current.UID != c.UID || current.Generation != c.Generation {
			continue
		}
		c = &current
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
			if err := r.Status().Patch(ctx, c, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return time.Time{}, err
			}
		}
	}
	return recheck, nil
}

// publishedRecheckDelay polls a fresh manifest for its Published verdict
// quickly, then backs off as the manifest ages, because nothing watches the
// verdict and a zone that never publishes must not hold a tight loop.
func publishedRecheckDelay(now, generatedAt time.Time) time.Duration {
	age := now.Sub(generatedAt)
	if age < time.Second {
		return time.Second
	}
	return age
}

func (r *Reconciler) compileZone(ctx context.Context, z *dnsv1alpha1.DNSZone, own *dnsv1alpha1.DNSPublicationOwnership, assocs []dnsv1alpha1.DNSZoneAssociation, regs []dnsv1alpha1.DNSRegistration, registrationZones map[types.UID]dnsv1alpha1.DNSObjectReference, cs []dnsv1alpha1.DNSRecordContribution, records []dnsv1alpha1.DNSRecordSet, now time.Time) (bool, error) {
	timer := newPhaseTimer(ctx)
	previous, err := r.loadPrevious(ctx, own)
	if err != nil {
		return false, err
	}
	timer.done("zone load")
	cs, err = r.confirmContributions(ctx, z.Namespace, cs, previous)
	if err != nil {
		return false, err
	}
	timer.done("zone confirm")
	input := CompileInput{Zone: z, Associations: assocs, Registrations: regs, Contributions: cs, StaticRecords: records, Previous: previous, Now: now}
	input.AssociationReferences, err = r.associationReferences(ctx, assocs)
	if err != nil {
		return false, err
	}
	input.RegistrationZoneReferences = registrationZones
	timer.done("zone references")
	tombstone := !z.DeletionTimestamp.IsZero()
	var payload []byte
	var ownPlan PublicationPlan
	if !tombstone {
		compiled, err := Compile(input)
		if err != nil {
			return false, err
		}
		ownPlan = compiled.Plan
		wire, err := WirePlan(compiled.Plan)
		if err != nil {
			return false, err
		}
		payload, err = json.Marshal(wire)
		if err != nil {
			return false, err
		}
		timer.done("zone compile")
		if err := r.patchInputStatuses(ctx, z, compiled, regs, cs, records, now); err != nil {
			return false, err
		}
		timer.done("zone statuses")
	}
	hash := model.Hash(payload)
	servingTargets, err := r.publicationTargets(ctx, own, ownPlan.ContextUIDs, tombstone)
	if err != nil {
		return false, err
	}
	timer.done("zone targets")
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
		return r.tombstoneAcknowledged(ctx, own), nil
	}
	previousRevision, err := r.activeRevision(ctx, own)
	if err != nil {
		return false, err
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
	own.Spec.LeaseUntil = metav1.NewTime(r.Options.Now().UTC().Add(r.Options.LeaseDuration))
	if err := r.Patch(ctx, own, client.MergeFromWithOptions(reservationBase, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, err
	}
	manifestName := artifactName("pub", z.UID, own.Spec.WriterEpoch, revision)
	manifest, err := r.persistPublication(ctx, z, manifestName, own.Spec.WriterEpoch, revision, previousRevision, tombstone, payload, hash, ownPlan.ContextUIDs, ownPlan.Contributions, ownPlan.Registrations, servingTargets, now)
	if err != nil {
		return false, err
	}
	base := own.DeepCopy()
	own.Spec.LastContentHash = stateHash
	own.Spec.ActiveManifestName = manifest.Name
	own.Spec.LeaseUntil = metav1.NewTime(r.Options.Now().UTC().Add(r.Options.LeaseDuration))
	if err := r.Patch(ctx, own, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, err
	}
	timer.done("zone persist")
	return false, nil
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

func (r *Reconciler) publicationTargets(ctx context.Context, own *dnsv1alpha1.DNSPublicationOwnership, contextsByUID []types.UID, includePrevious bool) ([]ServingRegion, error) {
	var bindings dnsv1alpha1.DNSResolverBindingList
	if err := r.listProjectBindings(ctx, &bindings); err != nil {
		return nil, err
	}
	byKey := map[string]ServingRegion{}
	for _, resolverContextUID := range contextsByUID {
		for _, target := range r.selectServingTargets(resolverContextUID, bindings.Items) {
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

func (r *Reconciler) persistPublication(ctx context.Context, z *dnsv1alpha1.DNSZone, name string, epoch, revision, previous int64, tombstone bool, payload []byte, hash string, contextUIDs []types.UID, fences []ContributionFence, registrations []RegistrationFence, servingTargets []ServingRegion, now time.Time) (*dnsv1alpha1.DNSPublicationManifest, error) {
	contextUIDs = append([]types.UID{}, contextUIDs...)
	refs := []dnsv1alpha1.DNSPublicationChunkReference{}
	depsByRegion := map[string][]string{}
	wireRefs := []model.ChunkRef{}
	parts := chunk(payload, r.Options.ChunkSize)
	for i, p := range parts {
		chunkName := fmt.Sprintf("%s-%03d", name, i)
		sum := model.Hash(p)
		obj := &dnsv1alpha1.DNSPublicationChunk{ObjectMeta: metav1.ObjectMeta{Name: chunkName, Namespace: r.Options.PlatformNamespace}, Spec: dnsv1alpha1.DNSPublicationChunkSpec{WriterEpoch: epoch, Revision: revision, Index: int32(i), SHA256: sum, Payload: p}}
		if err := r.createImmutable(ctx, obj); err != nil {
			return nil, err
		}
		refs = append(refs, dnsv1alpha1.DNSPublicationChunkReference{Name: chunkName, SHA256: sum, Size: int32(len(p))})
		wireRefs = append(wireRefs, model.ChunkRef{Index: i, SHA256: sum, Size: len(p)})
		wire := model.PublicationChunk{ManifestUID: name, ZoneUID: string(z.UID), WriterEpoch: uint64(epoch), Revision: uint64(revision), Index: i, SHA256: sum, Payload: p}
		for _, target := range servingTargets {
			token := regionToken(target)
			eventID := token + ":" + chunkName
			env, err := model.NewEnvelope(model.KindPublicationChunk, eventID, target.Region, target.Shard, string(z.UID), uint64(epoch), uint64(revision), now, wire)
			if err != nil {
				return nil, err
			}
			b, _ := json.Marshal(env)
			outName := boundedName(chunkName + "-" + token + "-export")
			out := r.outbox(outName, target.Region, target.Shard, model.RecordChunkSubject(target.Region, target.Shard, string(z.UID)), z.UID, epoch, revision, name, b, false, nil)
			if err := r.createImmutable(ctx, out); err != nil {
				return nil, err
			}
			depsByRegion[token] = append(depsByRegion[token], out.Name)
		}
	}
	apiFences := make([]dnsv1alpha1.DNSContributionFence, 0, len(fences))
	for _, f := range fences {
		apiFences = append(apiFences, dnsv1alpha1.DNSContributionFence{UID: f.UID, Generation: f.Generation, Sequence: f.Sequence, Retired: f.Retired, SourceName: f.SourceName})
	}
	apiRegistrations := make([]dnsv1alpha1.DNSRegistrationFence, 0, len(registrations))
	for _, f := range registrations {
		apiRegistrations = append(apiRegistrations, dnsv1alpha1.DNSRegistrationFence{UID: f.UID, Generation: f.Generation})
	}
	apiTargets := make([]dnsv1alpha1.DNSPublicationTarget, 0, len(servingTargets))
	for _, target := range servingTargets {
		apiTargets = append(apiTargets, dnsv1alpha1.DNSPublicationTarget{Region: target.Region, Shard: target.Shard})
	}
	m := &dnsv1alpha1.DNSPublicationManifest{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Options.PlatformNamespace, Labels: map[string]string{"internal-dns.miloapis.com/zone-uid": string(z.UID), "internal-dns.miloapis.com/project-uid": string(r.Options.ProjectUID)}}, Spec: dnsv1alpha1.DNSPublicationManifestSpec{ZoneRef: dnsv1alpha1.DNSObjectReference{Name: z.Name, UID: z.UID}, ZoneApex: z.Spec.DomainName, ContextUIDs: contextUIDs, WriterEpoch: epoch, Revision: revision, PreviousRevision: previous, Tombstone: tombstone, Chunks: refs, ContentHash: hash, GeneratedAt: metav1.NewTime(now), ContributionFences: apiFences, RegistrationFences: apiRegistrations, ServingTargets: apiTargets}}
	if err := r.createImmutable(ctx, m); err != nil {
		return nil, err
	}
	wmanifest := model.PublicationManifest{ManifestUID: name, ZoneUID: string(z.UID), Apex: z.Spec.DomainName, WriterEpoch: uint64(epoch), Revision: uint64(revision), PreviousRevision: uint64(previous), Tombstone: tombstone, Chunks: wireRefs, ContentHash: hash, GeneratedAt: now}
	if err := wmanifest.Validate(); err != nil {
		return nil, err
	}
	for _, target := range servingTargets {
		token := regionToken(target)
		eventID := token + ":" + name
		env, err := model.NewEnvelope(model.KindPublicationManifest, eventID, target.Region, target.Shard, string(z.UID), uint64(epoch), uint64(revision), now, wmanifest)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(env)
		out := r.outbox(model.PublicationActivationName(name, target.Region, target.Shard), target.Region, target.Shard, model.RecordManifestSubject(target.Region, target.Shard, string(z.UID)), z.UID, epoch, revision, name, b, true, depsByRegion[token])
		if err := r.createImmutable(ctx, out); err != nil {
			return nil, err
		}
	}
	return m, nil
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

type heldOwnership struct {
	own     *dnsv1alpha1.DNSPublicationOwnership
	retryAt time.Time
	err     error
}

// holdOwnership acquires or renews every zone lease before any zone is
// compiled, so a long pass cannot outlast the leases of zones it reaches late.
// A held lease is renewed once a third of it has elapsed, which every pass
// does because passes start at most half a lease apart.
func (r *Reconciler) holdOwnership(ctx context.Context, z *dnsv1alpha1.DNSZone) (*dnsv1alpha1.DNSPublicationOwnership, time.Time, error) {
	own, retryAt, err := r.acquireOwnership(ctx, z, r.Options.Now().UTC())
	if err != nil || own == nil {
		return nil, retryAt, err
	}
	renewedAt := r.Options.Now().UTC()
	if own.Spec.LeaseUntil.After(renewedAt.Add(r.Options.LeaseDuration * 2 / 3)) {
		return own, time.Time{}, nil
	}
	base := own.DeepCopy()
	own.Spec.LeaseUntil = metav1.NewTime(renewedAt.Add(r.Options.LeaseDuration))
	if err := r.Patch(ctx, own, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return nil, time.Time{}, err
	}
	return own, time.Time{}, nil
}

func (r *Reconciler) acquireOwnership(ctx context.Context, z *dnsv1alpha1.DNSZone, now time.Time) (*dnsv1alpha1.DNSPublicationOwnership, time.Time, error) {
	name := "owner-" + model.OpaqueToken(string(z.UID))[:20]
	o := &dnsv1alpha1.DNSPublicationOwnership{}
	key := client.ObjectKey{Namespace: r.Options.PlatformNamespace, Name: name}
	err := r.Get(ctx, key, o)
	if apierrors.IsNotFound(err) {
		o = &dnsv1alpha1.DNSPublicationOwnership{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: key.Namespace}, Spec: dnsv1alpha1.DNSPublicationOwnershipSpec{ZoneUID: z.UID, HolderIdentity: r.Options.Identity, WriterEpoch: 1, NextRevision: 1, LeaseUntil: metav1.NewTime(now.Add(r.Options.LeaseDuration))}}
		if err := r.Create(ctx, o); err != nil && !apierrors.IsAlreadyExists(err) {
			return nil, time.Time{}, err
		}
		return nil, now, nil
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	if o.Spec.ZoneUID != z.UID {
		return nil, time.Time{}, fmt.Errorf("ownership collision for %s", z.UID)
	}
	if o.Spec.HolderIdentity != r.Options.Identity {
		if o.Spec.LeaseUntil.After(now) {
			return nil, o.Spec.LeaseUntil.Time, nil
		}
		base := o.DeepCopy()
		o.Spec.HolderIdentity = r.Options.Identity
		o.Spec.WriterEpoch++
		o.Spec.LeaseUntil = metav1.NewTime(now.Add(r.Options.LeaseDuration))
		if err := r.Patch(ctx, o, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return nil, time.Time{}, err
		}
	}
	return o, time.Time{}, nil
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
		p.Contributions = append(p.Contributions, ContributionFence{UID: types.UID(f.ContributionUID), Generation: int64(f.Generation), Sequence: int64(f.Sequence), Retired: f.Retired, SourceName: f.SourceName})
	}
	return p, nil
}

func (r *Reconciler) patchInputStatuses(ctx context.Context, zone *dnsv1alpha1.DNSZone, result CompileResult, regs []dnsv1alpha1.DNSRegistration, cs []dnsv1alpha1.DNSRecordContribution, records []dnsv1alpha1.DNSRecordSet, now time.Time) error {
	for i := range records {
		record := &records[i]
		if record.Spec.DNSZoneRef.Name != zone.Name || !record.DeletionTimestamp.IsZero() {
			continue
		}
		base := record.DeepCopy()
		reason := result.Reasons[record.UID]
		if reason == "" {
			reason = dnsValueAccepted
		}
		setCondition(&record.Status.Conditions, dnsValueAccepted, boolCondition(result.Accepted[record.UID]), reason, reason, record.Generation, now)
		if !reflect.DeepEqual(base.Status, record.Status) {
			if err := r.Status().Patch(ctx, record, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	relevant := map[types.UID]bool{}
	for i := range regs {
		reg := &regs[i]
		if reg.Spec.DNSZoneRef.Name != zone.Name {
			continue
		}
		relevant[reg.UID] = true
		accepted := result.Accepted[reg.UID]
		reason := result.Reasons[reg.UID]
		if reason == "" {
			reason = dnsValueAccepted
		}
		base := reg.DeepCopy()
		reg.Status.CanonicalFQDN = absoluteOwner(reg.Spec.Name, result.Plan.ZoneApex)
		if !accepted {
			reg.Status.CanonicalFQDN = ""
			reg.Status.PublicationWriterEpoch = 0
			reg.Status.PublicationRevision = 0
			projectstatus.Published(&reg.Status.Conditions, false, reg.Generation, now)
		}
		reg.Status.ObservedGeneration = reg.Generation
		setCondition(&reg.Status.Conditions, dnsValueAccepted, boolCondition(accepted), reason, reason, reg.Generation, now)
		if reflect.DeepEqual(base.Status, reg.Status) {
			continue
		}
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
			reason = dnsValueAccepted
		}
		base := c.DeepCopy()
		c.Status.ObservedGeneration = c.Generation
		setCondition(&c.Status.Conditions, dnsValueAccepted, boolCondition(accepted), reason, reason, c.Generation, now)
		if reflect.DeepEqual(base.Status, c.Status) {
			continue
		}
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
	if !manifest.Spec.Tombstone || manifest.Spec.ZoneRef.UID != o.Spec.ZoneUID || manifest.Spec.WriterEpoch > o.Spec.WriterEpoch {
		return false
	}
	// Zero targets are safe only after this manifest became the ownership commit
	// pointer. publicationTargets retains prior targets for every withdrawal.
	return len(manifest.Spec.ServingTargets) == 0 || (c != nil && c.Status == metav1.ConditionTrue)
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

func (r *Reconciler) confirmContributions(ctx context.Context, namespace string, listed []dnsv1alpha1.DNSRecordContribution, previous *PublicationPlan) ([]dnsv1alpha1.DNSRecordContribution, error) {
	reader := r.sourceReader()
	names := map[string]bool{}
	for _, c := range listed {
		names[c.Name] = true
	}
	if previous != nil {
		for _, fence := range previous.Contributions {
			if !fence.Retired {
				if fence.SourceName == "" {
					return nil, fmt.Errorf("contribution %s has no source identity for lifetime confirmation", fence.UID)
				}
				names[fence.SourceName] = true
			}
		}
	}
	var current dnsv1alpha1.DNSRecordContributionList
	if err := reader.List(ctx, &current, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	confirmed := make([]dnsv1alpha1.DNSRecordContribution, 0, len(names))
	for _, c := range current.Items {
		if names[c.Name] {
			confirmed = append(confirmed, c)
		}
	}
	return confirmed, nil
}
