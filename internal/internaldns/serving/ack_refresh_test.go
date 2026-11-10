// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func TestHeartbeatRenewsTheLeaseWithOneAckBetweenRefreshes(t *testing.T) {
	t.Parallel()
	start := time.Now().UTC()
	now := start
	dir := t.TempDir()
	renderConfig, _ := renderFixture(t)
	renderConfig.Role = RenderNode
	renderConfig.NodeDNSDist.Path = filepath.Join(dir, "runtime", "node-dnsdist.conf")
	renderConfig.NodeCoreDNS.Path = filepath.Join(dir, "runtime", "node-coredns.json")
	renderConfig.ClusterDNSDist = DNSDistRenderConfig{}
	renderConfig.ClusterCoreDNS = CoreDNSRenderConfig{}
	publisher := &fakePublisher{}
	agent, err := NewAgent(Config{
		Region: "east", Shard: "s1", MemberID: "regional-0", StateDir: filepath.Join(dir, "state"), Mode: ModeResolver,
		Render: renderConfig, Transaction: TransactionConfig{StageDir: filepath.Join(dir, "stage"), FailClosed: []Command{{Path: "gate"}}},
		UnsafeAllowNoFailClosed: true, UnsafeAllowNoDNSVerify: true,
		AckInterval: 30 * time.Second, PublicationAckRefresh: 4 * time.Minute,
	}, Dependencies{Clock: func() time.Time { return now }, Runner: &recordingRunner{}, AckPublisher: publisher})
	if err != nil {
		t.Fatal(err)
	}
	zones := []string{"zone-a", "zone-b", "zone-c"}
	binding := model.Binding{BindingUID: "binding"}
	for _, zone := range zones {
		binding.Zones = append(binding.Zones, model.ZoneAttachment{ZoneUID: zone, RequiredPublicationEpoch: 1, RequiredPublicationRevision: 1})
		agent.state.Publications[zone] = publicationState{
			Fence:    fence{Epoch: 1, Revision: 1},
			Manifest: model.PublicationManifest{ManifestUID: "pub-" + zone, ZoneUID: zone, Apex: zone + ".example.", WriterEpoch: 1, Revision: 1},
			Plan:     model.PublicationPlan{ZoneUID: zone, Apex: zone + ".example."},
			Verified: true,
		}
	}
	agent.state.Snapshot = &model.ServingSnapshot{Bindings: []model.Binding{binding}}

	acked := func() []string {
		t.Helper()
		before := len(publisher.envs)
		if err := agent.heartbeatPublications(context.Background()); err != nil {
			t.Fatal(err)
		}
		uids := make([]string, 0, len(publisher.envs)-before)
		for _, env := range publisher.envs[before:] {
			var ack model.MemberAck
			if err := json.Unmarshal(env.Payload, &ack); err != nil {
				t.Fatal(err)
			}
			if ack.Phase != model.AckVerified || ack.Kind != model.KindPublicationManifest {
				t.Fatalf("heartbeat sent %s %s, want a verified publication ack", ack.Kind, ack.Phase)
			}
			uids = append(uids, ack.ResourceUID)
		}
		return uids
	}

	if got := acked(); len(got) != len(zones) {
		t.Fatalf("first heartbeat acked %v, want every publication once", got)
	}
	now = now.Add(time.Second)
	if got := acked(); len(got) != 0 {
		t.Fatalf("a heartbeat inside the ACK interval acked %v", got)
	}
	seen := map[string]bool{}
	for i := 1; now.Sub(start) < 4*time.Minute-30*time.Second; i++ {
		now = start.Add(time.Duration(i) * 30 * time.Second)
		got := acked()
		if len(got) != 1 {
			t.Fatalf("lease renewal %d acked %v, want exactly one publication", i, got)
		}
		seen[got[0]] = true
	}
	if len(seen) != len(zones) {
		t.Fatalf("lease renewals rotated through %v, want every publication", seen)
	}
	for _, zone := range zones {
		agent.lastPublicationAck[zone] = start
	}
	now = start.Add(4 * time.Minute)
	if got := acked(); len(got) != len(zones) {
		t.Fatalf("refresh acked %v, want every publication not acked for a refresh period", got)
	}
}
