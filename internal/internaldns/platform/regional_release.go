// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns/api/v1alpha1"
	"go.miloapis.com/dns/internal/internaldns/model"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	regionalClaimPrefix = "dns-address-claim-"
	regionalLeasePrefix = "dns-address-owner-"
	labelTrue           = "true"
	claimAddressKey     = "address"
	claimOwnerKey       = "owner"
)

// RegionalRelease counts one sweep. Released counts what a dry run would
// free. Conflicts counts routed addresses whose claim names another owner.
type RegionalRelease struct {
	InUse, Released, Restored, Conflicts int
}

type addressIndex struct {
	bindings map[string][]*dnsv1alpha1.DNSResolverBinding
	shards   map[string]map[string]bool
	shared   []string
}

func (a *Allocator) indexBindings(bindings []dnsv1alpha1.DNSResolverBinding) addressIndex {
	index := addressIndex{bindings: map[string][]*dnsv1alpha1.DNSResolverBinding{}, shards: map[string]map[string]bool{}}
	for i := range bindings {
		b := &bindings[i]
		region := b.Spec.Placement.Region
		if index.shards[region] == nil {
			index.shards[region] = map[string]bool{}
			if _, own := a.RegionalPrefixes[region]; !own {
				index.shared = append(index.shared, region)
			}
		}
		index.shards[region][b.Spec.Placement.Shard] = true
		if address, ok := normalAddress(b.Spec.Configuration.Query.RegionalAddress); ok {
			index.bindings[address] = append(index.bindings[address], b)
		}
	}
	return index
}

// errSnapshotUnavailable marks a shard whose snapshot failed to load earlier
// in the sweep; its error is reported once.
var errSnapshotUnavailable = errors.New("serving snapshot unavailable")

type snapshotCache struct {
	snapshots map[string]*model.ServingSnapshot
	failed    map[string]error
}

