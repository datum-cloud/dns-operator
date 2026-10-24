// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var zoneLeaseAcquisitions = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "internal_dns_zone_lease_acquisitions_total",
	Help: "Private zone lease acquisition attempts. result is held (this replica already holds the lease), created, taken_over, standby (a peer holds a live lease), conflict (lost an optimistic-lock race to a peer replica), or error.",
}, []string{"result"})

var zoneLeaseTakeovers = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "internal_dns_zone_lease_takeovers_total",
	Help: "Private zone leases this replica took from a different holder. reason is expired: the previous holder let its lease lapse.",
}, []string{"reason"})

var zoneLeaseRemaining = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "internal_dns_zone_lease_remaining_seconds",
	Help:    "Lease time left when this replica returns to a private zone it already holds. Zero or less means the lease had lapsed and a peer could have taken the zone.",
	Buckets: []float64{0, 1, 2, 5, 10, 15, 30, 60, 90, 120, 300},
})

func init() {
	metrics.Registry.MustRegister(zoneLeaseAcquisitions, zoneLeaseTakeovers, zoneLeaseRemaining)
}
