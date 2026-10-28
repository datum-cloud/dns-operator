package serving

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
)

func regionalTestConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	render, _ := renderFixture(t)
	render.Role = RenderRegionalCoreDNS
	render.ClusterCoreDNS.Path = filepath.Join(dir, "runtime", "regional.conf")
	return Config{Region: "east", Shard: "s1", MemberID: "regional-0", ReplicaID: "regional-0", Mode: ModeCombined, StateDir: filepath.Join(dir, "state"), Render: render, Transaction: TransactionConfig{StageDir: filepath.Join(dir, "stage"), Validate: []Command{{Path: "validate"}}, Reload: []Command{{Path: "reload"}}, FailClosed: []Command{{Path: "gate"}}}, UnsafeAllowNoDNSVerify: true}
}
func installTestPublication(t *testing.T, a *Agent, now time.Time) {
	t.Helper()
	snapshot := testSnapshot(now)
	snapshot.Members = []model.Member{{MemberID: "regional-0", ReplicaID: "regional-0", Role: "regional"}}
	chunk, manifest := testPublication(t, now)
	envs := []model.Envelope{
		envelope(t, model.KindPublicationChunk, "zone-a", 2, 4, chunk),
		envelope(t, model.KindServingSnapshot, "shard", 1, 1, snapshot),
		envelope(t, model.KindPublicationManifest, "zone-a", 2, 4, manifest),
	}
	ctx := context.Background()
	if err := a.loadBootstrap(ctx, bootstrapFunc(func(context.Context) ([]model.Envelope, error) { return envs, nil })); err != nil {
		t.Fatal(err)
	}
	if err := a.Step(ctx); err != nil {
		t.Fatal(err)
	}
}

func bootstrapFromCheckpoint(t *testing.T, a *Agent) []model.Envelope {
	t.Helper()
	var envs []model.Envelope
	for _, chunks := range a.state.Chunks {
		for _, chunk := range chunks {
			envs = append(envs, envelope(t, model.KindPublicationChunk, chunk.ZoneUID, chunk.WriterEpoch, chunk.Revision, chunk))
		}
	}
	if a.state.Snapshot != nil {
		envs = append(envs, envelope(t, model.KindServingSnapshot, "shard", a.state.Snapshot.ConfigurationEpoch, a.state.Snapshot.ConfigurationRevision, *a.state.Snapshot))
	}
	for uid, publication := range a.state.Publications {
		envs = append(envs, envelope(t, model.KindPublicationManifest, uid, publication.Fence.Epoch, publication.Fence.Revision, publication.Manifest))
	}
	return envs
}
func currentZone(t *testing.T, a *Agent) string {
	t.Helper()
	render := a.cfg.Render
	render.NodeAuthoritative = a.cfg.NodeAuthoritative
	files, err := renderPublications(render, *a.state.Snapshot, map[string]bool{}, a.state.Publications)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range files.Files {
		var cfg struct {
			Contexts []struct{ Zones []struct{ Data string } }
		}
		if json.Unmarshal(data, &cfg) != nil {
			continue
		}
		for _, entry := range cfg.Contexts {
			for _, zone := range entry.Zones {
				if zone.Data != "" {
					return zone.Data
				}
			}
		}
	}

	t.Fatal("no zone")
	return ""
}

func nodeAuthoritativeTestConfig(t *testing.T) Config {
	t.Helper()
	cfg := regionalTestConfig(t)
	render, _ := renderFixture(t)
	render.Role = RenderNode
	runtimeDir := filepath.Join(filepath.Dir(cfg.StateDir), "node-runtime")
	render.NodeCoreDNS.Path = filepath.Join(runtimeDir, "node-coredns.json")
	render.NodeDNSDist.Path = filepath.Join(runtimeDir, "node-dnsdist.conf")
	render.NodeCoreDNS.PublicUpstreams = []string{"1.1.1.1:53"}
	cfg.Render = render
	cfg.MemberID, cfg.ReplicaID = "node-0", "node-0"
	cfg.NodeAuthoritative = true
	return cfg
}

func TestNodeAuthoritativeRequiresExplicitCombinedNodeMode(t *testing.T) {
	cfg := nodeAuthoritativeTestConfig(t)
	cfg.PublicationDNSProbe.Server = net.JoinHostPort(cfg.Render.NodeCoreDNS.ListenAddress, fmt.Sprint(cfg.Render.NodeCoreDNS.Port))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid node-authoritative probe rejected: %v", err)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Mode = ModeResolver },
		func(c *Config) { c.Render.Role = RenderRegionalCoreDNS },
	} {
		candidate := cfg
		mutate(&candidate)
		if err := candidate.Validate(); err == nil {
			t.Fatal("invalid node-authoritative configuration was accepted")
		}
	}
	cfg.NodeAuthoritative = false
	if err := cfg.Validate(); err == nil {
		t.Fatal("combined node mode was accepted without the capability opt-in")
	}
}