// ReleaseRegionalAddresses frees regional claims and owner leases by the
// ReleaseDestinations rule: no binding routes the address and no committed
// serving snapshot carries it. Claims are matched by owner data, not project,
// because older claims carry no project label and some projects no longer
// run. Anything allocated within grace may still be awaiting its binding;
// Allocate restamps a lease it reuses, so its versioned delete also fails. A
// live binding whose claim is missing gets it back. With dryRun nothing is
// written.
func (a *Allocator) ReleaseRegionalAddresses(ctx context.Context, now time.Time, grace time.Duration, dryRun bool) (RegionalRelease, error) {
	var result RegionalRelease
	if a.Client == nil || a.Namespace == "" {
		return result, errors.New("regional address release requires a client and namespace")
	}
	var maps corev1.ConfigMapList
	if err := a.Client.List(ctx, &maps, client.InNamespace(a.Namespace), client.MatchingLabels{leaseLabel: labelTrue}); err != nil {
		return result, err
	}
	var bindings dnsv1alpha1.DNSResolverBindingList
	if err := a.Client.List(ctx, &bindings, client.InNamespace(a.Namespace)); err != nil {
		return result, err
	}
	index := a.indexBindings(bindings.Items)
	claims := map[string]*corev1.ConfigMap{}
	leases := map[string]*corev1.ConfigMap{}
	for i := range maps.Items {
		cm := &maps.Items[i]
		switch {
		case strings.HasPrefix(cm.Name, regionalClaimPrefix) && cm.Data["region"] == "" && cm.Data[claimOwnerKey] != "":
			if address, ok := normalAddress(cm.Data[claimAddressKey]); ok {
				claims[address] = cm
			}
		case strings.HasPrefix(cm.Name, regionalLeasePrefix):
			leases[cm.Name] = cm
		}
	}
	cache := &snapshotCache{snapshots: map[string]*model.ServingSnapshot{}, failed: map[string]error{}}
	var errs error
	routed := func(owner, address string) (bool, bool) {
		ok, err := a.regionalRouted(ctx, index, cache, a.ownerRegions(owner, index), address)
		if err != nil {
			if !errors.Is(err, errSnapshotUnavailable) {
				errs = errors.Join(errs, err)
			}
			return false, false
		}
		return ok, true
	}

	for address, claim := range claims {
		owner := claim.Data[claimOwnerKey]
		if !a.releasesOwner(owner) {
			continue
		}
		inUse, known := routed(owner, address)
		if !known {
			continue
		}
		lease := leases[regionalLeasePrefix+model.OpaqueToken(owner)]
		if inUse || young(claim, now, grace) || lease != nil && young(lease, now, grace) {
			result.InUse++
			continue
		}
		if dryRun {
			result.Released++
			continue
		}
		// Lease first: an orphaned lease would hand out an unreserved
		// address, an orphaned claim is freed by the next sweep.
		if lease != nil && leaseAddress(lease) == address {
			if err := a.deleteClaim(ctx, lease); err != nil {
				errs = errors.Join(errs, err)
				continue
			}
			delete(leases, lease.Name)
		}
		if err := a.deleteClaim(ctx, claim); err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		delete(claims, address)
		result.Released++
	}

	for _, lease := range leases {
		address, owner := leaseAddress(lease), leaseOwner(lease)
		if address == "" || owner == "" || !a.releasesOwner(owner) {
			continue
		}
		if claim := claims[address]; claim != nil {
			if claim.Data[claimOwnerKey] != owner {
				result.Conflicts++
			}
			continue
		}
		inUse, known := routed(owner, address)
		if !known {
			continue
		}
		if inUse {
			if !dryRun {
				if err := a.restoreClaim(ctx, owner, address); err != nil {
					errs = errors.Join(errs, err)
					continue
				}
			}
			claims[address] = &corev1.ConfigMap{Data: map[string]string{claimOwnerKey: owner}}
			result.Restored++
			continue
		}
		if young(lease, now, grace) {
			continue
		}
		if !dryRun {
			if err := a.deleteClaim(ctx, lease); err != nil {
				errs = errors.Join(errs, err)
				continue
			}
		}
		result.Released++
	}

	for i := range bindings.Items {
		b := &bindings.Items[i]
		region := b.Spec.Placement.Region
		address, ok := normalAddress(b.Spec.Configuration.Query.RegionalAddress)
		if !ok || b.Spec.Tombstone || !b.DeletionTimestamp.IsZero() || !a.releasesRegion(region) || b.Spec.Source.ProjectUID == "" || b.Spec.Source.ResolverContextRef.UID == "" {
			continue
		}
		owner := string(b.Spec.Source.ProjectUID) + "/" + string(b.Spec.Source.ResolverContextRef.UID)
		// A region given its own pool later keeps its shared-pool addresses.
		if prefix, own := a.RegionalPrefixes[region]; own {
			if parsed, err := ParseRegionalPrefix(prefix); err == nil && parsed.Contains(netip.MustParseAddr(address)) {
				owner += "/" + region
			}
		}
		if !a.releasesOwner(owner) {
			continue
		}
		if claim := claims[address]; claim != nil {
			if claim.Data[claimOwnerKey] != owner {
				result.Conflicts++
			}
			continue
		}
		if !dryRun {
			if err := a.restoreClaim(ctx, owner, address); err != nil {
				errs = errors.Join(errs, err)
				continue
			}
		}
		claims[address] = &corev1.ConfigMap{Data: map[string]string{claimOwnerKey: owner}}
		result.Restored++
	}
	return result, errs
}

// releasesOwner limits a regional control plane to its regions' own pools. A
// shared-pool claim may be routed in any region on that pool, so only a
// control plane that publishes every region frees it.
func (a *Allocator) releasesOwner(owner string) bool {
	parts := strings.Split(owner, "/")
	switch len(parts) {
	case 2:
		return len(a.Regions) == 0
	case 3:
		return a.releasesRegion(parts[2])
	default:
		return false
	}
}

