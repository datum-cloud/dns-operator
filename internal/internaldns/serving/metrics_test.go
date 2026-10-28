// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func TestAgentPublicationLagMetricsAggregateZones(t *testing.T) {
	a := &Agent{
		cfg: Config{Region: "metrics-region", Shard: "metrics-shard", Mode: ModeCombined, NodeAuthoritative: true},
		state: checkpoint{
			Snapshot: &model.ServingSnapshot{Bindings: []model.Binding{
				{Zones: []model.ZoneAttachment{
					{ZoneUID: "behind", RequiredPublicationEpoch: 2, RequiredPublicationRevision: 7},
					{ZoneUID: "missing", RequiredPublicationEpoch: 3, RequiredPublicationRevision: 1},
				}},
				{Zones: []model.ZoneAttachment{{ZoneUID: "behind", RequiredPublicationEpoch: 2, RequiredPublicationRevision: 8}}},
			}},
			Publications: map[string]publicationState{
				"behind": {Fence: fence{Epoch: 2, Revision: 8}, Verified: true},
			},
			Active: &activeConfiguration{Publications: map[string]publicationState{
				"behind": {Fence: fence{Epoch: 2, Revision: 4}, Verified: true},
			}},
		},
	}
	a.observePublicationLag()
	labels := []string{"metrics-region", "metrics-shard", "node-authoritative"}
	if got := testutil.ToFloat64(agentPublicationLaggingZones.WithLabelValues(labels...)); got != 2 {
		t.Fatalf("lagging zones = %v, want 2", got)
	}
	if got := testutil.ToFloat64(agentPublicationRevisionLag.WithLabelValues(labels...)); got != 4 {
		t.Fatalf("revision lag = %v, want 4", got)
	}
}

func TestAgentPublicationLagUsesActivatedNotReceivedRevision(t *testing.T) {
	a := &Agent{
		cfg: Config{Region: "active-region", Shard: "active-shard", Mode: ModeCombined, NodeAuthoritative: true},
		state: checkpoint{
			Snapshot:     &model.ServingSnapshot{Bindings: []model.Binding{{Zones: []model.ZoneAttachment{{ZoneUID: "zone", RequiredPublicationEpoch: 4, RequiredPublicationRevision: 9}}}}},
			Publications: map[string]publicationState{"zone": {Fence: fence{Epoch: 4, Revision: 9}, Verified: true}},
			Active:       &activeConfiguration{Publications: map[string]publicationState{"zone": {Fence: fence{Epoch: 4, Revision: 6}, Verified: true}}},
		},
	}
	a.observePublicationLag()
	labels := []string{"active-region", "active-shard", "node-authoritative"}
	if got := testutil.ToFloat64(agentPublicationLaggingZones.WithLabelValues(labels...)); got != 1 {
		t.Fatalf("lagging zones = %v, want 1", got)
	}
	if got := testutil.ToFloat64(agentPublicationRevisionLag.WithLabelValues(labels...)); got != 3 {
		t.Fatalf("revision lag = %v, want 3", got)
	}
}
