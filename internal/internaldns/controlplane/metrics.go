// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var passPhaseDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "internal_dns_project_pass_phase_seconds",
	Help:    "Time a project pass spends in each phase, including the API calls it makes.",
	Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
}, []string{"phase"})

func init() {
	metrics.Registry.MustRegister(passPhaseDuration)
}

type phaseTimer struct{ last time.Time }

func newPhaseTimer() *phaseTimer { return &phaseTimer{last: time.Now()} }

func (t *phaseTimer) done(phase string) {
	now := time.Now()
	passPhaseDuration.WithLabelValues(phase).Observe(now.Sub(t.last).Seconds())
	t.last = now
}
