// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	zoneLeaseResultConflict = "conflict"
	zoneLeaseResultError    = "error"
)

var zoneLeaseAcquisitionResults = []string{"held", "created", "taken_over", "standby", zoneLeaseResultConflict, zoneLeaseResultError}

var zoneLeaseAcquisitions = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "internal_dns_zone_lease_acquisitions_total",
	Help: "Private zone lease acquisition attempts. result is held (this replica already holds the lease), created, taken_over, standby (another replica holds a live lease), conflict (lost a create race with AlreadyExists, or lost the takeover patch to an optimistic-lock conflict), or error.",
}, []string{"result"})

var zoneLeaseTakeovers = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "internal_dns_zone_lease_takeovers_total",
	Help: "Private zone leases this replica took from a different holder after expiry or explicit release.",
})

var zoneLeaseRenewalResults = []string{"renewed", zoneLeaseResultConflict, zoneLeaseResultError}

var zoneLeaseRenewals = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "internal_dns_zone_lease_renewals_total",
	Help: "Writes that extend a held private zone lease. conflict means another writer changed the ownership object first, usually because the lease was lost mid-pass.",
}, []string{"result"})

var zoneLeaseRemainingPoints = []string{"acquire", "renew"}

var zoneLeaseRemaining = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "internal_dns_zone_lease_remaining_seconds",
	Help:    "Lease time left on a private zone this replica holds. point is acquire (start of the zone's pass) or renew (just before a write extends the lease). Zero or less means the lease had lapsed and another replica could take the zone.",
	Buckets: []float64{0, 1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900},
}, []string{"point"})

var zoneLeaseDuration = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "internal_dns_zone_lease_duration_seconds",
	Help: "Configured private zone lease length.",
})

func init() {
	metrics.Registry.MustRegister(zoneLeaseAcquisitions, zoneLeaseTakeovers, zoneLeaseRenewals, zoneLeaseRemaining, zoneLeaseDuration)
	for _, v := range zoneLeaseAcquisitionResults {
		zoneLeaseAcquisitions.WithLabelValues(v)
	}
	for _, v := range zoneLeaseRenewalResults {
		zoneLeaseRenewals.WithLabelValues(v)
	}
	for _, v := range zoneLeaseRemainingPoints {
		zoneLeaseRemaining.WithLabelValues(v)
	}
}

func (r *Reconciler) clock() time.Time {
	if r.Options.Now == nil {
		return time.Now()
	}
	return r.Options.Now()
}

func (r *Reconciler) patchLease(ctx context.Context, own, base *dnsv1alpha1.DNSPublicationOwnership) error {
	zoneLeaseRemaining.WithLabelValues("renew").Observe(base.Spec.LeaseUntil.Sub(r.clock()).Seconds())
	err := r.Patch(ctx, own, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	switch {
	case err == nil:
		zoneLeaseRenewals.WithLabelValues("renewed").Inc()
	case apierrors.IsConflict(err):
		zoneLeaseRenewals.WithLabelValues(zoneLeaseResultConflict).Inc()
	default:
		zoneLeaseRenewals.WithLabelValues(zoneLeaseResultError).Inc()
	}
	return err
}
