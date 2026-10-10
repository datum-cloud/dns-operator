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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func jsonEqual(a, b any) bool { return reflect.DeepEqual(a, b) }

func (r *Reconciler) reconcileNamingPolicies(ctx context.Context, items []dnsv1alpha1.DNSNamingPolicy, assocs []dnsv1alpha1.DNSZoneAssociation, zones map[string]*dnsv1alpha1.DNSZone, now time.Time) error {
	for i := range items {
		p := &items[i]
		vpcUID, contextUID, contextName := types.UID(""), types.UID(""), ""
		valid := false

		var resolverContext dnsv1alpha1.DNSResolverContext
		err := r.Get(ctx, client.ObjectKey{Namespace: p.Namespace, Name: p.Spec.ResolverContextRef.Name}, &resolverContext)
		ready := apimeta.FindStatusCondition(resolverContext.Status.Conditions, "Ready")
		valid = err == nil && p.Spec.ResolverContextRef.UID != "" && p.Spec.ResolverContextRef.UID == resolverContext.UID && resolverContext.DeletionTimestamp.IsZero() && ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == resolverContext.Generation
		if valid {
			vpcUID = types.UID(resolverContext.Spec.ConsumerID)
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
			key := string(rule.RegistrationClass) + "\x00" + canonicalName(rule.NamePrefix)
			if rule.NamePrefix == "" || seen[key] {
				valid = false
				continue
			}
			seen[key] = true
			resolved = append(resolved, dnsv1alpha1.DNSResolvedAdditionalNameRule{RegistrationClass: rule.RegistrationClass, DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: z.Name, UID: z.UID, Generation: z.Generation}, NamePrefix: canonicalName(rule.NamePrefix)})
		}
		base := p.DeepCopy()
		p.Status.ResolvedVPCRef = dnsv1alpha1.DNSObjectReference{Name: p.Spec.VPCRef.Name, UID: vpcUID}

		p.Status.ResolvedVPCRef.Name = contextName
		p.Status.ResolvedResolverContextRef = dnsv1alpha1.DNSObjectReference{Name: contextName, UID: contextUID}

		p.Status.ResolvedAdditionalNames = resolved
		status := metav1.ConditionTrue
		reason, msg := dnsValueAccepted, "naming policy is authorized"
		if !valid {
			status = metav1.ConditionFalse
			reason = "InvalidPolicy"
			msg = "all rules require an associated private zone and unique class/prefix"
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

func (r *Reconciler) reconcileManagedNamespaces(ctx context.Context, ns string, now time.Time) error {

	var list dnsv1alpha1.DNSManagedNamespaceList
	if err := r.List(ctx, &list, client.InNamespace(ns)); err != nil {
		return err
	}
	for i := range list.Items {
		m := &list.Items[i]
		if m.Spec.ProjectUID != r.Options.ProjectUID || m.Spec.VPCRef.UID == "" {
			if err := r.patchCondition(ctx, m, dnsValueAccepted, metav1.ConditionFalse, "InvalidIdentity", "project and pinned VPC UID are required", now); err != nil {
				return err
			}
			continue
		}
		lifetimeToken := model.OpaqueToken(m.Labels["internal-dns.miloapis.com/context-uid"])

		suffix := canonicalName(m.Spec.DomainSuffix)
		zoneName := "managed-" + lifetimeToken[:12]
		zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: zoneName, Namespace: ns, Labels: map[string]string{"dns.networking.miloapis.com/managed": "true"}}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: suffix, DNSZoneClassName: m.Spec.DNSZoneClassName, Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
		if r.Scheme != nil {
			if err := controllerutil.SetControllerReference(m, zone, r.Scheme); err != nil {
				return err
			}
		}
		if err := r.createIfAbsent(ctx, zone); err != nil {
			return err
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(zone), zone); err != nil {
			return err
		}
		assocName := zoneName + "-vpc"
		assocSpec := dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: zone.Name, UID: zone.UID}, VPCRef: m.Spec.VPCRef, Managed: true}

		assocSpec.VPCRef = dnsv1alpha1.DNSObjectReference{}
		var contextObject dnsv1alpha1.DNSResolverContext
		if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: m.Spec.VPCRef.Name}, &contextObject); err != nil {
			return err
		}
		assocSpec.ResolverContextRef = dnsv1alpha1.DNSObjectReference{Name: contextObject.Name, UID: contextObject.UID}

		assoc := &dnsv1alpha1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: assocName, Namespace: ns, Labels: map[string]string{"dns.networking.miloapis.com/managed": "true"}}, Spec: assocSpec}
		if r.Scheme != nil {
			if err := controllerutil.SetControllerReference(m, assoc, r.Scheme); err != nil {
				return err
			}
		}
		if err := r.createIfAbsent(ctx, assoc); err != nil {
			return err
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(assoc), assoc); err != nil {
			return err
		}
		base := m.DeepCopy()
		accepted := apimeta.FindStatusCondition(assoc.Status.Conditions, dnsValueAccepted)
		ready := zone.Spec.DomainName == suffix && metav1.IsControlledBy(zone, m) && accepted != nil && accepted.Status == metav1.ConditionTrue && assoc.Status.ResolvedDNSZoneRef.UID == zone.UID
		if ready {
			m.Status.DNSZoneRef = dnsv1alpha1.DNSObjectReference{Name: zone.Name, UID: zone.UID}
			m.Status.AssociationRef = dnsv1alpha1.DNSObjectReference{Name: assoc.Name, UID: assoc.UID}
			m.Status.CanonicalSuffix = suffix
			setCondition(&m.Status.Conditions, dnsValueAccepted, metav1.ConditionTrue, "Ready", "managed namespace is allocated", m.Generation, now)
		} else {
			m.Status.DNSZoneRef = dnsv1alpha1.DNSObjectReference{}
			m.Status.AssociationRef = dnsv1alpha1.DNSObjectReference{}
			m.Status.CanonicalSuffix = ""
			setCondition(&m.Status.Conditions, dnsValueAccepted, metav1.ConditionFalse, "PendingAssociation", "managed namespace association is not accepted", m.Generation, now)
		}
		if err := r.Status().Patch(ctx, m, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *Reconciler) reconcileResolverContexts(ctx context.Context, ns string, now time.Time) error {
	var contexts dnsv1alpha1.DNSResolverContextList
	if err := r.List(ctx, &contexts, client.InNamespace(ns)); err != nil {
		return err
	}
	sort.Slice(contexts.Items, func(i, j int) bool { return contexts.Items[i].Name < contexts.Items[j].Name })
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
		previousReady := apimeta.FindStatusCondition(c.Status.Conditions, "Ready")
		if !valid && c.Status.AccessWriterEpoch > 0 && previousReady != nil && previousReady.Status == metav1.ConditionTrue {
			c.Status.AccessWriterEpoch++
		}
		if valid && c.Status.AccessWriterEpoch == 0 {
			c.Status.AccessWriterEpoch = 1
		}
		c.Status.ServingTargets = nil
		if valid {
			for _, target := range r.Options.servingRegions() {
				c.Status.ServingTargets = append(c.Status.ServingTargets, dnsv1alpha1.DNSResolverContextServingTarget{Region: target.Region, Shard: target.Shard})
			}
		}
		ready := valid && !c.Spec.ManagedNamespace.Enabled
		if c.Spec.ManagedNamespace.Enabled && valid {
			name := "context-" + model.OpaqueToken(string(c.UID))[:20]
			m := &dnsv1alpha1.DNSManagedNamespace{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"internal-dns.miloapis.com/platform-owned": "true", "internal-dns.miloapis.com/context-uid": string(c.UID)}}, Spec: dnsv1alpha1.DNSManagedNamespaceSpec{ProjectUID: r.Options.ProjectUID, VPCRef: dnsv1alpha1.DNSObjectReference{Name: c.Name, UID: types.UID(c.Spec.ConsumerID)}, DomainSuffix: r.Options.ManagedDomainSuffix, DNSZoneClassName: r.Options.PrivateZoneClassName}}
			if r.Scheme != nil {
				if err := controllerutil.SetControllerReference(c, m, r.Scheme); err != nil {
					return err
				}
			}
			if err := r.createIfAbsent(ctx, m); err != nil {
				return err
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(m), m); err != nil {
				return err
			}
			c.Status.ManagedNamespace.DNSZoneRef = m.Status.DNSZoneRef
			c.Status.ManagedNamespace.Suffix = m.Status.CanonicalSuffix
			if condition := apimeta.FindStatusCondition(m.Status.Conditions, dnsValueAccepted); condition != nil && condition.Status == metav1.ConditionTrue && m.Status.DNSZoneRef.UID != "" {
				ready = true
			}
		}
		status, reason, msg := metav1.ConditionTrue, dnsValueAccepted, "resolver context is active"
		if !valid {
			status, reason, msg = metav1.ConditionFalse, "InvalidIdentity", "consumerID is required and context must be live"
			if duplicate {
				reason, msg = "DuplicateConsumerID", "another live resolver context owns this consumerID"
			}
		}
		if valid && !ready {
			status, reason, msg = metav1.ConditionFalse, "Preparing", "managed namespace is not ready"
		}
		setCondition(&c.Status.Conditions, "Ready", status, reason, msg, c.Generation, now)
		if !jsonEqual(base.Status, c.Status) {
			if err := r.Status().Patch(ctx, c, client.MergeFrom(base)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *Reconciler) reconcileAssociations(ctx context.Context, items []dnsv1alpha1.DNSZoneAssociation, zones map[string]*dnsv1alpha1.DNSZone, now time.Time) error {
	type apexKey struct {
		vpc  types.UID
		apex string
	}
	owners := map[apexKey]types.UID{}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	for i := range items {
		a := &items[i]
		z := zones[a.Spec.DNSZoneRef.Name]
		status := metav1.ConditionTrue
		reason, msg := dnsValueAccepted, "association accepted"
		vpcUID, vpcErr := types.UID(""), error(nil)

		var resolverContext dnsv1alpha1.DNSResolverContext
		vpcErr = r.Get(ctx, client.ObjectKey{Namespace: a.Namespace, Name: a.Spec.ResolverContextRef.Name}, &resolverContext)
		if vpcErr == nil && (a.Spec.ResolverContextRef.UID == "" || a.Spec.ResolverContextRef.UID != resolverContext.UID || !resolverContext.DeletionTimestamp.IsZero()) {
			vpcErr = errors.New("resolver context lifetime is not current")
		}
		if vpcErr == nil {
			vpcUID = resolverContext.UID
		}

		if !a.DeletionTimestamp.IsZero() || z == nil || z.Spec.Visibility != dnsv1alpha1.DNSZoneVisibilityPrivate || vpcErr != nil || !refMatches(a.Spec.DNSZoneRef, z.Name, z.UID, z.Generation) {
			status = metav1.ConditionFalse
			reason = "InvalidReference"
			msg = "private zone and pinned VPC/zone UIDs are required"
		} else if old := owners[apexKey{vpcUID, canonicalName(z.Spec.DomainName)}]; old != "" && old != z.UID {
			status = metav1.ConditionFalse
			reason = "ApexConflict"
			msg = "another zone owns this apex in the VPC"
		} else {
			owners[apexKey{vpcUID, canonicalName(z.Spec.DomainName)}] = z.UID
		}
		base := a.DeepCopy()
		a.Status.ResolvedVPCRef = dnsv1alpha1.DNSObjectReference{Name: a.Spec.VPCRef.Name, UID: vpcUID}

		a.Status.ResolvedVPCRef = dnsv1alpha1.DNSObjectReference{}
		a.Status.ResolvedResolverContextRef = dnsv1alpha1.DNSObjectReference{Name: a.Spec.ResolverContextRef.Name, UID: vpcUID}

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
	var contexts dnsv1alpha1.DNSResolverContextList
	var existing dnsv1alpha1.DNSResolverBindingList
	if err := r.List(ctx, &accesses, client.InNamespace(ns)); err != nil {
		return err
	}
	if err := r.List(ctx, &contexts, client.InNamespace(ns)); err != nil {
		return err
	}
	if err := r.List(ctx, &existing, client.InNamespace(r.Options.PlatformNamespace)); err != nil {
		return err
	}
	bindingsByName := map[string]*dnsv1alpha1.DNSResolverBinding{}
	for i := range existing.Items {
		b := &existing.Items[i]
		if b.Spec.Source.ProjectUID == r.Options.ProjectUID {
			bindingsByName[b.Name] = b
		}
	}
	contextByUID := map[types.UID]*dnsv1alpha1.DNSResolverContext{}
	for i := range contexts.Items {
		contextByUID[contexts.Items[i].UID] = &contexts.Items[i]
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
		spec := a.Spec
		contextObject := contextByUID[spec.ContextRef.UID]
		contextReady := resolverContextReady(contextObject)
		valid := a.DeletionTimestamp.IsZero() && contextObject != nil && contextObject.DeletionTimestamp.IsZero() && contextObject.Name == spec.ContextRef.Name && contextReady && contextObject.Status.AccessWriterEpoch > 0 && spec.Authorization.WriterEpoch == contextObject.Status.AccessWriterEpoch && spec.Authorization.Sequence >= a.Status.ObservedSequence && spec.Authorization.ValidUntil.After(now) && !spec.Authorization.ValidUntil.After(now.Add(r.Options.MaxAccessLease))
		bindingName := "binding-" + model.OpaqueToken(string(r.Options.ProjectUID) + "/" + string(a.UID))[:20]
		if prior := bindingsByName[bindingName]; prior != nil {
			compare := model.Compare(uint64(spec.Authorization.WriterEpoch), uint64(spec.Authorization.Sequence), uint64(prior.Spec.Authorization.WriterEpoch), uint64(prior.Spec.Authorization.Sequence))
			if compare < 0 || (compare == 0 && (!spec.Authorization.ValidUntil.Equal(&prior.Spec.Authorization.ValidUntil) || prior.Spec.Tombstone)) {
				valid = false
			}
		}
		if _, err := netip.ParseAddr(spec.QueryIdentity.Value); err != nil || spec.QueryIdentity.Type != dnsv1alpha1.DNSResolverQueryIdentityDestinationAddress || spec.Port < 1 {
			valid = false
		}
		if _, ok := r.targetForRegion(spec.Region); !ok || spec.Port != 53 || !validAccessTransports(spec.Transports) {
			valid = false
		}
		key := spec.Region + "\x00" + spec.QueryIdentity.Value + "\x00" + fmt.Sprint(spec.Port)
		if owner := destinations[key]; owner != "" && owner != a.UID {
			valid = false
		} else if valid {
			if r.Options.AddressAllocator == nil || r.Options.AddressAllocator.ClaimDestination(ctx, r.Options.ProjectUID, a.UID, spec.Region, netip.MustParseAddr(spec.QueryIdentity.Value).String(), spec.Port) != nil {
				valid = false
			} else {
				destinations[key] = a.UID
			}
		}
		status, reason, msg := metav1.ConditionTrue, dnsValueAccepted, "access binding is current"
		if !valid {
			status, reason, msg = metav1.ConditionFalse, "InvalidOrStaleAccess", "context lifetime, epoch, sequence, destination, and bounded deadline must be current"
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
		if exists {
			cluster = old.Spec.Configuration.Listeners.Regional.Address
			generation = old.Spec.Configuration.Generation
			revision = old.Spec.Configuration.Revision
			if !zoneUIDsEqual(old.Spec.Configuration.ZoneRefs, zids) {
				generation++
				revision++
			} else if old.Spec.Tombstone || old.Spec.Authorization.WriterEpoch != spec.Authorization.WriterEpoch || old.Spec.Authorization.Sequence != spec.Authorization.Sequence {
				revision++
			}
		} else {
			if r.Options.AddressAllocator == nil {
				return errors.New("address allocator is required for context binding")
			}
			_, cluster, err = r.Options.AddressAllocator.Allocate(ctx, r.Options.ProjectUID, contextObject.UID)
			if err != nil {
				return err
			}
		}
		binding := &dnsv1alpha1.DNSResolverBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Options.PlatformNamespace, Labels: map[string]string{"internal-dns.miloapis.com/context-uid": string(contextObject.UID), "internal-dns.miloapis.com/access-uid": string(a.UID), "internal-dns.miloapis.com/region": model.SafeToken(spec.Region), "internal-dns.miloapis.com/shard": model.SafeToken(r.shardForRegion(spec.Region))}}, Spec: dnsv1alpha1.DNSResolverBindingSpec{Source: dnsv1alpha1.DNSResolverBindingSource{ProjectUID: r.Options.ProjectUID, ResolverContextRef: dnsv1alpha1.DNSObjectReference{Name: contextObject.Name, UID: contextObject.UID}, AccessBindingRef: dnsv1alpha1.DNSObjectReference{Name: a.Name, UID: a.UID}}, Placement: dnsv1alpha1.DNSResolverBindingPlacement{Region: spec.Region, Shard: r.shardForRegion(spec.Region)}, Configuration: dnsv1alpha1.DNSResolverBindingConfiguration{Generation: generation, Revision: revision, Listeners: dnsv1alpha1.DNSResolverBindingListeners{Node: dnsv1alpha1.DNSResolverBindingListener{Address: spec.QueryIdentity.Value, Port: spec.Port, Transports: append([]string(nil), spec.Transports...)}, Regional: dnsv1alpha1.DNSResolverBindingListener{Address: cluster, Port: spec.Port, Transports: append([]string(nil), spec.Transports...)}}, ZoneRefs: zoneReferences(zids, zones)}, Authorization: dnsv1alpha1.DNSResolverAccessAuthorization{WriterEpoch: spec.Authorization.WriterEpoch, Sequence: spec.Authorization.Sequence, ValidUntil: spec.Authorization.ValidUntil}}}
		if !exists {
			if err := r.Create(ctx, binding); err != nil {
				return err
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(binding), binding); err != nil {
				return err
			}
		} else if !bindingEqual(old.Spec, binding.Spec) {
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
		a.Status.ObservedSequence = spec.Authorization.Sequence
		a.Status.BindingRef = &dnsv1alpha1.DNSObjectReference{Name: binding.Name, UID: binding.UID}
		setCondition(&a.Status.Conditions, "Ready", metav1.ConditionFalse, "PendingReplicaVerification", "serving fleet acknowledgement is pending", a.Generation, now)
		if c := apimeta.FindStatusCondition(binding.Status.Conditions, "Serving"); c != nil && c.Status == metav1.ConditionTrue && binding.Status.ObservedConfigurationRevision == binding.Spec.Configuration.Revision && bindingAckCurrent(binding, now) {
			setCondition(&a.Status.Conditions, "Ready", metav1.ConditionTrue, "Ready", "required serving replicas applied this access generation", a.Generation, now)
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
		withdraw, err := r.confirmAccessWithdrawal(ctx, ns, b, now)
		if err != nil {
			return err
		}
		if !withdraw {
			continue
		}
		base := b.DeepCopy()
		b.Spec.Tombstone = true
		b.Spec.Configuration.Revision++
		// Authorization belongs to the trusted integration. Preserve its fence and
		// deadline; withdrawal is ordered independently by configuration revision.
		if err := r.Patch(ctx, b, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
	}
	return nil
}

// confirmAccessWithdrawal prevents a stale or incomplete source List from
// withdrawing a binding renewed by another controller. Read uncertainty leaves
// the existing authorization intact; serving agents still enforce its deadline.
func (r *Reconciler) confirmAccessWithdrawal(ctx context.Context, namespace string, binding *dnsv1alpha1.DNSResolverBinding, now time.Time) (bool, error) {
	var access dnsv1alpha1.DNSResolverAccessBinding
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: binding.Spec.Source.AccessBindingRef.Name}, &access); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if access.UID != binding.Spec.Source.AccessBindingRef.UID || !access.DeletionTimestamp.IsZero() {
		return true, nil
	}
	authorization := access.Spec.Authorization
	prior := binding.Spec.Authorization
	compare := model.Compare(uint64(authorization.WriterEpoch), uint64(authorization.Sequence), uint64(prior.WriterEpoch), uint64(prior.Sequence))
	if compare < 0 || (compare == 0 && !authorization.ValidUntil.Equal(&prior.ValidUntil)) {
		return false, nil
	}
	if access.Spec.ContextRef.Name != binding.Spec.Source.ResolverContextRef.Name || access.Spec.ContextRef.UID != binding.Spec.Source.ResolverContextRef.UID {
		return true, nil
	}
	var resolverContext dnsv1alpha1.DNSResolverContext
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: binding.Spec.Source.ResolverContextRef.Name}, &resolverContext); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if resolverContext.UID != binding.Spec.Source.ResolverContextRef.UID || !resolverContext.DeletionTimestamp.IsZero() {
		return true, nil
	}
	ready := apimeta.FindStatusCondition(resolverContext.Status.Conditions, "Ready")
	if !authorization.ValidUntil.After(now) || resolverContext.Status.AccessWriterEpoch > authorization.WriterEpoch {
		return true, nil
	}
	// Readiness or epoch propagation can lag a healthy authorization. Such
	// uncertainty is not retirement evidence and must not consume its lease.
	if ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != resolverContext.Generation || resolverContext.Status.AccessWriterEpoch < authorization.WriterEpoch {
		return false, nil
	}
	valid := access.Spec.Region == binding.Spec.Placement.Region && access.Spec.QueryIdentity.Type == dnsv1alpha1.DNSResolverQueryIdentityDestinationAddress && access.Spec.QueryIdentity.Value == binding.Spec.Configuration.Listeners.Node.Address && access.Spec.Port == binding.Spec.Configuration.Listeners.Node.Port && validAccessTransports(access.Spec.Transports)
	return !valid, nil
}

