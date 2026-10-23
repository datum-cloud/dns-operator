// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	outcomeServedZone            = "served_zone"
	outcomeForwardedNext         = "forwarded_next"
	outcomeForwardedPublic       = "forwarded_public"
	outcomeRefusedUnknownContext = "refused_unknown_context"
	outcomeRefusedBadDestination = "refused_bad_destination"
	outcomeRefusedUnsupported    = "refused_unsupported"
	outcomeRefusedTransfer       = "refused_transfer"
	outcomeServfailNoState       = "servfail_no_state"
	outcomeServfailContext       = "servfail_context_pending"
	outcomeServfailZone          = "servfail_zone_pending"
	outcomeServfailNoUpstream    = "servfail_no_upstream"
)

var contextQueries = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "internal_dns_context_queries_total",
	Help: "Queries by how the context plugin handled them.",
}, []string{"outcome"})

var snapshotContexts = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "internal_dns_context_snapshot_contexts",
	Help: "Contexts in the active snapshot, by whether they are ready or pending.",
}, []string{"state"})

var snapshotReloads = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "internal_dns_context_snapshot_reloads_total",
	Help: "Snapshot loads, by whether the snapshot was applied or rejected. A rejected snapshot leaves the previous one serving.",
}, []string{"result"})

var snapshotApplied = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "internal_dns_context_snapshot_applied_timestamp_seconds",
	Help: "Unix timestamp when the active snapshot was applied.",
})

func init() {
	prometheus.MustRegister(contextQueries, snapshotContexts, snapshotReloads, snapshotApplied)
	for _, outcome := range []string{outcomeServedZone, outcomeForwardedNext, outcomeForwardedPublic, outcomeRefusedUnknownContext,
		outcomeRefusedBadDestination, outcomeRefusedUnsupported, outcomeRefusedTransfer, outcomeServfailNoState,
		outcomeServfailContext, outcomeServfailZone, outcomeServfailNoUpstream} {
		contextQueries.WithLabelValues(outcome)
	}
	for _, result := range []string{"applied", "rejected"} {
		snapshotReloads.WithLabelValues(result)
	}
}

func observeSnapshot(s *snapshot) {
	ready, pending := 0, 0
	for _, c := range s.contexts {
		if c.pending {
			pending++
		} else {
			ready++
		}
	}
	snapshotContexts.WithLabelValues("ready").Set(float64(ready))
	snapshotContexts.WithLabelValues("pending").Set(float64(pending))
	snapshotApplied.SetToCurrentTime()
	snapshotReloads.WithLabelValues("applied").Inc()
}

// serveMetrics exposes /metrics, /health while the process runs, and /ready
// once a snapshot is loaded. It stops when ctx ends.
func serveMetrics(ctx context.Context, address string, p *ContextPlugin) (<-chan error, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen for metrics on %s: %w", address, err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		if p.state.Load() == nil {
			http.Error(w, "no snapshot loaded", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	return done, nil
}
