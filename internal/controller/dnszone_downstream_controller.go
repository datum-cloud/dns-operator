// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	pdnsclient "go.miloapis.com/dns-operator/internal/pdns"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// DNSZoneReconciler reconciles a DNSZone object
type DNSZoneReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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
	logger.Info("dnszone reconcile start", "namespace", req.Namespace, "name", req.Name)

	var zone dnsv1alpha1.DNSZone
	if err := r.Get(ctx, req.NamespacedName, &zone); err != nil {
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
			return ctrl.Result{}, nil
		}
	} else {
		// --- Deletion path: remove from PDNS, then drop finalizer ---
		if controllerutil.ContainsFinalizer(&zone, downstreamZoneFinalizer) {
			// Only manage PDNS if this zone is handled by our controller
			var zc dnsv1alpha1.DNSZoneClass
			if zone.Spec.DNSZoneClassName != "" {
				if err := r.Get(ctx, client.ObjectKey{Name: zone.Spec.DNSZoneClassName}, &zc); err != nil {
					// TODO: should we delete if the class is not found?
					return ctrl.Result{}, client.IgnoreNotFound(err)
				}
			}
			if zc.Spec.ControllerName == ControllerNamePowerDNS {
				cli, err := pdnsclient.NewFromEnv()
				if err != nil {
					logger.Error(err, "pdns client init")
					return ctrl.Result{}, fmt.Errorf("pdns client: %w", err)
				}
				if err := cli.DeleteZone(ctx, zone.Spec.DomainName); err != nil {
					logger.Error(err, "delete pdns zone failed; will retry", "zone", zone.Spec.DomainName)
					return ctrl.Result{}, err
				}
			}

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
		}
		return ctrl.Result{}, nil
	}

	// If class is set and equals "powerdns", ensure in PDNS (status handled by replicator)
	if zone.Spec.DNSZoneClassName != "" {
		var zc dnsv1alpha1.DNSZoneClass
		if err := r.Get(ctx, client.ObjectKey{Name: zone.Spec.DNSZoneClassName}, &zc); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		if zc.Spec.ControllerName == ControllerNamePowerDNS {
			cli, err := pdnsclient.NewFromEnv()
			if err != nil {
				logger.Error(err, "pdns client init")
				return ctrl.Result{}, fmt.Errorf("pdns client: %w", err)
			}
			// Ensure zone exists or create (no status updates here)
			if _, err := cli.GetZone(ctx, zone.Spec.DomainName); err != nil {
				// nameservers from class policy (Static)
				var nss []string
				if zc.Spec.NameServerPolicy != nil && zc.Spec.NameServerPolicy.Mode == dnsv1alpha1.NameServerPolicyModeStatic && zc.Spec.NameServerPolicy.Static != nil {
					nss = append(nss, zc.Spec.NameServerPolicy.Static.Servers...)
				}
				if err := cli.CreateZone(ctx, zone.Spec.DomainName, nss); err != nil {
					logger.Error(err, "create pdns zone")
					return ctrl.Result{}, err
				}
			}

			// At this point the zone exists in PDNS; set downstream status nameservers from class if not already set
			var desiredNS []string
			if zc.Spec.NameServerPolicy != nil && zc.Spec.NameServerPolicy.Mode == dnsv1alpha1.NameServerPolicyModeStatic && zc.Spec.NameServerPolicy.Static != nil {
				desiredNS = append(desiredNS, zc.Spec.NameServerPolicy.Static.Servers...)
			}
			// Do not override once nameservers have been set downstream
			if len(zone.Status.Nameservers) == 0 && len(desiredNS) > 0 {
				base := zone.DeepCopy()
				zone.Status.Nameservers = desiredNS
				if err := r.Status().Patch(ctx, &zone, client.MergeFrom(base)); err != nil {
					logger.Error(err, "failed to update downstream nameservers status; will retry")
					return ctrl.Result{}, err
				}
			}
		}
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
