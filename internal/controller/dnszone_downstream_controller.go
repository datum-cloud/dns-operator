// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"go.miloapis.com/dns-operator/internal/dns"
	dnsutils "go.miloapis.com/dns-operator/internal/dns/utils"
)

// DNSZoneReconciler reconciles a DNSZone object
type DNSZoneReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	DNSHandler *dns.DNSHandler
}

const downstreamZoneFinalizer = "dns.networking.miloapis.com/finalize-dnszone-downstream"

// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones/finalizers,verbs=update
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszoneclasses,verbs=get;list;watch

// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.22.1/pkg/reconcile
func (r *DNSZoneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)
	logger.Info("dnszone reconcile start")

	var zone dnsv1alpha1.DNSZone
	if err := r.Get(ctx, req.NamespacedName, &zone); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if zone.Spec.DNSZoneClassName == "" || zone.Spec.DNSZoneClassName != r.DNSHandler.Client.Name {
		logger.Info("Resource belongs to different class. Not Reconciling")
		return ctrl.Result{}, nil
	}

	var zc dnsv1alpha1.DNSZoneClass
	if err := r.Get(ctx, client.ObjectKey{Name: zone.Spec.DNSZoneClassName}, &zc); err != nil {
		logger.Info("Failed to get zone class for zone. Not Reconciling")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// --- Ensure finalizer (non-deletion path) ---
	if zone.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(&zone, downstreamZoneFinalizer) {

			if controllerutil.ContainsFinalizer(&zone, downstreamZoneFinalizer) {
				return ctrl.Result{}, nil
			}
			base := zone.DeepCopy()
			controllerutil.AddFinalizer(&zone, downstreamZoneFinalizer)
			if err := r.Patch(ctx, &zone, client.MergeFrom(base)); err != nil {
				logger.Error(err, "failed to add zone finalizer")
				return ctrl.Result{}, err
			}
			logger.Info("Added finalizer to zone")
			return ctrl.Result{}, nil
		}
	} else {
		// --- Deletion path: remove from PDNS, then drop finalizer ---
		if controllerutil.ContainsFinalizer(&zone, downstreamZoneFinalizer) {
			var rrs dnsv1alpha1.DNSRecordSetList
			if err := r.Client.List(ctx, &rrs,
				client.InNamespace(zone.Namespace),
				client.MatchingFields{"spec.DNSZoneRef.Name": zone.Name},
			); err != nil {
				ctrl.LoggerFrom(ctx).Error(err, "failed to list recordsets for zone", "zone", zone.Name, "namespace", zone.Namespace)
				return ctrl.Result{}, err
			}

			if len(rrs.Items) > 0 {
				logger.Info("Found recordsets for zone, requeuing for deletion", "count", len(rrs.Items))

				// Manually triggering GB because once Zone is gone, we can't determine what class the recordset belongs to, so we can't rely on the GB to enqueue the recordsets for deletion.
				for _, rrs := range rrs.Items {
					logger.Info("Deleting recordset for zone", "recordset", rrs.Name)
					// Safe to ignore errors here. garbage collection kicks in at the same time
					r.Client.Delete(ctx, &rrs)
				}

				return ctrl.Result{Requeue: true}, nil
			}

			err := r.DNSHandler.Client.DeleteZone(ctx, zone)

			if err != nil {
				logger.Error(err, "failed to delete zone from downstream controller")
				return ctrl.Result{}, err
			}

			logger.Info("Deleted zone from downstream controller")

			// remove finalizer
			if !controllerutil.ContainsFinalizer(&zone, downstreamZoneFinalizer) {
				return ctrl.Result{}, nil
			}

			base := zone.DeepCopy()
			controllerutil.RemoveFinalizer(&zone, downstreamZoneFinalizer)
			if err := r.Patch(ctx, &zone, client.MergeFrom(base)); err != nil {
				logger.Error(err, "failed to remove zone finalizer")
				return ctrl.Result{}, err
			}
			logger.Info("Removed finalizer from zone")
		}
		return ctrl.Result{}, nil
	}

	// Deepcopy for patching Status
	base := zone.DeepCopy()

	condAccepted := metav1.Condition{
		Type:               CondAccepted,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonAccepted,
		Message:            "Zone Acepted",
		ObservedGeneration: zone.Generation,
		LastTransitionTime: metav1.Now(),
	}
	if apimeta.SetStatusCondition(&zone.Status.Conditions, condAccepted) {
		logger.Info("Setting Accepted condition")
		return ctrl.Result{}, r.Status().Patch(ctx, &zone, client.MergeFrom(base))
	}

	err := r.DNSHandler.Client.EnsureZone(ctx, zone, zc)

	if err != nil {
		logger.Error(err, "failed to ensure zone in downstream controller")
		return ctrl.Result{}, err
	}

	// Update the status of the DNSZone with the nameservers from the downstream controller
	desiredNS := r.DNSHandler.Client.GetZoneNameservers(ctx, zone, zc)
	currentNS := dnsutils.NormalizeStringSlice(zone.Status.Nameservers)
	if len(desiredNS) > 0 && !equality.Semantic.DeepEqual(currentNS, desiredNS) {
		zone.Status.Nameservers = desiredNS
		if err := r.Status().Patch(ctx, &zone, client.MergeFrom(base)); err != nil {
			logger.Error(err, "failed to update zone status")
			return ctrl.Result{}, err
		}
		logger.Info("Updated zone status")
	}

	condProgrammed := metav1.Condition{
		Type:               CondProgrammed,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonProgrammed,
		Message:            "Zone Programmed",
		ObservedGeneration: zone.Generation,
		LastTransitionTime: metav1.Now(),
	}
	if apimeta.SetStatusCondition(&zone.Status.Conditions, condProgrammed) {
		return ctrl.Result{}, r.Status().Patch(ctx, &zone, client.MergeFrom(base))
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *DNSZoneReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dnsv1alpha1.DNSZone{}).
		Named("dnszone").
		Complete(r)
}
