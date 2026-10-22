// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"sort"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func jsonEqual(a, b any) bool { return reflect.DeepEqual(a, b) }

func (r *Reconciler) reconcileNamingPolicies(ctx context.Context, items []dnsv1alpha1.DNSNamingPolicy, assocs []dnsv1alpha1.DNSZoneAssociation, zones map[string]*dnsv1alpha1.DNSZone, now time.Time) error {
	for i := range items {
		p := &items[i]
		contextUID, contextName := types.UID(""), ""
		valid := false

		var resolverContext dnsv1alpha1.DNSResolverContext
		err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: p.Spec.ResolverContextRef.Name}, &resolverContext)
		ready := apimeta.FindStatusCondition(resolverContext.Status.Conditions, "Ready")
		valid = err == nil && p.Spec.ResolverContextRef.UID != "" && p.Spec.ResolverContextRef.UID == resolverContext.UID && resolverContext.DeletionTimestamp.IsZero() && ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == resolverContext.Generation
		if valid {
			contextUID = resolverContext.UID
			contextName = resolverContext.Name
		}

		resolved := []dnsv1alpha1.DNSResolvedAdditionalNameRule{}
		seen := map[string]bool{}
		for _, rule := range p.Spec.AdditionalNames {
			z := zones[rule.DNSZoneRef.Name]
			associated := associatedWithContext(assocs, zUID(z), contextUID)

			if z == nil || z.Spec.Visibility != dnsv1alpha1.DNSZoneVisibilityPrivate || !refMatches(rule.DNSZoneRef, z.Name, z.UID, z.Generation) || !associated {
				valid = false
				continue
			}
			selector, err := metav1.LabelSelectorAsSelector(rule.Selector.AsLabelSelector())
			if err != nil {
				valid = false
				continue
			}
			key := rule.DNSZoneRef.Name + "\x00" + canonicalName(rule.NamePrefix) + "\x00" + selector.String()
			if rule.NamePrefix == "" || seen[key] {
				valid = false
				continue
			}
			seen[key] = true
			resolved = append(resolved, dnsv1alpha1.DNSResolvedAdditionalNameRule{Selector: rule.Selector.AsLabelSelector().DeepCopy(), DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: z.Name, UID: z.UID, Generation: z.Generation}, NamePrefix: canonicalName(rule.NamePrefix)})
		}
		base := p.DeepCopy()

		p.Status.ResolvedResolverContextRef = dnsv1alpha1.DNSObjectReference{Name: contextName, UID: contextUID}

		p.Status.ResolvedAdditionalNames = resolved
		status := metav1.ConditionTrue
		reason, msg := dnsValueAccepted, "naming policy is valid"
		if !valid {
			status = metav1.ConditionFalse
			reason = "InvalidPolicy"
			msg = "all rules require an associated private zone and unique naming rules"
		}
		if setCondition(&p.Status.Conditions, dnsValueAccepted, status, reason, msg, p.Generation, now) {
			if err := r.Status().Patch(ctx, p, client.MergeFrom(base)); err != nil {
				return err
			}
		}
	}
	return nil
}

func zUID(z *dnsv1alpha1.DNSZone) types.UID {
	if z == nil {
		return ""
	}
	return z.UID
}

func associatedWithContext(assocs []dnsv1alpha1.DNSZoneAssociation, zoneUID, contextUID types.UID) bool {
	for _, a := range assocs {
		c := apimeta.FindStatusCondition(a.Status.Conditions, dnsValueAccepted)
		if c != nil && c.Status == metav1.ConditionTrue && a.Status.ResolvedDNSZoneRef.UID == zoneUID && a.Status.ResolvedResolverContextRef.UID == contextUID {
			return true
		}
	}
	return false
}

