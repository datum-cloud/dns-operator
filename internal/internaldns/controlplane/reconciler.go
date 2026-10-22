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
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type ReconcilerOptions struct {
	ProjectUID            types.UID
	VerifyProjectIdentity func(context.Context) error
	ProjectNamespace      string
	PlatformNamespace     string
	Region                string
	Shard                 string
	Identity              string
	ChunkSize             int
	LeaseDuration         time.Duration
	Now                   func() time.Time
	AddressAllocator      AddressAllocator
	PrivateZoneClassName  string
	ServingRegions        []ServingRegion
}

type ServingRegion struct {
	Region string
	Shard  string
}

// AddressAllocator durably leases globally unique listener destinations. Its
// implementation must retain quarantine state after release; a pure hash is
// only a candidate and is never sufficient proof of uniqueness.
type AddressAllocator interface {
	Allocate(context.Context, types.UID, types.UID) (cluster string, err error)
	ClaimDestination(context.Context, types.UID, types.UID, string, string, int32) error
}

func (o *ReconcilerOptions) defaults() {
	if o.PlatformNamespace == "" {
		o.PlatformNamespace = defaultPlatformNamespace
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

// Reconciler compiles records within a configured project scope.
type Reconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Options   ReconcilerOptions
}

func (r *Reconciler) sourceReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.Options.defaults()
	if r.Options.ProjectUID == "" {
		return ctrl.Result{}, errors.New("projectUID is required")
	}
	if r.Options.VerifyProjectIdentity != nil {
		if err := r.Options.VerifyProjectIdentity(ctx); err != nil {
			return ctrl.Result{}, err
		}
	}
	ns := r.Options.ProjectNamespace
	if ns == "" {
		ns = req.Namespace
	}
	if ns == "" {
		return ctrl.Result{}, nil
	}
	return r.reconcileProject(ctx, ns)
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Options.defaults()
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	enqueue := handler.EnqueueRequestsFromMapFunc(projectRequest)
	controllerName := "internal-dns-control-plane-" + model.OpaqueToken(string(r.Options.ProjectUID))[:12]
	// Every pass reconciles the whole project, so every event shares one key:
	// the queue then holds at most one pending pass per project. Contribution
	// status is written by this controller and never needs another pass.
	b := ctrl.NewControllerManagedBy(mgr).Named(controllerName).
		Watches(&dnsv1alpha1.DNSRecordContribution{}, enqueue, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&dnsv1alpha1.DNSZone{}, enqueue).Watches(&dnsv1alpha1.DNSZoneAssociation{}, enqueue).
		Watches(&dnsv1alpha1.DNSRecordSet{}, enqueue).Watches(&dnsv1alpha1.DNSNamingPolicy{}, enqueue)

	return b.Watches(&dnsv1alpha1.DNSResolverContext{}, enqueue).Watches(&dnsv1alpha1.DNSResolverAccessBinding{}, enqueue).Complete(r)

}

// projectQueueKey names the single queue entry for each project namespace.
const projectQueueKey = "project"

// projectRequest maps any project object to the project's single queue key.
func projectRequest(_ context.Context, o client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: projectQueueKey}}}
}
