// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"errors"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const defaultManagedDomainSuffix = "datum.internal"

type ReconcilerOptions struct {
	ProjectUID           types.UID
	SourceClusterUID     string
	ProjectNamespace     string
	PlatformNamespace    string
	Region               string
	Shard                string
	Identity             string
	ChunkSize            int
	LeaseDuration        time.Duration
	Now                  func() time.Time
	AddressAllocator     AddressAllocator
	ManagedDomainSuffix  string
	PrivateZoneClassName string
	ServingRegions       []ServingRegion
	MaxAccessLease       time.Duration
}

type ServingRegion struct {
	Region string
	Shard  string
}

// AddressAllocator durably leases globally unique listener destinations. Its
// implementation must retain quarantine state after release; a pure hash is
// only a candidate and is never sufficient proof of uniqueness.
type AddressAllocator interface {
	Allocate(context.Context, types.UID, types.UID) (consumer, cluster string, err error)
	ClaimDestination(context.Context, types.UID, types.UID, string, string, int32) error
}

func (o *ReconcilerOptions) defaults() {
	if o.ManagedDomainSuffix == "" {
		o.ManagedDomainSuffix = defaultManagedDomainSuffix
	}
	if o.PlatformNamespace == "" {
		o.PlatformNamespace = "internal-dns-system"
	}
	if o.Region == "" {
		o.Region = "default"
	}
	if o.Shard == "" {
		o.Shard = "private-dns-01"
	}
	if o.Identity == "" {
		o.Identity = "internal-dns-controller"
	}
	if o.ChunkSize <= 0 {
		o.ChunkSize = 256 * 1024
	}
	if o.LeaseDuration <= 0 {
		o.LeaseDuration = 30 * time.Second
	}

	if o.MaxAccessLease <= 0 {
		o.MaxAccessLease = 5 * time.Minute
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
}

func (o ReconcilerOptions) servingRegions() []ServingRegion {
	if len(o.ServingRegions) > 0 {
		return o.ServingRegions
	}
	return []ServingRegion{{Region: o.Region, Shard: o.Shard}}
}

type Options = ReconcilerOptions

// Reconciler compiles one project's desired DNS state. ProjectUID and
// SourceClusterUID are trusted configuration supplied by the replication
// bridge; Kubernetes namespace identity is never treated as project identity.
type Reconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Options ReconcilerOptions
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Options.defaults()
	if r.Options.ProjectUID == "" || r.Options.SourceClusterUID == "" {
		return ctrl.Result{}, errors.New("projectUID and sourceClusterUID are required")
	}
	ns := r.Options.ProjectNamespace
	if ns == "" {
		ns = req.Namespace
	}
	if ns == "" || ns == r.Options.PlatformNamespace {
		return ctrl.Result{}, nil
	}
	return r.reconcileProject(ctx, ns)
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Options.defaults()
	enqueue := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: "project"}}}
	})
	controllerName := "internal-dns-control-plane-" + model.OpaqueToken(string(r.Options.ProjectUID))[:12]
	b := ctrl.NewControllerManagedBy(mgr).Named(controllerName).For(&dnsv1alpha1.DNSRecordContribution{}, builder.WithPredicates()).
		Watches(&dnsv1alpha1.DNSZone{}, enqueue).Watches(&dnsv1alpha1.DNSZoneAssociation{}, enqueue).
		Watches(&dnsv1alpha1.DNSRegistration{}, enqueue).Watches(&dnsv1alpha1.DNSContributionGrant{}, enqueue).
		Watches(&dnsv1alpha1.DNSRecordSet{}, enqueue).Watches(&dnsv1alpha1.DNSNamingPolicy{}, enqueue).
		Watches(&dnsv1alpha1.DNSManagedNamespace{}, enqueue)

	return b.Watches(&dnsv1alpha1.DNSResolverContext{}, enqueue).Watches(&dnsv1alpha1.DNSResolverAccessBinding{}, enqueue).Complete(r)

}
