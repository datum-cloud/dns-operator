// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
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
	// PassSlots, when set, is shared by every project reconciler in a process
	// and bounds how many project passes run at once, so a restart does not
	// start a full pass of every project against the API server together.
	PassSlots chan struct{}

	// OnPublished, when set, is called after a pass commits a new publication.
	OnPublished func()

	// PerZoneReconcile moves compiling and publishing zones out of the project
	// pass into zone keys that record, registration and contribution changes
	// queue directly, so a record change does not wait for a project pass.
	PerZoneReconcile bool
	// ZoneSlots, when set, is shared by every project reconciler in a process
	// and bounds how many zone keys compile at once across all projects.
	ZoneSlots chan struct{}
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

type Options = ReconcilerOptions

// Reconciler compiles records within a configured project scope.
type Reconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Options   ReconcilerOptions

	zoneQueue chan event.GenericEvent
	// ZoneReader serves zone keys' reads of associations, registrations,
	// contributions and record sets, normally the project's watch cache.
	ZoneReader client.Reader
	// identityVerifiedAt is the unix nano time zone keys last saw the project
	// identity verified.
	identityVerifiedAt atomic.Int64
	// published maps a zone UID to the last manifest this process wrote.
	published sync.Map
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
	if err := r.verifyIdentity(ctx, req.Name == projectQueueKey); err != nil {
		return ctrl.Result{}, err
	}
	ns := r.Options.ProjectNamespace
	if ns == "" {
		ns = req.Namespace
	}
	if ns == "" {
		return ctrl.Result{}, nil
	}
	if req.Name != projectQueueKey && r.Options.PerZoneReconcile {
		return r.reconcileQueueKey(ctx, ns, req.Name)
	}
	if slots := r.Options.PassSlots; slots != nil {
		waited := time.Now()
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
		passPhaseDuration.WithLabelValues("wait for slot").Observe(time.Since(waited).Seconds())
	}
	return r.reconcileProject(ctx, ns)
}

// identityReuse is how long a zone key relies on an earlier verification of
// the project identity. The project pass verifies on every pass.
const identityReuse = 10 * time.Second

func (r *Reconciler) verifyIdentity(ctx context.Context, always bool) error {
	if r.Options.VerifyProjectIdentity == nil {
		return nil
	}
	seen := r.identityVerifiedAt.Load()
	if !always && time.Since(time.Unix(0, seen)) < identityReuse {
		return nil
	}
	if err := r.Options.VerifyProjectIdentity(ctx); err != nil {
		r.identityVerifiedAt.Store(0)
		return err
	}
	// A failure recorded while this check ran wins over this success.
	r.identityVerifiedAt.CompareAndSwap(seen, time.Now().UnixNano())
	return nil
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
	if r.Options.PerZoneReconcile {
		return r.setupPerZone(mgr, controllerName, enqueue)
	}
	b := ctrl.NewControllerManagedBy(mgr).Named(controllerName).
		WithOptions(controller.Options{RateLimiter: projectRateLimiter()}).
		Watches(&dnsv1alpha1.DNSRecordContribution{}, enqueue, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&dnsv1alpha1.DNSZone{}, enqueue).Watches(&dnsv1alpha1.DNSZoneAssociation{}, enqueue).
		Watches(&dnsv1alpha1.DNSRecordSet{}, enqueue).Watches(&dnsv1alpha1.DNSNamingPolicy{}, enqueue)

	return b.Watches(&dnsv1alpha1.DNSResolverContext{}, enqueue).Watches(&dnsv1alpha1.DNSResolverAccessBinding{}, enqueue).Complete(r)

}

// zoneWorkers bounds how many keys of one project run at once, the project
// pass included. A key never runs on two workers at once.
const zoneWorkers = 4

// setupPerZone watches record inputs per zone. The project pass keeps every
// project-wide input and hands zones to the zone keys through zoneQueue.
func (r *Reconciler) setupPerZone(mgr ctrl.Manager, name string, project handler.EventHandler) error {
	r.zoneQueue = make(chan event.GenericEvent, 1024)
	if r.ZoneReader == nil {
		r.ZoneReader = mgr.GetCache()
	}
	generation := builder.WithPredicates(predicate.GenerationChangedPredicate{})
	return ctrl.NewControllerManagedBy(mgr).Named(name).
		WithOptions(controller.Options{RateLimiter: projectRateLimiter(), MaxConcurrentReconciles: zoneWorkers}).
		Watches(&dnsv1alpha1.DNSRecordSet{}, zoneEvents{keys: recordSetZone}, generation).
		Watches(&dnsv1alpha1.DNSRegistration{}, zoneEvents{keys: registrationZone}, generation).
		Watches(&dnsv1alpha1.DNSRecordContribution{}, zoneEvents{keys: contributionRegistration}, generation).
		Watches(&dnsv1alpha1.DNSZone{}, project).
		Watches(&dnsv1alpha1.DNSZone{}, zoneEvents{keys: zoneItself}, generation).
		Watches(&dnsv1alpha1.DNSZoneAssociation{}, project).Watches(&dnsv1alpha1.DNSNamingPolicy{}, project).
		Watches(&dnsv1alpha1.DNSResolverContext{}, project).Watches(&dnsv1alpha1.DNSResolverAccessBinding{}, project).
		WatchesRawSource(source.Channel(r.zoneQueue, zoneEvents{keys: zoneItself})).
		Complete(r)
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