func TestNodeAuthoritativeVerifierUsesConsumerPath(t *testing.T) {
	cfg := nodeAuthoritativeTestConfig(t)
	cfg.UnsafeAllowNoDNSVerify = false
	cfg.PublicationDNSProbe.Server = net.JoinHostPort(cfg.Render.NodeCoreDNS.ListenAddress, fmt.Sprint(cfg.Render.NodeCoreDNS.Port))
	agent, err := NewAgent(cfg, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	verifier, ok := agent.deps.Verifier.(DNSVerifier)
	if !ok || verifier.Config.UseClusterAddress {
		t.Fatalf("node authority verifier does not target the consumer path: %#v", agent.deps.Verifier)
	}
	publication, ok := agent.publicationVerifier.(PublicationDNSVerifier)
	if !ok || publication.Config.Server != cfg.PublicationDNSProbe.Server {
		t.Fatalf("node publication verifier does not target local CoreDNS: %#v", agent.publicationVerifier)
	}
}

func TestNodeAuthoritativePublicationActivatesWithoutRegionalAck(t *testing.T) {
	now := time.Now().UTC()
	cfg := nodeAuthoritativeTestConfig(t)
	pub := &fakePublisher{}
	agent, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }, AckPublisher: pub})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, agent, now)
	config, err := os.ReadFile(cfg.Render.NodeCoreDNS.Path)
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodedCoreDNS(t, config)
	if got := decoded.Contexts[0].Upstreams; len(got) != 0 {
		t.Fatalf("node authority retained regional upstreams: %v", got)
	}
	if !slices.Equal(decoded.PublicUpstreams, cfg.Render.NodeCoreDNS.PublicUpstreams) {
		t.Fatalf("node authority changed public recursion: %v", decoded.PublicUpstreams)
	}
	if data := decoded.Contexts[0].Zones[0].Data; !strings.Contains(data, "192.0.2.10") {
		t.Fatalf("node authority did not materialize the publication: %s", data)
	}
	views := agent.publicationViews(agent.state.Publications["zone-a"])
	if len(views) != 1 || views[0].DestinationAddress != agent.state.Snapshot.Bindings[0].ConsumerAddress {
		t.Fatalf("publication verification targeted the wrong node destination: %#v", views)
	}
	for _, env := range pub.envs {
		var ack model.MemberAck
		if err := json.Unmarshal(env.Payload, &ack); err != nil {
			t.Fatal(err)
		}
		if ack.Kind == model.KindPublicationManifest {
			t.Fatal("node authority emitted a regional publication acknowledgement")
		}
	}
}

func TestNodeAuthoritativeTombstoneFencesReplay(t *testing.T) {
	now := time.Now().UTC()
	cfg := nodeAuthoritativeTestConfig(t)
	agent, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, agent, now)
	_, old := testPublication(t, now)
	tombstone := model.PublicationManifest{ManifestUID: "deleted", ZoneUID: "zone-a", Apex: "prod.internal", WriterEpoch: 3, Revision: 1, Tombstone: true, GeneratedAt: now, ContentHash: model.EmptyContentHash()}
	if err := agent.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 3, 1, tombstone)); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(cfg.Render.NodeCoreDNS.Path)
	if err != nil {
		t.Fatal(err)
	}
	zone := decodedCoreDNS(t, config).Contexts[0].Zones[0]
	if !zone.Pending || zone.Data != "" {
		t.Fatalf("withdrawn zone remains authoritative: %#v", zone)
	}
	if err := agent.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 4, old)); err != nil {
		t.Fatal(err)
	}
	if !agent.state.Publications["zone-a"].Manifest.Tombstone {
		t.Fatal("replayed publication defeated the node tombstone")
	}
}

func TestNodeAuthoritativeCheckpointIsNotRestoredAsForwarder(t *testing.T) {
	now := time.Now().UTC()
	cfg := nodeAuthoritativeTestConfig(t)
	agent, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, agent, now)

	rollback := cfg
	rollback.Mode = ModeResolver
	rollback.NodeAuthoritative = false
	runner := &recordingRunner{}
	restarted, err := NewAgent(rollback, Dependencies{Runner: runner, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.restoreActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) == 0 || runner.calls[len(runner.calls)-1].Path != "gate" {
		t.Fatalf("rollback restored node-authoritative files as a forwarder: %#v", runner.calls)
	}
}

func TestNodeAuthoritativeUpgradeWaitsForCurrentDelivery(t *testing.T) {
	now := time.Now().UTC()
	authoritative := nodeAuthoritativeTestConfig(t)
	resolver := authoritative
	resolver.Mode = ModeResolver
	resolver.NodeAuthoritative = false
	seed, err := NewAgent(resolver, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, seed, now)

	runner := &recordingRunner{}
	upgraded, err := NewAgent(authoritative, Dependencies{Runner: runner, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := upgraded.Step(context.Background()); err == nil || !strings.Contains(err.Error(), "bootstrap") {
		t.Fatalf("node authority activated before current delivery: %v", err)
	}
	before := len(runner.calls)
	snapshot := *upgraded.state.Snapshot
	if err := upgraded.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", snapshot.ConfigurationEpoch, snapshot.ConfigurationRevision, snapshot)); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != before {
		t.Fatal("ordinary delivery bypassed the current-bootstrap gate")
	}
	envs := bootstrapFromCheckpoint(t, upgraded)
	if err := upgraded.loadBootstrap(context.Background(), bootstrapFunc(func(context.Context) ([]model.Envelope, error) { return envs, nil })); err != nil {
		t.Fatal(err)
	}
	if err := upgraded.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) == before || !strings.Contains(currentZone(t, upgraded), "192.0.2.10") {
		t.Fatal("validated bootstrap did not activate the persisted verified publication")
	}
}