func (r *Reconciler) reconcileResolverContexts(ctx context.Context, ns string, now time.Time) error {
	var contexts dnsv1alpha1.DNSResolverContextList
	var existing dnsv1alpha1.DNSResolverBindingList
	if err := r.List(ctx, &existing, client.InNamespace(r.Options.PlatformNamespace)); err != nil {
		return err
	}
	if err := r.List(ctx, &contexts, client.InNamespace(ns)); err != nil {
		return err
	}
	sort.Slice(contexts.Items, func(i, j int) bool {
		left, right := contexts.Items[i], contexts.Items[j]
		if !left.CreationTimestamp.Equal(&right.CreationTimestamp) {
			return left.CreationTimestamp.Before(&right.CreationTimestamp)
		}
		return left.UID < right.UID
	})
	consumerOwners := map[string]types.UID{}
	for i := range contexts.Items {
		c := &contexts.Items[i]
		base := c.DeepCopy()
		valid := c.Spec.ConsumerID != "" && c.DeletionTimestamp.IsZero()
		duplicate := consumerOwners[c.Spec.ConsumerID] != "" && consumerOwners[c.Spec.ConsumerID] != c.UID
		if valid && !duplicate {
			consumerOwners[c.Spec.ConsumerID] = c.UID
		}
		valid = valid && !duplicate
		invalidDomain := c.Spec.ManagedNamespace.Enabled && len(validation.IsDNS1123Subdomain(c.Spec.ManagedNamespace.DomainSuffix)) > 0
		valid = valid && !invalidDomain
		c.Status.ServingTargets = nil
		if valid {
			for _, target := range r.selectServingTargets(c.UID, existing.Items) {
				c.Status.ServingTargets = append(c.Status.ServingTargets, dnsv1alpha1.DNSResolverContextServingTarget{Region: target.Region, Shard: target.Shard})
			}
		}
		ready := valid && !c.Spec.ManagedNamespace.Enabled
		c.Status.ManagedNamespace = dnsv1alpha1.DNSResolverContextManagedNamespaceStatus{}
		if c.Spec.ManagedNamespace.Enabled && valid {
			var err error
			ready, err = r.reconcileManagedZone(ctx, c)
			if err != nil {
				return err
			}
		}
		status, reason, msg := metav1.ConditionTrue, dnsValueAccepted, "resolver context is active"
		if !valid {
			status, reason, msg = metav1.ConditionFalse, "InvalidIdentity", "consumerID is required and context must be live"
			if duplicate {
				reason, msg = "DuplicateConsumerID", "another live resolver context owns this consumerID"
			}
			if invalidDomain {
				reason, msg = "InvalidManagedDomain", "an enabled managed namespace requires a valid domainSuffix"
			}
		}
		if valid && !ready {
			status, reason, msg = metav1.ConditionFalse, "Preparing", "managed namespace is not ready"
		}
		setCondition(&c.Status.Conditions, "Ready", status, reason, msg, c.Generation, now)
		if !jsonEqual(base.Status, c.Status) {
			if err := r.Status().Patch(ctx, c, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Reconciler) reconcileAssociations(ctx context.Context, items []dnsv1alpha1.DNSZoneAssociation, zones map[string]*dnsv1alpha1.DNSZone, now time.Time) error {
	type apexKey struct {
		resolverContextUID types.UID
		apex               string
	}
	owners := map[apexKey]types.UID{}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	for i := range items {
		a := &items[i]
		z := zones[a.Spec.DNSZoneRef.Name]
		status := metav1.ConditionTrue
		reason, msg := dnsValueAccepted, "association accepted"
		contextUID, contextErr := types.UID(""), error(nil)

		var resolverContext dnsv1alpha1.DNSResolverContext
		contextErr = r.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: a.Spec.ResolverContextRef.Name}, &resolverContext)
		if contextErr == nil && (a.Spec.ResolverContextRef.UID == "" || a.Spec.ResolverContextRef.UID != resolverContext.UID || !resolverContext.DeletionTimestamp.IsZero()) {
			contextErr = errors.New("resolver context lifetime is not current")
		}
		if contextErr == nil {
			contextUID = resolverContext.UID
		}

		if !a.DeletionTimestamp.IsZero() || z == nil || z.Spec.Visibility != dnsv1alpha1.DNSZoneVisibilityPrivate || contextErr != nil || !refMatches(a.Spec.DNSZoneRef, z.Name, z.UID, z.Generation) {
			status = metav1.ConditionFalse
			reason = "InvalidReference"
			msg = "private zone and pinned context/zone UIDs are required"
		} else if old := owners[apexKey{contextUID, canonicalName(z.Spec.DomainName)}]; old != "" && old != z.UID {
			status = metav1.ConditionFalse
			reason = "ApexConflict"
			msg = "another zone owns this apex in the resolver context"
		} else {
			owners[apexKey{contextUID, canonicalName(z.Spec.DomainName)}] = z.UID
		}
		base := a.DeepCopy()

		a.Status.ResolvedResolverContextRef = dnsv1alpha1.DNSObjectReference{Name: a.Spec.ResolverContextRef.Name, UID: contextUID}

		if z != nil {
			a.Status.ResolvedDNSZoneRef = dnsv1alpha1.DNSObjectReference{Name: z.Name, UID: z.UID, Generation: z.Generation}
		}
		if setCondition(&a.Status.Conditions, dnsValueAccepted, status, reason, msg, a.Generation, now) {
			if err := r.Status().Patch(ctx, a, client.MergeFrom(base)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Reconciler) reconcileBindings(ctx context.Context, ns string, assocs []dnsv1alpha1.DNSZoneAssociation, zones map[string]*dnsv1alpha1.DNSZone, now time.Time) error {
	return r.reconcileContextBindings(ctx, ns, assocs, zones, now)
}

func (r *Reconciler) reconcileContextBindings(ctx context.Context, ns string, assocs []dnsv1alpha1.DNSZoneAssociation, zones map[string]*dnsv1alpha1.DNSZone, now time.Time) error {
	var accesses dnsv1alpha1.DNSResolverAccessBindingList
	var existing dnsv1alpha1.DNSResolverBindingList
	if err := r.List(ctx, &accesses, client.InNamespace(ns)); err != nil {
		return err
	}
	if err := r.List(ctx, &existing, client.InNamespace(r.Options.PlatformNamespace)); err != nil {
		return err
	}
	zonesByContext := map[types.UID][]types.UID{}
	for _, a := range assocs {
		z := zones[a.Spec.DNSZoneRef.Name]
		accepted := apimeta.FindStatusCondition(a.Status.Conditions, dnsValueAccepted)
		if z != nil && accepted != nil && accepted.Status == metav1.ConditionTrue {
			zonesByContext[a.Status.ResolvedResolverContextRef.UID] = append(zonesByContext[a.Status.ResolvedResolverContextRef.UID], z.UID)
		}
	}
	sort.Slice(accesses.Items, func(i, j int) bool { return accesses.Items[i].Name < accesses.Items[j].Name })
	destinations := map[string]types.UID{}
	desired := map[string]bool{}
	for i := range accesses.Items {
		a := &accesses.Items[i]
		base := a.DeepCopy()
		var current dnsv1alpha1.DNSResolverAccessBinding
		if err := r.sourceReader().Get(ctx, client.ObjectKeyFromObject(a), &current); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if current.UID != a.UID || current.ResourceVersion != a.ResourceVersion {
			continue
		}
		spec := a.Spec
		var contextObject *dnsv1alpha1.DNSResolverContext
		var currentContext dnsv1alpha1.DNSResolverContext
		if err := r.sourceReader().Get(ctx, client.ObjectKey{Namespace: ns, Name: spec.ContextRef.Name}, &currentContext); err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}
			contextObject = nil
		} else if currentContext.UID == spec.ContextRef.UID {
			contextObject = &currentContext
		} else {
			contextObject = nil
		}
		contextReady := resolverContextReady(contextObject)
		valid := a.DeletionTimestamp.IsZero() && contextObject != nil && contextObject.DeletionTimestamp.IsZero() && contextObject.Name == spec.ContextRef.Name && contextReady
		if _, err := netip.ParseAddr(spec.DestinationAddress); err != nil {
			valid = false
		}
		if _, ok := r.targetForRegion(spec.Region); !ok {
			valid = false
		}
		key := spec.Region + "\x00" + spec.DestinationAddress
		if owner := destinations[key]; owner != "" && owner != a.UID {
			valid = false
		} else if valid {
			if r.Options.AddressAllocator == nil {
				valid = false
			} else if err := r.Options.AddressAllocator.ClaimDestination(ctx, r.Options.ProjectUID, contextObject.UID, spec.Region, netip.MustParseAddr(spec.DestinationAddress).String(), 53); errors.Is(err, model.ErrDestinationRefused) {
				valid = false
			} else if err != nil {
				// A transient API failure is not a refusal: retry rather
				// than flip an accepted binding to AccessRejected.
				return fmt.Errorf("claim service destination: %w", err)
			} else {
				destinations[key] = a.UID
			}
		}
		status, reason, msg := metav1.ConditionTrue, dnsValueAccepted, "access binding is current"
		if !valid {
			status, reason, msg = metav1.ConditionFalse, "InvalidOrStaleAccess", "context lifetime and destination must be current"
		}
		setCondition(&a.Status.Conditions, dnsValueAccepted, status, reason, msg, a.Generation, now)
		if !valid {
			a.Status.BindingRef = nil
			setCondition(&a.Status.Conditions, "Ready", metav1.ConditionFalse, "AccessRejected", "access binding is not accepted", a.Generation, now)
			if !jsonEqual(base.Status, a.Status) {
				if err := r.Status().Patch(ctx, a, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
					return err
				}
			}
			continue
		}
		zids := append([]types.UID(nil), zonesByContext[contextObject.UID]...)
		sort.Slice(zids, func(i, j int) bool { return zids[i] < zids[j] })
		name := "binding-" + model.OpaqueToken(string(r.Options.ProjectUID) + "/" + string(a.UID))[:20]
		desired[name] = true
		var old dnsv1alpha1.DNSResolverBinding
		err := r.Get(ctx, client.ObjectKey{Namespace: r.Options.PlatformNamespace, Name: name}, &old)
		exists := err == nil
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		cluster := ""
		generation, revision := int64(1), int64(1)
		version := dnsv1alpha1.DNSResolverAccessVersion{Epoch: 1, Sequence: 1}
		if exists {
			version = old.Spec.Version
			if old.Spec.Tombstone {
				version.Sequence++
			}
			cluster = old.Spec.Configuration.Query.RegionalAddress
			generation = old.Spec.Configuration.Generation
			revision = old.Spec.Configuration.Revision
			if !zoneUIDsEqual(old.Spec.Configuration.ZoneRefs, zids) {
				generation++
				revision++
			} else if old.Spec.Tombstone {
				revision++
			}
		} else {
			if r.Options.AddressAllocator == nil {
				return errors.New("address allocator is required for context binding")
			}
			cluster, err = r.Options.AddressAllocator.Allocate(ctx, r.Options.ProjectUID, contextObject.UID)
			if err != nil {
				return err
			}
		}
		binding := &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Options.PlatformNamespace, Labels: map[string]string{"internal-dns.miloapis.com/source-namespace": ns, "internal-dns.miloapis.com/context-uid": string(contextObject.UID), "internal-dns.miloapis.com/access-uid": string(a.UID), "internal-dns.miloapis.com/region": model.SafeToken(spec.Region), "internal-dns.miloapis.com/shard": model.SafeToken(r.shardForContextRegion(contextObject.UID, spec.Region, existing.Items))}}, Spec: dnsv1alpha1.DNSResolverBindingSpec{Source: dnsv1alpha1.DNSResolverBindingSource{ProjectUID: r.Options.ProjectUID, ResolverContextRef: dnsv1alpha1.DNSObjectReference{Name: contextObject.Name, UID: contextObject.UID}, AccessBindingRef: dnsv1alpha1.DNSObjectReference{Name: a.Name, UID: a.UID, Generation: a.Generation}}, Placement: dnsv1alpha1.DNSResolverBindingPlacement{Region: spec.Region, Shard: r.shardForContextRegion(contextObject.UID, spec.Region, existing.Items)}, Configuration: dnsv1alpha1.DNSResolverBindingConfiguration{Generation: generation, Revision: revision, Query: dnsv1alpha1.DNSResolverBindingQuery{NodeAddress: spec.DestinationAddress, RegionalAddress: cluster}, ZoneRefs: zoneReferences(zids, zones), ZoneApices: zoneApices(zids, zones)}, Version: version}}
		if !exists {
			if err := r.Create(ctx, binding); err != nil {
				return err
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(binding), binding); err != nil {
				return err
			}
		} else if !bindingEqual(old.Spec, binding.Spec) || old.Labels["internal-dns.miloapis.com/source-namespace"] != ns {
			baseOld := old.DeepCopy()
			old.Spec = binding.Spec
			old.Labels = binding.Labels
			if err := r.Patch(ctx, &old, client.MergeFromWithOptions(baseOld, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
			binding = &old
		} else {
			binding = &old
		}
		a.Status.ObservedGeneration = a.Generation
		a.Status.BindingRef = &dnsv1alpha1.DNSObjectReference{Name: binding.Name, UID: binding.UID}
		setCondition(&a.Status.Conditions, "Ready", metav1.ConditionFalse, "PendingReplicaVerification", "serving fleet acknowledgement is pending", a.Generation, now)
		if c := apimeta.FindStatusCondition(binding.Status.Conditions, "Serving"); c != nil && c.Status == metav1.ConditionTrue && binding.Status.ObservedConfigurationRevision == binding.Spec.Configuration.Revision && bindingAckCurrent(binding, now) {
			setCondition(&a.Status.Conditions, "Ready", metav1.ConditionTrue, "Ready", "serving capacity applied this access generation", a.Generation, now)
		} else if c != nil && c.Status == metav1.ConditionUnknown {
			setCondition(&a.Status.Conditions, "Ready", metav1.ConditionUnknown, "ObservationUnavailable", "current serving health is unavailable", a.Generation, now)
		}
		if !jsonEqual(base.Status, a.Status) {
			if err := r.Status().Patch(ctx, a, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
	}
	for i := range existing.Items {
		b := &existing.Items[i]
		if b.Spec.Source.ProjectUID != r.Options.ProjectUID || desired[b.Name] || b.Spec.Tombstone {
			continue
		}
		if b.Labels["internal-dns.miloapis.com/context-uid"] == "" {
			continue
		}
		sourceNamespace := b.Labels["internal-dns.miloapis.com/source-namespace"]
		if sourceNamespace != "" && sourceNamespace != ns {
			continue
		}
		if sourceNamespace == "" && r.Options.ProjectNamespace == "" {
			var sourceAccesses dnsv1alpha1.DNSResolverAccessBindingList
			if err := r.sourceReader().List(ctx, &sourceAccesses); err != nil {
				return err
			}
			for j := range sourceAccesses.Items {
				if sourceAccesses.Items[j].UID == b.Spec.Source.AccessBindingRef.UID {
					sourceNamespace = sourceAccesses.Items[j].Namespace
					break
				}
			}
			if sourceNamespace != "" && sourceNamespace != ns {
				continue
			}
		}
		withdraw, err := r.confirmAccessWithdrawal(ctx, ns, b)
		if err != nil {
			return err
		}
		if !withdraw {
			continue
		}
		base := b.DeepCopy()
		b.Spec.Tombstone = true
		b.Spec.Configuration.Revision++
		b.Spec.Version.Sequence++
		if err := r.Patch(ctx, b, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	return nil
}

// confirmAccessWithdrawal prevents a stale or incomplete source List from
// withdrawing a binding renewed by another controller. Read uncertainty leaves
// the existing version intact; serving agents retain the applied configuration.
func (r *Reconciler) confirmAccessWithdrawal(ctx context.Context, namespace string, binding *dnsv1alpha1.DNSResolverBinding) (bool, error) {
	var access dnsv1alpha1.DNSResolverAccessBinding
	if err := r.sourceReader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: binding.Spec.Source.AccessBindingRef.Name}, &access); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if access.UID != binding.Spec.Source.AccessBindingRef.UID || !access.DeletionTimestamp.IsZero() {
		return true, nil
	}
	if access.Spec.ContextRef.Name != binding.Spec.Source.ResolverContextRef.Name || access.Spec.ContextRef.UID != binding.Spec.Source.ResolverContextRef.UID {
		return true, nil
	}
	var resolverContext dnsv1alpha1.DNSResolverContext
	if err := r.sourceReader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: binding.Spec.Source.ResolverContextRef.Name}, &resolverContext); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if resolverContext.UID != binding.Spec.Source.ResolverContextRef.UID || !resolverContext.DeletionTimestamp.IsZero() {
		return true, nil
	}
	ready := apimeta.FindStatusCondition(resolverContext.Status.Conditions, "Ready")
	if ready == nil || ready.ObservedGeneration != resolverContext.Generation {
		return false, nil
	}
	if ready.Status != metav1.ConditionTrue {
		return ready.Reason == "InvalidIdentity" || ready.Reason == "DuplicateConsumerID" || ready.Reason == "InvalidManagedDomain", nil
	}
	if access.Generation < binding.Spec.Source.AccessBindingRef.Generation {
		return false, nil
	}
	valid := access.Spec.Region == binding.Spec.Placement.Region && access.Spec.DestinationAddress == binding.Spec.Configuration.Query.NodeAddress
	return !valid, nil
}

func (r *Reconciler) shardForContextRegion(contextUID types.UID, region string, existing []dnsv1alpha1.DNSResolverBinding) string {
	for _, target := range r.selectServingTargets(contextUID, existing) {
		if target.Region == region {
			return target.Shard
		}
	}
	return ""
}

func (r *Reconciler) targetForRegion(region string) (ServingRegion, bool) {
	for _, s := range r.Options.servingRegions() {
		if s.Region == region {
			return s, true
		}
	}
	return ServingRegion{}, false
}

func bindingAckCurrent(binding *dnsv1alpha1.DNSResolverBinding, now time.Time) bool {
	for _, ack := range binding.Status.MemberAcknowledgements {
		// Serving=True is the aggregate full-member gate computed by AckSink.
		// This row check additionally prevents a stale condition from surviving
		// past the exact configuration/version generation or its local ACK lease.
		if ack.Phase == string(model.AckVerified) && ack.Revision == binding.Spec.Configuration.Revision && ack.AccessEpoch == binding.Spec.Version.Epoch && ack.AccessSequence == binding.Spec.Version.Sequence && ack.ValidUntil != nil && ack.ValidUntil.After(now) {
			return true
		}
	}
	return false
}

// selectServingTargets assigns one shared shard in each region. An existing
// live assignment wins while its shard remains configured, so adding capacity
// does not silently move established resolver listeners. Explicit migration is a
// separate operation; only a missing/removed assignment invokes rendezvous.
func (r *Reconciler) selectServingTargets(resolverContextUID types.UID, existing []dnsv1alpha1.DNSResolverBinding) []ServingRegion {
	byRegion := map[string][]ServingRegion{}
	for _, target := range r.Options.servingRegions() {
		byRegion[target.Region] = append(byRegion[target.Region], target)
	}
	regions := make([]string, 0, len(byRegion))
	for region := range byRegion {
		regions = append(regions, region)
	}
	sort.Strings(regions)
	var selected []ServingRegion
	for _, region := range regions {
		candidates := byRegion[region]
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].Shard < candidates[j].Shard })
		var preserved *ServingRegion
		for _, binding := range existing {
			if binding.Spec.Source.ProjectUID != r.Options.ProjectUID || binding.Spec.Source.ResolverContextRef.UID != resolverContextUID || binding.Spec.Placement.Region != region || binding.Spec.Tombstone || !binding.DeletionTimestamp.IsZero() {
				continue
			}
			for _, candidate := range candidates {
				if binding.Spec.Placement.Shard == candidate.Shard {
					c := candidate
					if preserved == nil || c.Shard < preserved.Shard {
						preserved = &c
					}
				}
			}
		}
		if preserved != nil {
			selected = append(selected, *preserved)
			continue
		}
		best := candidates[0]
		bestScore := ""
		for _, candidate := range candidates {
			sum := sha256.Sum256([]byte(string(r.Options.ProjectUID) + "/" + string(resolverContextUID) + "/" + region + "/" + candidate.Shard))
			score := string(sum[:])
			if bestScore == "" || score > bestScore {
				best, bestScore = candidate, score
			}
		}
		selected = append(selected, best)
	}
	return selected
}

