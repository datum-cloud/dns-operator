// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/netip"
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

type AddressLease struct {
	Owner          string    `json:"owner"`
	ProjectUID     string    `json:"projectUID"`
	ContextUID     string    `json:"contextUID"`
	ClusterAddress string    `json:"clusterAddress"`
	CreatedAt      time.Time `json:"createdAt"`
}

// Allocator reserves all generated addresses through atomic, globally named Kubernetes address claims.
// Each owner has its own lease, avoiding a fleet-wide size limit or write lock. Leases are retained after revocation: addresses are never reused by
// another resolver context lifetime. Explicit garbage collection needs a quarantine policy.
type Allocator struct {
	Client        client.Client
	Namespace     string
	ClusterPrefix string
}

func (a *Allocator) Allocate(ctx context.Context, projectUID, contextUID types.UID) (string, error) {
	lease, err := a.reserve(ctx, string(projectUID), string(contextUID))
	return lease.ClusterAddress, err
}

// ClaimDestination atomically reserves an integration-selected service
// destination across every project sharing this platform control plane. Claims
// are retained after revocation so a stale route cannot become valid for a new
// context lifetime.
func (a *Allocator) ClaimDestination(ctx context.Context, projectUID, contextUID types.UID, region, address string, port int32) error {
	parsed, err := netip.ParseAddr(address)
	if err != nil || !parsed.IsGlobalUnicast() || projectUID == "" || contextUID == "" || region == "" || port != 53 {
		return fmt.Errorf("%w: destination claim identity, region, IP, and DNS port are required", model.ErrDestinationRefused)
	}
	parsed = parsed.Unmap()
	prefix, err := a.regionalPrefix()
	if err != nil {
		return err
	}
	if prefix.Contains(parsed) {
		return fmt.Errorf("%w: service destination must be outside the regional resolver address pool", model.ErrDestinationRefused)
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
	claim := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "dns-destination-claim-" + model.OpaqueToken(region+"/"+address+"/"+fmt.Sprint(port)), Namespace: a.Namespace, Labels: map[string]string{leaseLabel: "true"}}, Data: map[string]string{"owner": owner, "region": region, "address": address, "port": fmt.Sprint(port)}}
	if err := a.Client.Create(ctx, claim); apierrors.IsAlreadyExists(err) {
		var existing corev1.ConfigMap
		if err := a.Client.Get(ctx, client.ObjectKeyFromObject(claim), &existing); err != nil {
			return err
		}
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
					return a.Client.Patch(ctx, &existing, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
				}
			}
			return fmt.Errorf("%w: service destination is already reserved", model.ErrDestinationRefused)
		}
		return nil
	} else {
		return err
	}
}

func (a *Allocator) reserve(ctx context.Context, projectUID, contextUID string) (AddressLease, error) {
	if a.Client == nil || a.Namespace == "" || projectUID == "" || contextUID == "" {
		return AddressLease{}, fmt.Errorf("allocator requires a client, namespace, and immutable project/context identities")
	}
	owner := projectUID + "/" + contextUID

	parsed, err := a.regionalPrefix()
	if err != nil {
		return AddressLease{}, err
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
			var existing corev1.ConfigMap
			if err := a.Client.Get(ctx, client.ObjectKeyFromObject(claim), &existing); err != nil {
				return AddressLease{}, err
			}
			if existing.Data["owner"] != owner || existing.Data["address"] != candidate.ClusterAddress {
				continue
			}
		} else if err != nil {
			return AddressLease{}, err
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

func (a *Allocator) regionalPrefix() (netip.Prefix, error) {
	prefix := a.ClusterPrefix
	if prefix == "" {
		prefix = "fd54::/64"
	}
	parsed, err := netip.ParsePrefix(prefix)
	if err != nil || !parsed.Addr().Is6() || parsed.Bits() > 112 {
		return netip.Prefix{}, fmt.Errorf("allocator prefix must be IPv6 with at least 16 host bits: %q", prefix)
	}
	return parsed.Masked(), nil
}
