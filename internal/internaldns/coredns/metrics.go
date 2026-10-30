// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

const (
	outcomeDraining               = "draining"
	outcomeServedZone             = "served_zone"
	outcomeForwardedNext          = "forwarded_next"
	outcomeForwardedPublic        = "forwarded_public"
	outcomeRefusedUnknownContext  = "refused_unknown_context"
	outcomeRefusedBadDestination  = "refused_bad_destination"
	outcomeRefusedBadIdentity     = "refused_bad_identity"
	outcomeRefusedUnsupported     = "refused_unsupported"
	outcomeRefusedTransfer        = "refused_transfer"
	outcomeServfailNoState        = "servfail_no_state"
	outcomeServfailContext        = "servfail_context_pending"
	outcomeServfailZone           = "servfail_zone_pending"
	outcomeServfailNoUpstream     = "servfail_no_upstream"
	outcomeReadinessCanary        = "readiness_canary"
	outcomeServfailCanaryIdentity = "servfail_canary_bad_identity"
)

var contextQueries = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "internal_dns_context_queries_total",
	Help: "Queries by how the context plugin handled them.",
}, []string{"outcome"})

var outcomeQueries = map[string]prometheus.Counter{}

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
	for _, outcome := range []string{outcomeDraining, outcomeServedZone, outcomeForwardedNext, outcomeForwardedPublic, outcomeRefusedUnknownContext,
		outcomeRefusedBadDestination, outcomeRefusedBadIdentity, outcomeRefusedUnsupported, outcomeRefusedTransfer, outcomeServfailNoState,
		outcomeServfailContext, outcomeServfailZone, outcomeServfailNoUpstream, outcomeReadinessCanary, outcomeServfailCanaryIdentity} {
		outcomeQueries[outcome] = contextQueries.WithLabelValues(outcome)
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

// tenantFreeGatherer drops CoreDNS proxy families, which label every series
// with the upstream address. Node contexts forward to per-binding addresses,
// and those series outlive the binding.
type tenantFreeGatherer struct{ prometheus.Gatherer }

func (g tenantFreeGatherer) Gather() ([]*dto.MetricFamily, error) {
	families, err := g.Gatherer.Gather()
	kept := families[:0]
	for _, family := range families {
		if !strings.HasPrefix(family.GetName(), "coredns_proxy_") {
			kept = append(kept, family)
		}
	}
	return kept, err
}

func serveMetrics(ctx context.Context, address string, p *ContextPlugin) (<-chan error, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen for metrics on %s: %w", address, err)
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(tenantFreeGatherer{prometheus.DefaultGatherer}, promhttp.HandlerOpts{}))
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/ready", func(w http.ResponseWriter, _ *http.Request) {
		if p.state.Load() == nil || p.draining.Load() {
			http.Error(w, "no snapshot loaded or draining", http.StatusServiceUnavailable)
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