func (a *Allocator) releasesRegion(region string) bool {
	return region != "" && (len(a.Regions) == 0 || slices.Contains(a.Regions, region))
}

// ownerRegions lists the regions whose snapshots may carry the owner's address.
func (a *Allocator) ownerRegions(owner string, index addressIndex) []string {
	if parts := strings.Split(owner, "/"); len(parts) == 3 {
		return []string{parts[2]}
	}
	return index.shared
}

func (a *Allocator) regionalRouted(ctx context.Context, index addressIndex, cache *snapshotCache, regions []string, address string) (bool, error) {
	for _, b := range index.bindings[address] {
		if !b.Spec.Tombstone && b.DeletionTimestamp.IsZero() {
			return true, nil
		}
		fullyReplicated := apimeta.FindStatusCondition(b.Status.Conditions, "FullyReplicated")
		if b.Spec.Tombstone && (b.Status.ObservedConfigurationRevision != b.Spec.Configuration.Revision || b.Status.Phase != dnsValueWithdrawn || fullyReplicated == nil || fullyReplicated.Status != metav1.ConditionTrue || fullyReplicated.ObservedGeneration != b.Generation) {
			return true, nil
		}
	}
	for _, region := range regions {
		for shard := range index.shards[region] {
			key := region + "/" + shard
			if cache.failed[key] != nil {
				return false, errSnapshotUnavailable
			}
			snapshot, cached := cache.snapshots[key]
			if !cached {
				reader := a.Client
				if a.RegionClient != nil {
					reader = a.RegionClient(region)
				}
				var err error
				if _, snapshot, err = LoadCommittedServingSnapshot(ctx, reader, a.Namespace, region, shard); err != nil {
					cache.failed[key] = err
					return false, err
				}
				cache.snapshots[key] = snapshot
			}
			if snapshot == nil {
				continue
			}
			for _, b := range snapshot.Bindings {
				if !b.Tombstone && sameAddress(b.ClusterAddress, address) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func (a *Allocator) restoreClaim(ctx context.Context, owner, address string) error {
	claim := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: regionalClaimPrefix + model.OpaqueToken(address), Namespace: a.Namespace, Labels: map[string]string{leaseLabel: labelTrue}}, Data: map[string]string{claimOwnerKey: owner, claimAddressKey: address}}
	if err := a.Client.Create(ctx, claim); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func young(obj *corev1.ConfigMap, now time.Time, grace time.Duration) bool {
	created := obj.CreationTimestamp.Time
	if lease, err := decodeStoredLease(obj); err == nil && lease.CreatedAt.After(created) {
		created = lease.CreatedAt
	}
	if stamped, err := time.Parse(time.RFC3339, obj.Annotations[leaseAllocatedAnnotation]); err == nil && stamped.After(created) {
		created = stamped
	}
	return !created.IsZero() && now.Sub(created) < grace
}

func decodeStoredLease(obj *corev1.ConfigMap) (AddressLease, error) {
	var lease AddressLease
	raw, ok := obj.Data["lease"]
	if !ok {
		return lease, errors.New("not a lease")
	}
	err := json.Unmarshal([]byte(raw), &lease)
	return lease, err
}

func leaseAddress(obj *corev1.ConfigMap) string {
	lease, err := decodeStoredLease(obj)
	if err != nil {
		return ""
	}
	address, _ := normalAddress(lease.ClusterAddress)
	return address
}

func leaseOwner(obj *corev1.ConfigMap) string {
	lease, err := decodeStoredLease(obj)
	if err != nil || obj.Name != regionalLeasePrefix+model.OpaqueToken(lease.Owner) {
		return ""
	}
	return lease.Owner
}

func normalAddress(address string) (string, bool) {
	parsed, err := netip.ParseAddr(address)
	if err != nil {
		return "", false
	}
	return parsed.Unmap().String(), true
}