func TestNodeAuthoritativeBootstrapIsCompleteBeforeActivation(t *testing.T) {
	now := time.Now().UTC()
	cfg := nodeAuthoritativeTestConfig(t)
	seed, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, seed, now)

	runner := &recordingRunner{}
	restarted, err := NewAgent(cfg, Dependencies{Runner: runner, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.restoreActive(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := len(runner.calls)

	// A valid ordinary event is not a substitute for the current, complete KV
	// bootstrap cut and must not certify or reactivate cached authority.
	snapshot := *restarted.state.Snapshot
	if err := restarted.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", snapshot.ConfigurationEpoch, snapshot.ConfigurationRevision, snapshot)); err != nil {
		t.Fatal(err)
	}
	if restarted.bootstrapComplete || len(runner.calls) != before {
		t.Fatal("ordinary delivery bypassed bootstrap completion")
	}

	// A cut that withholds the attached zone's manifest is incomplete. Reject it
	// before changing desired state or reconciling the still-approved snapshot.
	incomplete := []model.Envelope{envelope(t, model.KindServingSnapshot, "shard", snapshot.ConfigurationEpoch, snapshot.ConfigurationRevision, snapshot)}
	if err := restarted.loadBootstrap(context.Background(), bootstrapFunc(func(context.Context) ([]model.Envelope, error) { return incomplete, nil })); err == nil || !strings.Contains(err.Error(), "no current publication") {
		t.Fatalf("incomplete bootstrap accepted: %v", err)
	}
	if restarted.bootstrapComplete || len(runner.calls) != before {
		t.Fatal("incomplete bootstrap changed active serving")
	}
	invalid := []model.Envelope{envelope(t, model.KindServingSnapshot, "shard", snapshot.ConfigurationEpoch, snapshot.ConfigurationRevision, snapshot)}
	invalid[0].Payload = json.RawMessage(`{"region":"east"}`)
	if err := restarted.loadBootstrap(context.Background(), bootstrapFunc(func(context.Context) ([]model.Envelope, error) { return invalid, nil })); err == nil {
		t.Fatal("invalid bootstrap payload certified readiness")
	}
	if restarted.bootstrapComplete || len(runner.calls) != before {
		t.Fatal("invalid bootstrap changed active serving")
	}

	// The complete current cut contains a newer withdrawal. Loading the batch is
	// atomic with respect to serving; only the following reconciliation may
	// remove the previously approved record.
	tombstone := model.PublicationManifest{ManifestUID: "deleted", ZoneUID: "zone-a", Apex: "prod.internal", WriterEpoch: 3, Revision: 1, Tombstone: true, GeneratedAt: now, ContentHash: model.EmptyContentHash()}
	complete := []model.Envelope{
		envelope(t, model.KindServingSnapshot, "shard", snapshot.ConfigurationEpoch, snapshot.ConfigurationRevision, snapshot),
		envelope(t, model.KindPublicationManifest, "zone-a", tombstone.WriterEpoch, tombstone.Revision, tombstone),
	}
	if err := restarted.loadBootstrap(context.Background(), bootstrapFunc(func(context.Context) ([]model.Envelope, error) { return complete, nil })); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != before {
		t.Fatal("bootstrap reconciled before the complete batch was accepted")
	}
	if err := restarted.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restarted.state.Active == nil || !restarted.state.Active.Publications["zone-a"].Manifest.Tombstone {
		t.Fatal("newer bootstrap withdrawal was not activated")
	}
}

func TestNodeAuthoritativeRestartWithdrawsExpiredCheckpointWithoutBootstrap(t *testing.T) {
	for _, bootstrap := range []string{"not attempted", "load failed", "incomplete"} {
		t.Run(bootstrap, func(t *testing.T) {
			now := time.Now().UTC()
			cfg := nodeAuthoritativeTestConfig(t)
			seed, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			installTestPublication(t, seed, now)

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

			switch bootstrap {
			case "load failed":
				err = restarted.loadBootstrap(ctx, bootstrapFunc(func(context.Context) ([]model.Envelope, error) {
					return nil, errors.New("delivery unavailable")
				}))
			case "incomplete":
				snapshot := *restarted.state.Snapshot
				err = restarted.loadBootstrap(ctx, bootstrapFunc(func(context.Context) ([]model.Envelope, error) {
					return []model.Envelope{envelope(t, model.KindServingSnapshot, "shard", snapshot.ConfigurationEpoch, snapshot.ConfigurationRevision, snapshot)}, nil
				}))
			}
			if bootstrap != "not attempted" && err == nil {
				t.Fatal("unusable bootstrap unexpectedly completed")
			}
			if restarted.bootstrapComplete {
				t.Fatal("unusable bootstrap certified the restored checkpoint")
			}

			gated := func() int {
				n := 0
				for _, call := range runner.calls {
					if call.Path == "gate" {
						n++
					}
				}
				return n
			}
			now = now.Add(restoredCheckpointLimit - time.Second)
			if err := restarted.Step(ctx); err == nil || !strings.Contains(err.Error(), "waiting for a current bootstrap") || gated() != 0 {
				t.Fatalf("checkpoint was not retained until its deadline: err=%v gates=%d", err, gated())
			}
			now = now.Add(time.Second)
			if err := restarted.Step(ctx); err == nil || !strings.Contains(err.Error(), "no DNS state received") || gated() != 1 {
				t.Fatalf("expired checkpoint kept serving without a valid bootstrap: err=%v gates=%d", err, gated())
			}
			if err := restarted.Step(ctx); err != nil || gated() != 1 {
				t.Fatalf("withdrawn checkpoint repeated fail-closed activation: err=%v gates=%d", err, gated())
			}
		})
	}
}
func TestCoreDNSPublicationActivatesLocalPrivateAuthority(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cfg := regionalTestConfig(t)
	pub := &fakePublisher{}
	agent, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }, AckPublisher: pub})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, agent, now)
	data := currentZone(t, agent)
	if !strings.Contains(data, "192.0.2.10") || !strings.Contains(data, publicationMailbox(agent.state.Publications["zone-a"])) {
		t.Fatalf("bad zone: %s", data)
	}
	config, err := os.ReadFile(cfg.Render.ClusterCoreDNS.Path)
	if err != nil {
		t.Fatal(err)
	}
	if decodedCoreDNS(t, config).Contexts[0].Zones[0].Data == "" {
		t.Fatalf("not direct private authority: %s", config)
	}
	if !agent.state.Publications["zone-a"].Verified {
		t.Fatal("publication not verified")
	}
}
func TestCoreDNSPublicationSurvivesBrokerOutageAndRestart(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	clock := now
	cfg := regionalTestConfig(t)
	a, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return clock }, AckPublisher: stalledPublisher{}})
	if err != nil {
		t.Fatal(err)
	}
	// Publish without a transport dependency during setup; partition afterwards.
	a.deps.AckPublisher = nil
	installTestPublication(t, a, now)
	a.deps.AckPublisher = newBoundedAckPublisher(stalledPublisher{})
	a.cfg.AckPublishTimeout = time.Millisecond
	clock = now.Add(24 * time.Hour)
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if data := currentZone(t, a); !strings.Contains(data, "192.0.2.10") {
		t.Fatalf("outage removed the published record: %s", data)
	}
	local := a.state.Publications["zone-a"].LocalRevision
	restarted, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if restarted.state.Publications["zone-a"].LocalRevision != local {
		t.Fatal("restart changed the applied publication")
	}
	if data := currentZone(t, restarted); !strings.Contains(data, "192.0.2.10") {
		t.Fatal("restart lost the published record")
	}
}
func TestCoreDNSActivationFailurePersistsObservationFence(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cfg := regionalTestConfig(t)
	runner := &recordingRunner{}
	a, err := NewAgent(cfg, Dependencies{Runner: runner, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := testSnapshot(now)
	if err := a.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 1, snapshot)); err != nil {
		t.Fatal(err)
	}
	chunk, m := testPublication(t, now)
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationChunk, "zone-a", 2, 4, chunk)); err != nil {
		t.Fatal(err)
	}
	runner.failAt = len(runner.calls) + 1
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 4, m)); err == nil {
		t.Fatal("invalid staged configuration accepted")
	}
	saved, err := (stateStore{path: filepath.Join(cfg.StateDir, "checkpoint.json")}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if f := saved.ContributionFences["contrib-a"]; f.Generation != 1 || f.Sequence != 1 {
		t.Fatalf("observation fence lost: %#v", f)
	}
	if saved.Publications["zone-a"].Verified {
		t.Fatal("failed publication acknowledged")
	}
	if runner.calls[len(runner.calls)-1].Path == "gate" || saved.Active == nil {
		t.Fatal("rejected expansion discarded the active configuration")
	}
	runner.failAt = 0
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 4, m)); err != nil {
		t.Fatalf("equal fenced retry failed: %v", err)
	}
}
func TestCoreDNSTombstoneFencesReplayAcrossRestart(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cfg := regionalTestConfig(t)
	a, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, a, now)
	_, old := testPublication(t, now)
	tombstone := model.PublicationManifest{ManifestUID: "deleted", ZoneUID: "zone-a", Apex: "prod.internal", WriterEpoch: 3, Revision: 1, Tombstone: true, GeneratedAt: now, ContentHash: model.EmptyContentHash()}
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 3, 1, tombstone)); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 4, old)); err != nil {
		t.Fatal(err)
	}
	if !restarted.state.Publications["zone-a"].Manifest.Tombstone || restarted.publicationsReady(testSnapshot(now).Bindings[0]) {
		t.Fatal("old snapshot defeated tombstone")
	}
}
func TestNodeRequiresPublicationAndRetainsCacheDuringOutage(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	clock := now
	cfg := regionalTestConfig(t)
	render, snapshot := renderFixture(t)
	render.Role = RenderNode
	render.NodeCoreDNS.Path = filepath.Join(t.TempDir(), "node.conf")
	render.NodeDNSDist.Path = filepath.Join(filepath.Dir(render.NodeCoreDNS.Path), "dnsdist.conf")
	cfg.Render = render
	cfg.Mode = ModeResolver
	runner := &recordingRunner{}
	a, err := NewAgent(cfg, Dependencies{Runner: runner, Clock: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	snapshot = testSnapshot(now)
	a.state.Snapshot = &snapshot
	a.state.BindingFences, _ = advanceBindingFences(nil, snapshot)
	a.state.Acks["zone-a"] = map[string]replicaAck{"regional-0/regional-0": {Fence: fence{2, 4}, ObservedAt: now, ValidUntil: now.Add(time.Minute)}}
	if a.publicationsReady(snapshot.Bindings[0]) {
		t.Fatal("node enabled from ACK without publication plan")
	}
	chunk, m := testPublication(t, now)
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationChunk, "zone-a", 2, 4, chunk)); err != nil {
		t.Fatal(err)
	}
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 4, m)); err != nil {
		t.Fatal(err)
	}
	if !a.publicationsReady(snapshot.Bindings[0]) {
		t.Fatal("complete plan + current proof not eligible")
	}
	runner.calls = nil
	clock = now.Add(24 * time.Hour)
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}

}
func TestCoreDNSPublicationVersionProofIncludesFullContentFingerprint(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	chunk, m := testPublication(t, now)
	var plan model.PublicationPlan
	if err := json.Unmarshal(chunk.Payload, &plan); err != nil {
		t.Fatal(err)
	}
	p := publicationState{Fence: fence{m.WriterEpoch, m.Revision}, Plan: plan}
	first := publicationMailbox(p)
	p.Plan.RRSets[0].Records[0].Content = "192.0.2.11"
	if first == publicationMailbox(p) {
		t.Fatal("different installed data shares fingerprint")
	}
	if len(first) > 255 {
		t.Fatal("fingerprint name too long")
	}
}

