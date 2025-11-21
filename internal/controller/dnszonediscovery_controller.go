// SPDX-License-Identifier: AGPL-3.0-only
package controller

import (
	"context"
	"fmt"
	"time"

	"go.miloapis.com/dns-operator/internal/discovery"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// DNSZoneDiscoveryReplicator performs a one-shot discovery of records for a DNSZone on the upstream cluster.
// It does not replicate downstream; it only updates upstream status.
type DNSZoneDiscoveryReplicator struct {
	mgr mcmanager.Manager
}

// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszonediscoveries,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszonediscoveries/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dns.networking.miloapis.com,resources=dnszones,verbs=get;list;watch

func (r *DNSZoneDiscoveryReplicator) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx).WithValues("cluster", req.ClusterName, "namespace", req.Namespace, "name", req.Name)
	ctx = logf.IntoContext(ctx, logger)

	upstreamCluster, err := r.mgr.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, err
	}

	var dzd dnsv1alpha1.DNSZoneDiscovery
	if err := upstreamCluster.GetClient().Get(ctx, req.NamespacedName, &dzd); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// No lifecycle beyond initial discovery.
	if !dzd.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// If already discovered, nothing to do at all.
	if apimeta.IsStatusConditionTrue(dzd.Status.Conditions, CondDiscovered) {
		return ctrl.Result{}, nil
	}

	// Fetch the referenced DNSZone
	var zone dnsv1alpha1.DNSZone
	if err := upstreamCluster.GetClient().Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: dzd.Spec.DNSZoneRef.Name}, &zone); err != nil {
		base := dzd.DeepCopy()
		msg := fmt.Sprintf("dnszone %q not found", dzd.Spec.DNSZoneRef.Name)
		if apimeta.SetStatusCondition(&dzd.Status.Conditions, metav1.Condition{
			Type:               CondAccepted,
			Status:             metav1.ConditionFalse,
			Reason:             ReasonPending,
			Message:            msg,
			ObservedGeneration: dzd.Generation,
			LastTransitionTime: metav1.NewTime(time.Now()),
		}) {
			if err := upstreamCluster.GetClient().Status().Patch(ctx, &dzd, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Ensure OwnerReference to referenced DNSZone early
	if !metav1.IsControlledBy(&dzd, &zone) {
		base := dzd.DeepCopy()
		if err := controllerutil.SetControllerReference(&zone, &dzd, upstreamCluster.GetScheme()); err != nil {
			return ctrl.Result{}, err
		}
		if err := upstreamCluster.GetClient().Patch(ctx, &dzd, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("set controller OwnerReference to DNSZone", "dnsZone", zone.Name)
		return ctrl.Result{}, nil
	}

	// Mark Accepted true if not already
	if !apimeta.IsStatusConditionTrue(dzd.Status.Conditions, CondAccepted) {
		base := dzd.DeepCopy()
		if apimeta.SetStatusCondition(&dzd.Status.Conditions, metav1.Condition{
			Type:               CondAccepted,
			Status:             metav1.ConditionTrue,
			Reason:             ReasonAccepted,
			Message:            "Discovery Accepted",
			ObservedGeneration: dzd.Generation,
			LastTransitionTime: metav1.NewTime(time.Now()),
		}) {
			if err := upstreamCluster.GetClient().Status().Patch(ctx, &dzd, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Perform discovery (one-shot)
	recordSets, err := discovery.DiscoverZoneRecords(ctx, zone.Spec.DomainName)
	if err != nil {
		logger.Error(err, "discovery failed; will retry", "zone", zone.Spec.DomainName)
		return ctrl.Result{}, err
	}

	// Log how many records were discovered
	logger.Info("discovered records", "count", len(recordSets))

	base := dzd.DeepCopy()
	dzd.Status.RecordSets = recordSets
	apimeta.SetStatusCondition(&dzd.Status.Conditions, metav1.Condition{
		Type:               CondDiscovered,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonDiscovered,
		Message:            "Zone Records Discovered",
		ObservedGeneration: dzd.Generation,
		LastTransitionTime: metav1.NewTime(time.Now()),
	})
	if err := upstreamCluster.GetClient().Status().Patch(ctx, &dzd, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager wires the controller with the multicluster manager.
func (r *DNSZoneDiscoveryReplicator) SetupWithManager(mgr mcmanager.Manager) error {
	r.mgr = mgr
	return mcbuilder.ControllerManagedBy(mgr).
		For(&dnsv1alpha1.DNSZoneDiscovery{}).
		Named("dnszonediscovery-replicator").
		Complete(r)
}
