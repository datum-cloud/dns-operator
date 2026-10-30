// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var agentDeliveryConnected = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "internal_dns_agent_delivery_connected",
	Help: "Whether the serving agent currently has a connected regional state-delivery transport.",
}, []string{"region", "shard", "mode"})

var agentPublicationLaggingZones = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "internal_dns_agent_publication_lagging_zones",
	Help: "Attached zones whose locally applied publication is absent, unverified, withdrawn, or below the desired fence.",
}, []string{"region", "shard", "mode"})

var agentPublicationRevisionLag = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "internal_dns_agent_publication_revision_lag",
	Help: "Largest desired-minus-applied publication revision among lagging attached zones in the same writer epoch; use lagging_zones for missing or changed epochs.",
}, []string{"region", "shard", "mode"})

var agentActivationFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "internal_dns_agent_activation_failures_total",
	Help: "Serving configuration activations that failed installation or DNS verification.",
}, []string{"region", "shard", "mode", "stage"})

var agentLastActivationSuccess = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "internal_dns_agent_last_activation_success_timestamp_seconds",
	Help: "Unix timestamp of the last locally verified serving configuration activation.",
}, []string{"region", "shard", "mode"})

func init() {
	metrics.Registry.MustRegister(agentDeliveryConnected, agentPublicationLaggingZones, agentPublicationRevisionLag, agentActivationFailures, agentLastActivationSuccess)
}

func agentMode(c Config) string {
	if c.NodeAuthoritative {
		return "node-authoritative"
	}
	if c.Mode == ModeCombined {
		return "regional-authoritative"
	}
	return "resolver"
}

func (a *Agent) metricLabels() (string, string, string) {
	return a.cfg.Region, a.cfg.Shard, agentMode(a.cfg)
}

func (a *Agent) SetDeliveryConnected(connected bool) {
	value := 0.0
	if connected {
		value = 1
	}
	region, shard, mode := a.metricLabels()
	agentDeliveryConnected.WithLabelValues(region, shard, mode).Set(value)
}

func (a *Agent) observePublicationLag() {
	lagging := 0
	var revisionLag uint64
	type desiredPublication struct {
		fence     fence
		pending   bool
		tombstone bool
	}
	desired := map[string]desiredPublication{}
	if a.state.Snapshot != nil {
		for _, binding := range a.state.Snapshot.Bindings {
			if binding.Tombstone || binding.ConfigurationPending {
				continue
			}
			for _, zone := range binding.Zones {
				current, ok := desired[zone.ZoneUID]
				if !ok || zone.Pending || !current.pending && model.Compare(zone.RequiredPublicationEpoch, zone.RequiredPublicationRevision, current.fence.Epoch, current.fence.Revision) > 0 {
					desired[zone.ZoneUID] = desiredPublication{fence: fence{Epoch: zone.RequiredPublicationEpoch, Revision: zone.RequiredPublicationRevision}, pending: zone.Pending}
				}
			}
		}
	}
	// A binding's required publication is its initial activation floor. Ordinary
	// record updates deliberately do not rewrite that floor, so the latest
	// accepted publication fence is the desired watermark after activation.
	for uid, want := range desired {
		if received, ok := a.state.Publications[uid]; ok && model.Compare(received.Fence.Epoch, received.Fence.Revision, want.fence.Epoch, want.fence.Revision) >= 0 {
			want.fence = received.Fence
			want.tombstone = received.Manifest.Tombstone
			desired[uid] = want
		}
	}
	for uid, want := range desired {
		var publication publicationState
		ok := false
		if a.state.Active != nil {
			publication, ok = a.state.Active.Publications[uid]
		}
		comparison := model.Compare(publication.Fence.Epoch, publication.Fence.Revision, want.fence.Epoch, want.fence.Revision)
		if want.pending || !ok || !publication.Verified || comparison < 0 || comparison == 0 && publication.Manifest.Tombstone != want.tombstone {
			lagging++
			if ok && publication.Fence.Epoch == want.fence.Epoch && publication.Fence.Revision < want.fence.Revision {
				revisionLag = max(revisionLag, want.fence.Revision-publication.Fence.Revision)
			}
		}
	}
	region, shard, mode := a.metricLabels()
	agentPublicationLaggingZones.WithLabelValues(region, shard, mode).Set(float64(lagging))
	agentPublicationRevisionLag.WithLabelValues(region, shard, mode).Set(float64(revisionLag))
}

func (a *Agent) recordActivationFailure(stage string) {
	region, shard, mode := a.metricLabels()
	agentActivationFailures.WithLabelValues(region, shard, mode, stage).Inc()
}

func (a *Agent) recordActivationSuccess(at time.Time) {
	region, shard, mode := a.metricLabels()
	agentLastActivationSuccess.WithLabelValues(region, shard, mode).Set(float64(at.Unix()))
}