func TestLegacyBackendCheckpointRejectedWithoutDiscardingFences(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	old := []byte(`{"contributionFences":{"endpoint":{"epoch":7,"sequence":33}}}`)
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := (stateStore{path: path}).Load(); err == nil {
		t.Fatal("old backend checkpoint silently accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(old) {
		t.Fatal("unsupported checkpoint discarded observation fences")
	}
}

func TestRegionalPoolRequiresOneMemberWithEveryCurrentZone(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	snapshot := testSnapshot(now)
	b := snapshot.Bindings[0]
	b.Zones = append(b.Zones, model.ZoneAttachment{ZoneUID: "zone-b", Apex: "custom.internal", RequiredPublicationEpoch: 2, RequiredPublicationRevision: 4})
	snapshot.Bindings = []model.Binding{b}
	snapshot.Members = []model.Member{{MemberID: "regional-0", Role: "regional"}, {MemberID: "regional-1", Role: "regional"}}
	a := &Agent{cfg: Config{Mode: ModeResolver}, state: newCheckpoint(), deps: Dependencies{Clock: func() time.Time { return now }}}
	a.state.Snapshot = &snapshot
	a.state.BindingFences, _ = advanceBindingFences(nil, snapshot)
	for _, z := range b.Zones {
		a.state.Publications[z.ZoneUID] = publicationState{Fence: fence{2, 4}, Verified: true}
		a.state.Acks[z.ZoneUID] = map[string]replicaAck{}
	}
	proof := replicaAck{Fence: fence{2, 4}, ObservedAt: now, ValidUntil: now.Add(time.Minute)}
	a.state.Acks["zone-a"]["regional-0/"] = proof
	a.state.Acks["zone-b"]["regional-1/"] = proof
	if a.publicationsReady(b) {
		t.Fatal("different member per zone incorrectly made whole context ready")
	}
	a.state.Acks["zone-b"]["regional-0/"] = proof
	ready := a.eligibleRegionalMembers(b)
	if !ready["regional-0"] || ready["regional-1"] {
		t.Fatalf("wrong member eligibility: %#v", ready)
	}
	c, _ := renderFixture(t)
	c.Role = RenderRegionalDNSDist
	c.ClusterDNSDist.Backends = []model.Backend{{MemberID: "regional-0", Address: "10.253.0.20", Port: 5300}, {MemberID: "regional-1", Address: "10.253.0.21", Port: 5300}}
	c.ReadyMembers = map[string]map[string]bool{b.BindingUID: ready}
	rendered, err := Render(c, snapshot, map[string]bool{b.BindingUID: true})
	if err != nil {
		t.Fatal(err)
	}
	conf := string(rendered.Files[c.ClusterDNSDist.Path])
	if strings.Count(conf, "]:addPool(") != 1 {
		t.Fatalf("stale live member remains in context pool:\n%s", conf)
	}
	a.state.Acks["zone-a"]["regional-1/"] = replicaAck{Fence: fence{2, 5}, ObservedAt: now, ValidUntil: now.Add(time.Minute)}
	if ready := a.eligibleRegionalMembers(b); !ready["regional-0"] || !ready["regional-1"] {
		t.Fatalf("ACK ahead of local publication emptied or narrowed the pool: %#v", ready)
	}
	a.state.Acks["zone-a"]["regional-1/"] = replicaAck{Fence: fence{2, 3}, ObservedAt: now, ValidUntil: now.Add(time.Minute)}
	if a.eligibleRegionalMembers(b)["regional-1"] {
		t.Fatal("member below the required publication was selected")
	}
	a.state.Acks["zone-a"]["regional-1/"] = replicaAck{Fence: fence{2, 5}, ObservedAt: now, Rejected: true}
	if a.eligibleRegionalMembers(b)["regional-1"] {
		t.Fatal("member with a rejected publication was selected")
	}
}

func TestZoneUpdateKeepsServingMembersOnPreviousPublication(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	snapshot := testSnapshot(now)
	b := snapshot.Bindings[0]
	snapshot.Members = []model.Member{{MemberID: "regional-0", Role: "regional"}, {MemberID: "regional-1", Role: "regional"}}
	a := &Agent{cfg: Config{Mode: ModeResolver}, state: newCheckpoint(), deps: Dependencies{Clock: func() time.Time { return now }}}
	a.state.Snapshot = &snapshot
	a.state.BindingFences, _ = advanceBindingFences(nil, snapshot)
	ack := func(member string, f fence) {
		a.state.Acks["zone-a"][member+"/"] = replicaAck{Fence: f, ObservedAt: now, ValidUntil: now.Add(time.Minute)}
	}
	a.state.Publications["zone-a"] = publicationState{Fence: fence{2, 4}, Verified: true}
	a.state.Acks["zone-a"] = map[string]replicaAck{}
	ack("regional-0", fence{2, 4})
	ack("regional-1", fence{2, 4})

	c, _ := renderFixture(t)
	c.ClusterDNSDist.Backends = []model.Backend{{MemberID: "regional-0", Address: "10.253.0.20", Port: 5300}, {MemberID: "regional-1", Address: "10.253.0.21", Port: 5300}}
	servfail := fmt.Sprintf("appendRule(AndRule({TagRule(\"dns-context\",%q),QNameSuffixRule(%q)}),RCodeAction(DNSRCode.SERVFAIL))", b.ViewName(), model.AbsoluteName("prod.internal"))
	assertServing := func(step string, want ...string) {
		t.Helper()
		ready := a.eligibleRegionalMembers(b)
		if len(ready) != len(want) {
			t.Fatalf("%s: ready members %#v, want %v", step, ready, want)
		}
		for _, member := range want {
			if !ready[member] {
				t.Fatalf("%s: ready members %#v, want %v", step, ready, want)
			}
		}
		c.ReadyZoneMembers = map[string]map[string]map[string]bool{b.BindingUID: {"zone-a": ready}}
		rendered, err := Render(c, snapshot, map[string]bool{b.BindingUID: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{c.NodeDNSDist.Path, c.ClusterDNSDist.Path} {
			if strings.Contains(string(rendered.Files[path]), servfail) {
				t.Fatalf("%s: %s answers SERVFAIL for the zone", step, path)
			}
		}
	}

	assertServing("before update", "regional-0", "regional-1")
	ack("regional-0", fence{2, 5})
	assertServing("member ACK ahead of resolver", "regional-0", "regional-1")
	a.state.Publications["zone-a"] = publicationState{Fence: fence{2, 5}, Verified: true}
	assertServing("resolver holds new publication", "regional-0", "regional-1")
	ack("regional-1", fence{2, 5})
	assertServing("all members on new publication", "regional-0", "regional-1")
	a.state.Publications["zone-a"] = publicationState{Fence: fence{3, 1}, Verified: true}
	assertServing("writer epoch bump", "regional-0", "regional-1")
	ack("regional-0", fence{3, 1})
	assertServing("first member on new epoch", "regional-0", "regional-1")
}
func TestMissingOrStalePrivatePublicationHasNoPublicFallback(t *testing.T) {
	t.Parallel()
	c, snapshot := renderFixture(t)
	c.Role = RenderRegionalCoreDNS
	publications := map[string]publicationState{"zone-a": {Fence: fence{1, 1}, Plan: model.PublicationPlan{ZoneUID: "zone-a", Apex: "prod.internal"}}}
	rendered, err := renderPublications(c, snapshot, map[string]bool{}, publications)
	if err != nil {
		t.Fatal(err)
	}
	conf := string(rendered.Files[c.ClusterCoreDNS.Path])
	if !decodedCoreDNS(t, []byte(conf)).Contexts[0].Zones[0].Pending {
		t.Fatalf("private zone can recurse publicly: %s", conf)
	}
	for path := range rendered.Files {
		if strings.HasSuffix(path, ".db") {
			t.Fatal("stale publication installed as private authority")
		}
	}
}

func TestRejectedMemberProofRevokesRoutingAndIgnoresOlderReplay(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	clock := now
	cfg := regionalTestConfig(t)
	cfg.Mode = ModeResolver
	cfg.Render.Role = RenderRegionalDNSDist
	cfg.Render.ClusterDNSDist.Path = filepath.Join(t.TempDir(), "frontend.conf")
	a, err := NewAgent(cfg, Dependencies{Clock: func() time.Time { return clock }, Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := testSnapshot(now)
	a.state.Snapshot = &snapshot
	a.state.BindingFences, _ = advanceBindingFences(nil, snapshot)
	chunk, _ := testPublication(t, now)
	var plan model.PublicationPlan
	if err := json.Unmarshal(chunk.Payload, &plan); err != nil {
		t.Fatal(err)
	}
	a.state.Publications["zone-a"] = publicationState{Fence: fence{2, 4}, Plan: plan, Verified: true, EffectiveHash: effectivePlanHash(plan)}
	a.state.Acks["zone-a"] = map[string]replicaAck{"regional-0/regional-0": {Fence: fence{2, 4}, ObservedAt: now, ValidUntil: now.Add(time.Minute)}}
	ack := model.MemberAck{MemberID: "regional-0", ReplicaID: "regional-0", ResourceUID: "zone-a", Kind: model.KindPublicationManifest, Epoch: 2, Revision: 4, Phase: model.AckRejected, ObservedAt: now.Add(-time.Second), ValidUntil: now.Add(time.Minute)}
	if err := a.Handle(context.Background(), envelope(t, model.KindMemberAck, "zone-a", 2, 4, ack)); err != nil {
		t.Fatal(err)
	}
	if !a.publicationsReady(snapshot.Bindings[0]) {
		t.Fatal("older rejection replay revoked fresh proof")
	}
	clock = now.Add(time.Second)
	ack.ObservedAt = clock
	if err := a.Handle(context.Background(), envelope(t, model.KindMemberAck, "zone-a", 2, 4, ack)); err != nil {
		t.Fatal(err)
	}
	if a.publicationsReady(snapshot.Bindings[0]) {
		t.Fatal("new rejection retained stale member routing")
	}
	ack.Phase = model.AckVerified
	ack.ObservedAt = now
	if err := a.Handle(context.Background(), envelope(t, model.KindMemberAck, "zone-a", 2, 4, ack)); err != nil {
		t.Fatal(err)
	}
	if a.publicationsReady(snapshot.Bindings[0]) {
		t.Fatal("old verified ACK resurrected rejected member")
	}
}

func TestPublicationProbeTimeoutDoesNotApproveConfiguration(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cfg := regionalTestConfig(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	port := listener.Addr().(*net.TCPAddr).Port
	cfg.Render.ClusterCoreDNS.ListenAddress = "127.0.0.1"
	cfg.Render.ClusterCoreDNS.Port = uint16(port)
	cfg.PublicationDNSProbe = PublicationDNSVerifierConfig{Server: listener.Addr().String(), Timeout: 50 * time.Millisecond}
	cfg.UnsafeAllowNoDNSVerify = false
	a, err := NewAgent(cfg, Dependencies{Clock: func() time.Time { return now }, Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := testSnapshot(now)
	if err := a.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 1, snapshot)); err != nil {
		t.Fatal(err)
	}
	chunk, m := testPublication(t, now)
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationChunk, "zone-a", 2, 4, chunk)); err != nil {
		t.Fatal(err)
	}
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 4, m)); err == nil {
		t.Fatal("timed out publication accepted")
	}
	if a.state.Publications["zone-a"].Verified {
		t.Fatal("failed publication query proof became active")
	}
}

type publicationVerifierFunc func(context.Context, model.PublicationPlan, []PublicationView) error

func (f publicationVerifierFunc) VerifyPublication(ctx context.Context, p model.PublicationPlan, views []PublicationView) error {
	return f(ctx, p, views)
}

type afterReloadRunner struct {
	recordingRunner
	after func()
}

func (r *afterReloadRunner) Run(ctx context.Context, command Command) ([]byte, error) {
	data, err := r.recordingRunner.Run(ctx, command)
	if err == nil && command.Path == "reload" && r.after != nil {
		after := r.after
		r.after = nil
		after()
	}
	return data, err
}

func installedFileVerifier(t *testing.T, configPath string) PublicationVerifier {
	t.Helper()
	return publicationVerifierFunc(func(_ context.Context, plan model.PublicationPlan, views []PublicationView) error {
		config, err := os.ReadFile(configPath)
		if err != nil {
			return err
		}
		cfg := decodedCoreDNS(t, config)
		var data string
		for _, entry := range cfg.Contexts {
			for _, zone := range entry.Zones {
				if zone.Apex == model.AbsoluteName(plan.Apex) && !zone.Pending {
					data = zone.Data
				}
			}
		}
		if data == "" {
			return errors.New("no activated private zone")
		}
		parser := dns.NewZoneParser(strings.NewReader(data), model.AbsoluteName(plan.Apex), configPath)
		for rr, ok := parser.Next(); ok; rr, ok = parser.Next() {
			if soa, ok := rr.(*dns.SOA); ok {
				for _, view := range views {
					if soa.Serial != view.Serial || soa.Mbox != view.FingerprintMailbox {
						return fmt.Errorf("proof expects %d/%s but activated file contains %d/%s", view.Serial, view.FingerprintMailbox, soa.Serial, soa.Mbox)
					}
				}
				return nil
			}
		}
		return errors.New("activated zone lacks SOA")
	})
}

func TestFailedCoreDNSActivationRetainsPriorInstalledProof(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cfg := regionalTestConfig(t)
	runner := &recordingRunner{}
	a, err := NewAgent(cfg, Dependencies{Runner: runner, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, a, now)
	prior := a.installedPublications["zone-a"].Identity
	p := a.state.Publications["zone-a"]
	p.LocalRevision++
	a.state.Publications["zone-a"] = p
	runner.failAt = len(runner.calls) + 1
	if err := a.reconcile(context.Background()); err == nil {
		t.Fatal("failed validator was accepted")
	}
	if a.installedPublications["zone-a"].Identity != prior {
		t.Fatal("failed activation advanced installed proof")
	}
}

func TestCoreDNSDoesNotProbeAuthorityBelowRequiredPublicationFence(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cfg := regionalTestConfig(t)
	calls := 0
	a, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }, PublicationVerifier: publicationVerifierFunc(func(context.Context, model.PublicationPlan, []PublicationView) error {
		calls++
		return errors.New("unexpected authoritative proof")
	})})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := testSnapshot(now)
	snapshot.Bindings[0].Zones[0].RequiredPublicationEpoch = 2
	snapshot.Bindings[0].Zones[0].RequiredPublicationRevision = 5
	a.state.Snapshot = &snapshot
	a.state.BindingFences, _ = advanceBindingFences(nil, snapshot)
	chunk, manifest := testPublication(t, now)
	var plan model.PublicationPlan
	if err := json.Unmarshal(chunk.Payload, &plan); err != nil {
		t.Fatal(err)
	}
	a.state.Publications["zone-a"] = publicationState{Fence: fence{2, 4}, Manifest: manifest, Plan: plan, Verified: true, EffectiveHash: effectivePlanHash(plan)}
	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || len(a.publicationViews(a.state.Publications["zone-a"])) != 0 {
		t.Fatal("SERVFAIL zone attempted authoritative publication proof")
	}
	config, err := os.ReadFile(cfg.Render.ClusterCoreDNS.Path)
	if err != nil || !decodedCoreDNS(t, config).Contexts[0].Zones[0].Pending {
		t.Fatalf("behind-required private zone was not gated: %s %v", config, err)
	}
}

