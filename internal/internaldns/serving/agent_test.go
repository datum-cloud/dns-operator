package serving

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	b := model.Binding{BindingUID: "binding-a", ProjectUID: "project-a", ContextUID: "vpc-a", BindingGeneration: 1, ConfigurationRevision: 1, ConsumerAddress: "10.253.0.51", ClusterAddress: "10.253.0.41", Port: 53, Transports: []model.Transport{model.TransportUDP, model.TransportTCP}, Zones: []model.ZoneAttachment{{ZoneUID: "zone-a", Apex: "prod.internal", RequiredPublicationEpoch: 2, RequiredPublicationRevision: 4}}, Authorization: model.Authorization{IssuerEpoch: 1, Revision: 1, ValidUntil: now.Add(10 * time.Minute)}}
	return model.ServingSnapshot{Region: "east", Shard: "s1", ConfigurationEpoch: 1, ConfigurationRevision: 1, GeneratedAt: now, Bindings: []model.Binding{b}, Members: []model.Member{{MemberID: "regional-0", ReplicaID: "regional-0", Role: "regional"}}}
}

func testPublication(t *testing.T, now time.Time) (model.PublicationChunk, model.PublicationManifest) {
	t.Helper()
	validUntil := now.Add(time.Minute)
	plan := model.PublicationPlan{ZoneUID: "zone-a", Apex: "prod.internal", ObservationFences: []model.ObservationFence{{ContributionUID: "contrib-a", GrantUID: "grant-a", WriterEpoch: 1, Sequence: 1, ValidUntil: validUntil}}, Owners: []string{"api.prod.internal"}, RRSets: []model.RRSet{{Name: "api.prod.internal", Type: "A", TTL: 30, Records: []model.RRRecord{{Content: "192.0.2.10", ContributionUID: "contrib-a", WriterEpoch: 1, Sequence: 1, Eligible: true, ValidUntil: validUntil}}}}}
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
	now := time.Now().UTC()
	agent := &Agent{state: newCheckpoint()}
	withdrawn := model.PublicationPlan{
		ZoneUID: "zone-a", Apex: "prod.internal",
		ObservationFences: []model.ObservationFence{{ContributionUID: "contrib-a", GrantUID: "grant-a", WriterEpoch: 7, Sequence: 33, ValidUntil: now.Add(-time.Minute)}},
		RRSets:            []model.RRSet{},
	}
	if err := withdrawn.Validate(); err != nil {
		t.Fatal(err)
	}
	agent.commitContributionFences(withdrawn)
	olderDeadline := now.Add(time.Minute)
	olderHealthy := model.PublicationPlan{
		ZoneUID: "zone-a", Apex: "prod.internal",
		ObservationFences: []model.ObservationFence{{ContributionUID: "contrib-a", GrantUID: "grant-a", WriterEpoch: 7, Sequence: 32, ValidUntil: olderDeadline}},
		RRSets: []model.RRSet{{Name: "api.prod.internal", Type: "A", TTL: 30, Records: []model.RRRecord{{
			Content: "192.0.2.10", ContributionUID: "contrib-a", WriterEpoch: 7, Sequence: 32, Eligible: true, ValidUntil: olderDeadline,
		}}}},
	}
	if err := olderHealthy.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := agent.validateContributionFences(olderHealthy); err == nil {
		t.Fatal("older healthy observation was accepted after a higher withdrawn observation")
	}
}

func TestObservationFenceReplayCannotExtendDeadline(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	agent := &Agent{state: newCheckpoint()}
	original := model.PublicationPlan{ZoneUID: "zone-a", Apex: "prod.internal", ObservationFences: []model.ObservationFence{{ContributionUID: "contrib-a", GrantUID: "grant-a", WriterEpoch: 2, Sequence: 4, ValidUntil: now}}, RRSets: []model.RRSet{}}
	agent.commitContributionFences(original)
	replay := original
	replay.ObservationFences = append([]model.ObservationFence(nil), original.ObservationFences...)
	replay.ObservationFences[0].ValidUntil = now.Add(time.Hour)
	if err := agent.validateContributionFences(replay); err == nil {
		t.Fatal("equal observation fence extended its original deadline")
	}
}

