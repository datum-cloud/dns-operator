// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func TestHeartbeatRenewsTheLeaseWithAFewAcksBetweenRefreshes(t *testing.T) {
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
		AckInterval: 30 * time.Second, PublicationAckRefresh: 2 * time.Minute,
	}, Dependencies{Clock: func() time.Time { return now }, Runner: &recordingRunner{}, AckPublisher: publisher})
	if err != nil {
		t.Fatal(err)
	}
	zones := []string{"zone-a", "zone-b", "zone-c", "zone-d", "zone-e", "zone-f"}
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
	for i := 1; now.Sub(start) < 2*time.Minute-30*time.Second; i++ {
		now = start.Add(time.Duration(i) * 30 * time.Second)
		got := acked()
		if len(got) != leaseRenewalAcks {
			t.Fatalf("lease renewal %d acked %v, want %d publications", i, got, leaseRenewalAcks)
		}
		for _, uid := range got {
			seen[uid] = true
		}
	}
	if len(seen) != len(zones) {
		t.Fatalf("lease renewals rotated through %v, want every publication", seen)
	}
	for _, zone := range zones {
		agent.lastPublicationAck[zone] = start
	}
	now = start.Add(2 * time.Minute)
	if got := acked(); len(got) != len(zones) {
		t.Fatalf("refresh acked %v, want every publication not acked for a refresh period", got)
	}
}

func TestHeartbeatAcksOtherPublicationsWhenOneFailsToVerify(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
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
		UnsafeAllowNoFailClosed: true, UnsafeAllowNoDNSVerify: true, AckInterval: 30 * time.Second,
	}, Dependencies{Clock: func() time.Time { return now }, Runner: &recordingRunner{}, AckPublisher: publisher})
	if err != nil {
		t.Fatal(err)
	}
	agent.publicationVerifier = failingVerifier{zone: "zone-a"}
	binding := model.Binding{BindingUID: "binding"}
	for _, zone := range []string{"zone-a", "zone-b"} {
		binding.Zones = append(binding.Zones, model.ZoneAttachment{ZoneUID: zone, RequiredPublicationEpoch: 1, RequiredPublicationRevision: 1})
		p := publicationState{
			Fence:    fence{Epoch: 1, Revision: 1},
			Manifest: model.PublicationManifest{ManifestUID: "pub-" + zone, ZoneUID: zone, Apex: zone + ".example.", WriterEpoch: 1, Revision: 1},
			Plan:     model.PublicationPlan{ZoneUID: zone, Apex: zone + ".example."},
			Verified: true,
		}
		agent.state.Publications[zone] = p
		agent.installedPublications[zone] = installedPublication{Identity: publicationIdentity(p)}
	}
	agent.state.Snapshot = &model.ServingSnapshot{Bindings: []model.Binding{binding}}

	if err := agent.heartbeatPublications(context.Background()); err == nil {
		t.Fatal("a publication that failed verification was not reported")
	}
	phases := map[string]model.AckPhase{}
	for _, env := range publisher.envs {
		var ack model.MemberAck
		if err := json.Unmarshal(env.Payload, &ack); err != nil {
			t.Fatal(err)
		}
		phases[ack.ResourceUID] = ack.Phase
	}
	if phases["zone-a"] != model.AckRejected || phases["zone-b"] != model.AckVerified {
		t.Fatalf("acks = %v, want zone-a rejected and zone-b still verified", phases)
	}
}

type failingVerifier struct{ zone string }

func (v failingVerifier) VerifyPublication(_ context.Context, plan model.PublicationPlan, _ []PublicationView) error {
	if plan.ZoneUID == v.zone {
		return errors.New("probe failed")
	}
	return nil
}