type partitioningAckPublisher struct {
	disconnected atomic.Bool
	verified     atomic.Int64
}

func (p *partitioningAckPublisher) Publish(_ context.Context, _ string, env model.Envelope) (transport.PublishAck, error) {
	if p.disconnected.Load() {
		return transport.PublishAck{}, errors.New("nats: server is disconnected")
	}
	var ack model.MemberAck
	if err := json.Unmarshal(env.Payload, &ack); err != nil {
		return transport.PublishAck{}, err
	}
	if ack.Kind == model.KindPublicationManifest && ack.Phase == model.AckVerified {
		p.verified.Add(1)
	}
	return transport.PublishAck{}, nil
}

func TestCoreDNSActivationRacingBrokerDisconnectKeepsServing(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	clock := now
	cfg := regionalTestConfig(t)
	runner := &afterReloadRunner{}
	publisher := &partitioningAckPublisher{}
	a, err := NewAgent(cfg, Dependencies{Runner: runner, Clock: func() time.Time { return clock }, AckPublisher: publisher, PublicationVerifier: installedFileVerifier(t, cfg.Render.ClusterCoreDNS.Path)})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, a, now)
	chunk, manifest := testPublication(t, now)
	var plan model.PublicationPlan
	if err := json.Unmarshal(chunk.Payload, &plan); err != nil {
		t.Fatal(err)
	}
	plan.ObservationFences[0].Sequence = 2
	plan.RRSets[0].Records[0].Sequence = 2
	plan.RRSets[0].Records[0].Content = "192.0.2.11"
	chunk.Payload, err = json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	chunk.ManifestUID = "manifest-b"
	chunk.Revision = 5
	chunk.SHA256 = model.Hash(chunk.Payload)
	manifest.ManifestUID = chunk.ManifestUID
	manifest.PreviousRevision = 4
	manifest.Revision = chunk.Revision
	manifest.ContentHash = chunk.SHA256
	manifest.Chunks = []model.ChunkRef{{Index: 0, SHA256: chunk.SHA256, Size: len(chunk.Payload)}}
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationChunk, "zone-a", 2, 5, chunk)); err != nil {
		t.Fatal(err)
	}
	// Disconnect after CoreDNS reload, exactly before durable/proved activation
	// attempts its ACK. A transport error must not become a failed installation.
	runner.after = func() { publisher.disconnected.Store(true) }
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 5, manifest)); err != nil {
		t.Fatalf("safe activation treated broker failure as materialization failure: %v", err)
	}
	if !a.state.Publications["zone-a"].Verified || a.needsActivation {
		t.Fatal("ACK partition revoked proved publication")
	}
	assertAppliedCheckpoint(t, a)
	// Identical manifest redelivery and snapshot delivery must share the same
	// best-effort ACK boundary without changing the observation.
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 5, manifest)); err != nil {
		t.Fatal(err)
	}
	snapshot := testSnapshot(now)
	snapshot.ConfigurationRevision = 2
	snapshot.Bindings[0].Version.Sequence = 2
	if err := a.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 2, snapshot)); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(24 * time.Hour)
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(currentZone(t, a), "192.0.2.11") {
		t.Fatal("ACK partition removed an applied record")
	}
	assertAppliedCheckpoint(t, a)
	if a.needsActivation {
		t.Fatal("healthy serving was gated by ACK outage")
	}
	before := publisher.verified.Load()
	publisher.disconnected.Store(false)
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if publisher.verified.Load() <= before {
		t.Fatal("broker recovery did not retry publication readiness ACK")
	}
}

func TestCoreDNSTombstoneAckFailureDoesNotGateDurableWithdrawal(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cfg := regionalTestConfig(t)
	publisher := &partitioningAckPublisher{}
	a, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }, AckPublisher: publisher, PublicationVerifier: installedFileVerifier(t, cfg.Render.ClusterCoreDNS.Path)})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, a, now)
	publisher.disconnected.Store(true)
	manifest := model.PublicationManifest{ManifestUID: "retired-zone", ZoneUID: "zone-a", Apex: "prod.internal", WriterEpoch: 2, Revision: 5, PreviousRevision: 4, ContentHash: model.EmptyContentHash(), GeneratedAt: now, Tombstone: true}
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 5, manifest)); err != nil {
		t.Fatal(err)
	}
	p := a.state.Publications["zone-a"]
	if !p.Verified || !p.Manifest.Tombstone || a.needsActivation {
		t.Fatal("failed tombstone ACK invalidated safely withdrawn zone")
	}
	assertAppliedCheckpoint(t, a)
}
