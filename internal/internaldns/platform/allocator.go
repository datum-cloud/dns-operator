// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const leaseLabel = "internal-dns.miloapis.com/address-lease"
const destinationProjectLabel = "internal-dns.miloapis.com/destination-project"

type AddressLease struct {
	Owner          string    `json:"owner"`
	ProjectUID     string    `json:"projectUID"`
	ContextUID     string    `json:"contextUID"`
	ClusterAddress string    `json:"clusterAddress"`
	CreatedAt      time.Time `json:"createdAt"`
}

// Allocator reserves all generated addresses through atomic, globally named Kubernetes address claims.
// Each owner has its own lease, avoiding a fleet-wide size limit or write lock. Regional leases are retained after revocation: those addresses
// are never reused by another resolver context lifetime. Service destination claims are released once no binding routes them.
type Allocator struct {
	Client        client.Client
	Namespace     string
	ClusterPrefix string
	// RegionalPrefixes gives a region its own regional resolver pool, routed to
	// that region's regional router. Regions without one share ClusterPrefix.
	RegionalPrefixes map[string]string
	// RegionClient, when set, is the client a region's serving snapshot is
	// read with, so one region's broker never holds up another's.
	RegionClient func(region string) client.Client
}

// Allocate leases a context's regional address in a region. A region on the
// shared pool keeps the context's one lease across regions, as before regions
// had their own pools; a region with its own pool leases separately.
func (a *Allocator) Allocate(ctx context.Context, projectUID, contextUID types.UID, region string) (string, error) {
	prefix, err := a.prefixFor(region)
	if err != nil {
		return "", err
	}
	owner := string(projectUID) + "/" + string(contextUID)
	_, own := a.RegionalPrefixes[region]
	if own {
		owner += "/" + region
	}
	lease, err := a.reserve(ctx, string(projectUID), string(contextUID), owner, prefix)
	if err != nil {
		return "", err
	}
	// A region's pool is fixed once leased from: a lease outside it means the
	// pool was renumbered, and its regional router no longer routes the lease.
	if own {
		if address, parseErr := netip.ParseAddr(lease.ClusterAddress); parseErr != nil || !prefix.Contains(address) {
			return "", fmt.Errorf("region %q lease %s is outside its regional pool %s; regional pools cannot be renumbered", region, lease.ClusterAddress, prefix)
		}
	}
	return lease.ClusterAddress, nil
}

