// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/config"
	"go.miloapis.com/dns-operator/internal/dns"
)

// DNSRecordSetReconciler reconciles a DNSRecordSet object
type DNSRecordSetReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	DNSHandler *dns.DNSHandler

	// Config tunes the controller's concurrency and retry backoff. A zero value
	// falls back to the same defaults the server config declares.
	Config config.DNSRecordSetPowerDNSControllerConfig
}

const (
	// defaultRecordSetMaxConcurrentReconciles matches the server config default.
	// PowerDNS runs with lmdb-shards=1 and therefore admits one writer at a
	// time, so this is deliberately modest: it is enough that a single large
	// record set no longer blocks every other zone, without queueing writers
	// behind a lock.
	defaultRecordSetMaxConcurrentReconciles = 4

	defaultRecordSetRateLimiterBaseDelay = 1 * time.Second
	defaultRecordSetRateLimiterMaxDelay  = 30 * time.Second
)

// downstreamRSFinalizer is the finalizer for the DNSRecordSetDownstream controller
const downstreamRSFinalizer = "dns.networking.miloapis.com/finalize-dnsrecordset-downstream"

// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnsrecordsets/finalizers,verbs=update
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones,verbs=get;list;watch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszoneclasses,verbs=get;list;watch

// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.1/pkg/reconcile
func (r *DNSRecordSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	logger.Info("dnsrecordset reconcile start")

	var rs dnsv1alpha1.DNSRecordSet
	if err := r.Get(ctx, req.NamespacedName, &rs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var zone dnsv1alpha1.DNSZone
	if err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: rs.Spec.DNSZoneRef.Name}, &zone); err != nil {
		if apierrors.IsNotFound(err) {
			if err := r.setAcceptedCondition(ctx, &rs, metav1.ConditionFalse, ReasonPending,
				fmt.Sprintf("waiting for DNSZone %q", rs.Spec.DNSZoneRef.Name)); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if zone.Spec.DNSZoneClassName == "" || zone.Spec.DNSZoneClassName != r.DNSHandler.Client.Name {
		logger.Info("Resource belongs to a different class. Not Reconciling")
		return ctrl.Result{}, nil
	}

	// Ensure finalizer is present while not deleting.
	if rs.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&rs, downstreamRSFinalizer) {
			base := rs.DeepCopy()
			controllerutil.AddFinalizer(&rs, downstreamRSFinalizer)
			if err := r.Patch(ctx, &rs, client.MergeFrom(base)); err != nil {
				logger.Error(err, "failed to add finalizer", "namespace", rs.Namespace, "name", rs.Name)
				return ctrl.Result{}, err
			}
			logger.Info("Added finalizer to recordset")
			return ctrl.Result{}, nil
		}
	} else {
		if controllerutil.ContainsFinalizer(&rs, downstreamRSFinalizer) {
			err := r.DNSHandler.Client.DeleteRecordSet(ctx, zone, rs)

			if err != nil {
				logger.Error(err, "failed to delete recordset from downstream controller", "namespace", rs.Namespace, "name", rs.Name)
				return ctrl.Result{}, err
			}

			// Re-fetch Here, since a lot has happened in between. Updates cache.
			var rs dnsv1alpha1.DNSRecordSet
			if err := r.Get(ctx, req.NamespacedName, &rs); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}

			base := rs.DeepCopy()
			controllerutil.RemoveFinalizer(&rs, downstreamRSFinalizer)
			if err := r.Patch(ctx, &rs, client.MergeFrom(base)); err != nil {
				logger.Error(err, "failed to remove finalizer", "namespace", rs.Namespace, "name", rs.Name)
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if metav1.IsControlledBy(&rs, &zone) {
		// This block is valid only for migration phase. DNSRecordSets are managed here and not by the DNSZone controller. So, we should not have a controller reference to the zone. We should only have an owner reference.
		base := rs.DeepCopy()
		logger.Info("RecordSet is already controlled by zone. Should not be controller by. Should be owner reference only. Removing controller reference.")
		err := controllerutil.RemoveControllerReference(&zone, &rs, r.Scheme)

		if err != nil {
			logger.Error(err, "failed to remove controller reference", "namespace", rs.Namespace, "name", rs.Name)
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, r.Patch(ctx, &rs, client.MergeFrom(base))
	}

	isOwner, err := controllerutil.HasOwnerReference(rs.OwnerReferences, &zone, r.Scheme)

	if err != nil {
		logger.Error(err, "failed to check owner reference", "namespace", rs.Namespace, "name", rs.Name)
		return ctrl.Result{}, err
	}

	if !isOwner {
		base := rs.DeepCopy()
		logger.Info("RecordSet is not owned by zone. Setting owner reference.")
		if err := controllerutil.SetOwnerReference(&zone, &rs, r.Scheme); err != nil {
			logger.Error(err, "failed to set owner reference", "namespace", rs.Namespace, "name", rs.Name)
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.Patch(ctx, &rs, client.MergeFrom(base))
	}

	base := rs.DeepCopy()
	cond := metav1.Condition{
		Type:               CondAccepted,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonAccepted,
		Message:            "RecordSet is accepted for processing",
		ObservedGeneration: rs.Generation,
		LastTransitionTime: metav1.Now(),
	}

	if apimeta.SetStatusCondition(&rs.Status.Conditions, cond) {
		return ctrl.Result{}, r.Status().Patch(ctx, &rs, client.MergeFrom(base))
	}

	// Nothing to program: the records at this generation are already in the
	// provider and the status says so. Every status write the controller makes
	// comes back as an update event, so without this the object reprograms
	// itself once per write — for a record set holding thousands of names that
	// is the difference between a reconcile and a reconcile loop. Drift written
	// outside the operator is still corrected on the manager's resync.
	if recordSetProgrammedAtGeneration(&rs) {
		logger.Info("RecordSet already programmed at this generation", "generation", rs.Generation)
		return ctrl.Result{}, nil
	}

	statuses, err := r.DNSHandler.Client.EnsureRecordSet(ctx, zone, rs)
	return ctrl.Result{}, r.updateStatus(ctx, &rs, err, statuses)
}

// recordSetProgrammedAtGeneration reports whether the record set's current spec
// generation has already been programmed successfully.
func recordSetProgrammedAtGeneration(rs *dnsv1alpha1.DNSRecordSet) bool {
	cond := apimeta.FindStatusCondition(rs.Status.Conditions, CondProgrammed)
	return cond != nil &&
		cond.Status == metav1.ConditionTrue &&
		cond.ObservedGeneration == rs.Generation
}

func (r *DNSRecordSetReconciler) updateStatus(ctx context.Context, rs *dnsv1alpha1.DNSRecordSet, err error, statuses []dnsv1alpha1.RecordSetStatus) error {
	var condProgrammed metav1.Condition
	if err != nil {
		condProgrammed = metav1.Condition{
			Type:               CondProgrammed,
			Status:             metav1.ConditionFalse,
			Reason:             ReasonPending,
			Message:            err.Error(),
			ObservedGeneration: rs.Generation,
			LastTransitionTime: metav1.Now(),
		}
	} else {
		allProgrammed, reason, message := aggregateProgrammedStatus(statuses)
		if allProgrammed {
			condProgrammed = metav1.Condition{
				Type:               CondProgrammed,
				Status:             metav1.ConditionTrue,
				Reason:             ReasonProgrammed,
				Message:            "RecordSet is programmed in downstream controller",
				ObservedGeneration: rs.Generation,
				LastTransitionTime: metav1.Now(),
			}
		} else {
			condProgrammed = metav1.Condition{
				Type:               CondProgrammed,
				Status:             metav1.ConditionFalse,
				Reason:             reason,
				Message:            message,
				ObservedGeneration: rs.Generation,
				LastTransitionTime: metav1.Now(),
			}
		}
	}

	base := rs.DeepCopy()
	changed := false
	changed = apimeta.SetStatusCondition(&rs.Status.Conditions, condProgrammed)

	sort.SliceStable(statuses, func(i, j int) bool {
		return statuses[i].Name < statuses[j].Name
	})

	if rs.Status.RecordSets == nil {
		rs.Status.RecordSets = statuses
		changed = true
	} else {
		if !reflect.DeepEqual(rs.Status.RecordSets, statuses) {
			rs.Status.RecordSets = statuses
			changed = true
		}
	}

	if changed {
		return r.Status().Patch(ctx, rs, client.MergeFrom(base))
	}
	return nil
}

func (r *DNSRecordSetReconciler) setAcceptedCondition(
	ctx context.Context,
	rs *dnsv1alpha1.DNSRecordSet,
	status metav1.ConditionStatus,
	reason, message string,
) error {
	base := rs.DeepCopy()
	cond := metav1.Condition{
		Type:               CondAccepted,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: rs.Generation,
		LastTransitionTime: metav1.Now(),
	}
	if !apimeta.SetStatusCondition(&rs.Status.Conditions, cond) {
		return nil
	}
	return r.Status().Patch(ctx, rs, client.MergeFrom(base))
}

// SetupWithManager wires watches:
//   - Reconciles DNSRecordSet
//   - Requeues DNSRecordSets when their DNSZone (same ns, same spec.zoneName) changes
//   - Uses an exponential backoff rate limiter for gentle retries while waiting on zone readiness
func (r *DNSRecordSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	maxConcurrentReconciles := r.Config.MaxConcurrentReconciles
	if maxConcurrentReconciles <= 0 {
		maxConcurrentReconciles = defaultRecordSetMaxConcurrentReconciles
	}
	baseDelay := defaultRecordSetRateLimiterBaseDelay
	if r.Config.RateLimiterBaseDelay != nil {
		baseDelay = r.Config.RateLimiterBaseDelay.Duration
	}
	maxDelay := defaultRecordSetRateLimiterMaxDelay
	if r.Config.RateLimiterMaxDelay != nil {
		maxDelay = r.Config.RateLimiterMaxDelay.Duration
	}

	// index DNSRecordSet by spec.DNSZoneRef.Name for quick fan-out from a DNSZone event
	rl := workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](baseDelay, maxDelay)

	return ctrl.NewControllerManagedBy(mgr).
		For(&dnsv1alpha1.DNSRecordSet{}).
		// When a DNSZone in this namespace becomes ready, enqueue its recordsets
		Watches(
			&dnsv1alpha1.DNSZone{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
				zone := obj.(*dnsv1alpha1.DNSZone)
				var rrs dnsv1alpha1.DNSRecordSetList
				if err := mgr.GetClient().List(ctx, &rrs,
					client.InNamespace(zone.Namespace),
					client.MatchingFields{"spec.DNSZoneRef.Name": zone.Name},
				); err != nil {
					ctrl.LoggerFrom(ctx).Error(err, "failed to list recordsets for zone", "zone", zone.Name, "namespace", zone.Namespace)
					return nil
				}
				out := make([]ctrl.Request, 0, len(rrs.Items))
				for i := range rrs.Items {
					out = append(out, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&rrs.Items[i])})
				}
				return out
			}),
			builder.WithPredicates(zoneBecameProgrammed()),
		).
		WithOptions(controller.Options{
			RateLimiter:             rl,
			MaxConcurrentReconciles: maxConcurrentReconciles,
		}).
		Named("dnsrecordset").
		Complete(r)
}

// zoneBecameProgrammed admits only the DNSZone events that can change what a
// record set should do: the zone is programmed and was not before.
//
// The fan-out this gates enqueues every record set in a zone, so an unfiltered
// watch turned each of a zone's own status writes into a full reconcile of every
// record set it holds. A zone that is already programmed and stays programmed
// tells its record sets nothing new.
func zoneBecameProgrammed() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return zoneProgrammed(e.Object)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return !zoneProgrammed(e.ObjectOld) && zoneProgrammed(e.ObjectNew)
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

func zoneProgrammed(obj client.Object) bool {
	zone, ok := obj.(*dnsv1alpha1.DNSZone)
	if !ok {
		return false
	}
	cond := apimeta.FindStatusCondition(zone.Status.Conditions, CondProgrammed)
	return cond != nil && cond.Status == metav1.ConditionTrue
}

// cleanupPDNSForRecordSet ensures the RRsets represented by rs are removed from PDNS.
// Returns nil when cleanup is complete (or nothing to do), or error on failure.