func (r *Reconciler) shardForRegion(region string) string {
	t, _ := r.targetForRegion(region)
	return t.Shard
}

func (r *Reconciler) targetForRegion(region string) (ServingRegion, bool) {
	for _, s := range r.Options.servingRegions() {
		if s.Region == region {
			return s, true
		}
	}
	return ServingRegion{}, false
}

func validAccessTransports(values []string) bool {
	if len(values) != 2 {
		return false
	}
	seen := map[string]bool{}
	for _, v := range values {
		if (v != "UDP" && v != "TCP") || seen[v] {
			return false
		}
		seen[v] = true
	}
	return seen["UDP"] && seen["TCP"]
}

func bindingAckCurrent(binding *dnsv1alpha1.DNSResolverBinding, now time.Time) bool {
	for _, ack := range binding.Status.MemberAcknowledgements {
		// Serving=True is the aggregate full-member gate computed by AckSink.
		// This row check additionally prevents a stale condition from surviving
		// past the exact configuration/authorization generation or its local ACK
		// lease. An ACK lease may be shorter than the access deadline, but never
		// longer because AckSink caps it before persisting the row.
		if ack.Phase == string(model.AckVerified) && ack.Revision == binding.Spec.Configuration.Revision && ack.AuthorizationIssuerEpoch == binding.Spec.Authorization.WriterEpoch && ack.AuthorizationRevision == binding.Spec.Authorization.Sequence && ack.ValidUntil != nil && ack.ValidUntil.After(now) && !ack.ValidUntil.After(binding.Spec.Authorization.ValidUntil.Time) {
			return true
		}
	}
	return false
}

