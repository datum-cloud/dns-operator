package runtime

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var operationResults = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "internal_dns_operations_total",
	Help: "Deprecated: use internal_dns_loop_runs_total. Completed internal DNS operations by configured loop and result.",
}, []string{"operation", "result"})

var operationLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "internal_dns_operation_last_success_timestamp_seconds",
	Help: "Deprecated: use internal_dns_loop_last_success_timestamp_seconds. Unix timestamp of the latest successful internal DNS operation.",
}, []string{"operation"})

var loopRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "internal_dns_loop_runs_total",
	Help: "Completed control-plane loop runs. result is success, conflict (lost an optimistic-lock race to a peer replica), or error.",
}, []string{"loop", "region", "shard", "result"})

var loopDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "internal_dns_loop_duration_seconds",
	Help:    "Duration of control-plane loop runs.",
	Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
}, []string{"loop", "region"})

var loopLastSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "internal_dns_loop_last_success_timestamp_seconds",
	Help: "Unix timestamp of the latest successful control-plane loop run.",
}, []string{"loop", "region", "shard"})

var loopConsecutiveFailures = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "internal_dns_loop_consecutive_failures",
	Help: "Failed runs since the loop last succeeded. Conflicts are not failures.",
}, []string{"loop", "region", "shard"})

func init() {
	metrics.Registry.MustRegister(operationResults, operationLastSuccess, loopRuns, loopDuration, loopLastSuccess, loopConsecutiveFailures)
}

type loopID struct {
	Loop, Region, Shard string
}

func (l loopID) operation() string {
	switch {
	case l.Shard != "":
		return fmt.Sprintf("%s %s/%s", l.Loop, l.Region, l.Shard)
	case l.Region != "":
		return l.Loop + " " + l.Region
	default:
		return l.Loop
	}
}

func loopResult(err error) string {
	switch {
	case err == nil:
		return "success"
	case onlyConflicts(err):
		return "conflict"
	default:
		return "error"
	}
}

// onlyConflicts is true when every error joined into err is an optimistic-lock
// conflict. A pass that also hit a real failure is an error.
func onlyConflicts(err error) bool {
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		found := false
		for _, child := range e.Unwrap() {
			if child == nil {
				continue
			}
			if !onlyConflicts(child) {
				return false
			}
			found = true
		}
		return found
	case interface{ Unwrap() error }:
		if apierrors.IsConflict(err) && !errors.As(err, new(interface{ Unwrap() []error })) {
			return true
		}
		return onlyConflicts(e.Unwrap())
	default:
		return err != nil && apierrors.IsConflict(err)
	}
}

func observeLoop(id loopID, started time.Time, err error) {
	loopDuration.WithLabelValues(id.Loop, id.Region).Observe(time.Since(started).Seconds())
	countLoop(id, err)
}

// countLoop records a run without its duration, for steps that mostly wait
// on the broker and would otherwise report idle time as work.
func countLoop(id loopID, err error) {
	result := loopResult(err)
	now := time.Now()
	for _, r := range []string{"success", "conflict", "error"} {
		loopRuns.WithLabelValues(id.Loop, id.Region, id.Shard, r)
	}
	loopRuns.WithLabelValues(id.Loop, id.Region, id.Shard, result).Inc()
	failures := loopConsecutiveFailures.WithLabelValues(id.Loop, id.Region, id.Shard)
	lastSuccess := loopLastSuccess.WithLabelValues(id.Loop, id.Region, id.Shard)
	switch result {
	case "success":
		lastSuccess.Set(float64(now.Unix()))
		failures.Set(0)
	case "error":
		failures.Inc()
	}

	operation := id.operation()
	operationSuccess := operationLastSuccess.WithLabelValues(operation)
	if err == nil {
		operationSuccess.Set(float64(now.Unix()))
		operationResults.WithLabelValues(operation, "success").Inc()
	} else {
		operationResults.WithLabelValues(operation, "error").Inc()
	}
}

// loopLiveness fails only for a loop that stops completing runs; a loop that
// fails every run is still alive.
type loopLiveness struct {
	mu     sync.Mutex
	now    func() time.Time
	loops  map[loopID]loopTick
	starts uint64
}

type loopTick struct {
	run   uint64
	last  time.Time
	limit time.Duration
}

const minLoopStall = 10 * time.Minute

func newLoopLiveness() *loopLiveness {
	return &loopLiveness{now: time.Now, loops: map[loopID]loopTick{}}
}

// start returns the run that tick and stop must name, so a restarted loop's
// predecessor cannot refresh or remove the new run's entry.
func (l *loopLiveness) start(id loopID, interval time.Duration) uint64 {
	if l == nil {
		return 0
	}
	limit := max(3*interval, minLoopStall)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.starts++
	l.loops[id] = loopTick{run: l.starts, last: l.now(), limit: limit}
	return l.starts
}

func (l *loopLiveness) tick(id loopID, run uint64) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.loops[id]; ok && t.run == run {
		t.last = l.now()
		l.loops[id] = t
	}
}

func (l *loopLiveness) stop(id loopID, run uint64) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.loops[id]; ok && t.run == run {
		delete(l.loops, id)
	}
}

func (l *loopLiveness) check(*http.Request) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	var stalled []error
	for id, t := range l.loops {
		if age := now.Sub(t.last); age > t.limit {
			stalled = append(stalled, fmt.Errorf("loop %q has not completed a run in %s", id.operation(), age.Truncate(time.Second)))
		}
	}
	return errors.Join(stalled...)
}
