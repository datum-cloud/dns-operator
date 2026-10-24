// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	contextdns "go.miloapis.com/dns-operator/internal/internaldns/coredns"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func TestRejectedExpansionPreservesServingAndRestarts(t *testing.T) {
	now := time.Now().UTC()
	runner := &recordingRunner{}
	cfg := regionalTestConfig(t)
	agent, err := NewAgent(cfg, Dependencies{Runner: runner, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, agent, now)
	activeHash := agent.state.RenderedHash
	candidate := *agent.state.Snapshot
	candidate.ConfigurationRevision++
	candidate.Bindings[0].ConfigurationRevision++
	candidate.Bindings[0].Zones = append(candidate.Bindings[0].Zones, model.ZoneAttachment{ZoneUID: "pending-zone", Apex: "pending.internal", Pending: true, RequiredPublicationEpoch: 1, RequiredPublicationRevision: 1})
	runner.failAt = len(runner.calls) + 1
	if err := agent.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", candidate.ConfigurationEpoch, candidate.ConfigurationRevision, candidate)); err == nil {
		t.Fatal("invalid expansion was accepted")
	}
	if agent.state.RenderedHash != activeHash || agent.state.Active == nil {
		t.Fatal("invalid expansion interrupted the active configuration")
	}
	now = now.Add(time.Hour)
	if !installedFilesMatch(agent.state.Active.Files) {
		t.Fatal("rejected update changed the approved files")
	}
	restarted, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.restoreActive(context.Background()); err != nil {
		t.Fatalf("restart could not recover active configuration: %v", err)
	}
	withdrawn := candidate
	withdrawn.ConfigurationRevision++
	withdrawn.Bindings = nil
	runner.failAt = len(runner.calls) + 1
	if err := agent.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", withdrawn.ConfigurationEpoch, withdrawn.ConfigurationRevision, withdrawn)); err == nil {
		t.Fatal("failed withdrawal was accepted")
	}
	if agent.state.Active != nil {
		t.Fatal("failed withdrawal retained revoked access")
	}
}

func TestPendingZoneIsIsolatedFromReadyZone(t *testing.T) {
	now := time.Now().UTC()
	cfg := regionalTestConfig(t)
	agent, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, agent, now)
	pub := &fakePublisher{}
	agent.deps.AckPublisher = pub
	now = now.Add(time.Minute)
	binding := agent.state.Snapshot.Bindings[0]
	binding.Zones = append(binding.Zones, model.ZoneAttachment{ZoneUID: "pending", Apex: "pending.internal", Pending: true, RequiredPublicationEpoch: 1, RequiredPublicationRevision: 1})
	if zones := agent.readyZones(binding); len(zones) != 1 || zones[0].ZoneUID != "zone-a" {
		t.Fatalf("pending zone suppressed healthy zone: %#v", zones)
	}
	candidate := *agent.state.Snapshot
	candidate.ConfigurationRevision++
	candidate.Bindings[0] = binding
	candidate.Bindings[0].ConfigurationRevision++
	if err := agent.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", candidate.ConfigurationEpoch, candidate.ConfigurationRevision, candidate)); err != nil {
		t.Fatal(err)
	}
	if agent.state.Active == nil || agent.publicationsReady(binding) {
		t.Fatal("partial serving state was not distinguished from full readiness")
	}
	if zone := currentZone(t, agent); zone == "" {
		t.Fatal("ready zone was not materialized")
	}
	verified := false
	for _, env := range pub.envs {
		var ack model.MemberAck
		if err := json.Unmarshal(env.Payload, &ack); err != nil {
			t.Fatal(err)
		}
		if ack.Kind == model.KindServingSnapshot && ack.Revision == candidate.ConfigurationRevision && ack.Phase == model.AckVerified {
			verified = true
		}
	}
	if !verified {
		t.Fatal("pending zone prevented resolver access from becoming ready")
	}
}

type blockedValidationRunner struct {
	recordingRunner
	blocked bool
	started chan struct{}
	release chan struct{}
}