func (r *Reconciler) createIfAbsent(ctx context.Context, o client.Object) error {
	err := r.Create(ctx, o)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func setCondition(cs *[]metav1.Condition, t string, status metav1.ConditionStatus, reason, msg string, generation int64, now time.Time) bool {
	return apimeta.SetStatusCondition(cs, metav1.Condition{Type: t, Status: status, Reason: reason, Message: msg, ObservedGeneration: generation, LastTransitionTime: metav1.NewTime(now)})
}

// BindingAddressCandidates supplies stable IPv6 candidates to a durable global
// allocator. Callers must collision-check, persist, and quarantine leases.
func BindingAddressCandidates(uid types.UID) (string, string) {
	sum := sha256.Sum256([]byte(uid))
	a := binary.BigEndian.Uint64(sum[:8])
	b := binary.BigEndian.Uint64(sum[8:16])
	return netip.AddrFrom16([16]byte{0xfd, 0x53, byte(a >> 56), byte(a >> 48), byte(a >> 40), byte(a >> 32), byte(a >> 24), byte(a >> 16), byte(a >> 8), byte(a), byte(b >> 40), byte(b >> 32), byte(b >> 24), byte(b >> 16), byte(b >> 8), byte(b)}).String(), netip.AddrFrom16([16]byte{0xfd, 0x54, byte(a >> 56), byte(a >> 48), byte(a >> 40), byte(a >> 32), byte(a >> 24), byte(a >> 16), byte(a >> 8), byte(a), byte(b >> 40), byte(b >> 32), byte(b >> 24), byte(b >> 16), byte(b >> 8), byte(b)}).String()
}

func bindingEqual(a, b dnsv1alpha1.DNSResolverBindingSpec) bool {
	x := a
	y := b
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return string(xb) == string(yb)
}

func zoneApices(uids []types.UID, zones map[string]*dnsv1alpha1.DNSZone) map[string]string {
	wanted := map[types.UID]bool{}
	for _, uid := range uids {
		wanted[uid] = true
	}
	apices := map[string]string{}
	for _, zone := range zones {
		if wanted[zone.UID] {
			apices[string(zone.UID)] = zone.Spec.DomainName
		}
	}
	return apices
}

func zoneReferences(uids []types.UID, zones map[string]*dnsv1alpha1.DNSZone) []dnsv1alpha1.DNSObjectReference {
	refs := make([]dnsv1alpha1.DNSObjectReference, 0, len(uids))
	for _, uid := range uids {
		for _, zone := range zones {
			if zone.UID == uid {
				refs = append(refs, dnsv1alpha1.DNSObjectReference{Name: zone.Name, UID: uid})
				break
			}
		}
	}
	return refs
}

func zoneUIDsEqual(refs []dnsv1alpha1.DNSObjectReference, uids []types.UID) bool {
	if len(refs) != len(uids) {
		return false
	}
	for i := range refs {
		if refs[i].UID != uids[i] {
			return false
		}
	}
	return true
}

func resolverContextReady(contextObject *dnsv1alpha1.DNSResolverContext) bool {
	if contextObject == nil {
		return false
	}
	ready := apimeta.FindStatusCondition(contextObject.Status.Conditions, "Ready")
	return ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == contextObject.Generation
}

func (r *Reconciler) reconcileManagedZone(ctx context.Context, c *dnsv1alpha1.DNSResolverContext) (bool, error) {
	suffix := canonicalName(c.Spec.ManagedNamespace.DomainSuffix)
	zoneName := "managed-" + model.OpaqueToken(string(c.UID))[:12]
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: zoneName, Namespace: c.Namespace, Labels: map[string]string{"dns.networking.miloapis.com/managed": "true"}}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: suffix, DNSZoneClassName: r.Options.PrivateZoneClassName, Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	if r.Scheme != nil {
		if err := controllerutil.SetControllerReference(c, zone, r.Scheme, controllerutil.WithBlockOwnerDeletion(false)); err != nil {
			return false, err
		}
	}
	if err := r.createIfAbsent(ctx, zone); err != nil {
		return false, err
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(zone), zone); err != nil {
		return false, err
	}
	association := &dnsv1alpha1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: zoneName + "-context", Namespace: c.Namespace, Labels: map[string]string{"dns.networking.miloapis.com/managed": "true"}}, Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: zone.Name, UID: zone.UID}, ResolverContextRef: dnsv1alpha1.DNSObjectReference{Name: c.Name, UID: c.UID}}}
	if r.Scheme != nil {
		if err := controllerutil.SetControllerReference(c, association, r.Scheme, controllerutil.WithBlockOwnerDeletion(false)); err != nil {
			return false, err
		}
	}
	if err := r.createIfAbsent(ctx, association); err != nil {
		return false, err
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(association), association); err != nil {
		return false, err
	}
	accepted := apimeta.FindStatusCondition(association.Status.Conditions, dnsValueAccepted)
	ready := metav1.IsControlledBy(zone, c) && metav1.IsControlledBy(association, c) && zone.Spec.Visibility == dnsv1alpha1.DNSZoneVisibilityPrivate && zone.Spec.DomainName == suffix && zone.DeletionTimestamp.IsZero() && accepted != nil && accepted.Status == metav1.ConditionTrue && accepted.ObservedGeneration == association.Generation && association.Status.ResolvedDNSZoneRef.UID == zone.UID && association.Status.ResolvedResolverContextRef.UID == c.UID
	if ready {
		c.Status.ManagedNamespace = dnsv1alpha1.DNSResolverContextManagedNamespaceStatus{DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: zone.Name, UID: zone.UID}, Suffix: suffix}
	}
	return ready, nil
}
