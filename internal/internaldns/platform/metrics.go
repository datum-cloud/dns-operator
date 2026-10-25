// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var outboxPublishAge = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "internal_dns_outbox_publish_age_seconds",
	Help:    "Time from a project pass generating an outbox message to the broker acknowledging it, by message kind.",
	Buckets: []float64{.1, .25, .5, 1, 2, 5, 10, 20, 30, 60, 120},
}, []string{"kind"})

var outboxSuperseded = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "internal_dns_outbox_superseded_total",
	Help: "Outbox messages marked superseded before they were published, by message kind.",
}, []string{"kind"})

func init() {
	metrics.Registry.MustRegister(outboxPublishAge, outboxSuperseded)
}