func (r *blockedValidationRunner) Run(ctx context.Context, command Command) ([]byte, error) {
	if command.Path == "validate" && r.blocked {
		close(r.started)
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return r.recordingRunner.Run(ctx, command)
}

func TestApprovedConfigurationRemainsInstalledDuringCandidateValidation(t *testing.T) {
	now := time.Now().UTC()
	runner := &blockedValidationRunner{started: make(chan struct{}), release: make(chan struct{})}
	cfg := regionalTestConfig(t)
	agent, err := NewAgent(cfg, Dependencies{Runner: runner, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, agent, now)
	runner.blocked = true
	candidate := *agent.state.Snapshot
	candidate.ConfigurationRevision++
	candidate.Bindings[0].ConfigurationRevision++
	candidate.Bindings[0].Zones = append(candidate.Bindings[0].Zones, model.ZoneAttachment{ZoneUID: "pending", Apex: "pending.internal", Pending: true, RequiredPublicationEpoch: 1, RequiredPublicationRevision: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- agent.Handle(ctx, envelope(t, model.KindServingSnapshot, "shard", candidate.ConfigurationEpoch, candidate.ConfigurationRevision, candidate))
	}()
	select {
	case <-runner.started:
	case <-ctx.Done():
		t.Fatal("candidate validation did not start")
	}
	if !installedFilesMatch(agent.state.Active.Files) || activationPending(cfg.StateDir) {
		t.Fatal("candidate validation interrupted the approved configuration")
	}
	close(runner.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestFailedRestartInstallationRequiresActivation(t *testing.T) {
	cfg := regionalTestConfig(t)
	agent, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, agent, time.Now().UTC())
	for path := range agent.state.Active.Files {
		if err := os.WriteFile(path, []byte("incomplete configuration"), 0644); err != nil {
			t.Fatal(err)
		}
		break
	}
	runner := &recordingRunner{failAt: 1}
	restarted, err := NewAgent(cfg, Dependencies{Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.restoreActive(context.Background()); err == nil {
		t.Fatal("failed installation was accepted")
	}
	if !restarted.needsActivation {
		t.Fatal("failed installation must force daemon activation on retry")
	}
	runner.failAt = 0
	if err := restarted.restoreActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restarted.needsActivation || !installedFilesMatch(restarted.state.Active.Files) {
		t.Fatal("successful retry did not restore approved configuration")
	}
}

func nodeRestartConfig(t *testing.T) Config {
	t.Helper()
	cfg := regionalTestConfig(t)
	render, _ := renderFixture(t)
	render.Role = RenderNode
	runtimeDir := filepath.Join(filepath.Dir(cfg.StateDir), "runtime")
	render.NodeCoreDNS.Path = filepath.Join(runtimeDir, "node-coredns.json")
	render.NodeDNSDist.Path = filepath.Join(runtimeDir, "node-dnsdist.conf")
	cfg.Render = render
	cfg.Mode = ModeResolver
	cfg.MemberID, cfg.ReplicaID = "node-0", "node-0"
	return cfg
}

func TestRestartedNodeServesZoneFromPersistedProofBeforeNewAck(t *testing.T) {
	for _, tc := range []struct {
		name   string
		phase  model.AckPhase
		fence  fence
		served bool
	}{
		{name: "verified current publication", phase: model.AckVerified, fence: fence{2, 4}, served: true},
		{name: "rejected publication", phase: model.AckRejected, fence: fence{2, 4}},
		{name: "proof below required publication", phase: model.AckVerified, fence: fence{1, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			clock := now
			cfg := nodeRestartConfig(t)
			ctx := context.Background()
			agent, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return clock }})
			if err != nil {
				t.Fatal(err)
			}
			snapshot := testSnapshot(now)
			if err := agent.Handle(ctx, envelope(t, model.KindServingSnapshot, "shard", 1, 1, snapshot)); err != nil {
				t.Fatal(err)
			}
			chunk, manifest := testPublication(t, now)
			if err := agent.Handle(ctx, envelope(t, model.KindPublicationChunk, "zone-a", 2, 4, chunk)); err != nil {
				t.Fatal(err)
			}
			if err := agent.Handle(ctx, envelope(t, model.KindPublicationManifest, "zone-a", 2, 4, manifest)); err != nil {
				t.Fatal(err)
			}
			proof := model.MemberAck{MemberID: "regional-0", ReplicaID: "regional-0", ResourceUID: "zone-a", Kind: model.KindPublicationManifest, Epoch: tc.fence.Epoch, Revision: tc.fence.Revision, Phase: tc.phase, ObservedAt: now, ValidUntil: now.Add(time.Minute)}
			if err := agent.Handle(ctx, envelope(t, model.KindMemberAck, "zone-a", tc.fence.Epoch, tc.fence.Revision, proof)); err != nil {
				t.Fatal(err)
			}
			servfail := fmt.Sprintf("appendRule(AndRule({TagRule(\"dns-context\",%q),QNameSuffixRule(%q)}),RCodeAction(DNSRCode.SERVFAIL))", snapshot.Bindings[0].ViewName(), model.AbsoluteName("prod.internal"))
			assertZone := func(step string) {
				t.Helper()
				installed, err := os.ReadFile(cfg.Render.NodeDNSDist.Path)
				if err != nil {
					t.Fatal(err)
				}
				if gated := strings.Contains(string(installed), servfail); gated == tc.served {
					t.Fatalf("%s: zone gated=%t, want served=%t", step, gated, tc.served)
				}
			}
			assertZone("before restart")

			clock = now.Add(30 * time.Second)
			runner := &recordingRunner{}
			restarted, err := NewAgent(cfg, Dependencies{Runner: runner, Clock: func() time.Time { return clock }})
			if err != nil {
				t.Fatal(err)
			}
			if err := restarted.restoreActive(ctx); err != nil {
				t.Fatal(err)
			}
			if err := restarted.Step(ctx); err != nil {
				t.Fatal(err)
			}
			assertZone("after restart without a new ACK")
			if gateCalls(runner) != 0 {
				t.Fatalf("restart gated approved serving: %#v", runner.calls)
			}
		})
	}
}

func TestRestartWaitsForStartingDaemonsInsteadOfGating(t *testing.T) {
	cfg := nodeRestartConfig(t)
	coreDNSListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dnsdistTCP, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dnsdistUDP, err := net.ListenPacket("udp", dnsdistTCP.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	coreDNSPort := coreDNSListener.Addr().(*net.TCPAddr).Port
	cfg.Render.NodeCoreDNS.ListenAddress = "127.0.0.1"
	cfg.Render.NodeCoreDNS.Port = uint16(coreDNSPort)
	cfg.Render.NodeDNSDist.ListenAddress = dnsdistTCP.Addr().String()
	agent, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := testSnapshot(time.Now().UTC())
	if err := agent.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 1, snapshot)); err != nil {
		t.Fatal(err)
	}
	if agent.state.Active == nil || !installedFilesMatch(agent.state.Active.Files) {
		t.Fatal("fixture did not approve and install a configuration")
	}

	const startDelay = 500 * time.Millisecond
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		_ = w.WriteMsg(readinessCanaryReply(request, dns.RcodeSuccess, contextdns.ReadinessCanaryText))
	})
	udpServer := &dns.Server{PacketConn: dnsdistUDP, Handler: handler}
	tcpServer := &dns.Server{Listener: dnsdistTCP, Handler: handler}
	t.Cleanup(func() { _ = udpServer.Shutdown(); _ = tcpServer.Shutdown(); _ = coreDNSListener.Close() })
	go func() {
		time.Sleep(startDelay)
		go func() { _ = udpServer.ActivateAndServe() }()
		go func() { _ = tcpServer.ActivateAndServe() }()
		serveProxiedTCPDNS(coreDNSListener)
	}()

	cfg.UnsafeAllowNoDNSVerify = false
	runner := &recordingRunner{}
	restarted, err := NewAgent(cfg, Dependencies{Runner: runner, Verifier: resolverVerifierFunc(func(context.Context, model.Binding) error { return nil })})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := restarted.restoreActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 || restarted.needsActivation {
		t.Fatalf("restart gated and reactivated daemons that were still starting: %#v", runner.calls)
	}
	if time.Since(started) < startDelay/2 {
		t.Fatal("restore did not observe the daemons starting")
	}
}