func TestClusterRoleAutomaticallyVerifiesClusterVIP(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	renderConfig, _ := renderFixture(t)
	renderConfig.Role = RenderCluster
	renderConfig.ClusterDNSDist.Path = filepath.Join(dir, "cluster-dnsdist.conf")
	renderConfig.ClusterBIND.Path = filepath.Join(dir, "cluster-bind.conf")
	renderConfig.NodeDNSDist = DNSDistRenderConfig{}
	renderConfig.NodeBIND = BINDRenderConfig{}
	agent, err := NewAgent(Config{
		Region: "east", Shard: "s1", MemberID: "cluster-0", StateDir: filepath.Join(dir, "state"), Mode: ModeResolver,
		Render: renderConfig, Transaction: TransactionConfig{StageDir: filepath.Join(dir, "stage")},
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

func TestStartForceActivatesPersistedUnchangedConfigBeforeLease(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	renderConfig, snapshot := renderFixture(t)
	renderConfig.Role = RenderNode
	renderConfig.NodeDNSDist.Path = filepath.Join(dir, "runtime", "node-dnsdist.conf")
	renderConfig.NodeBIND.Path = filepath.Join(dir, "runtime", "node-bind.conf")
	renderConfig.ClusterDNSDist = DNSDistRenderConfig{}
	renderConfig.ClusterBIND = BINDRenderConfig{}
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
	leasePath := filepath.Join(dir, "lease.json")
	config := Config{
		Region: "east", Shard: "s1", MemberID: "node-0", StateDir: stateDir, Mode: ModeResolver,
		Render: renderConfig, UnsafeAllowNoDNSVerify: true, ExternalWatchdog: true,
		WatchdogLeasePath: leasePath, WatchdogLease: 5 * time.Second, ExpiryInterval: time.Hour,
		Transaction: TransactionConfig{
			StageDir:   filepath.Join(dir, "stage"),
			FailClosed: []Command{{Path: "gate"}},
			Validate: []Command{
				{Path: "validate-dnsdist", Args: []string{"{stageDir}/node-dnsdist.conf"}, WhenChanged: []string{renderConfig.NodeDNSDist.Path}},
				{Path: "validate-bind", Args: []string{"{stageDir}/node-bind.conf"}, WhenChanged: []string{renderConfig.NodeBIND.Path}},
			},
			Reload: []Command{
				{Path: "reload-dnsdist", WhenChanged: []string{renderConfig.NodeDNSDist.Path}},
				{Path: "reload-bind", WhenChanged: []string{renderConfig.NodeBIND.Path}},
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
		if _, err := os.Stat(leasePath); err == nil {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatal("startup did not activate unchanged config and write its lease")
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
	wantPrefix := []string{"gate", "validate-dnsdist", "validate-bind", "reload-dnsdist", "reload-bind"}
	if len(calls) < len(wantPrefix) {
		t.Fatalf("startup calls = %#v", calls)
	}
	for i, want := range wantPrefix {
		if calls[i] != want {
			t.Fatalf("startup calls = %#v, want prefix %#v", calls, wantPrefix)
		}
	}
}

func TestStepBoundsStalledAckPublishAndRenewsLocalLease(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	renderConfig, snapshot := renderFixture(t)
	renderConfig.Role = RenderNode
	renderConfig.NodeDNSDist.Path = filepath.Join(dir, "runtime", "node-dnsdist.conf")
	renderConfig.NodeBIND.Path = filepath.Join(dir, "runtime", "node-bind.conf")
	renderConfig.ClusterDNSDist = DNSDistRenderConfig{}
	renderConfig.ClusterBIND = BINDRenderConfig{}
	snapshot.Bindings[0].Zones = nil
	leasePath := filepath.Join(dir, "lease.json")
	var reported []error
	agent, err := NewAgent(Config{
		Region: "east", Shard: "s1", MemberID: "node-0", StateDir: filepath.Join(dir, "state"), Mode: ModeResolver,
		Render: renderConfig, Transaction: TransactionConfig{StageDir: filepath.Join(dir, "stage")},
		UnsafeAllowNoDNSVerify: true, ExternalWatchdog: true, WatchdogLeasePath: leasePath,
		WatchdogLease: 5 * time.Second, AckPublishTimeout: 30 * time.Millisecond,
	}, Dependencies{AckPublisher: stalledPublisher{}, Runner: &recordingRunner{}, OnError: func(err error) { reported = append(reported, err) }})
	if err != nil {
		t.Fatal(err)
	}
	agent.state.Snapshot = &snapshot
	agent.state.BindingFences, _ = advanceBindingFences(nil, snapshot)
	agent.state.ServingFence = fence{Epoch: 1, Revision: 1}
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
	if _, err := os.Stat(leasePath); err != nil {
		t.Fatalf("locally healthy Step did not renew watchdog lease: %v", err)
	}
}

// Wait for the ACK boundary rather than imposing a startup SLA on durable
// file staging. Production ACK and watchdog deadlines remain unchanged.
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

func TestResolverRenewsWatchdogBeforeBrokerAckCompletes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	renderConfig, snapshot := renderFixture(t)
	renderConfig.Role = RenderNode
	renderConfig.NodeDNSDist.Path = filepath.Join(dir, "runtime", "node-dnsdist.conf")
	renderConfig.NodeBIND.Path = filepath.Join(dir, "runtime", "node-bind.conf")
	renderConfig.ClusterDNSDist = DNSDistRenderConfig{}
	renderConfig.ClusterBIND = BINDRenderConfig{}
	snapshot.Bindings[0].Zones = nil
	leasePath := filepath.Join(dir, "lease.json")
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	agent, err := NewAgent(Config{
		Region: "east", Shard: "s1", MemberID: "node-0", StateDir: filepath.Join(dir, "state"), Mode: ModeResolver,
		Render: renderConfig, Transaction: TransactionConfig{StageDir: filepath.Join(dir, "stage")},
		UnsafeAllowNoDNSVerify: true, ExternalWatchdog: true, WatchdogLeasePath: leasePath,
		WatchdogLease: 5 * time.Second, AckPublishTimeout: time.Second,
	}, Dependencies{AckPublisher: blockingPublisher{started: started, release: release}, Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	agent.state.Snapshot = &snapshot
	agent.state.BindingFences, _ = advanceBindingFences(nil, snapshot)
	agent.state.ServingFence = fence{Epoch: 1, Revision: 1}
	finish := stepUntilBlockedAck(t, agent, started, release)
	if _, err := os.Stat(leasePath); err != nil {
		t.Fatalf("resolver did not renew watchdog before blocked broker ACK: %v", err)
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}
}

func TestResolverGatesFailedTenantButRenewsSharedLeaseForHealthyTenant(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	renderConfig, snapshot := renderFixture(t)
	renderConfig.Role = RenderNode
	renderConfig.NodeDNSDist.Path = filepath.Join(dir, "runtime", "node-dnsdist.conf")
	renderConfig.NodeBIND.Path = filepath.Join(dir, "runtime", "node-bind.conf")
	renderConfig.ClusterDNSDist = DNSDistRenderConfig{}
	renderConfig.ClusterBIND = BINDRenderConfig{}
	snapshot.Bindings[0].Zones = nil
	failedUID := snapshot.Bindings[0].BindingUID
	healthy := snapshot.Bindings[0]
	healthy.BindingUID = "binding-b"
	healthy.ProjectUID = "project-b"
	healthy.ContextUID = "vpc-b"
	healthy.ConsumerAddress = "10.253.0.52"
	healthy.ClusterAddress = "10.253.0.42"
	snapshot.Bindings = append(snapshot.Bindings, healthy)
	leasePath := filepath.Join(dir, "lease.json")
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	agent, err := NewAgent(Config{
		Region: "east", Shard: "s1", MemberID: "node-0", StateDir: filepath.Join(dir, "state"), Mode: ModeResolver,
		CacheFlush: []Command{{Path: "flush"}}, Render: renderConfig, Transaction: TransactionConfig{StageDir: filepath.Join(dir, "stage")},
		UnsafeAllowNoFailClosed: true, ExternalWatchdog: true, WatchdogLeasePath: leasePath,
		WatchdogLease: 5 * time.Second, AckPublishTimeout: time.Second,
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
	if _, err := os.Stat(leasePath); err != nil {
		t.Fatalf("healthy tenant did not renew shared watchdog after failed tenant was gated: %v", err)
	}
	if err := finish(); err != nil {
		t.Fatal(err)
	}
}

func TestStartContinuouslyRenewsResolverLeaseWhileBrokerDisconnected(t *testing.T) {
	dir := t.TempDir()
	renderConfig, snapshot := renderFixture(t)
	renderConfig.Role = RenderNode
	renderConfig.NodeDNSDist.Path = filepath.Join(dir, "runtime", "node-dnsdist.conf")
	renderConfig.NodeBIND.Path = filepath.Join(dir, "runtime", "node-bind.conf")
	renderConfig.ClusterDNSDist = DNSDistRenderConfig{}
	renderConfig.ClusterBIND = BINDRenderConfig{}
	snapshot.Bindings[0].Zones = nil
	snapshot.Bindings[0].Authorization.ValidUntil = time.Now().Add(10 * time.Second)
	leasePath := filepath.Join(dir, "lease.json")
	publishStarted := make(chan struct{}, 1)
	publishRelease := make(chan struct{})
	publisher := &contextIgnoringPublisher{started: publishStarted, release: publishRelease}
	agent, err := NewAgent(Config{
		Region: "east", Shard: "s1", MemberID: "node-0", StateDir: filepath.Join(dir, "state"), Mode: ModeResolver,
		Render: renderConfig, Transaction: TransactionConfig{StageDir: filepath.Join(dir, "stage")},
		UnsafeAllowNoFailClosed: true, UnsafeAllowNoDNSVerify: true, ExternalWatchdog: true,
		WatchdogLeasePath: leasePath, WatchdogLease: 500 * time.Millisecond,
		ExpiryInterval: 20 * time.Millisecond, AckInterval: 50 * time.Millisecond,
		AckPublishTimeout: 10 * time.Millisecond, LocalServingLease: time.Second,
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
				stopErr = errors.New("resolver did not stop after continuous renewal test")
			}
		})
	}
	defer stopAgent()
	var releaseOnce sync.Once
	releasePublisher := func() { releaseOnce.Do(func() { close(publishRelease) }) }
	defer releasePublisher()
	select {
	case <-publishStarted:
	case <-time.After(time.Second):
		t.Fatal("resolver did not enter context-ignoring broker publish")
	}

	initialDeadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(leasePath); err == nil {
			break
		}
		if time.Now().After(initialDeadline) {
			t.Fatal("resolver did not write its initial lease")
		}
		time.Sleep(5 * time.Millisecond)
	}
	started := time.Now()
	lastValidUntil := time.Time{}
	var continuityErr error
	for time.Since(started) < 1200*time.Millisecond {
		payload, err := os.ReadFile(leasePath)
		if err != nil {
			continuityErr = fmt.Errorf("read continuously renewed lease: %w", err)
			break
		}
		var lease watchdogLease
		if err := json.Unmarshal(payload, &lease); err != nil {
			continuityErr = fmt.Errorf("decode continuously renewed lease: %w", err)
			break
		}
		if !lease.ValidUntil.After(time.Now()) {
			continuityErr = fmt.Errorf("resolver lease expired during broker partition at %s", lease.ValidUntil)
			break
		}
		if lease.ValidUntil.After(lastValidUntil) {
			lastValidUntil = lease.ValidUntil
		}
		time.Sleep(20 * time.Millisecond)
	}
	if continuityErr == nil {
		if remaining := time.Until(lastValidUntil); remaining < 250*time.Millisecond {
			continuityErr = fmt.Errorf("resolver lease was not refreshed across multiple windows: remaining %s", remaining)
		}
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

func TestAckEventRenewsWatchdogAfterSuccessfulReconcile(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	dir := t.TempDir()
	renderConfig, snapshot := renderFixture(t)
	renderConfig.Role = RenderNode
	renderConfig.NodeDNSDist.Path = filepath.Join(dir, "runtime", "node-dnsdist.conf")
	renderConfig.NodeBIND.Path = filepath.Join(dir, "runtime", "node-bind.conf")
	renderConfig.ClusterDNSDist = DNSDistRenderConfig{}
	renderConfig.ClusterBIND = BINDRenderConfig{}
	snapshot.Members = []model.Member{{MemberID: "regional-0", ReplicaID: "regional-0", Role: "regional"}}
	leasePath := filepath.Join(dir, "lease.json")
	agent, err := NewAgent(Config{
		Region: "east", Shard: "s1", MemberID: "node-0", StateDir: filepath.Join(dir, "state"), Mode: ModeResolver,
		Render: renderConfig, Transaction: TransactionConfig{StageDir: filepath.Join(dir, "stage")},
		UnsafeAllowNoFailClosed: true, UnsafeAllowNoDNSVerify: true, ExternalWatchdog: true,
		WatchdogLeasePath: leasePath, WatchdogLease: 5 * time.Second,
	}, Dependencies{Clock: func() time.Time { return now }, Runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Handle(context.Background(), envelope(t, model.KindServingSnapshot, "shard", snapshot.ConfigurationEpoch, snapshot.ConfigurationRevision, snapshot)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(leasePath); err != nil {
		t.Fatal(err)
	}
	ack := model.MemberAck{MemberID: "regional-0", ReplicaID: "regional-0", ResourceUID: "zone-a", Kind: model.KindPublicationManifest, Epoch: 2, Revision: 4, Phase: model.AckVerified, ObservedAt: now, ValidUntil: now.Add(time.Minute)}
	if err := agent.Handle(context.Background(), envelope(t, model.KindMemberAck, "zone-a", 2, 4, ack)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leasePath); err != nil {
		t.Fatalf("successful ACK reconcile did not renew watchdog lease: %v", err)
	}
}

func TestExpiredKnownAckReplayDrainsWithoutStateOrLeaseChange(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	dir := t.TempDir()
	leasePath := filepath.Join(dir, "lease.json")
	agent := &Agent{
		cfg:  Config{Region: "east", Shard: "s1", Mode: ModeResolver, WatchdogLeasePath: leasePath, WatchdogLease: 5 * time.Second},
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
	if _, err := os.Stat(leasePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired ACK renewed watchdog lease: %v", err)
	}
	ack.MemberID = "unknown"
	if err := agent.Handle(context.Background(), envelope(t, model.KindMemberAck, "zone-a", 2, 4, ack)); err == nil {
		t.Fatal("expired ACK from an unknown member was accepted")
	}
	ack.MemberID = "regional-0"
	ack.ValidUntil = ack.ObservedAt.Add(2*time.Minute + time.Nanosecond)
	if err := agent.Handle(context.Background(), envelope(t, model.KindMemberAck, "zone-a", 2, 4, ack)); err == nil {
		t.Fatal("overlong ACK lease was accepted")
	}
}