// ClaimDestination atomically reserves an integration-selected service
// destination across every project sharing this platform control plane.
func (a *Allocator) ClaimDestination(ctx context.Context, projectUID, contextUID types.UID, region, address string, port int32) error {
	parsed, err := netip.ParseAddr(address)
	if err != nil || parsed.Zone() != "" || !parsed.IsGlobalUnicast() || projectUID == "" || contextUID == "" || region == "" || port != 53 {
		return fmt.Errorf("%w: destination claim identity, region, IP, and DNS port are required", model.ErrDestinationRefused)
	}
	parsed = parsed.Unmap()
	pools, err := a.pools()
	if err != nil {
		return err
	}
	for _, prefix := range pools {
		if prefix.Contains(parsed) {
			return fmt.Errorf("%w: service destination must be outside the regional resolver address pools", model.ErrDestinationRefused)
		}
	}
	address = parsed.String()
	var regionalClaim corev1.ConfigMap
	claimKey := client.ObjectKey{Namespace: a.Namespace, Name: "dns-address-claim-" + model.OpaqueToken(address)}
	if err := a.Client.Get(ctx, claimKey, &regionalClaim); err == nil {
		return fmt.Errorf("%w: service destination is reserved for regional resolution", model.ErrDestinationRefused)
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	owner := string(projectUID) + "/" + string(contextUID)
	claim := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "dns-destination-claim-" + model.OpaqueToken(region+"/"+address+"/"+fmt.Sprint(port)), Namespace: a.Namespace, Labels: map[string]string{leaseLabel: "true", destinationProjectLabel: model.OpaqueToken(string(projectUID))}}, Data: map[string]string{"owner": owner, "region": region, "address": address, "port": fmt.Sprint(port)}}
	// Every project pass claims its destinations again; reading first keeps an
	// existing claim from costing a rejected create.
	var existing corev1.ConfigMap
	switch err = a.Client.Get(ctx, client.ObjectKeyFromObject(claim), &existing); {
	case err == nil:
		err = apierrors.NewAlreadyExists(corev1.Resource("configmaps"), claim.Name)
	case apierrors.IsNotFound(err):
		err = a.Client.Create(ctx, claim)
		if apierrors.IsAlreadyExists(err) {
			if err := a.Client.Get(ctx, client.ObjectKeyFromObject(claim), &existing); err != nil {
				return err
			}
		}
	default:
		return err
	}
	if err == nil {
		if err := a.Client.Get(ctx, claimKey, &regionalClaim); err == nil {
			if err := a.deleteClaim(ctx, claim); err != nil {
				return err
			}
			return fmt.Errorf("%w: service destination is reserved for regional resolution", model.ErrDestinationRefused)
		} else if !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}
	if apierrors.IsAlreadyExists(err) {
		if existing.Data["owner"] != owner || existing.Data["region"] != region || existing.Data["address"] != address || existing.Data["port"] != fmt.Sprint(port) {
			var bindings dnsv1alpha1.DNSResolverBindingList
			if err := a.Client.List(ctx, &bindings, client.InNamespace(a.Namespace)); err != nil {
				return err
			}
			for _, binding := range bindings.Items {
				b := binding.Spec
				if b.Source.ProjectUID == projectUID && b.Source.ResolverContextRef.UID == contextUID &&
					b.Source.AccessBindingRef.UID != "" && existing.Data["owner"] == string(projectUID)+"/"+string(b.Source.AccessBindingRef.UID) &&
					b.Placement.Region == region && b.Configuration.Query.NodeAddress == address && port == 53 &&
					existing.Data["region"] == region && existing.Data["address"] == address && existing.Data["port"] == fmt.Sprint(port) {
					base := existing.DeepCopy()
					existing.Data["owner"] = owner
					existing.Labels = claim.Labels
					return a.Client.Patch(ctx, &existing, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
				}
			}
			return fmt.Errorf("%w: service destination is already reserved", model.ErrDestinationRefused)
		}
		if existing.Labels[destinationProjectLabel] != claim.Labels[destinationProjectLabel] {
			base := existing.DeepCopy()
			existing.Labels = claim.Labels
			return a.Client.Patch(ctx, &existing, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		}
		return nil
	}
	return err
}

// ReleaseDestinations deletes the project's destination claims that are not in
// held and that no binding still routes. A destination stays routed while a
// live binding names it or while a committed serving snapshot for its region
// still carries it, so a claim is never freed before its route is withdrawn.
func (a *Allocator) ReleaseDestinations(ctx context.Context, projectUID types.UID, held map[string]bool) error {
	if a.Client == nil || a.Namespace == "" || projectUID == "" {
		return fmt.Errorf("destination release requires a client, namespace, and project identity")
	}
	var claims corev1.ConfigMapList
	if err := a.Client.List(ctx, &claims, client.InNamespace(a.Namespace), client.MatchingLabels{leaseLabel: "true", destinationProjectLabel: model.OpaqueToken(string(projectUID))}); err != nil {
		return err
	}
	var bindings *dnsv1alpha1.DNSResolverBindingList
	snapshots := map[string]*model.ServingSnapshot{}
	for i := range claims.Items {
		claim := &claims.Items[i]
		region, address := claim.Data["region"], claim.Data["address"]
		if region == "" || !strings.HasPrefix(claim.Data["owner"], string(projectUID)+"/") || held[model.DestinationKey(region, address)] {
			continue
		}
		if bindings == nil {
			bindings = &dnsv1alpha1.DNSResolverBindingList{}
			if err := a.Client.List(ctx, bindings, client.InNamespace(a.Namespace)); err != nil {
				return err
			}
		}
		if routed, err := a.destinationRouted(ctx, bindings.Items, snapshots, region, address); err != nil || routed {
			continue
		}
		if err := a.deleteClaim(ctx, claim); err != nil {
			return err
		}
	}
	return nil
}

func (a *Allocator) destinationRouted(ctx context.Context, bindings []dnsv1alpha1.DNSResolverBinding, snapshots map[string]*model.ServingSnapshot, region, address string) (bool, error) {
	shards := map[string]bool{}
	for _, b := range bindings {
		if b.Spec.Placement.Region != region {
			continue
		}
		shards[b.Spec.Placement.Shard] = true
		if !b.Spec.Tombstone && b.DeletionTimestamp.IsZero() && sameAddress(b.Spec.Configuration.Query.NodeAddress, address) {
			return true, nil
		}
	}
	for shard := range shards {
		key := region + "/" + shard
		snapshot, cached := snapshots[key]
		if !cached {
			var err error
			reader := a.Client
			if a.RegionClient != nil {
				reader = a.RegionClient(region)
			}
			if _, snapshot, err = LoadCommittedServingSnapshot(ctx, reader, a.Namespace, region, shard); err != nil {
				return false, err
			}
			snapshots[key] = snapshot
		}
		if snapshot == nil {
			continue
		}
		for _, b := range snapshot.Bindings {
			if sameAddress(b.ConsumerAddress, address) {
				return true, nil
			}
		}
	}
	return false, nil
}

func sameAddress(a, b string) bool {
	left, err := netip.ParseAddr(a)
	if err != nil {
		return a == b
	}
	right, err := netip.ParseAddr(b)
	return err == nil && left.Unmap() == right.Unmap()
}

func (a *Allocator) destinationClaimed(ctx context.Context, address string) (bool, error) {
	var claims corev1.ConfigMapList
	if err := a.Client.List(ctx, &claims, client.InNamespace(a.Namespace), client.MatchingLabels{leaseLabel: "true"}); err != nil {
		return false, err
	}
	for _, claim := range claims.Items {
		if claim.Data["region"] != "" && claim.Data["address"] == address {
			return true, nil
		}
	}
	return false, nil
}

func (a *Allocator) deleteClaim(ctx context.Context, claim *corev1.ConfigMap) error {
	uid, version := claim.UID, claim.ResourceVersion
	err := a.Client.Delete(ctx, claim, client.Preconditions{UID: &uid, ResourceVersion: &version})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (a *Allocator) reserve(ctx context.Context, projectUID, contextUID, owner string, parsed netip.Prefix) (AddressLease, error) {
	if a.Client == nil || a.Namespace == "" || projectUID == "" || contextUID == "" {
		return AddressLease{}, fmt.Errorf("allocator requires a client, namespace, and immutable project/context identities")
	}
	key := client.ObjectKey{Namespace: a.Namespace, Name: "dns-address-owner-" + model.OpaqueToken(owner)}
	var stored corev1.ConfigMap
	if err := a.Client.Get(ctx, key, &stored); err == nil {
		return decodeLease(stored, owner)
	} else if !apierrors.IsNotFound(err) {
		return AddressLease{}, err
	}
	for salt := 0; salt < 1024; salt++ {
		candidate := AddressLease{Owner: owner, ProjectUID: projectUID, ContextUID: contextUID, CreatedAt: time.Now().UTC()}

		candidate.ClusterAddress = addressInPrefix(parsed, owner+"/cluster", salt)
		claim := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "dns-address-claim-" + model.OpaqueToken(candidate.ClusterAddress), Namespace: a.Namespace, Labels: map[string]string{leaseLabel: "true"}}, Data: map[string]string{"owner": owner, "address": candidate.ClusterAddress}}
		if err := a.Client.Create(ctx, claim); apierrors.IsAlreadyExists(err) {
			if err := a.Client.Get(ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
				return AddressLease{}, err
			}
			if claim.Data["owner"] != owner || claim.Data["address"] != candidate.ClusterAddress {
				continue
			}
		} else if err != nil {
			return AddressLease{}, err
		}
		// Destination claims are checked after the regional claim exists, and
		// ClaimDestination checks regional claims after its own exists, so two
		// racing claimants cannot both miss each other.
		claimed, err := a.destinationClaimed(ctx, candidate.ClusterAddress)
		if err != nil {
			return AddressLease{}, err
		}
		if claimed {
			if err := a.deleteClaim(ctx, claim); err != nil {
				return AddressLease{}, err
			}
			continue
		}
		b, err := json.Marshal(candidate)
		if err != nil {
			return AddressLease{}, err
		}
		lease := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: map[string]string{leaseLabel: "true"}}, Data: map[string]string{"lease": string(b)}}
		if err := a.Client.Create(ctx, lease); apierrors.IsAlreadyExists(err) {
			if err := a.Client.Get(ctx, key, &stored); err != nil {
				return AddressLease{}, err
			}
			return decodeLease(stored, owner)
		} else if err != nil {
			return AddressLease{}, err
		}
		return candidate, nil
	}
	return AddressLease{}, fmt.Errorf("address collision budget exhausted")
}

