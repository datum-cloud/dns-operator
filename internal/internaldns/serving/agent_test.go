package serving

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
)

type fakePublisher struct{ envs []model.Envelope }

func (f *fakePublisher) Publish(_ context.Context, _ string, e model.Envelope) (transport.PublishAck, error) {
	f.envs = append(f.envs, e)
	return transport.PublishAck{}, nil
}

type stalledPublisher struct{}

func (stalledPublisher) Publish(ctx context.Context, _ string, _ model.Envelope) (transport.PublishAck, error) {
	<-ctx.Done()
	return transport.PublishAck{}, ctx.Err()
}

func TestServingAckDedupDoesNotSuppressANewerWithdrawalFence(t *testing.T) {
	now := time.Now().UTC()
	publisher := &fakePublisher{}
	agent := &Agent{
		cfg:            Config{Region: "east", Shard: "shared", MemberID: "member-a", ReplicaID: "member-a", AckInterval: time.Minute},
		deps:           Dependencies{AckPublisher: publisher, Clock: func() time.Time { return now }},
		state:          checkpoint{ServingFence: fence{Epoch: 1, Revision: 1}},
		lastServingAck: map[string]servingAckState{},
	}
	binding := model.Binding{BindingUID: "binding-a", ConfigurationRevision: 1, Version: model.AccessVersion{Epoch: 1, Sequence: 1}}
	if err := agent.emitServingAck(context.Background(), binding, model.AckVerified); err != nil {
		t.Fatal(err)
	}
	agent.state.ServingFence.Revision = 2
	binding.ConfigurationRevision = 2
	binding.Version.Sequence = 2
	binding.Tombstone = true
	if err := agent.emitServingAck(context.Background(), binding, model.AckVerified); err != nil {
		t.Fatal(err)
	}
	if len(publisher.envs) != 2 {
		t.Fatalf("new exact withdrawal ACK was deduplicated with the prior active ACK: got %d envelopes", len(publisher.envs))
	}
}

type contextIgnoringPublisher struct {
	mu      sync.Mutex
	calls   int
	started chan<- struct{}
	release <-chan struct{}
}

func (p *contextIgnoringPublisher) Publish(context.Context, string, model.Envelope) (transport.PublishAck, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	select {
	case p.started <- struct{}{}:
	default:
	}
	<-p.release
	return transport.PublishAck{}, errors.New("nats: server disconnected")
}

func (p *contextIgnoringPublisher) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func TestBoundedAckPublisherDoesNotStartExpiredRequest(t *testing.T) {
	t.Parallel()
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	close(release)
	delegate := &contextIgnoringPublisher{started: started, release: release}
	publisher := newBoundedAckPublisher(delegate)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := publisher.Publish(ctx, "ack", model.Envelope{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expired ACK request error = %v, want context cancellation", err)
	}
	if calls := delegate.callCount(); calls != 0 {
		t.Fatalf("expired ACK request invoked delegate %d times", calls)
	}
}

type blockingPublisher struct {
	started chan<- struct{}
	release <-chan struct{}
}

type resolverVerifierFunc func(context.Context, model.Binding) error

func (f resolverVerifierFunc) Verify(ctx context.Context, binding model.Binding) error {
	return f(ctx, binding)
}

func (p blockingPublisher) Publish(ctx context.Context, _ string, _ model.Envelope) (transport.PublishAck, error) {
	select {
	case p.started <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
		return transport.PublishAck{}, nil
	case <-ctx.Done():
		return transport.PublishAck{}, ctx.Err()
	}
}

type commandChannelRunner struct{ calls chan Command }

func (r *commandChannelRunner) Run(_ context.Context, command Command) ([]byte, error) {
	r.calls <- command
	return nil, nil
}

type boundedResolverVerifier struct {
	mu         sync.Mutex
	active     int
	maximum    int
	calls      int
	release    <-chan struct{}
	started    chan<- struct{}
	waitForCtx bool
}

func (v *boundedResolverVerifier) Verify(ctx context.Context, _ model.Binding) error {
	v.mu.Lock()
	v.active++
	v.calls++
	if v.active > v.maximum {
		v.maximum = v.active
	}
	v.mu.Unlock()
	if v.started != nil {
		select {
		case v.started <- struct{}{}:
		default:
		}
	}
	var err error
	if v.waitForCtx {
		<-ctx.Done()
		err = ctx.Err()
	} else {
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-v.release:
		}
	}
	v.mu.Lock()
	v.active--
	v.mu.Unlock()
	return err
}

func (v *boundedResolverVerifier) snapshot() (calls, maximum int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.calls, v.maximum
}