func TestTombstonesRenewTheLeaseWhenNothingElseIsServed(t *testing.T) {
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
		UnsafeAllowNoFailClosed: true, UnsafeAllowNoDNSVerify: true, AckInterval: 30 * time.Second,
	}, Dependencies{Clock: func() time.Time { return now }, Runner: &recordingRunner{}, AckPublisher: publisher})
	if err != nil {
		t.Fatal(err)
	}
	agent.state.Publications["zone-gone"] = publicationState{
		Fence:    fence{Epoch: 1, Revision: 2},
		Manifest: model.PublicationManifest{ManifestUID: "pub-gone", ZoneUID: "zone-gone", WriterEpoch: 1, Revision: 2, Tombstone: true},
		Verified: true,
	}
	for i := range 4 {
		now = start.Add(time.Duration(i) * 30 * time.Second)
		before := len(publisher.envs)
		if err := agent.heartbeatPublications(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(publisher.envs)-before != 1 {
			t.Fatalf("interval %d sent %d acks; a member serving only a withdrawal must still renew its lease", i, len(publisher.envs)-before)
		}
	}
}

func TestPublicationAckRefreshIsBounded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	renderConfig, _ := renderFixture(t)
	renderConfig.Role = RenderNode
	renderConfig.NodeDNSDist.Path = filepath.Join(dir, "runtime", "node-dnsdist.conf")
	renderConfig.NodeCoreDNS.Path = filepath.Join(dir, "runtime", "node-coredns.json")
	renderConfig.ClusterDNSDist = DNSDistRenderConfig{}
	renderConfig.ClusterCoreDNS = CoreDNSRenderConfig{}
	for _, refresh := range []time.Duration{-time.Second, defaultPublicationAckRefresh + time.Second} {
		_, err := NewAgent(Config{
			Region: "east", Shard: "s1", MemberID: "regional-0", StateDir: filepath.Join(dir, "state"), Mode: ModeResolver,
			Render: renderConfig, Transaction: TransactionConfig{StageDir: filepath.Join(dir, "stage"), FailClosed: []Command{{Path: "gate"}}},
			UnsafeAllowNoFailClosed: true, UnsafeAllowNoDNSVerify: true, PublicationAckRefresh: refresh,
		}, Dependencies{Runner: &recordingRunner{}})
		if err == nil {
			t.Fatalf("refresh %v was accepted", refresh)
		}
	}
}

func TestDueWithdrawalsDoNotCrowdOutLiveRenewals(t *testing.T) {
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
		UnsafeAllowNoFailClosed: true, UnsafeAllowNoDNSVerify: true, AckInterval: 30 * time.Second, PublicationAckRefresh: 2 * time.Minute,
	}, Dependencies{Clock: func() time.Time { return now }, Runner: &recordingRunner{}, AckPublisher: publisher})
	if err != nil {
		t.Fatal(err)
	}
	agent.state.Publications["zone-live"] = publicationState{
		Fence:    fence{Epoch: 1, Revision: 1},
		Manifest: model.PublicationManifest{ManifestUID: "pub-live", ZoneUID: "zone-live", Apex: "live.example.", WriterEpoch: 1, Revision: 1},
		Plan:     model.PublicationPlan{ZoneUID: "zone-live", Apex: "live.example."},
		Verified: true,
	}
	agent.state.Snapshot = &model.ServingSnapshot{Bindings: []model.Binding{{BindingUID: "binding", Zones: []model.ZoneAttachment{{ZoneUID: "zone-live", RequiredPublicationEpoch: 1, RequiredPublicationRevision: 1}}}}}
	agent.lastPublicationAck["zone-live"] = start
	agent.lastLeaseAck = start
	for i := 1; i <= 3; i++ {
		now = start.Add(time.Duration(i) * 30 * time.Second)
		for j := range 3 {
			uid := "zone-gone-" + strconv.Itoa(i) + "-" + strconv.Itoa(j)
			agent.state.Publications[uid] = publicationState{
				Fence:    fence{Epoch: 1, Revision: 2},
				Manifest: model.PublicationManifest{ManifestUID: "pub-" + uid, ZoneUID: uid, WriterEpoch: 1, Revision: 2, Tombstone: true},
				Verified: true,
			}
		}
		before := len(publisher.envs)
		if err := agent.heartbeatPublications(context.Background()); err != nil {
			t.Fatal(err)
		}
		live := false
		for _, env := range publisher.envs[before:] {
			if env.ResourceUID == "zone-live" {
				live = true
			}
		}
		if !live {
			t.Fatalf("interval %d renewed the lease only with withdrawals", i)
		}
	}
}