func decodeLease(stored corev1.ConfigMap, owner string) (AddressLease, error) {
	var lease AddressLease
	if err := json.Unmarshal([]byte(stored.Data["lease"]), &lease); err != nil {
		return lease, fmt.Errorf("corrupt address lease: %w", err)
	}
	if lease.Owner != owner || lease.ProjectUID == "" || lease.ContextUID == "" {
		return lease, fmt.Errorf("address lease identity mismatch")
	}
	return lease, nil
}

func addressInPrefix(prefix netip.Prefix, owner string, salt int) string {
	base := prefix.Addr().As16()
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", owner, salt)))
	for bit := prefix.Bits(); bit < 128; bit++ {
		i, mask := bit/8, byte(1<<uint(7-bit%8))
		if hash[i]&mask != 0 {
			base[i] |= mask
		} else {
			base[i] &= ^mask
		}
	}
	base[15] |= 1 // Never allocate the subnet's all-zero host address.
	return netip.AddrFrom16(base).String()
}

func (a *Allocator) prefixFor(region string) (netip.Prefix, error) {
	if prefix, ok := a.RegionalPrefixes[region]; ok {
		return ParseRegionalPrefix(prefix)
	}
	return ParseRegionalPrefix(a.ClusterPrefix)
}

// pools lists every regional resolver pool, which service destinations must
// stay out of.
func (a *Allocator) pools() ([]netip.Prefix, error) {
	shared, err := ParseRegionalPrefix(a.ClusterPrefix)
	if err != nil {
		return nil, err
	}
	result := []netip.Prefix{shared}
	for _, prefix := range a.RegionalPrefixes {
		parsed, err := ParseRegionalPrefix(prefix)
		if err != nil {
			return nil, err
		}
		result = append(result, parsed)
	}
	return result, nil
}

// ParseRegionalPrefix validates a regional resolver pool. An empty prefix is
// the default shared pool.
func ParseRegionalPrefix(prefix string) (netip.Prefix, error) {
	if prefix == "" {
		prefix = "fd54::/64"
	}
	parsed, err := netip.ParsePrefix(prefix)
	if err != nil || !parsed.Addr().Is6() || parsed.Bits() > 112 {
		return netip.Prefix{}, fmt.Errorf("allocator prefix must be IPv6 with at least 16 host bits: %q", prefix)
	}
	return parsed.Masked(), nil
}
