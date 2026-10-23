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

// loopID names one configured control-plane loop. Region and shard are empty
// for loops that span every region.
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
		children := e.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if child != nil && !onlyConflicts(child) {
				return false
			}
		}
		return true
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
	loopRuns.WithLabelValues(id.Loop, id.Region, id.Shard, result).Inc()
	failures := loopConsecutiveFailures.WithLabelValues(id.Loop, id.Region, id.Shard)
	switch result {
	case "success":
		loopLastSuccess.WithLabelValues(id.Loop, id.Region, id.Shard).Set(float64(now.Unix()))
		failures.Set(0)
	case "error":
		failures.Inc()
	}

	operation := id.operation()
	if err == nil {
		operationLastSuccess.WithLabelValues(operation).Set(float64(now.Unix()))
		operationResults.WithLabelValues(operation, "success").Inc()
	} else {
		operationResults.WithLabelValues(operation, "error").Inc()
	}
}

// loopLiveness reports a periodic loop that has stopped completing runs, such
// as one blocked on a call that never returns. It does not judge whether runs
// succeed; a loop that fails every run is still alive.
type loopLiveness struct {
	mu    sync.Mutex
	now   func() time.Time
	loops map[loopID]loopTick
}

type loopTick struct {
	last  time.Time
	limit time.Duration
}

const minLoopStall = 2 * time.Minute

func newLoopLiveness() *loopLiveness {
	return &loopLiveness{now: time.Now, loops: map[loopID]loopTick{}}
}

func (l *loopLiveness) start(id loopID, interval time.Duration) {
	if l == nil {
		return
	}
	limit := max(3*interval, minLoopStall)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.loops[id] = loopTick{last: l.now(), limit: limit}
}

func (l *loopLiveness) tick(id loopID) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.loops[id]; ok {
		t.last = l.now()
		l.loops[id] = t
	}
}

func (l *loopLiveness) stop(id loopID) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.loops, id)
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
