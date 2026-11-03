// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var passPhaseDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "internal_dns_project_pass_phase_seconds",
	Help:    "Duration of each reached project-pass phase or zone-compile step, including failed attempts and API calls. Zone steps also run independently in per-zone mode.",
	Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
}, []string{"phase"})

var passDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "internal_dns_project_pass_seconds",
	Help:    "Duration of project passes, including failed attempts; excludes queue and slot waits, identity verification, and independent zone reconciles.",
	Buckets: []float64{.1, .25, .5, 1, 2.5, 5, 10, 20, 30, 60, 90, 120, 180, 300, 600},
})

func init() {
	metrics.Registry.MustRegister(passPhaseDuration, passDuration)
}

// Only fixed phase names from the controller are used: no project, zone or
// resource labels. A deferred finish includes errors in the current phase;
// phases that were never reached produce no observations.
type phaseTimer struct {
	phase   string
	started time.Time
}

func newPhaseTimer(phase string) *phaseTimer {
	return &phaseTimer{phase: phase, started: time.Now()}
}

func (t *phaseTimer) next(phase string) {
	now := time.Now()
	t.observe(now)
	t.phase, t.started = phase, now
}

func (t *phaseTimer) finish() { t.observe(time.Now()) }

func (t *phaseTimer) observe(now time.Time) {
	passPhaseDuration.WithLabelValues(t.phase).Observe(now.Sub(t.started).Seconds())
}