func envelope(t *testing.T, kind model.Kind, uid string, epoch, revision uint64, payload any) model.Envelope {
	t.Helper()
	e, err := model.NewEnvelope(kind, "event-"+string(kind)+uid+time.Now().String(), "east", "s1", uid, epoch, revision, time.Now(), payload)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func testSnapshot(now time.Time) model.ServingSnapshot {
	b := model.Binding{BindingUID: "binding-a", ProjectUID: "project-a", ContextUID: "vpc-a", BindingGeneration: 1, ConfigurationRevision: 1, ConsumerAddress: "10.253.0.51", ClusterAddress: "10.253.0.41", Port: 53, Transports: []model.Transport{model.TransportUDP, model.TransportTCP}, Zones: []model.ZoneAttachment{{ZoneUID: "zone-a", Apex: "prod.internal", RequiredPublicationEpoch: 2, RequiredPublicationRevision: 4}}, Version: model.AccessVersion{Epoch: 1, Sequence: 1}}
	return model.ServingSnapshot{Region: "east", Shard: "s1", ConfigurationEpoch: 1, ConfigurationRevision: 1, GeneratedAt: now, Bindings: []model.Binding{b}, Members: []model.Member{{MemberID: "regional-0", ReplicaID: "regional-0", Role: "regional"}}}
}

func testPublication(t *testing.T, now time.Time) (model.PublicationChunk, model.PublicationManifest) {
	t.Helper()
	plan := model.PublicationPlan{ZoneUID: "zone-a", Apex: "prod.internal", ObservationFences: []model.ObservationFence{{ContributionUID: "contrib-a", Sequence: 1, Generation: 1}}, Owners: []string{"api.prod.internal"}, RRSets: []model.RRSet{{Name: "api.prod.internal", Type: "A", TTL: 30, Records: []model.RRRecord{{Content: "192.0.2.10", ContributionUID: "contrib-a", Sequence: 1, Eligible: true, Generation: 1}}}}}
	payload, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	chunk := model.PublicationChunk{ManifestUID: "manifest-a", ZoneUID: "zone-a", WriterEpoch: 2, Revision: 4, Index: 0, SHA256: model.Hash(payload), Payload: payload}
	manifest := model.PublicationManifest{ManifestUID: "manifest-a", ZoneUID: "zone-a", Apex: "prod.internal", WriterEpoch: 2, Revision: 4, Chunks: []model.ChunkRef{{Index: 0, SHA256: chunk.SHA256, Size: len(payload)}}, ContentHash: model.Hash(payload), GeneratedAt: now}
	return chunk, manifest
}

func TestWithdrawnObservationFencesOlderHealthyRecord(t *testing.T) {
	t.Parallel()
	agent := &Agent{state: newCheckpoint()}
	withdrawn := model.PublicationPlan{
		ZoneUID: "zone-a", Apex: "prod.internal",
		ObservationFences: []model.ObservationFence{{ContributionUID: "contrib-a", Sequence: 33, Generation: 7}},
		RRSets:            []model.RRSet{},
	}
	if err := withdrawn.Validate(); err != nil {
		t.Fatal(err)
	}
	agent.commitContributionFences(withdrawn)
	olderHealthy := model.PublicationPlan{
		ZoneUID: "zone-a", Apex: "prod.internal",
		ObservationFences: []model.ObservationFence{{ContributionUID: "contrib-a", Sequence: 32, Generation: 7}},
		RRSets:            []model.RRSet{{Name: "api.prod.internal", Type: "A", TTL: 30, Records: []model.RRRecord{{Content: "192.0.2.10", ContributionUID: "contrib-a", Sequence: 32, Eligible: true, Generation: 7}}}},
	}
	if err := olderHealthy.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := agent.validateContributionFences(olderHealthy); err == nil {
		t.Fatal("older healthy observation was accepted after a higher withdrawn observation")
	}
}

func TestClusterRoleAutomaticallyVerifiesClusterVIP(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	renderConfig, _ := renderFixture(t)
	renderConfig.Role = RenderCluster
	renderConfig.ClusterDNSDist.Path = filepath.Join(dir, "cluster-dnsdist.conf")
	renderConfig.ClusterCoreDNS.Path = filepath.Join(dir, "cluster-coredns.json")
	renderConfig.NodeDNSDist = DNSDistRenderConfig{}
	renderConfig.NodeCoreDNS = CoreDNSRenderConfig{}
	agent, err := NewAgent(Config{
		Region: "east", Shard: "s1", MemberID: "cluster-0", StateDir: filepath.Join(dir, "state"), Mode: ModeResolver,
		Render: renderConfig, Transaction: TransactionConfig{StageDir: filepath.Join(dir, "stage"), FailClosed: []Command{{Path: "gate"}}},
		UnsafeAllowNoFailClosed: true,
	}, Dependencies{Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	verifier, ok := agent.deps.Verifier.(DNSVerifier)
	if !ok || !verifier.Config.UseClusterAddress {
		t.Fatalf("cluster agent verifier = %#v, want cluster VIP", agent.deps.Verifier)
	}
}

func TestStartActivatesPersistedDesiredConfiguration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	renderConfig, snapshot := renderFixture(t)
	renderConfig.Role = RenderNode
	renderConfig.NodeDNSDist.Path = filepath.Join(dir, "runtime", "node-dnsdist.conf")
	renderConfig.NodeCoreDNS.Path = filepath.Join(dir, "runtime", "node-coredns.json")
	renderConfig.ClusterDNSDist = DNSDistRenderConfig{}
	renderConfig.ClusterCoreDNS = CoreDNSRenderConfig{}
	rendered, err := Render(renderConfig, snapshot, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	for path, contents := range rendered.Files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, contents, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stateDir := filepath.Join(dir, "state")
	state := newCheckpoint()
	state.Snapshot = &snapshot
	state.BindingFences, _ = advanceBindingFences(nil, snapshot)
	state.ServingFence = fence{Epoch: snapshot.ConfigurationEpoch, Revision: snapshot.ConfigurationRevision}
	state.RenderedHash = hashFiles(rendered.Files)
	if err := (stateStore{path: filepath.Join(stateDir, "checkpoint.json")}).Save(state); err != nil {
		t.Fatal(err)
	}
	config := Config{
		Region: "east", Shard: "s1", MemberID: "node-0", StateDir: stateDir, Mode: ModeResolver,
		Render: renderConfig, UnsafeAllowNoDNSVerify: true,
		ReconcileInterval: time.Hour,
		Transaction: TransactionConfig{
			StageDir:   filepath.Join(dir, "stage"),
			FailClosed: []Command{{Path: "gate"}},
			Validate: []Command{
				{Path: "validate-dnsdist", Args: []string{"{stageDir}/node-dnsdist.conf"}, WhenChanged: []string{renderConfig.NodeDNSDist.Path}},
				{Path: "validate-bind", Args: []string{"{stageDir}/node-coredns.json"}, WhenChanged: []string{renderConfig.NodeCoreDNS.Path}},
			},
			Reload: []Command{
				{Path: "reload-dnsdist", WhenChanged: []string{renderConfig.NodeDNSDist.Path}},
				{Path: "reload-bind", WhenChanged: []string{renderConfig.NodeCoreDNS.Path}},
			},
		},
	}
	runner := &commandChannelRunner{calls: make(chan Command, 16)}
	agent, err := NewAgent(config, Dependencies{Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.Start(ctx) }()
	deadline := time.After(2 * time.Second)
	for {
		if state, err := agent.store.Load(); err == nil && state.Active != nil {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("startup did not approve persisted desired configuration")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var calls []string
	for len(runner.calls) > 0 {
		calls = append(calls, (<-runner.calls).Path)
	}
	for _, required := range []string{"validate-dnsdist", "validate-bind", "reload-dnsdist", "reload-bind"} {
		if !slices.Contains(calls, required) {
			t.Fatalf("startup omitted %s: %#v", required, calls)
		}
	}
	assertAppliedCheckpoint(t, agent)

}

func TestStepBoundsStalledAckPublishAndRetainsAppliedConfiguration(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	renderConfig, snapshot := renderFixture(t)
	renderConfig.Role = RenderNode
	renderConfig.NodeDNSDist.Path = filepath.Join(dir, "runtime", "node-dnsdist.conf")
	renderConfig.NodeCoreDNS.Path = filepath.Join(dir, "runtime", "node-coredns.json")
	renderConfig.ClusterDNSDist = DNSDistRenderConfig{}
	renderConfig.ClusterCoreDNS = CoreDNSRenderConfig{}
	snapshot.Bindings[0].Zones = nil
	var reported []error
	agent, err := NewAgent(Config{
		Region: "east", Shard: "s1", MemberID: "node-0", StateDir: filepath.Join(dir, "state"), Mode: ModeResolver,
		Render: renderConfig, Transaction: TransactionConfig{StageDir: filepath.Join(dir, "stage"), FailClosed: []Command{{Path: "gate"}}},
		UnsafeAllowNoDNSVerify: true, AckPublishTimeout: 30 * time.Millisecond,
	}, Dependencies{Runner: &recordingRunner{}, OnError: func(err error) { reported = append(reported, err) }})
	if err != nil {
		t.Fatal(err)
	}
	agent.state.Snapshot = &snapshot
	agent.state.BindingFences, _ = advanceBindingFences(nil, snapshot)
	agent.state.ServingFence = fence{Epoch: 1, Revision: 1}
	// Complete durable activation before measuring the ACK publication bound.
	if err := agent.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	agent.deps.AckPublisher = stalledPublisher{}
	agent.lastServingAck = make(map[string]servingAckState)
	started := time.Now()
	if err := agent.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Step blocked %s on an unavailable ACK broker", elapsed)
	}
	if len(reported) == 0 || !strings.Contains(reported[0].Error(), "publish serving ACK") {
		t.Fatalf("stalled ACK publish was not reported: %#v", reported)
	}
	assertAppliedCheckpoint(t, agent)
}

func stepUntilBlockedAck(t *testing.T, agent *Agent, started <-chan struct{}, release chan struct{}) func() error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	var stepErr error
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		cancel()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("Step did not finish after unconditional ACK release and cancellation")
		}
	})
	go func() {
		stepErr = agent.Step(ctx)
		close(finished)
	}()
	select {
	case <-started:
	case <-finished:
		t.Fatalf("Step finished before blocked ACK verification: %v", stepErr)
	case <-time.After(10 * time.Second):
		t.Fatal("resolver did not reach broker ACK publication")
	}
	return func() error {
		unblock()
		select {
		case <-finished:
			return stepErr
		case <-time.After(10 * time.Second):
			return errors.New("Step did not finish after ACK release")
		}
	}
}

func TestResolverApprovesConfigurationBeforeBrokerAckCompletes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	renderConfig, snapshot := renderFixture(t)
	renderConfig.Role = RenderNode
	renderConfig.NodeDNSDist.Path = filepath.Join(dir, "runtime", "node-dnsdist.conf")
	renderConfig.NodeCoreDNS.Path = filepath.Join(dir, "runtime", "node-coredns.json")
	renderConfig.ClusterDNSDist = DNSDistRenderConfig{}
	renderConfig.ClusterCoreDNS = CoreDNSRenderConfig{}
	snapshot.Bindings[0].Zones = nil
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	agent, err := NewAgent(Config{
		Region: "east", Shard: "s1", MemberID: "node-0", StateDir: filepath.Join(dir, "state"), Mode: ModeResolver,
		Render: renderConfig, Transaction: TransactionConfig{StageDir: filepath.Join(dir, "stage"), FailClosed: []Command{{Path: "gate"}}},
		UnsafeAllowNoDNSVerify: true, AckPublishTimeout: time.Second,
	}, Dependencies{AckPublisher: blockingPublisher{started: started, release: release}, Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	agent.state.Snapshot = &snapshot
	agent.state.BindingFences, _ = advanceBindingFences(nil, snapshot)
	agent.state.ServingFence = fence{Epoch: 1, Revision: 1}
	finish := stepUntilBlockedAck(t, agent, started, release)
	assertAppliedCheckpoint(t, agent)
	if err := finish(); err != nil {
		t.Fatal(err)
	}
}

func TestResolverGatesFailedTenantAndRetainsHealthyTenant(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	renderConfig, snapshot := renderFixture(t)
	renderConfig.Role = RenderNode
	renderConfig.NodeDNSDist.Path = filepath.Join(dir, "runtime", "node-dnsdist.conf")
	renderConfig.NodeCoreDNS.Path = filepath.Join(dir, "runtime", "node-coredns.json")
	renderConfig.ClusterDNSDist = DNSDistRenderConfig{}
	renderConfig.ClusterCoreDNS = CoreDNSRenderConfig{}
	snapshot.Bindings[0].Zones = nil
	failedUID := snapshot.Bindings[0].BindingUID
	healthy := snapshot.Bindings[0]
	healthy.BindingUID = "binding-b"
	healthy.ProjectUID = "project-b"
	healthy.ContextUID = "vpc-b"
	healthy.ConsumerAddress = "10.253.0.52"
	healthy.ClusterAddress = "10.253.0.42"
	snapshot.Bindings = append(snapshot.Bindings, healthy)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	agent, err := NewAgent(Config{
		Region: "east", Shard: "s1", MemberID: "node-0", StateDir: filepath.Join(dir, "state"), Mode: ModeResolver,
		Render: renderConfig, Transaction: TransactionConfig{StageDir: filepath.Join(dir, "stage"), FailClosed: []Command{{Path: "gate"}}},
		UnsafeAllowNoFailClosed: true, AckPublishTimeout: time.Second,
	}, Dependencies{
		AckPublisher: blockingPublisher{started: started, release: release}, Runner: &recordingRunner{},
		Verifier: resolverVerifierFunc(func(_ context.Context, binding model.Binding) error {
			if binding.BindingUID == failedUID {
				return errors.New("probe failed")
			}
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.state.Snapshot = &snapshot
	agent.state.BindingFences, _ = advanceBindingFences(nil, snapshot)
	agent.state.ServingFence = fence{Epoch: 1, Revision: 1}
	finish := stepUntilBlockedAck(t, agent, started, release)
	config, err := os.ReadFile(renderConfig.NodeDNSDist.Path)
	if err != nil {
		t.Fatal(err)
	}
	if bindingReadyInConfig(string(config), snapshot.Bindings[0].ConsumerAddress) {
		t.Fatal("failed tenant remained routed")
	}
	if !strings.Contains(string(config), healthy.ConsumerAddress) {
		t.Fatal("healthy tenant was gated with failed tenant")
	}
	assertAppliedCheckpoint(t, agent)
	if err := finish(); err != nil {
		t.Fatal(err)
	}
}

func TestStartPreservesAppliedConfigurationWhileBrokerDisconnected(t *testing.T) {
	dir := t.TempDir()
	renderConfig, snapshot := renderFixture(t)
	renderConfig.Role = RenderNode
	renderConfig.NodeDNSDist.Path = filepath.Join(dir, "runtime", "node-dnsdist.conf")
	renderConfig.NodeCoreDNS.Path = filepath.Join(dir, "runtime", "node-coredns.json")
	renderConfig.ClusterDNSDist = DNSDistRenderConfig{}
	renderConfig.ClusterCoreDNS = CoreDNSRenderConfig{}
	snapshot.Bindings[0].Zones = nil
	publishStarted := make(chan struct{}, 1)
	publishRelease := make(chan struct{})
	publisher := &contextIgnoringPublisher{started: publishStarted, release: publishRelease}
	agent, err := NewAgent(Config{
		Region: "east", Shard: "s1", MemberID: "node-0", StateDir: filepath.Join(dir, "state"), Mode: ModeResolver,
		Render: renderConfig, Transaction: TransactionConfig{StageDir: filepath.Join(dir, "stage"), FailClosed: []Command{{Path: "gate"}}},
		UnsafeAllowNoFailClosed: true, UnsafeAllowNoDNSVerify: true,
		ReconcileInterval: 20 * time.Millisecond, AckInterval: 50 * time.Millisecond,
		AckPublishTimeout: 10 * time.Millisecond,
	}, Dependencies{AckPublisher: publisher, Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	agent.state.Snapshot = &snapshot
	agent.state.BindingFences, _ = advanceBindingFences(nil, snapshot)
	agent.state.ServingFence = fence{Epoch: 1, Revision: 1}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- agent.Start(ctx) }()
	var stopOnce sync.Once
	var stopErr error
	stopAgent := func() {
		stopOnce.Do(func() {
			cancel()
			select {
			case stopErr = <-done:
			case <-time.After(time.Second):
				stopErr = errors.New("resolver did not stop after continuity test")
			}
		})
	}
	defer stopAgent()
	var releaseOnce sync.Once
	releasePublisher := func() { releaseOnce.Do(func() { close(publishRelease) }) }
	defer releasePublisher()
	select {
	case <-publishStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("resolver did not enter context-ignoring broker publish")
	}

	started := time.Now()
	var continuityErr error
	for time.Since(started) < 1200*time.Millisecond {
		state, err := agent.store.Load()
		if err != nil || state.Active == nil || !installedFilesMatch(state.Active.Files) || activationPending(agent.cfg.StateDir) {
			continuityErr = fmt.Errorf("applied configuration lost during broker partition: %w", err)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if calls := publisher.callCount(); calls != 1 {
		continuityErr = fmt.Errorf("context-ignoring publisher calls = %d, want one bounded inflight call", calls)
	}
	releasePublisher()
	stopAgent()
	if stopErr != nil {
		t.Fatal(stopErr)
	}
	if continuityErr != nil {
		t.Fatal(continuityErr)
	}
}

func TestResolverVerificationUsesBoundedConcurrentBatch(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	started := make(chan struct{}, 8)
	verifier := &boundedResolverVerifier{release: release, started: started}
	agent := &Agent{
		cfg:  Config{ResolverVerifyConcurrency: 3, ResolverDNSProbe: DNSVerifierConfig{Timeout: time.Second}},
		deps: Dependencies{Verifier: verifier},
	}
	bindings := make([]model.Binding, 8)
	active := make(map[string]bool, len(bindings))
	for i := range bindings {
		bindings[i].BindingUID = fmt.Sprintf("binding-%d", i)
		active[bindings[i].BindingUID] = true
	}
	done := make(chan bool, 1)
	go func() { done <- agent.verifyResolverBindings(context.Background(), bindings, active) }()
	for range 3 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("verification workers did not start concurrently")
		}
	}
	if calls, maximum := verifier.snapshot(); calls != 3 || maximum != 3 {
		t.Fatalf("before release calls=%d max=%d, want exactly three workers", calls, maximum)
	}
	close(release)
	if failed := <-done; failed {
		t.Fatal("successful resolver proof batch was marked failed")
	}
	if calls, maximum := verifier.snapshot(); calls != len(bindings) || maximum > 3 {
		t.Fatalf("completed calls=%d max=%d, want calls=%d max<=3", calls, maximum, len(bindings))
	}
}

func TestResolverVerificationHasOneSharedDeadline(t *testing.T) {
	t.Parallel()
	verifier := &boundedResolverVerifier{waitForCtx: true}
	agent := &Agent{
		cfg:  Config{ResolverVerifyConcurrency: 2, ResolverDNSProbe: DNSVerifierConfig{Timeout: 40 * time.Millisecond}},
		deps: Dependencies{Verifier: verifier},
	}
	bindings := make([]model.Binding, 10)
	active := make(map[string]bool, len(bindings))
	for i := range bindings {
		bindings[i].BindingUID = fmt.Sprintf("binding-%d", i)
		active[bindings[i].BindingUID] = true
	}
	started := time.Now()
	if failed := agent.verifyResolverBindings(context.Background(), bindings, active); !failed {
		t.Fatal("timed out resolver proof batch was accepted")
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("shared resolver proof deadline took %s", elapsed)
	}
	if len(active) != 0 {
		t.Fatalf("timed out resolver proof batch left %d bindings active", len(active))
	}
}

func TestAckEventPreservesAppliedConfigurationAfterReconcile(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	dir := t.TempDir()
	renderConfig, snapshot := renderFixture(t)
	renderConfig.Role = RenderNode
	renderConfig.NodeDNSDist.Path = filepath.Join(dir, "runtime", "node-dnsdist.conf")
	renderConfig.NodeCoreDNS.Path = filepath.Join(dir, "runtime", "node-coredns.json")
	renderConfig.ClusterDNSDist = DNSDistRenderConfig{}
	renderConfig.ClusterCoreDNS = CoreDNSRenderConfig{}
	snapshot.Members = []model.Member{{MemberID: "regional-0", ReplicaID: "regional-0", Role: "regional"}}
	agent, err := NewAgent(Config{
		Region: "east", Shard: "s1", MemberID: "node-0", StateDir: filepath.Join(dir, "state"), Mode: ModeResolver,
		Render: renderConfig, Transaction: TransactionConfig{StageDir: filepath.Join(dir, "stage"), FailClosed: []Command{{Path: "gate"}}},
		UnsafeAllowNoFailClosed: true, UnsafeAllowNoDNSVerify: true,
	}, Dependencies{Clock: func() time.Time { return now }, Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", snapshot.ConfigurationEpoch, snapshot.ConfigurationRevision, snapshot)); err != nil {
		t.Fatal(err)
	}
	ack := model.MemberAck{MemberID: "regional-0", ReplicaID: "regional-0", ResourceUID: "zone-a", Kind: model.KindPublicationManifest, Epoch: 2, Revision: 4, Phase: model.AckVerified, ObservedAt: now, ValidUntil: now.Add(time.Minute)}
	if err := agent.Handle(context.Background(), envelope(t, model.KindMemberAck, "zone-a", 2, 4, ack)); err != nil {
		t.Fatal(err)
	}
	assertAppliedCheckpoint(t, agent)
}

func TestExpiredKnownAckReplayDrainsWithoutStateChange(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	agent := &Agent{
		cfg:  Config{Region: "east", Shard: "s1", Mode: ModeResolver},
		deps: Dependencies{Clock: func() time.Time { return now }}, state: newCheckpoint(),
		lastServingAck: map[string]servingAckState{}, lastPublicationAck: map[string]time.Time{},
	}
	snapshot := testSnapshot(now)
	agent.state.Snapshot = &snapshot
	agent.state.BindingFences, _ = advanceBindingFences(nil, snapshot)
	ack := model.MemberAck{MemberID: "regional-0", ReplicaID: "regional-0", ResourceUID: "zone-a", Kind: model.KindPublicationManifest, Epoch: 2, Revision: 4, Phase: model.AckVerified, ObservedAt: now.Add(-2 * time.Minute), ValidUntil: now.Add(-time.Minute)}
	if err := agent.Handle(context.Background(), envelope(t, model.KindMemberAck, "zone-a", 2, 4, ack)); err != nil {
		t.Fatalf("expired known ACK replay was not drained: %v", err)
	}
	if len(agent.state.Acks) != 0 {
		t.Fatalf("expired ACK changed readiness state: %#v", agent.state.Acks)
	}
	if agent.state.Active != nil {
		t.Fatal("expired ACK changed applied configuration")
	}
	ack.MemberID = "regional-0"
	ack.ValidUntil = ack.ObservedAt.Add(2*time.Minute + time.Nanosecond)
	if err := agent.Handle(context.Background(), envelope(t, model.KindMemberAck, "zone-a", 2, 4, ack)); err == nil {
		t.Fatal("overlong ACK lease was accepted")
	}
}

func assertAppliedCheckpoint(t *testing.T, agent *Agent) {
	t.Helper()
	state, err := agent.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Active == nil || !installedFilesMatch(state.Active.Files) || activationPending(agent.cfg.StateDir) {
		t.Fatal("approved configuration is not durably active")
	}
}

func TestResolverVerificationProbesOnlyChangedBindings(t *testing.T) {
	t.Parallel()
	var probed []string
	failing := false
	agent := &Agent{
		cfg: Config{ResolverVerifyConcurrency: 1, ResolverDNSProbe: DNSVerifierConfig{Timeout: time.Second}},
		deps: Dependencies{Verifier: resolverVerifierFunc(func(_ context.Context, binding model.Binding) error {
			probed = append(probed, binding.BindingUID)
			if failing {
				return errors.New("probe timed out")
			}
			return nil
		})},
	}
	bindings := []model.Binding{{BindingUID: "binding-0"}, {BindingUID: "binding-1"}}
	activeSet := func() map[string]bool { return map[string]bool{"binding-0": true, "binding-1": true} }

	if failed := agent.verifyResolverBindings(context.Background(), bindings, activeSet()); failed || len(probed) != 2 {
		t.Fatalf("first verification failed=%v probed=%v", failed, probed)
	}
	failing, probed = true, nil
	active := activeSet()
	if failed := agent.verifyResolverBindings(context.Background(), bindings, active); failed || len(probed) != 0 || !active["binding-0"] || !active["binding-1"] {
		t.Fatalf("unchanged verified bindings were probed again or withdrawn: failed=%v probed=%v active=%v", failed, probed, active)
	}
	bindings[1].Version.Sequence++
	active = activeSet()
	if failed := agent.verifyResolverBindings(context.Background(), bindings, active); !failed || len(probed) != 1 || probed[0] != "binding-1" || !active["binding-0"] || active["binding-1"] {
		t.Fatalf("changed binding was not proven alone: failed=%v probed=%v active=%v", failed, probed, active)
	}
	failing, probed = false, nil
	if failed := agent.verifyResolverBindings(context.Background(), bindings, activeSet()); failed || len(probed) != 1 || probed[0] != "binding-1" {
		t.Fatalf("withdrawn binding was not proven again: failed=%v probed=%v", failed, probed)
	}
	agent.verifiedPaths = nil
	probed = nil
	if agent.verifyResolverBindings(context.Background(), bindings, activeSet()); len(probed) != 2 {
		t.Fatalf("cleared proofs did not reprove every binding: %v", probed)
	}
	agent.tx.restarts++
	probed = nil
	if agent.verifyResolverBindings(context.Background(), bindings, activeSet()); len(probed) != 2 {
		t.Fatalf("daemon restart did not reprove every binding: %v", probed)
	}
}

func TestAckFromUnknownMemberIsDrainedWithoutStateChange(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	var reported []error
	agent := &Agent{
		cfg:  Config{Region: "east", Shard: "s1", Mode: ModeResolver},
		deps: Dependencies{Clock: func() time.Time { return now }, OnError: func(err error) { reported = append(reported, err) }}, state: newCheckpoint(),
		lastServingAck: map[string]servingAckState{}, lastPublicationAck: map[string]time.Time{},
	}
	snapshot := testSnapshot(now)
	agent.state.Snapshot = &snapshot
	agent.state.BindingFences, _ = advanceBindingFences(nil, snapshot)
	for _, ack := range []model.MemberAck{
		{MemberID: "unknown", ReplicaID: "unknown", ResourceUID: "zone-a", Kind: model.KindPublicationManifest, Epoch: 2, Revision: 4, Phase: model.AckRejected, ObservedAt: now, ValidUntil: now.Add(time.Minute)},
		{MemberID: "unknown", ReplicaID: "unknown", ResourceUID: "zone-a", Kind: model.KindPublicationManifest, Epoch: 2, Revision: 4, Phase: model.AckVerified, ObservedAt: now.Add(-2 * time.Minute), ValidUntil: now.Add(-time.Minute)},
	} {
		if err := agent.Handle(context.Background(), envelope(t, model.KindMemberAck, "zone-a", 2, 4, ack)); err != nil {
			t.Fatalf("ACK from an unknown member was left for redelivery: %v", err)
		}
	}
	if len(agent.state.Acks) != 0 || agent.state.Active != nil {
		t.Fatalf("ACK from an unknown member changed state: acks=%#v", agent.state.Acks)
	}
	if len(reported) != 1 {
		t.Fatalf("want only the live ACK from an unknown member reported, got %v", reported)
	}
}

func TestExpiredAckDrainsWhileAgentIsBusy(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	agent := &Agent{
		cfg:  Config{Region: "east", Shard: "s1", Mode: ModeResolver},
		deps: Dependencies{Clock: func() time.Time { return now }}, state: newCheckpoint(),
		lastServingAck: map[string]servingAckState{}, lastPublicationAck: map[string]time.Time{},
	}
	snapshot := testSnapshot(now)
	agent.state.Snapshot = &snapshot
	ack := model.MemberAck{MemberID: "regional", ReplicaID: "r1", ResourceUID: "zone-a", Kind: model.KindPublicationManifest, Epoch: 2, Revision: 4, Phase: model.AckVerified, ObservedAt: now.Add(-2 * time.Minute), ValidUntil: now.Add(-time.Minute)}
	env := envelope(t, model.KindMemberAck, "zone-a", 2, 4, ack)
	agent.mu.Lock()
	defer agent.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- agent.Handle(context.Background(), env) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expired ACK was left for redelivery: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("expired ACK waited for the agent lock")
	}
}

func TestSameFenceAckRefreshDoesNotRewriteCheckpoint(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	dir := t.TempDir()
	renderConfig, snapshot := renderFixture(t)
	renderConfig.Role = RenderNode
	renderConfig.NodeDNSDist.Path = filepath.Join(dir, "runtime", "node-dnsdist.conf")
	renderConfig.NodeCoreDNS.Path = filepath.Join(dir, "runtime", "node-coredns.json")
	renderConfig.ClusterDNSDist = DNSDistRenderConfig{}
	renderConfig.ClusterCoreDNS = CoreDNSRenderConfig{}
	snapshot.Members = []model.Member{{MemberID: "regional-0", ReplicaID: "regional-0", Role: "regional"}}
	agent, err := NewAgent(Config{
		Region: "east", Shard: "s1", MemberID: "node-0", StateDir: filepath.Join(dir, "state"), Mode: ModeResolver,
		Render: renderConfig, Transaction: TransactionConfig{StageDir: filepath.Join(dir, "stage"), FailClosed: []Command{{Path: "gate"}}},
		UnsafeAllowNoFailClosed: true, UnsafeAllowNoDNSVerify: true,
	}, Dependencies{Clock: func() time.Time { return now }, Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", snapshot.ConfigurationEpoch, snapshot.ConfigurationRevision, snapshot)); err != nil {
		t.Fatal(err)
	}
	ack := model.MemberAck{MemberID: "regional-0", ReplicaID: "regional-0", ResourceUID: "zone-a", Kind: model.KindPublicationManifest, Epoch: 2, Revision: 4, Phase: model.AckVerified, ObservedAt: now, ValidUntil: now.Add(time.Minute)}
	if err := agent.Handle(context.Background(), envelope(t, model.KindMemberAck, "zone-a", 2, 4, ack)); err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(dir, "state", "checkpoint.json")
	saved, err := os.ReadFile(checkpoint)
	if err != nil {
		t.Fatal(err)
	}

	ack.ObservedAt, ack.ValidUntil = now.Add(time.Second), now.Add(time.Minute+time.Second)
	if err := agent.Handle(context.Background(), envelope(t, model.KindMemberAck, "zone-a", 2, 4, ack)); err != nil {
		t.Fatal(err)
	}
	if got := agent.state.Acks["zone-a"]["regional-0/regional-0"].ValidUntil; !got.Equal(ack.ValidUntil) {
		t.Fatalf("refresh did not extend the lease in memory: %s", got)
	}
	if after, err := os.ReadFile(checkpoint); err != nil || !bytes.Equal(saved, after) {
		t.Fatalf("same-fence refresh rewrote the checkpoint (err %v)", err)
	}

	ack.Revision = 5
	if err := agent.Handle(context.Background(), envelope(t, model.KindMemberAck, "zone-a", 2, 5, ack)); err != nil {
		t.Fatal(err)
	}
	if after, err := os.ReadFile(checkpoint); err != nil || bytes.Equal(saved, after) {
		t.Fatalf("a new fence was not persisted (err %v)", err)
	}

	rejected := ack
	rejected.Phase, rejected.ObservedAt = model.AckRejected, now.Add(2*time.Second)
	if err := agent.Handle(context.Background(), envelope(t, model.KindMemberAck, "zone-a", 2, 5, rejected)); err != nil {
		t.Fatal(err)
	}
	ack.ObservedAt, ack.ValidUntil = now.Add(3*time.Second), now.Add(time.Minute+3*time.Second)
	if err := agent.Handle(context.Background(), envelope(t, model.KindMemberAck, "zone-a", 2, 5, ack)); err != nil {
		t.Fatal(err)
	}
	state, err := agent.store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Acks["zone-a"]["regional-0/regional-0"].Rejected {
		t.Fatal("a verified ack after a rejection at the same fence was not persisted")
	}
}
