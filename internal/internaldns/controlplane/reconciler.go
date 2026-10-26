// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"errors"
	"slices"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
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
	// ManagedDomainSuffixes lists the suffixes a managed namespace domain must
	// fall strictly below. Empty means DefaultManagedDomainSuffixes.
	ManagedDomainSuffixes []string
	ServingRegions        []ServingRegion
	// Regions limits this reconciler to the serving regions its control plane
	// publishes when each region runs its own. ServingRegions still lists every
	// region, so contexts keep one shard assignment everywhere. Empty means
	// this reconciler publishes every region.
	Regions []string
	// PassSlots, when set, is shared by every project reconciler in a process
	// and bounds how many project passes run at once, so a restart does not
	// start a full pass of every project against the API server together.
	PassSlots chan struct{}

	// Stopping, when set and true, stops the reconciler taking or renewing
	// zone leases so the process can release them before it exits.
	Stopping func() bool
}

type ServingRegion struct {
	Region string
	Shard  string
}

// AddressAllocator durably leases globally unique listener destinations. A
// pure hash is only a candidate and is never sufficient proof of uniqueness.
// ReleaseDestinations frees the project's destination claims outside held
// (keys from model.DestinationKey) once nothing routes them.
type AddressAllocator interface {
	Allocate(ctx context.Context, projectUID, contextUID types.UID, region string) (cluster string, err error)
	ClaimDestination(context.Context, types.UID, types.UID, string, string, int32) error
	ReleaseDestinations(ctx context.Context, projectUID types.UID, held map[string]bool) error
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

// local reports whether this reconciler publishes region.
func (o ReconcilerOptions) local(region string) bool {
	return !o.regional() || slices.Contains(o.Regions, region)
}

// regional reports whether each region runs its own control plane.
func (o ReconcilerOptions) regional() bool { return len(o.Regions) > 0 }

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
	if slots := r.Options.PassSlots; slots != nil {
		// Waiting longer than a quarter lease could let this project's zone
		// leases lapse behind other projects' slow passes, so after that the
		// pass runs without a slot.
		wait := time.NewTimer(r.Options.LeaseDuration / 4)
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-wait.C:
		case <-ctx.Done():
			wait.Stop()
			return ctrl.Result{}, ctx.Err()
		}
		wait.Stop()
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
		WithOptions(controller.Options{RateLimiter: projectRateLimiter()}).
		Watches(&dnsv1alpha1.DNSRecordContribution{}, enqueue, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&dnsv1alpha1.DNSZone{}, enqueue).Watches(&dnsv1alpha1.DNSZoneAssociation{}, enqueue).
		Watches(&dnsv1alpha1.DNSRecordSet{}, enqueue).Watches(&dnsv1alpha1.DNSNamingPolicy{}, enqueue)

	return b.Watches(&dnsv1alpha1.DNSResolverContext{}, enqueue).Watches(&dnsv1alpha1.DNSResolverAccessBinding{}, enqueue).Complete(r)

}

// A whole project shares one queue key, so its backoff delays every name in it.
const projectRetryLimit = 30 * time.Second

func projectRateLimiter() workqueue.TypedRateLimiter[reconcile.Request] {
	return workqueue.NewTypedMaxOfRateLimiter(
		workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](time.Second, projectRetryLimit),
		&workqueue.TypedBucketRateLimiter[reconcile.Request]{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)
}

// projectQueueKey names the single queue entry for each project namespace.
const projectQueueKey = "project"

// projectRequest maps any project object to the project's single queue key.
func projectRequest(_ context.Context, o client.Object) []reconcile.Request {
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: projectQueueKey}}}
}
