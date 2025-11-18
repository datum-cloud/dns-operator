// SPDX-License-Identifier: AGPL-3.0-only
package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.miloapis.com/dns-operator/internal/discovery"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	// If already discovered, nothing to do.
	if getCondStatus(dzd.Status.Conditions, CondDiscovered) == metav1.ConditionTrue {
		return ctrl.Result{}, nil
	}

	// Validate reference
	if strings.TrimSpace(dzd.Spec.DNSZoneRef.Name) == "" {
		base := dzd.DeepCopy()
		if setCond(&dzd.Status.Conditions, CondAccepted, ReasonPending, "spec.dnsZoneRef.name is required", metav1.ConditionFalse, dzd.Generation) {
			if err := upstreamCluster.GetClient().Status().Patch(ctx, &dzd, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Fetch the referenced DNSZone
	var zone dnsv1alpha1.DNSZone
	if err := upstreamCluster.GetClient().Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: dzd.Spec.DNSZoneRef.Name}, &zone); err != nil {
		base := dzd.DeepCopy()
		msg := fmt.Sprintf("dnszone %q not found", dzd.Spec.DNSZoneRef.Name)
		if setCond(&dzd.Status.Conditions, CondAccepted, ReasonPending, msg, metav1.ConditionFalse, dzd.Generation) {
			_ = upstreamCluster.GetClient().Status().Patch(ctx, &dzd, client.MergeFrom(base))
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Mark Accepted true if not already
	if getCondStatus(dzd.Status.Conditions, CondAccepted) != metav1.ConditionTrue {
		base := dzd.DeepCopy()
		if setCond(&dzd.Status.Conditions, CondAccepted, ReasonAccepted, "discovery accepted", metav1.ConditionTrue, dzd.Generation) {
			if err := upstreamCluster.GetClient().Status().Patch(ctx, &dzd, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Perform discovery (one-shot)
	recordSets, err := discovery.DiscoverZoneRecords(ctx, zone.Spec.DomainName)
	if err != nil {
		logger.Error(err, "discovery failed; will retry", "zone", zone.Spec.DomainName)
		return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
	}

	// Log how many records were discovered
	logger.Info("discovered records", "count", len(recordSets))

	base := dzd.DeepCopy()
	dzd.Status.RecordSets = recordSets
	_ = setCond(&dzd.Status.Conditions, CondDiscovered, ReasonDiscovered, "zone records discovered", metav1.ConditionTrue, dzd.Generation)
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
