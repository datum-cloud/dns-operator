// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	pdnsclient "go.miloapis.com/dns-operator/internal/pdns"
)

// DNSRecordSetReconciler reconciles a DNSRecordSet object
type DNSRecordSetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

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

	var rs dnsv1alpha1.DNSRecordSet
	if err := r.Get(ctx, req.NamespacedName, &rs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// --- Ensure finalizer on creation/update (non-deletion path) ---
	if rs.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(&rs, downstreamRSFinalizer) {
		if controllerutil.ContainsFinalizer(&rs, downstreamRSFinalizer) {
			return ctrl.Result{}, nil
		}
		base := rs.DeepCopy()
		controllerutil.AddFinalizer(&rs, downstreamRSFinalizer)
		if err := r.Patch(ctx, &rs, client.MergeFrom(base)); err != nil {
			logger.Error(err, "failed to add finalizer", "ns", rs.Namespace, "name", rs.Name)
			return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
		}
		// After mutating the object, return so we reconcile again with updated state.
		return ctrl.Result{}, nil
	}

	// --- Deletion path: MUST clean PDNS before removing finalizer ---
	if !rs.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&rs, downstreamRSFinalizer) {
			// Fetch zone (if absent or empty => nothing to clean; treat as success)
			var zone dnsv1alpha1.DNSZone
			err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: rs.Spec.DNSZoneRef.Name}, &zone)
			if err != nil {
				// If the zone is already gone, treat as success: nothing to clean in PDNS
				if client.IgnoreNotFound(err) == nil {
					base := rs.DeepCopy()
					controllerutil.RemoveFinalizer(&rs, downstreamRSFinalizer)
					if err := r.Patch(ctx, &rs, client.MergeFrom(base)); err != nil {
						logger.Error(err, "failed to remove finalizer after zone missing", "ns", rs.Namespace, "name", rs.Name)
						return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
					}
					return ctrl.Result{}, nil
				}
				logger.Error(err, "failed to get zone", "ns", req.Namespace, "name", rs.Spec.DNSZoneRef.Name)
				return ctrl.Result{}, err
			}

			// If the zone is being deleted, skip PDNS cleanup for this recordset
			if !zone.DeletionTimestamp.IsZero() {

				// remove our finalizer
				controllerutil.RemoveFinalizer(&rs, downstreamRSFinalizer)
				base := rs.DeepCopy()
				if err := r.Patch(ctx, &rs, client.MergeFrom(base)); err != nil {
					logger.Error(err, "failed to remove finalizer", "ns", rs.Namespace, "name", rs.Name)
					return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
				}
			}

			ok, err := r.cleanupPDNSForRecordSet(ctx, &rs, &zone)
			if err != nil {
				logger.Error(err, "pdns cleanup failed; will retry", "zone", zone.Spec.DomainName)
				// Requeue to try again; keep finalizer so object doesn't disappear prematurely
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
			if !ok {
				logger.Error(err, "not ok; pdns cleanup failed; will retry", "zone", zone.Spec.DomainName)
				return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
			}

			// PDNS cleanup succeeded (or was a no-op) -> remove finalizer (conflict-safe)
			base := rs.DeepCopy()
			controllerutil.RemoveFinalizer(&rs, downstreamRSFinalizer)
			if err := r.Patch(ctx, &rs, client.MergeFrom(base)); err != nil {
				logger.Error(err, "failed to remove finalizer", "ns", rs.Namespace, "name", rs.Name)
				return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
			}
		}
		return ctrl.Result{}, nil
	}

	// Fetch zone to locate class
	var zone dnsv1alpha1.DNSZone
	if err := r.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: rs.Spec.DNSZoneRef.Name}, &zone); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Ensure the DNSZone is an owner of this DNSRecordSet so GC cascades on zone deletion.
	if !hasOwnerRef(&rs, &zone) {
		base := rs.DeepCopy()
		if err := controllerutil.SetOwnerReference(&zone, &rs, r.Scheme); err != nil {
			logger.Error(err, "failed to set owner reference", "rs", rs.Name, "zone", zone.Name)
			return ctrl.Result{}, err
		}
		if err := r.Patch(ctx, &rs, client.MergeFrom(base)); err != nil {
			logger.Error(err, "failed to patch owner reference", "rs", rs.Name, "zone", zone.Name)
			// mild backoff; we'll try again
			return ctrl.Result{RequeueAfter: 300 * time.Millisecond}, nil
		}
		// Requeue so we continue with a stable/latest object copy
		return ctrl.Result{}, nil
	}

	// If the zone is deleting, do not attempt to program PDNS for this recordset
	if !zone.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if zone.Spec.DNSZoneClassName == "" {
		return ctrl.Result{}, nil
	}
	var zc dnsv1alpha1.DNSZoneClass
	if err := r.Get(ctx, client.ObjectKey{Name: zone.Spec.DNSZoneClassName}, &zc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if zc.Spec.ControllerName != ControllerNamePowerDNS {
		return ctrl.Result{}, nil
	}

	cli, err := pdnsclient.NewFromEnv()
	if err != nil {
		logger.Error(err, "pdns client init")
		return ctrl.Result{}, fmt.Errorf("pdns client: %w", err)
	}

	// Ensure the zone exists in PDNS before attempting to apply rrsets
	if _, err := cli.GetZone(ctx, zone.Spec.DomainName); err != nil {
		logger.Info("pdns zone not ready yet; requeueing", "zone", zone.Spec.DomainName, "err", err.Error())
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	if err := cli.ApplyRecordSetAuthoritative(ctx, zone.Spec.DomainName, rs); err != nil {
		logger.Error(err, "apply pdns recordset")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager wires watches:
//   - Reconciles DNSRecordSet
//   - Requeues DNSRecordSets when their DNSZone (same ns, same spec.zoneName) changes
//   - Uses an exponential backoff rate limiter for gentle retries while waiting on zone readiness
func (r *DNSRecordSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// index DNSRecordSet by spec.DNSZoneRef.Name for quick fan-out from a DNSZone event
	if err := mgr.GetFieldIndexer().IndexField(context.Background(),
		&dnsv1alpha1.DNSRecordSet{}, "spec.DNSZoneRef.Name",
		func(obj client.Object) []string {
			rs := obj.(*dnsv1alpha1.DNSRecordSet)
			return []string{rs.Spec.DNSZoneRef.Name}
		},
	); err != nil {
		return err
	}

	rl := workqueue.NewTypedItemExponentialFailureRateLimiter[reconcile.Request](1*time.Second, 30*time.Second)

	return ctrl.NewControllerManagedBy(mgr).
		For(&dnsv1alpha1.DNSRecordSet{}).
		// When a DNSZone in this namespace becomes ready, enqueue its recordsets
		Watches(
			&dnsv1alpha1.DNSZone{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
				zone := obj.(*dnsv1alpha1.DNSZone)
				var rrs dnsv1alpha1.DNSRecordSetList
				_ = mgr.GetClient().List(ctx, &rrs,
					client.InNamespace(zone.Namespace),
					client.MatchingFields{"spec.DNSZoneRef.Name": zone.Name},
				)
				out := make([]ctrl.Request, 0, len(rrs.Items))
				for i := range rrs.Items {
					out = append(out, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&rrs.Items[i])})
				}
				return out
			}),
			builder.WithPredicates(), // use default (fire on status/spec changes alike)
		).
		WithOptions(controller.Options{
			RateLimiter: rl,
		}).
		Named("dnsrecordset").
		Complete(r)
}