// selectServingTargets assigns one shared shard in each region. An existing
// live assignment wins while its shard remains configured, so adding capacity
// does not silently move established VPC listeners. Explicit migration is a
// separate operation; only a missing/removed assignment invokes rendezvous.
func (r *Reconciler) selectServingTargets(vpc types.UID, existing []dnsv1alpha1.DNSResolverBinding) []ServingRegion {
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
			if binding.Spec.Source.ProjectUID != r.Options.ProjectUID || binding.Spec.Source.ResolverContextRef.UID != vpc || binding.Spec.Placement.Region != region || binding.Spec.Tombstone || !binding.DeletionTimestamp.IsZero() {
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
			sum := sha256.Sum256([]byte(string(r.Options.ProjectUID) + "/" + string(vpc) + "/" + region + "/" + candidate.Shard))
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

func (r *Reconciler) patchCondition(ctx context.Context, m *dnsv1alpha1.DNSManagedNamespace, t string, status metav1.ConditionStatus, reason, msg string, now time.Time) error {
	base := m.DeepCopy()
	if !setCondition(&m.Status.Conditions, t, status, reason, msg, m.Generation, now) {
		return nil
	}
	return r.Status().Patch(ctx, m, client.MergeFrom(base))
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
	x.Authorization.ValidUntil = metav1.Time{}
	y.Authorization.ValidUntil = metav1.Time{}
	xb, _ := json.Marshal(x)
	yb, _ := json.Marshal(y)
	return string(xb) == string(yb)
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
