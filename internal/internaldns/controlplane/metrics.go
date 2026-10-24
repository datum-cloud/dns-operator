// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var passPhaseDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "internal_dns_project_pass_phase_seconds",
	Help:    "Time a project pass spends in each phase, including the API calls it makes.",
	Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
}, []string{"phase"})

var passDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "internal_dns_project_pass_seconds",
	Help:    "Duration of whole project passes. Reaches past the 60 s ceiling of the controller-runtime reconcile histogram.",
	Buckets: []float64{.1, .25, .5, 1, 2.5, 5, 10, 20, 30, 60, 90, 120, 180, 300, 600},
})

func init() {
	metrics.Registry.MustRegister(passPhaseDuration, passDuration)
}

// slowPass is how long a project pass may take before it logs where its time
// went.
const slowPass = 10 * time.Second

type passTotals map[string]time.Duration

type passTotalsKey struct{}

type phaseTimer struct {
	last   time.Time
	totals passTotals
}

func newPhaseTimer(ctx context.Context) *phaseTimer {
	totals, _ := ctx.Value(passTotalsKey{}).(passTotals)
	return &phaseTimer{last: time.Now(), totals: totals}
}

func (t *phaseTimer) done(phase string) {
	now := time.Now()
	elapsed := now.Sub(t.last)
	passPhaseDuration.WithLabelValues(phase).Observe(elapsed.Seconds())
	if t.totals != nil {
		t.totals[phase] += elapsed
	}
	t.last = now
}

func (t passTotals) seconds() map[string]float64 {
	out := make(map[string]float64, len(t))
	for phase, d := range t {
		out[phase] = d.Round(time.Millisecond).Seconds()
	}
	return out
}
