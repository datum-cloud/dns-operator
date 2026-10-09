package serving

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"

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
	render.Role = RenderRegionalBIND
	render.ClusterBIND.Path = filepath.Join(dir, "runtime", "regional.conf")
	return Config{Region: "east", Shard: "s1", MemberID: "regional-0", ReplicaID: "regional-0", Mode: ModeCombined, StateDir: filepath.Join(dir, "state"), Render: render, Transaction: TransactionConfig{StageDir: filepath.Join(dir, "stage"), Validate: []Command{{Path: "validate"}}, Reload: []Command{{Path: "reload"}}, FailClosed: []Command{{Path: "gate"}}}, UnsafeAllowNoDNSVerify: true, WatchdogLeasePath: filepath.Join(dir, "lease.json")}
}
func installTestPublication(t *testing.T, a *Agent, now time.Time) {
	t.Helper()
	ctx := context.Background()
	snapshot := testSnapshot(now)
	snapshot.Members = []model.Member{{MemberID: "regional-0", ReplicaID: "regional-0", Role: "regional"}}
	if err := a.Handle(ctx, envelope(t, model.KindServingSnapshot, "shard", 1, 1, snapshot)); err != nil {
		t.Fatal(err)
	}
	chunk, manifest := testPublication(t, now)
	if err := a.Handle(ctx, envelope(t, model.KindPublicationChunk, "zone-a", 2, 4, chunk)); err != nil {
		t.Fatal(err)
	}
	if err := a.Handle(ctx, envelope(t, model.KindPublicationManifest, "zone-a", 2, 4, manifest)); err != nil {
		t.Fatal(err)
	}
}
func currentZone(t *testing.T, a *Agent) string {
	t.Helper()
	files, err := renderPublications(a.cfg.Render, *a.state.Snapshot, map[string]bool{}, a.state.Publications, a.deps.Clock())
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range files.Files {
		if strings.HasSuffix(path, ".db") {
			return string(data)
		}
	}
	t.Fatal("no zone")
	return ""
}
func TestBINDPublicationActivatesLocalPrivateAuthority(t *testing.T) {
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
	if !strings.Contains(data, "192.0.2.10") || !strings.Contains(data, publicationMailbox(agent.state.Publications["zone-a"], now)) {
		t.Fatalf("bad zone: %s", data)
	}
	config, err := os.ReadFile(cfg.Render.ClusterBIND.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "type primary") || strings.Contains(string(config), "static-stub") || strings.Contains(string(config), "query-source") {
		t.Fatalf("not direct private authority: %s", config)
	}
	if !agent.state.Publications["zone-a"].Verified {
		t.Fatal("publication not verified")
	}
}
func TestBINDExpirySurvivesBrokerOutageAndRestart(t *testing.T) {
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
	clock = now.Add(61 * time.Second)
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if data := currentZone(t, a); strings.Contains(data, "192.0.2.10") || !strings.Contains(data, "api.prod.internal. 5 IN "+model.OwnershipMarkerType) {
		t.Fatalf("expiry did not preserve NODATA ownership: %s", data)
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
		t.Fatal("restart renewed expired observation")
	}
	if data := currentZone(t, restarted); strings.Contains(data, "192.0.2.10") {
		t.Fatal("restart resurrected record")
	}
}
func TestBINDActivationFailurePersistsObservationFence(t *testing.T) {
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
	if f := saved.ContributionFences["contrib-a"]; f.Epoch != 1 || f.Sequence != 1 || !f.ValidUntil.Equal(now.Add(time.Minute)) {
		t.Fatalf("observation fence lost: %#v", f)
	}
	if saved.Publications["zone-a"].Verified {
		t.Fatal("failed publication acknowledged")
	}
	if runner.calls[len(runner.calls)-1].Path != "gate" {
		t.Fatal("failed activation did not fail closed")
	}
	runner.failAt = 0
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 4, m)); err != nil {
		t.Fatalf("equal fenced retry failed: %v", err)
	}
}
func TestBINDFailedExpiryDoesNotRenewWatchdog(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	clock := now
	cfg := regionalTestConfig(t)
	runner := &recordingRunner{}
	a, err := NewAgent(cfg, Dependencies{Runner: runner, Clock: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, a, now)
	if err := os.Remove(cfg.WatchdogLeasePath); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(61 * time.Second)
	runner.failAt = len(runner.calls) + 1
	if err := a.Step(context.Background()); err == nil {
		t.Fatal("expiry validation failure accepted")
	}
	if _, err := os.Stat(cfg.WatchdogLeasePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed expiry renewed watchdog")
	}
	runner.failAt = 0
	if err := a.Step(context.Background()); err != nil {
		t.Fatalf("pending expiry retry failed: %v", err)
	}
	if strings.Contains(currentZone(t, a), "192.0.2.10") {
		t.Fatal("retry resurrected expired record")
	}
}
func TestBINDTombstoneFencesReplayAcrossRestart(t *testing.T) {
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
func TestNodeRequiresDeadlinePlanAndExpiresCacheLocally(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	clock := now
	cfg := regionalTestConfig(t)
	render, snapshot := renderFixture(t)
	render.Role = RenderNode
	render.NodeBIND.Path = filepath.Join(t.TempDir(), "node.conf")
	render.NodeDNSDist.Path = filepath.Join(filepath.Dir(render.NodeBIND.Path), "dnsdist.conf")
	cfg.Render = render
	cfg.Mode = ModeResolver
	cfg.CacheFlush = []Command{{Path: "flush", Args: []string{"{name}"}}}
	runner := &recordingRunner{}
	a, err := NewAgent(cfg, Dependencies{Runner: runner, Clock: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	snapshot = testSnapshot(now)
	a.state.Snapshot = &snapshot
	a.state.BindingFences, _ = advanceBindingFences(nil, snapshot)
	a.state.Acks["zone-a"] = map[string]replicaAck{"regional-0/regional-0": {Fence: fence{2, 4}, ValidUntil: now.Add(time.Minute)}}
	if a.publicationsReady(snapshot.Bindings[0]) {
		t.Fatal("node enabled from ACK without deadline plan")
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
	clock = now.Add(61 * time.Second)
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	flushed := false
	for _, call := range runner.calls {
		if call.Path == "flush" && call.Args[0] == "api.prod.internal." {
			flushed = true
		}
	}
	if !flushed {
		t.Fatal("node did not flush original-deadline record without broker")
	}
	restarted, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	if restarted.state.Publications["zone-a"].Plan.ObservationFences[0].ValidUntil != now.Add(time.Minute) {
		t.Fatal("restart changed original deadline")
	}
}
func TestBINDPublicationVersionProofIncludesFullContentFingerprint(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	chunk, m := testPublication(t, now)
	var plan model.PublicationPlan
	if err := json.Unmarshal(chunk.Payload, &plan); err != nil {
		t.Fatal(err)
	}
	p := publicationState{Fence: fence{m.WriterEpoch, m.Revision}, Plan: plan}
	first := publicationMailbox(p, now)
	p.Plan.RRSets[0].Records[0].Content = "192.0.2.11"
	if first == publicationMailbox(p, now) {
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
	proof := replicaAck{Fence: fence{2, 4}, ValidUntil: now.Add(time.Minute)}
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
	if strings.Count(conf, "server:addPool(") != 1 {
		t.Fatalf("stale live member remains in context pool:\n%s", conf)
	}
	// A newer ACK without the matching deadline plan gates even the formerly
	// current member; a missed record stream cannot serve data with old leases.
	a.state.Acks["zone-a"]["regional-1/"] = replicaAck{Fence: fence{2, 5}, ValidUntil: now.Add(time.Minute)}
	if a.publicationsReady(b) {
		t.Fatal("ACK ahead of local deadline plan activated context")
	}
}
func TestMissingOrStalePrivatePublicationHasNoPublicFallback(t *testing.T) {
	t.Parallel()
	c, snapshot := renderFixture(t)
	c.Role = RenderRegionalBIND
	publications := map[string]publicationState{"zone-a": {Fence: fence{1, 1}, Plan: model.PublicationPlan{ZoneUID: "zone-a", Apex: "prod.internal"}}}
	rendered, err := renderPublications(c, snapshot, map[string]bool{}, publications, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	conf := string(rendered.Files[c.ClusterBIND.Path])
	if !strings.Contains(conf, "type forward; forward only; forwarders { 127.0.0.1 port 9; }") {
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
	a.state.Publications["zone-a"] = publicationState{Fence: fence{2, 4}, Plan: plan, Verified: true, EffectiveHash: effectivePlanHash(plan, now)}
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

func TestPublicationProbeTimeoutDoesNotRenewWatchdog(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cfg := regionalTestConfig(t)
	udp, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = udp.Close() }()
	port := udp.LocalAddr().(*net.UDPAddr).Port
	cfg.Render.ClusterBIND.ListenAddress = "127.0.0.1"
	cfg.Render.ClusterBIND.Port = uint16(port)
	cfg.PublicationDNSProbe = PublicationDNSVerifierConfig{Server: udp.LocalAddr().String(), Timeout: 50 * time.Millisecond}
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
	if _, err := os.Stat(cfg.WatchdogLeasePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed publication query proof renewed watchdog")
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
		pattern := regexp.MustCompile(`zone "` + regexp.QuoteMeta(strings.TrimSuffix(plan.Apex, ".")) + `" \{ type primary; file "([^"]+)"`)
		match := pattern.FindSubmatch(config)
		if len(match) != 2 {
			return errors.New("no activated primary zone")
		}
		data, err := os.ReadFile(string(match[1]))
		if err != nil {
			return err
		}
		parser := dns.NewZoneParser(strings.NewReader(string(data)), model.AbsoluteName(plan.Apex), string(match[1]))
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

func TestBINDProofPinsActivatedFilesWhenDeadlineCrossesReload(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	clock := now
	cfg := regionalTestConfig(t)
	runner := &afterReloadRunner{}
	a, err := NewAgent(cfg, Dependencies{Runner: runner, Clock: func() time.Time { return clock }, PublicationVerifier: installedFileVerifier(t, cfg.Render.ClusterBIND.Path)})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, a, now)
	clock = now.Add(59 * time.Second)
	a.needsActivation = true
	runner.after = func() { clock = now.Add(61 * time.Second) }
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.state.Publications["zone-a"].LocalRevision == 0 || strings.Contains(currentZone(t, a), "192.0.2.10") {
		t.Fatal("expiry crossing reload was not materialized before watchdog renewal")
	}
	if _, err := os.Stat(cfg.WatchdogLeasePath); err != nil {
		t.Fatalf("safe expired authority did not renew watchdog: %v", err)
	}
	// Restart must reconstruct file proofs through activation, never trust the
	// earlier process's in-memory proof or its persisted rendered hash alone.
	restarted, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return clock }, PublicationVerifier: installedFileVerifier(t, cfg.Render.ClusterBIND.Path)})
	if err != nil {
		t.Fatal(err)
	}
	if len(restarted.installedPublications) != 0 {
		t.Fatal("restart inherited unverified process file proofs")
	}
	restarted.needsActivation = true
	if err := restarted.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(restarted.installedPublications["zone-a"].Views) == 0 {
		t.Fatal("restart did not reconstruct exact activated zone proof")
	}
}

func TestFailedBINDActivationRetainsPriorInstalledProof(t *testing.T) {
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

func TestBINDDoesNotProbeAuthorityBelowRequiredPublicationFence(t *testing.T) {
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
	a.state.Publications["zone-a"] = publicationState{Fence: fence{2, 4}, Manifest: manifest, Plan: plan, Verified: true, EffectiveHash: effectivePlanHash(plan, now)}
	if err := a.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || len(a.publicationViews(a.state.Publications["zone-a"])) != 0 {
		t.Fatal("SERVFAIL zone attempted authoritative publication proof")
	}
	config, err := os.ReadFile(cfg.Render.ClusterBIND.Path)
	if err != nil || !strings.Contains(string(config), "forward only; forwarders { 127.0.0.1 port 9; }") {
		t.Fatalf("behind-required private zone was not gated: %s %v", config, err)
	}
}

func TestBINDWatchdogCannotRenewExpiredInstalledSnapshot(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	clock := now
	cfg := regionalTestConfig(t)
	a, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return clock }})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, a, now)
	clock = now.Add(61 * time.Second)
	if err := a.renewWatchdogLease(context.Background()); err == nil {
		t.Fatal("expired installed snapshot renewed watchdog")
	}
	if _, err := os.Stat(cfg.WatchdogLeasePath); !errors.Is(err, os.ErrNotExist) || !a.needsActivation {
		t.Fatalf("expired snapshot was not gated: %v", err)
	}
}

func TestBINDExpiryCrossingProofSettlesBeforeWatchdogRenewal(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	clock := now
	cfg := regionalTestConfig(t)
	files := installedFileVerifier(t, cfg.Render.ClusterBIND.Path)
	advance := false
	verifier := publicationVerifierFunc(func(ctx context.Context, plan model.PublicationPlan, views []PublicationView) error {
		err := files.VerifyPublication(ctx, plan, views)
		if advance {
			advance = false
			clock = now.Add(61 * time.Second)
		}
		return err
	})
	a, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return clock }, PublicationVerifier: verifier})
	if err != nil {
		t.Fatal(err)
	}
	installTestPublication(t, a, now)
	clock = now.Add(59 * time.Second)
	advance = true
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(currentZone(t, a), "192.0.2.10") || a.state.Publications["zone-a"].LocalRevision == 0 {
		t.Fatal("deadline crossing proof renewed stale authority")
	}
	if _, err := os.Stat(cfg.WatchdogLeasePath); err != nil {
		t.Fatalf("settled expiry unnecessarily gated process: %v", err)
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

func TestBINDActivationRacingBrokerDisconnectKeepsSafeLeaseAndOriginalDeadline(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	clock := now
	cfg := regionalTestConfig(t)
	runner := &afterReloadRunner{}
	publisher := &partitioningAckPublisher{}
	a, err := NewAgent(cfg, Dependencies{Runner: runner, Clock: func() time.Time { return clock }, AckPublisher: publisher, PublicationVerifier: installedFileVerifier(t, cfg.Render.ClusterBIND.Path)})
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
	// Disconnect after BIND reload, exactly before durable/proved activation
	// attempts its ACK. A transport error must not become a failed installation.
	runner.after = func() { publisher.disconnected.Store(true) }
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 5, manifest)); err != nil {
		t.Fatalf("safe activation treated broker failure as materialization failure: %v", err)
	}
	if !a.state.Publications["zone-a"].Verified || a.needsActivation {
		t.Fatal("ACK partition revoked proved publication")
	}
	if _, err := os.Stat(cfg.WatchdogLeasePath); err != nil {
		t.Fatalf("ACK partition removed safe local lease: %v", err)
	}
	// Identical manifest redelivery and snapshot delivery must share the same
	// best-effort ACK boundary without extending observation deadlines.
	if err := a.Handle(context.Background(), envelope(t, model.KindPublicationManifest, "zone-a", 2, 5, manifest)); err != nil {
		t.Fatal(err)
	}
	snapshot := testSnapshot(now)
	snapshot.ConfigurationRevision = 2
	snapshot.Bindings[0].Authorization.Revision = 2
	if err := a.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", 1, 2, snapshot)); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(61 * time.Second)
	if err := a.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(currentZone(t, a), "192.0.2.11") {
		t.Fatal("ACK partition left records past their original deadline")
	}
	if !a.state.Publications["zone-a"].Plan.ObservationFences[0].ValidUntil.Equal(now.Add(time.Minute)) {
		t.Fatal("activation or ACK retry renewed the observation deadline")
	}
	if _, err := os.Stat(cfg.WatchdogLeasePath); err != nil || a.needsActivation {
		t.Fatalf("safe local expiry was gated by ACK outage: %v", err)
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

func TestBINDTombstoneAckFailureDoesNotGateDurableWithdrawal(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	cfg := regionalTestConfig(t)
	publisher := &partitioningAckPublisher{}
	a, err := NewAgent(cfg, Dependencies{Runner: &recordingRunner{}, Clock: func() time.Time { return now }, AckPublisher: publisher, PublicationVerifier: installedFileVerifier(t, cfg.Render.ClusterBIND.Path)})
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
	if _, err := os.Stat(cfg.WatchdogLeasePath); err != nil {
		t.Fatalf("withdrawal lost safe lease because broker was unavailable: %v", err)
	}
}