// cleanupPDNSForRecordSet ensures the RRsets represented by rs are removed from PDNS.
// Returns (true,nil) when cleanup is complete (or nothing to do), (false,nil) when
// it should be retried shortly, or (false,err) on failure.
func (r *DNSRecordSetReconciler) cleanupPDNSForRecordSet(ctx context.Context, rs *dnsv1alpha1.DNSRecordSet, zone *dnsv1alpha1.DNSZone) (bool, error) {
	// If we don't know the domain, there's nothing to clean with PDNS.
	if zone == nil || zone.Spec.DomainName == "" {
		return true, nil
	}

	cli, err := pdnsclient.NewFromEnv()
	if err != nil {
		return false, fmt.Errorf("pdns client: %w", err)
	}

	// If PDNS zone doesn't exist, consider cleanup done.
	if _, err := cli.GetZone(ctx, zone.Spec.DomainName); err != nil {
		return true, nil
	}

	// Authoritatively apply an empty set for this recordset (delete semantics).
	toDelete := rs.DeepCopy()
	toDelete.Spec.Records = nil

	// Bound the external call; but allow enough to finish deterministically.
	pdnsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := cli.ApplyRecordSetAuthoritative(pdnsCtx, zone.Spec.DomainName, *toDelete); err != nil {
		// distinguish transient vs permanent if your client exposes that; otherwise retry
		return false, fmt.Errorf("pdns apply delete: %w", err)
	}

	// Optionally: verify deletion (idempotency) by a lightweight read if your client supports it.
	// If not, trust the 2xx response above and declare success.
	return true, nil
}

// helper near the bottom of the file
func hasOwnerRef(obj metav1.Object, owner metav1.Object) bool {
	for _, or := range obj.GetOwnerReferences() {
		if or.UID == owner.GetUID() {
			return true
		}
	}
	return false
}