func serveProxiedTCPDNS(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			header := make([]byte, 16)
			if _, err := io.ReadFull(conn, header); err != nil {
				return
			}
			if _, err := io.ReadFull(conn, make([]byte, binary.BigEndian.Uint16(header[14:16]))); err != nil {
				return
			}
			size := make([]byte, 2)
			if _, err := io.ReadFull(conn, size); err != nil {
				return
			}
			wire := make([]byte, binary.BigEndian.Uint16(size))
			if _, err := io.ReadFull(conn, wire); err != nil {
				return
			}
			request := new(dns.Msg)
			if request.Unpack(wire) != nil {
				return
			}
			reply, err := readinessCanaryReply(request, dns.RcodeSuccess, contextdns.ReadinessCanaryText).Pack()
			if err != nil {
				return
			}
			_, _ = conn.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(reply))), reply...))
		}()
	}
}

func TestRestoreReportsDaemonsThatNeverAnswer(t *testing.T) {
	cfg := regionalTestConfig(t)
	agent, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, agent, time.Now().UTC())

	cfg.UnsafeAllowNoDNSVerify = false
	cfg.Render.ClusterCoreDNS.ListenAddress = "127.0.0.1"
	cfg.Render.ClusterCoreDNS.Port = 1
	now := time.Now()
	var reported []error
	runner := &recordingRunner{}
	restarted, err := NewAgent(cfg, Dependencies{
		Runner:              runner,
		PublicationVerifier: publicationVerifierFunc(func(context.Context, model.PublicationPlan, []PublicationView) error { return nil }),
		Clock:               func() time.Time { now = now.Add(localDaemonStartGrace / 2); return now },
		OnError:             func(err error) { reported = append(reported, err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.restoreActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(reported) != 1 || !strings.Contains(reported[0].Error(), "did not answer within") {
		t.Fatalf("expired daemon wait was not reported: %v", reported)
	}
	if len(runner.calls) == 0 {
		t.Fatal("expired daemon wait did not reapply the approved configuration")
	}
}

type bootstrapFunc func(context.Context) ([]model.Envelope, error)

func (f bootstrapFunc) Load(ctx context.Context) ([]model.Envelope, error) { return f(ctx) }

func TestRestoredCheckpointIsWithdrawnWithoutState(t *testing.T) {
	cfg := regionalTestConfig(t)
	agent, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, agent, time.Now().UTC())

	now := time.Now()
	runner := &recordingRunner{}
	restarted, err := NewAgent(cfg, Dependencies{Runner: runner, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := restarted.restoreActive(ctx); err != nil {
		t.Fatal(err)
	}
	restarted.restoredAt = now
	gated := func() int {
		n := 0
		for _, c := range runner.calls {
			if c.Path == "gate" {
				n++
			}
		}
		return n
	}

	now = now.Add(restoredCheckpointLimit - time.Second)
	if err := restarted.Step(ctx); err != nil || gated() != 0 {
		t.Fatalf("checkpoint withdrawn before the limit: err=%v gates=%d", err, gated())
	}
	now = now.Add(time.Second)
	if err := restarted.Step(ctx); err == nil || !strings.Contains(err.Error(), "no DNS state received") || gated() != 1 {
		t.Fatalf("expired checkpoint kept serving: err=%v gates=%d", err, gated())
	}
	if err := restarted.Step(ctx); err != nil || gated() != 1 {
		t.Fatalf("withdrawn checkpoint was reapplied without state: err=%v gates=%d", err, gated())
	}

	if err := restarted.loadBootstrap(ctx, bootstrapFunc(func(context.Context) ([]model.Envelope, error) { return nil, nil })); err != nil {
		t.Fatal(err)
	}
	calls := len(runner.calls)
	if err := restarted.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if restarted.needsActivation || len(runner.calls) == calls {
		t.Fatal("received state did not reactivate serving")
	}
}

func TestRestoredCheckpointServesOnceStateArrives(t *testing.T) {
	cfg := regionalTestConfig(t)
	agent, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, agent, time.Now().UTC())

	now := time.Now()
	runner := &recordingRunner{}
	restarted, err := NewAgent(cfg, Dependencies{Runner: runner, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := restarted.restoreActive(ctx); err != nil {
		t.Fatal(err)
	}
	restarted.restoredAt = now
	if err := restarted.loadBootstrap(ctx, bootstrapFunc(func(context.Context) ([]model.Envelope, error) { return nil, nil })); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * restoredCheckpointLimit)
	if err := restarted.Step(ctx); err != nil {
		t.Fatal(err)
	}
	for _, c := range runner.calls {
		if c.Path == "gate" {
			t.Fatal("checkpoint withdrawn after DNS state arrived")
		}
	}
}
