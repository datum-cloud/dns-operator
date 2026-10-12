// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
)

type Mode string

const (
	ModeResolver Mode = "resolver"
	ModeCombined Mode = "combined"
)

type Config struct {
	Region                    string                       `json:"region"`
	Shard                     string                       `json:"shard"`
	MemberID                  string                       `json:"memberID"`
	ReplicaID                 string                       `json:"replicaID,omitempty"`
	StateDir                  string                       `json:"stateDir"`
	Mode                      Mode                         `json:"mode"`
	Render                    RenderConfig                 `json:"render"`
	Transaction               TransactionConfig            `json:"transaction"`
	CacheFlush                []Command                    `json:"cacheFlush,omitempty"`
	ExpiryInterval            time.Duration                `json:"expiryInterval,omitempty"`
	RetryDelay                time.Duration                `json:"retryDelay,omitempty"`
	AckInterval               time.Duration                `json:"ackInterval,omitempty"`
	AckLease                  time.Duration                `json:"ackLease,omitempty"`
	AckPublishTimeout         time.Duration                `json:"ackPublishTimeout,omitempty"`
	LocalServingLease         time.Duration                `json:"localServingLease,omitempty"`
	MaxBindings               int                          `json:"maxBindings,omitempty"`
	WatchdogLeasePath         string                       `json:"watchdogLeasePath,omitempty"`
	WatchdogLease             time.Duration                `json:"watchdogLease,omitempty"`
	ExternalWatchdog          bool                         `json:"externalWatchdog,omitempty"`
	UnsafeAllowNoFailClosed   bool                         `json:"unsafeAllowNoFailClosed,omitempty"`
	PublicationDNSProbe       PublicationDNSVerifierConfig `json:"publicationDNSProbe,omitempty"`
	ResolverDNSProbe          DNSVerifierConfig            `json:"resolverDNSProbe,omitempty"`
	ResolverVerifyConcurrency int                          `json:"resolverVerifyConcurrency,omitempty"`
	UnsafeAllowNoDNSVerify    bool                         `json:"unsafeAllowNoDNSVerify,omitempty"`
}

func (c Config) Validate() error {
	if c.Region == "" || c.Shard == "" || c.MemberID == "" || !filepath.IsAbs(c.StateDir) {
		return errors.New("region, shard, member ID, and absolute state directory are required")
	}
	if c.Mode != ModeResolver && c.Mode != ModeCombined {
		return fmt.Errorf("unsupported serving mode %q", c.Mode)
	}
	if c.Mode == ModeResolver || c.Mode == ModeCombined {
		if err := validateRenderConfig(c.Render); err != nil {
			return err
		}
		if !filepath.IsAbs(c.Transaction.StageDir) {
			return errors.New("resolver mode requires an absolute transaction stage directory")
		}
	}

	if c.Mode == ModeResolver && (c.Render.Role == RenderNode || c.Render.Role == RenderBoth) && len(c.CacheFlush) == 0 && !c.UnsafeAllowNoDNSVerify {
		return errors.New("node BIND cache withdrawal hooks are required")
	}
	if c.AckLease > 2*time.Minute {
		return errors.New("ack lease may not exceed two minutes")
	}
	if c.AckPublishTimeout < 0 || c.AckPublishTimeout > 2*time.Second {
		return errors.New("ACK publish timeout must be between zero and two seconds")
	}

	if c.MaxBindings < 0 {
		return errors.New("maximum bindings may not be negative")
	}
	if c.ResolverVerifyConcurrency < 0 || c.ResolverVerifyConcurrency > 64 {
		return errors.New("resolver verification concurrency must be between zero and 64")
	}
	if !c.UnsafeAllowNoFailClosed && !c.ExternalWatchdog && len(c.Transaction.FailClosed) == 0 {
		return errors.New("failClosed command is required; use unsafeAllowNoFailClosed only in tests")
	}
	if c.WatchdogLeasePath != "" && !filepath.IsAbs(c.WatchdogLeasePath) {
		return errors.New("watchdog lease path must be absolute")
	}
	if c.ExternalWatchdog && c.WatchdogLeasePath == "" {
		return errors.New("external watchdog requires watchdogLeasePath")
	}
	if c.WatchdogLease > 5*time.Second {
		return errors.New("watchdog lease may not exceed five seconds")
	}
	if c.Mode == ModeCombined {
		if c.Render.Role != RenderCluster && c.Render.Role != RenderBoth && c.Render.Role != RenderRegionalBIND && c.Render.Role != "" {
			return errors.New("combined publication serving requires an owned regional BIND process")
		}
		if c.PublicationDNSProbe.Server != "" && c.PublicationDNSProbe.Server != net.JoinHostPort(c.Render.ClusterBIND.ListenAddress, strconv.Itoa(int(c.Render.ClusterBIND.Port))) {
			return errors.New("publication probe must target this member's protected BIND listener")
		}
	}
	return nil
}

type EventSource interface {
	Next(context.Context) (transport.Message, error)
}

type BootstrapSource interface {
	Load(context.Context) ([]model.Envelope, error)
}

type Verifier interface {
	Verify(context.Context, model.Binding) error
}

type Dependencies struct {
	ServingSource       EventSource
	RecordSource        EventSource
	AckSource           EventSource
	Bootstrap           BootstrapSource
	AckPublisher        transport.ExportPublisher
	Runner              Runner
	Verifier            Verifier
	Clock               func() time.Time
	PublicationVerifier PublicationVerifier
	OnError             func(error)
}

type ackPublishResult struct {
	ack transport.PublishAck
	err error
}

// boundedAckPublisher isolates local serving safety from a transport client
// blocked before it observes the publish context (for example, nats.Conn's
// connection-state mutex during a reconnect dial). There is no queue: at most
// one underlying call exists, and later state-derived ACKs expire at their
// caller deadline until that call returns. The next reconciliation publishes
// current state, naturally superseding intermediate heartbeats.
type boundedAckPublisher struct {
	delegate transport.ExportPublisher
	inflight chan struct{}
}

func newBoundedAckPublisher(delegate transport.ExportPublisher) transport.ExportPublisher {
	if delegate == nil {
		return nil
	}
	if _, ok := delegate.(*boundedAckPublisher); ok {
		return delegate
	}
	return &boundedAckPublisher{delegate: delegate, inflight: make(chan struct{}, 1)}
}

func (p *boundedAckPublisher) Publish(ctx context.Context, subject string, env model.Envelope) (transport.PublishAck, error) {
	if err := ctx.Err(); err != nil {
		return transport.PublishAck{}, err
	}
	select {
	case p.inflight <- struct{}{}:
	case <-ctx.Done():
		return transport.PublishAck{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		<-p.inflight
		return transport.PublishAck{}, err
	}
	result := make(chan ackPublishResult, 1)
	go func() {
		ack, err := p.delegate.Publish(ctx, subject, env)
		<-p.inflight
		result <- ackPublishResult{ack: ack, err: err}
	}()
	select {
	case result := <-result:
		return result.ack, result.err
	case <-ctx.Done():
		return transport.PublishAck{}, ctx.Err()
	}
}

type Agent struct {
	cfg                   Config
	deps                  Dependencies
	store                 stateStore
	tx                    FileTransaction
	publicationVerifier   PublicationVerifier
	mu                    sync.Mutex
	state                 checkpoint
	needsActivation       bool
	lastServingAck        map[string]servingAckState
	lastPublicationAck    map[string]time.Time
	installedPublications map[string]installedPublication
}

// This identity describes the exact files successfully activated, not a
// fingerprint recomputed later when an observation deadline may have passed.
type installedPublication struct {
	Identity      string
	EffectiveHash string
	Views         []PublicationView
}

func publicationIdentity(p publicationState) string {
	data, _ := json.Marshal(p.Plan)
	return fmt.Sprintf("%d/%d/%d/%s", p.Fence.Epoch, p.Fence.Revision, p.LocalRevision, model.Hash(data))
}

type servingAckState struct {
	phase model.AckPhase
	at    time.Time
}

func NewAgent(c Config, d Dependencies) (*Agent, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if d.Clock == nil {
		d.Clock = time.Now
	}
	if d.Runner == nil {
		d.Runner = ExecRunner{}
	}
	d.AckPublisher = newBoundedAckPublisher(d.AckPublisher)
	if (c.Mode == ModeResolver || c.Mode == ModeCombined) && c.Render.Role != RenderRegionalBIND && d.Verifier == nil && !c.UnsafeAllowNoDNSVerify {
		probeConfig := c.ResolverDNSProbe
		if c.Render.Role == RenderCluster || c.Render.Role == RenderRegionalDNSDist {
			probeConfig.UseClusterAddress = true
		}
		d.Verifier = DNSVerifier{Config: probeConfig}
	}
	publicationVerifier := d.PublicationVerifier
	if c.Mode == ModeCombined {
		if publicationVerifier == nil && c.PublicationDNSProbe.Server != "" {
			publicationVerifier = PublicationDNSVerifier{Config: c.PublicationDNSProbe}
		}
		if publicationVerifier == nil && !c.UnsafeAllowNoDNSVerify {
			return nil, errors.New("regional materialization DNS query verifier is required before readiness ACK")
		}
	}
	store := stateStore{path: filepath.Join(c.StateDir, "checkpoint.json")}
	state, err := store.Load()
	if err != nil {
		return nil, err
	}
	return &Agent{cfg: c, deps: d, store: store, tx: FileTransaction{Config: c.Transaction, Runner: d.Runner}, publicationVerifier: publicationVerifier, state: state, lastServingAck: map[string]servingAckState{}, lastPublicationAck: map[string]time.Time{}, installedPublications: map[string]installedPublication{}}, nil
}

func (a *Agent) Start(ctx context.Context) error {
	defer func() { _ = a.tx.FailClosed(context.Background()) }()
	// A restarted regional materialization must enforce deadlines persisted in its local
	// checkpoint before waiting on any remote service. The watchdog lease is not
	// renewed here, so an inconsistent or unavailable bootstrap stays gated.
	if err := a.expirePersisted(ctx); err != nil {
		a.report(fmt.Errorf("enforce persisted expiry at startup: %w", err))
	}
	if !a.gateUntilClosed(ctx) {
		return nil
	}
	// FailClosed may stop or firewall the DNS daemon. A persisted content hash
	// proves only what was installed before restart, not that the live process
	// still serves it, so force one validated apply before renewing the lease.
	a.mu.Lock()
	a.needsActivation = true
	a.mu.Unlock()
	for {
		err := a.bootstrapAndStep(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil
		}
		a.report(err)
		if !a.gateUntilClosed(ctx) {
			return nil
		}
		retry := a.cfg.RetryDelay
		if retry <= 0 {
			retry = time.Second
		}
		if !waitRetry(ctx, retry) {
			return nil
		}
	}
	errCh := make(chan error, 3)
	start := func(source EventSource) {
		if source == nil {
			return
		}
		go func() { errCh <- a.consume(ctx, source) }()
	}
	start(a.deps.ServingSource)
	start(a.deps.RecordSource)
	if a.cfg.Mode == ModeResolver || a.cfg.Mode == ModeCombined {
		start(a.deps.AckSource)
	}
	interval := a.cfg.ExpiryInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errCh:
			if err != nil && !errors.Is(err, context.Canceled) {
				a.report(err)
			}
		case <-ticker.C:
			if err := a.Step(ctx); err != nil {
				a.report(err)
			}
		}
	}
}

func (a *Agent) failClosed(ctx context.Context) error {
	a.needsActivation = true
	// Invalidate independent watchdog readiness immediately, even if the daemon
	// gate command itself fails. It must never inherit a prior healthy lease.
	if a.cfg.WatchdogLeasePath != "" {
		_ = os.Remove(a.cfg.WatchdogLeasePath)
	}
	return a.tx.FailClosed(ctx)
}

func (a *Agent) gateUntilClosed(ctx context.Context) bool {
	retry := a.cfg.RetryDelay
	if retry <= 0 {
		retry = time.Second
	}
	for {
		if err := a.failClosed(ctx); err == nil {
			return true
		} else {
			a.report(fmt.Errorf("gate serving during startup: %w", err))
		}
		if !waitRetry(ctx, retry) {
			return false
		}
	}
}

func (a *Agent) expirePersisted(ctx context.Context) error {
	if a.cfg.Mode != ModeCombined {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.expireRecords(ctx)
}

func (a *Agent) bootstrapAndStep(ctx context.Context) error {
	if a.deps.Bootstrap != nil {
		envs, err := a.deps.Bootstrap.Load(ctx)
		if err != nil {
			return fmt.Errorf("load bootstrap snapshot: %w", err)
		}
		sort.SliceStable(envs, func(i, j int) bool {
			order := func(k model.Kind) int {
				switch k {
				case model.KindPublicationChunk:
					return 0
				case model.KindServingSnapshot:
					// Context destination mappings are required to query-proof a
					// publication before its manifest can be acknowledged.
					return 1
				default:
					return 2
				}
			}
			return order(envs[i].Kind) < order(envs[j].Kind)
		})
		for _, env := range envs {
			if err := a.Handle(ctx, env); err != nil {
				return fmt.Errorf("apply bootstrap %s: %w", env.Kind, err)
			}
		}
	}
	if err := a.Step(ctx); err != nil {
		return fmt.Errorf("initial serving reconciliation: %w", err)
	}
	return nil
}

func (a *Agent) consume(ctx context.Context, source EventSource) error {
	retry := a.cfg.RetryDelay
	if retry <= 0 {
		retry = time.Second
	}
	for {
		msg, err := source.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			a.report(err)
			if !waitRetry(ctx, retry) {
				return ctx.Err()
			}
			continue
		}
		err = a.Handle(ctx, msg.Envelope())
		if err != nil {
			env := msg.Envelope()
			a.report(fmt.Errorf("apply %s %s at epoch %d revision %d: %w", env.Kind, env.ResourceUID, env.Epoch, env.Revision, err))
			_ = msg.Retry(retry)
			continue
		}
		if err := msg.Ack(ctx); err != nil {
			a.report(err)
			_ = msg.Retry(retry)
			if !waitRetry(ctx, retry) {
				return ctx.Err()
			}
		}
	}
}

func (a *Agent) report(err error) {
	if err != nil && a.deps.OnError != nil {
		a.deps.OnError(err)
	}
}

func waitRetry(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (a *Agent) Handle(ctx context.Context, env model.Envelope) error {
	if err := env.Validate(); err != nil {
		return err
	}
	if env.Region != a.cfg.Region || env.Shard != a.cfg.Shard {
		return fmt.Errorf("event targets %s/%s, agent serves %s/%s", env.Region, env.Shard, a.cfg.Region, a.cfg.Shard)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch env.Kind {
	case model.KindServingSnapshot:
		return a.handleServing(ctx, env)
	case model.KindPublicationChunk:

		return a.handleChunk(env)
	case model.KindPublicationManifest:

		if err := a.handleManifest(ctx, env); err != nil {
			_ = a.failClosed(ctx)
			return err
		}
		return a.finishEvent(ctx)
	case model.KindMemberAck:

		return a.handleAck(ctx, env)
	default:
		return fmt.Errorf("unsupported event kind %q", env.Kind)
	}
}

func (a *Agent) handleServing(ctx context.Context, env model.Envelope) error {
	var snapshot model.ServingSnapshot
	if err := json.Unmarshal(env.Payload, &snapshot); err != nil {
		return err
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	maximum := a.cfg.MaxBindings
	if maximum == 0 {
		maximum = 4096
	}
	if len(snapshot.Bindings) > maximum {
		return fmt.Errorf("serving snapshot has %d bindings, exceeding member limit %d", len(snapshot.Bindings), maximum)
	}
	if snapshot.Region != env.Region || snapshot.Shard != env.Shard || snapshot.ConfigurationEpoch != env.Epoch || snapshot.ConfigurationRevision != env.Revision {
		return errors.New("serving envelope identity does not match payload")
	}
	cmp := model.Compare(env.Epoch, env.Revision, a.state.ServingFence.Epoch, a.state.ServingFence.Revision)
	if cmp < 0 {
		return nil
	}
	if cmp == 0 && a.state.Snapshot != nil {
		if !equalJSON(snapshot, *a.state.Snapshot) {
			return errors.New("conflicting serving snapshot at the same epoch and revision")
		}
		return a.applyServingSnapshot(ctx, snapshot, false)
	}
	return a.applyServingSnapshot(ctx, snapshot, true)
}

func (a *Agent) applyServingSnapshot(ctx context.Context, snapshot model.ServingSnapshot, advance bool) error {
	if advance {
		bindingFences, err := advanceBindingFences(a.state.BindingFences, snapshot)
		if err != nil {
			return err
		}
		a.state.BindingFences = bindingFences
		a.state.ServingFence = fence{snapshot.ConfigurationEpoch, snapshot.ConfigurationRevision}
		a.state.Snapshot = &snapshot
		if err := a.store.Save(a.state); err != nil {
			return err
		}
	}
	if err := a.reconcile(ctx); err != nil {
		return err
	}
	return a.finishEvent(ctx)
}

func (a *Agent) handleChunk(env model.Envelope) error {
	var c model.PublicationChunk
	if err := json.Unmarshal(env.Payload, &c); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if c.ZoneUID != env.ResourceUID || c.WriterEpoch != env.Epoch || c.Revision != env.Revision {
		return errors.New("chunk envelope identity does not match payload")
	}
	current := a.state.Publications[c.ZoneUID].Fence
	if model.Compare(c.WriterEpoch, c.Revision, current.Epoch, current.Revision) < 0 {
		return nil
	}
	if a.state.Chunks[c.ManifestUID] == nil {
		a.state.Chunks[c.ManifestUID] = map[string]model.PublicationChunk{}
	}
	key := chunkKey(c.Index)
	if old, ok := a.state.Chunks[c.ManifestUID][key]; ok && old.SHA256 != c.SHA256 {
		return errors.New("conflicting chunk at the same manifest index")
	}
	a.state.Chunks[c.ManifestUID][key] = c
	return a.store.Save(a.state)
}

func (a *Agent) handleManifest(ctx context.Context, env model.Envelope) error {
	var m model.PublicationManifest
	if err := json.Unmarshal(env.Payload, &m); err != nil {
		return err
	}
	if err := m.Validate(); err != nil {
		return err
	}
	if m.ZoneUID != env.ResourceUID || m.WriterEpoch != env.Epoch || m.Revision != env.Revision {
		return errors.New("manifest envelope identity does not match payload")
	}
	old := a.state.Publications[m.ZoneUID]
	cmp := model.Compare(m.WriterEpoch, m.Revision, old.Fence.Epoch, old.Fence.Revision)
	if cmp < 0 {
		return nil
	}
	if cmp == 0 && old.Fence.Revision > 0 && !equalJSON(m, old.Manifest) {
		return errors.New("conflicting publication manifest at the same epoch and revision")
	}
	if cmp == 0 && old.Verified {
		if a.cfg.Mode == ModeResolver {
			return a.recordResolverPublication(ctx, m.ZoneUID, old, old)
		}
		if !equalJSON(m, old.Manifest) {
			return errors.New("conflicting publication manifest at the same epoch and revision")
		}
		if err := a.verifyPublication(ctx, old); err != nil {
			_ = a.emitAck(ctx, model.KindPublicationManifest, m.ZoneUID, m.WriterEpoch, m.Revision, model.AckRejected, time.Time{}, err.Error())
			return err
		}
		for _, name := range publicationNames(old.Plan) {
			if err := a.flushName(ctx, name); err != nil {
				return err
			}
		}
		return a.emitVerifiedPublicationAck(ctx, m.ZoneUID, m.WriterEpoch, m.Revision)
	}
	// Publications are complete snapshots. The control plane reserves a revision
	// before staging immutable artifacts, so a failed or superseded staging pass
	// can legitimately leave a gap. Accept a snapshot compiled from a base at or
	// ahead of the locally applied revision, while rejecting a newer revision
	// compiled from state older than the regional materialization has already committed.
	if cmp > 0 && old.Fence.Epoch == m.WriterEpoch && old.Fence.Revision > 0 && m.PreviousRevision < old.Fence.Revision {
		return fmt.Errorf("publication revision %d is based on revision %d older than applied revision %d", m.Revision, m.PreviousRevision, old.Fence.Revision)
	}
	if m.Tombstone {
		return a.applyTombstone(ctx, m, old)
	}
	chunkMap := a.state.Chunks[m.ManifestUID]
	chunks := make([]model.PublicationChunk, 0, len(chunkMap))
	for _, c := range chunkMap {
		chunks = append(chunks, c)
	}
	payload, err := model.VerifyManifest(m, chunks)
	if err != nil {
		return err
	}
	var plan model.PublicationPlan
	if err := json.Unmarshal(payload, &plan); err != nil {
		return err
	}
	if err := plan.Validate(); err != nil {
		return err
	}
	if plan.ZoneUID != m.ZoneUID || model.AbsoluteName(plan.Apex) != model.AbsoluteName(m.Apex) {
		return errors.New("publication plan identity does not match manifest")
	}
	if err := a.validateContributionFences(plan); err != nil {
		return err
	}
	// Persist every accepted observation high-water mark before mutating the
	// authoritative server. If the process dies during materialization, restart
	// must still reject an older healthy observation that could resurrect data
	// withdrawn by this plan. An identical retry remains idempotent.
	a.commitContributionFences(plan)
	if err := a.store.Save(a.state); err != nil {
		return err
	}
	next := publicationState{Fence: fence{m.WriterEpoch, m.Revision}, Manifest: m, Plan: plan, EffectiveHash: effectivePlanHash(plan, a.deps.Clock())}
	if a.cfg.Mode == ModeResolver {
		return a.recordResolverPublication(ctx, m.ZoneUID, next, old)
	}
	return a.activatePublication(ctx, m.ZoneUID, next, old)
}

func (a *Agent) applyTombstone(ctx context.Context, m model.PublicationManifest, old publicationState) error {
	next := publicationState{Fence: fence{m.WriterEpoch, m.Revision}, Manifest: m, Plan: old.Plan}
	if a.cfg.Mode == ModeResolver {
		return a.recordResolverPublication(ctx, m.ZoneUID, next, old)
	}
	return a.activatePublication(ctx, m.ZoneUID, next, old)
}

// Commit the immutable fence before installing files. Failed activation stays
// gated and an identical redelivery retries the accepted snapshot.
func (a *Agent) recordResolverPublication(ctx context.Context, uid string, next, old publicationState) error {
	next.Verified = false
	a.state.Publications[uid] = next
	if err := a.store.Save(a.state); err != nil {
		return err
	}
	for _, name := range append(publicationNames(old.Plan), publicationNames(next.Plan)...) {
		if err := a.flushName(ctx, name); err != nil {
			_ = a.failClosed(ctx)
			return err
		}
	}
	next.Verified = true
	a.state.Publications[uid] = next
	return a.store.Save(a.state)
}

func (a *Agent) activatePublication(ctx context.Context, uid string, next, old publicationState) error {
	a.state.Publications[uid] = next
	if err := a.store.Save(a.state); err != nil {
		return err
	}
	if err := a.reconcile(ctx); err != nil {
		return err
	}
	if !next.Manifest.Tombstone && len(a.publicationViews(next)) == 0 {
		return nil
	}
	if err := a.verifyPublication(ctx, next); err != nil {
		_ = a.failClosed(ctx)
		return err
	}
	for _, name := range append(publicationNames(old.Plan), publicationNames(next.Plan)...) {
		if err := a.flushName(ctx, name); err != nil {
			_ = a.failClosed(ctx)
			return err
		}
	}
	next.Verified = true
	a.state.Publications[uid] = next
	if err := a.store.Save(a.state); err != nil {
		return err
	}
	if err := a.reconcile(ctx); err != nil {
		return err
	}
	return a.emitVerifiedPublicationAck(ctx, uid, next.Fence.Epoch, next.Fence.Revision)
}

func (a *Agent) handleAck(ctx context.Context, env model.Envelope) error {
	var ack model.MemberAck
	if err := json.Unmarshal(env.Payload, &ack); err != nil {
		return err
	}
	if ack.ResourceUID != env.ResourceUID || ack.Epoch != env.Epoch || ack.Revision != env.Revision {
		return errors.New("ack envelope identity does not match payload")
	}
	if ack.ValidUntil.IsZero() || ack.ObservedAt.IsZero() || ack.ValidUntil.After(ack.ObservedAt.Add(2*time.Minute)) {
		return errors.New("ack has an invalid serving lease")
	}
	if a.state.Snapshot == nil {
		return nil
	}
	known := false
	for _, m := range a.state.Snapshot.Members {
		if m.MemberID == ack.MemberID && m.ReplicaID == ack.ReplicaID {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("ack from unknown member %q replica %q", ack.MemberID, ack.ReplicaID)
	}
	// A durable ACK consumer can legitimately replay a once-valid lease after a
	// backlog. Drain it without changing readiness or renewing the local lease;
	// retrying an already expired ACK can otherwise create a permanent NACK loop.
	if !a.deps.Clock().Before(ack.ValidUntil) {
		return nil
	}
	if ack.Kind != model.KindPublicationManifest {
		return nil
	}
	if ack.Phase == model.AckExpired {
		if err := a.flushPublicationZone(ctx, ack.ResourceUID); err != nil {
			return err
		}
		if err := a.reconcile(ctx); err != nil {
			return err
		}
		return a.finishEvent(ctx)
	}
	if ack.Phase != model.AckVerified {
		if ack.Phase != model.AckRejected && ack.Phase != model.AckApplied {
			return errors.New("unsupported publication ACK phase")
		}
		if a.state.Acks[ack.ResourceUID] == nil {
			a.state.Acks[ack.ResourceUID] = map[string]replicaAck{}
		}
		key := ack.MemberID + "/" + ack.ReplicaID
		old := a.state.Acks[ack.ResourceUID][key]
		cmp := model.Compare(ack.Epoch, ack.Revision, old.Fence.Epoch, old.Fence.Revision)
		if cmp < 0 || (cmp == 0 && !ack.ObservedAt.After(old.ObservedAt)) {
			return nil
		}
		a.state.Acks[ack.ResourceUID][key] = replicaAck{Fence: fence{ack.Epoch, ack.Revision}, ObservedAt: ack.ObservedAt}
		if err := a.store.Save(a.state); err != nil {
			return err
		}
		if err := a.flushPublicationZone(ctx, ack.ResourceUID); err != nil {
			_ = a.failClosed(ctx)
			return err
		}
		if err := a.reconcile(ctx); err != nil {
			return err
		}
		return a.finishEvent(ctx)
	}
	if a.state.Acks[ack.ResourceUID] == nil {
		a.state.Acks[ack.ResourceUID] = map[string]replicaAck{}
	}
	key := ack.MemberID + "/" + ack.ReplicaID
	old := a.state.Acks[ack.ResourceUID][key]
	comparison := model.Compare(ack.Epoch, ack.Revision, old.Fence.Epoch, old.Fence.Revision)
	if comparison < 0 || (comparison == 0 && !ack.ObservedAt.After(old.ObservedAt)) {
		return nil
	}
	highest := fence{}
	for _, existing := range a.state.Acks[ack.ResourceUID] {
		if model.Compare(existing.Fence.Epoch, existing.Fence.Revision, highest.Epoch, highest.Revision) > 0 {
			highest = existing.Fence
		}
	}
	if model.Compare(ack.Epoch, ack.Revision, highest.Epoch, highest.Revision) > 0 {
		if err := a.flushPublicationZone(ctx, ack.ResourceUID); err != nil {
			return err
		}
	}
	if comparison > 0 || (ack.Epoch == old.Fence.Epoch && ack.Revision == old.Fence.Revision && ack.ValidUntil.After(old.ValidUntil)) {
		a.state.Acks[ack.ResourceUID][key] = replicaAck{Fence: fence{ack.Epoch, ack.Revision}, ValidUntil: ack.ValidUntil, ObservedAt: ack.ObservedAt}
		if err := a.store.Save(a.state); err != nil {
			return err
		}
	}
	if err := a.reconcile(ctx); err != nil {
		return err
	}
	return a.finishEvent(ctx)
}

func (a *Agent) Step(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg.Mode == ModeResolver {
		if err := a.retryPendingPublications(ctx); err != nil {
			_ = a.failClosed(ctx)
			return err
		}
		if err := a.expireResolverCache(ctx); err != nil {
			_ = a.failClosed(ctx)
			return err
		}
	}
	if a.cfg.Mode == ModeCombined {
		if err := a.retryPendingPublications(ctx); err != nil {
			_ = a.failClosed(ctx)
			return err
		}
		if err := a.expireRecords(ctx); err != nil {
			_ = a.failClosed(ctx)
			return err
		}
		if a.needsActivation || len(a.installedPublications) == 0 {
			if err := a.reconcile(ctx); err != nil {
				return err
			}
		}
		if err := a.heartbeatPublications(ctx); err != nil {
			return err
		}
	}
	if err := a.reconcile(ctx); err != nil {
		return err
	}
	if a.cfg.Mode == ModeCombined {
		if err := a.expireRecords(ctx); err != nil {
			_ = a.failClosed(ctx)
			return err
		}
	}
	return a.renewSafeWatchdogLease(ctx)
}

// finishEvent gives event-driven reconciliation the same regional materialization safety
// gate as the periodic Step path before extending the independent watchdog
// lease. This matters when a sustained JetStream backlog repeatedly acquires
// the agent lock and delays the ticker.
func (a *Agent) finishEvent(ctx context.Context) error {
	if a.cfg.Mode == ModeResolver {
		if err := a.expireResolverCache(ctx); err != nil {
			_ = a.failClosed(ctx)
			return err
		}
	}
	if a.cfg.Mode == ModeCombined {
		if err := a.expireRecords(ctx); err != nil {
			_ = a.failClosed(ctx)
			return err
		}
		if err := a.heartbeatPublications(ctx); err != nil {
			return err
		}
	}
	return a.renewSafeWatchdogLease(ctx)
}

func (a *Agent) reconcile(ctx context.Context) error {
	if a.state.Snapshot == nil {
		return nil
	}

	now := a.deps.Clock()
	active := map[string]bool{}
	filtered := *a.state.Snapshot
	filtered.Bindings = nil
	for _, b := range a.state.Snapshot.Bindings {
		eligible := !b.Tombstone && now.Before(b.Authorization.ValidUntil) && a.publicationsReady(b)
		if eligible {
			active[b.BindingUID] = true
		}
		if !b.Tombstone && now.Before(b.Authorization.ValidUntil) {
			lease := a.cfg.LocalServingLease
			if lease <= 0 {
				lease = 90 * time.Second
			}
			horizon := now.Add(lease)
			quantum := a.cfg.AckInterval
			if quantum <= 0 {
				quantum = 30 * time.Second
			}
			horizon = horizon.Truncate(quantum).Add(quantum)
			if horizon.Before(b.Authorization.ValidUntil) {
				b.Authorization.ValidUntil = horizon
			}
			filtered.Bindings = append(filtered.Bindings, b)
		}
	}
	renderConfig := a.cfg.Render
	renderConfig.ReadyMembers = map[string]map[string]bool{}
	for _, binding := range filtered.Bindings {
		renderConfig.ReadyMembers[binding.BindingUID] = a.eligibleRegionalMembers(binding)
	}
	rendered, err := renderPublications(renderConfig, filtered, active, a.state.Publications, now)
	if err != nil {
		return err
	}
	hash := hashFiles(rendered.Files)
	if hash != a.state.RenderedHash || a.needsActivation {
		var err error
		if a.needsActivation {
			err = a.tx.ApplyForce(ctx, rendered.Files)
		} else {
			err = a.tx.Apply(ctx, rendered.Files)
		}
		if err != nil {
			_ = a.failClosed(ctx)
			return err
		}
		a.state.RenderedHash = hash
		if err := a.store.Save(a.state); err != nil {
			return err
		}
		a.needsActivation = false
	}
	if a.cfg.Mode == ModeCombined {
		installed := map[string]installedPublication{}
		for uid, p := range a.state.Publications {
			installed[uid] = installedPublication{Identity: publicationIdentity(p), EffectiveHash: effectivePlanHash(p.Plan, now), Views: a.publicationViewsAt(p, now)}
		}
		a.installedPublications = installed
		for _, p := range a.state.Publications {
			if p.Verified && !p.Manifest.Tombstone && len(a.publicationViews(p)) > 0 {
				if err := a.verifyPublication(ctx, p); err != nil {
					_ = a.failClosed(ctx)
					a.needsActivation = true
					return err
				}
			}
		}
	}
	// End-to-end verification must run after the candidate mapping is live.
	// Any binding that fails either UDP or TCP is immediately removed again.
	probeFailed := a.verifyResolverBindings(ctx, filtered.Bindings, active)
	if probeFailed {
		gated, err := renderPublications(renderConfig, filtered, active, a.state.Publications, now)
		if err != nil {
			return err
		}
		gatedHash := hashFiles(gated.Files)
		if gatedHash != a.state.RenderedHash {
			if err := a.tx.Apply(ctx, gated.Files); err != nil {
				_ = a.failClosed(ctx)
				return err
			}
			a.state.RenderedHash = gatedHash
			if err := a.store.Save(a.state); err != nil {
				return err
			}
		}
	}
	// Resolver health is a local safety decision. Once every eligible binding
	// has passed its end-to-end proof (and every failed binding has been
	// durably re-rendered gated), renew the independent watchdog before any
	// best-effort broker ACK. A NATS partition must expire remote readiness
	// leases, but it must not consume the local five-second watchdog window for
	// a dataplane that has just been proved healthy. Combined mode renews later,
	// after its regional materialization expiry and health checks also complete.
	if a.cfg.Mode == ModeResolver {
		if err := a.renewSafeWatchdogLease(ctx); err != nil {
			return err
		}
	}
	for _, b := range a.state.Snapshot.Bindings {
		if b.Tombstone {
			continue
		}
		p := model.AckVerified
		if !active[b.BindingUID] {
			p = model.AckApplied
			if !now.Before(b.Authorization.ValidUntil) {
				p = model.AckExpired
			}
		}
		if err := a.emitServingAck(ctx, b, p); err != nil {
			a.report(fmt.Errorf("publish serving ACK %s: %w", b.BindingUID, err))
			break
		}
	}
	return nil
}

type resolverProbeResult struct {
	bindingUID string
	duration   time.Duration
	err        error
}

// verifyResolverBindings probes a shard in a bounded worker pool. All probes
// share one deadline so adding VPCs cannot make reconciliation grow by one
// complete probe timeout per binding.
func (a *Agent) verifyResolverBindings(ctx context.Context, bindings []model.Binding, active map[string]bool) bool {
	if a.deps.Verifier == nil {
		return false
	}
	eligible := make([]model.Binding, 0, len(bindings))
	for _, binding := range bindings {
		if active[binding.BindingUID] {
			eligible = append(eligible, binding)
		}
	}
	if len(eligible) == 0 {
		return false
	}

	timeout := a.cfg.ResolverDNSProbe.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	concurrency := a.cfg.ResolverVerifyConcurrency
	if concurrency == 0 {
		concurrency = 16
	}
	concurrency = min(concurrency, len(eligible))
	jobs := make(chan model.Binding)
	results := make(chan resolverProbeResult, len(eligible))
	for range concurrency {
		go func() {
			for binding := range jobs {
				started := time.Now()
				err := a.deps.Verifier.Verify(probeCtx, binding)
				results <- resolverProbeResult{bindingUID: binding.BindingUID, duration: time.Since(started), err: err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, binding := range eligible {
			select {
			case jobs <- binding:
			case <-probeCtx.Done():
				return
			}
		}
	}()

	pending := make(map[string]bool, len(eligible))
	for _, binding := range eligible {
		pending[binding.BindingUID] = true
	}
	probeFailed := false
	for len(pending) > 0 {
		select {
		case result := <-results:
			if !pending[result.bindingUID] {
				continue
			}
			delete(pending, result.bindingUID)
			if result.err != nil {
				a.report(fmt.Errorf("verify resolver binding %s after %s: %w", result.bindingUID, result.duration.Round(time.Millisecond), result.err))
				delete(active, result.bindingUID)
				probeFailed = true
			}
		case <-probeCtx.Done():
			for bindingUID := range pending {
				a.report(fmt.Errorf("verify resolver binding %s within shared %s batch deadline: %w", bindingUID, timeout, probeCtx.Err()))
				delete(active, bindingUID)
			}
			return true
		}
	}
	return probeFailed
}

func (a *Agent) publicationsReady(b model.Binding) bool {
	if len(b.Zones) == 0 {
		return true
	}
	if a.cfg.Mode == ModeCombined {
		for _, z := range b.Zones {
			p, ok := a.state.Publications[z.ZoneUID]
			if !ok || !p.Verified || p.Manifest.Tombstone || model.Compare(p.Fence.Epoch, p.Fence.Revision, z.RequiredPublicationEpoch, z.RequiredPublicationRevision) < 0 {
				return false
			}
		}
		return true
	}
	return len(a.eligibleRegionalMembers(b)) > 0
}

// Every selected member must prove every attached zone at the exact publication
// plan version this resolver holds. One zone per different member is unsafe.
func (a *Agent) eligibleRegionalMembers(b model.Binding) map[string]bool {
	out := map[string]bool{}
	if a.state.Snapshot == nil {
		return out
	}
	if a.cfg.Mode == ModeCombined {
		if a.publicationsReady(b) {
			out[a.cfg.MemberID] = true
		}
		return out
	}
	for _, member := range a.state.Snapshot.Members {
		if member.Role != "regional" && member.Role != "cluster" {
			continue
		}
		ready := true
		for _, z := range b.Zones {
			p, ok := a.state.Publications[z.ZoneUID]
			if !ok || !p.Verified || p.Manifest.Tombstone || model.Compare(p.Fence.Epoch, p.Fence.Revision, z.RequiredPublicationEpoch, z.RequiredPublicationRevision) < 0 {
				ready = false
				break
			}
			for _, known := range a.state.Acks[z.ZoneUID] {
				if model.Compare(known.Fence.Epoch, known.Fence.Revision, p.Fence.Epoch, p.Fence.Revision) > 0 {
					ready = false
				}
			}
			proof := a.state.Acks[z.ZoneUID][member.MemberID+"/"+member.ReplicaID]
			if !a.deps.Clock().Before(proof.ValidUntil) || proof.Fence != p.Fence {
				ready = false
				break
			}
		}
		if ready {
			out[member.MemberID] = true
		}
	}
	return out
}

func (a *Agent) verifyPublication(ctx context.Context, p publicationState) error {
	if p.Manifest.Tombstone {
		return nil
	}
	if a.publicationVerifier != nil {
		if len(a.publicationViews(p)) == 0 {
			return nil
		}
		installed, ok := a.installedPublications[p.Plan.ZoneUID]
		if !ok || installed.Identity != publicationIdentity(p) {
			return errors.New("publication has no matching activated file identity")
		}
		return a.publicationVerifier.VerifyPublication(ctx, p.Plan, installed.Views)
	}
	return nil
}

func (a *Agent) publicationViews(p publicationState) []PublicationView {
	return a.publicationViewsAt(p, a.deps.Clock())
}

func (a *Agent) publicationViewsAt(p publicationState, now time.Time) []PublicationView {
	if a.state.Snapshot == nil {
		return nil
	}
	var out []PublicationView
	for _, b := range a.state.Snapshot.Bindings {
		if b.Tombstone || !now.Before(b.Authorization.ValidUntil) {
			continue
		}
		for _, z := range b.Zones {
			if z.ZoneUID == p.Plan.ZoneUID && model.Compare(p.Fence.Epoch, p.Fence.Revision, z.RequiredPublicationEpoch, z.RequiredPublicationRevision) >= 0 {
				out = append(out, PublicationView{View: b.ViewName(), DestinationAddress: b.ClusterAddress, DestinationPort: b.Port, Serial: publicationSerial(p, now), FingerprintMailbox: publicationMailbox(p, now)})
			}
		}
	}
	return out
}

func (a *Agent) retryPendingPublications(ctx context.Context) error {
	for uid, p := range a.state.Publications {
		if p.Verified {
			continue
		}
		if a.cfg.Mode == ModeResolver {
			if err := a.recordResolverPublication(ctx, uid, p, p); err != nil {
				return err
			}
			continue
		}
		if !p.Manifest.Tombstone && len(a.publicationViews(p)) == 0 {
			continue
		}
		if err := a.reconcile(ctx); err != nil {
			return err
		}
		if err := a.verifyPublication(ctx, p); err != nil {
			return err
		}
		for _, name := range publicationNames(p.Plan) {
			if err := a.flushName(ctx, name); err != nil {
				return err
			}
		}
		p.Verified = true
		p.EffectiveHash = effectivePlanHash(p.Plan, a.deps.Clock())
		a.state.Publications[uid] = p
		if err := a.store.Save(a.state); err != nil {
			return err
		}
	}
	return nil
}

// A node retains the publication deadlines so a broker outage cannot leave
// healthy records in its positive cache after the regional member withdraws.
func (a *Agent) expireResolverCache(ctx context.Context) error {
	now := a.deps.Clock()
	for uid, p := range a.state.Publications {
		if p.Manifest.Tombstone {
			continue
		}
		hash := effectivePlanHash(p.Plan, now)
		if hash == p.EffectiveHash {
			continue
		}
		for _, name := range publicationNames(p.Plan) {
			if err := a.flushName(ctx, name); err != nil {
				return err
			}
		}
		p.EffectiveHash = hash
		p.LocalRevision++
		a.state.Publications[uid] = p
		if err := a.store.Save(a.state); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) expireRecords(ctx context.Context) error {
	now := a.deps.Clock()
	for uid, p := range a.state.Publications {
		if !p.Verified || p.Manifest.Tombstone {
			continue
		}
		hash := effectivePlanHash(p.Plan, now)
		if hash == p.EffectiveHash {
			continue
		}
		p.LocalRevision++
		p.EffectiveHash = hash
		p.Verified = false
		a.state.Publications[uid] = p
		if err := a.store.Save(a.state); err != nil {
			return err
		}
		if err := a.reconcile(ctx); err != nil {
			return err
		}
		if len(a.publicationViews(p)) == 0 {
			continue
		}
		if err := a.verifyPublication(ctx, p); err != nil {
			return err
		}
		for _, name := range publicationNames(p.Plan) {
			if err := a.flushName(ctx, name); err != nil {
				return err
			}
		}
		p.Verified = true
		a.state.Publications[uid] = p
		if err := a.store.Save(a.state); err != nil {
			return err
		}
		if err := a.reconcile(ctx); err != nil {
			return err
		}
		if err := a.emitAck(ctx, model.KindPublicationManifest, uid, p.Fence.Epoch, p.Fence.Revision, model.AckExpired, time.Time{}, ""); err != nil {
			a.report(err)
		}
	}
	return nil
}

func (a *Agent) flushName(ctx context.Context, name string) error {

	for _, command := range a.cfg.CacheFlush {
		command.Args = append([]string(nil), command.Args...)
		for i := range command.Args {
			command.Args[i] = strings.ReplaceAll(command.Args[i], "{name}", model.AbsoluteName(name))
		}
		if _, err := a.deps.Runner.Run(ctx, command); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) flushPublicationZone(ctx context.Context, zoneUID string) error {
	if a.state.Snapshot == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, binding := range a.state.Snapshot.Bindings {
		for _, zone := range binding.Zones {
			if zone.ZoneUID == zoneUID && !seen[zone.Apex] {
				seen[zone.Apex] = true
				if err := a.flushName(ctx, zone.Apex); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (a *Agent) emitAck(ctx context.Context, kind model.Kind, uid string, epoch, revision uint64, phase model.AckPhase, validUntil time.Time, message string) error {
	if a.deps.AckPublisher == nil {
		return nil
	}
	now := a.deps.Clock().UTC()
	lease := a.cfg.AckLease
	if lease <= 0 {
		lease = 90 * time.Second
	}
	leaseUntil := now.Add(lease)
	if !validUntil.IsZero() && validUntil.Before(leaseUntil) {
		leaseUntil = validUntil
	}
	ack := model.MemberAck{MemberID: a.cfg.MemberID, ReplicaID: a.cfg.ReplicaID, ResourceUID: uid, Kind: kind, Epoch: epoch, Revision: revision, Phase: phase, ObservedAt: now, ValidUntil: leaseUntil, Error: message}
	eventID := fmt.Sprintf("ack-%s-%s-%d-%d-%s-%d", model.OpaqueToken(a.cfg.MemberID+"/"+a.cfg.ReplicaID), model.OpaqueToken(uid), epoch, revision, phase, now.UnixNano())
	env, err := model.NewEnvelope(model.KindMemberAck, eventID, a.cfg.Region, a.cfg.Shard, uid, epoch, revision, a.deps.Clock(), ack)
	if err != nil {
		return err
	}
	publishTimeout := a.cfg.AckPublishTimeout
	if publishTimeout <= 0 {
		publishTimeout = 500 * time.Millisecond
	}
	publishCtx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	_, err = a.deps.AckPublisher.Publish(publishCtx, model.AckSubject(a.cfg.Region, a.cfg.Shard, a.cfg.MemberID), env)
	return err
}

func (a *Agent) emitVerifiedPublicationAck(ctx context.Context, uid string, epoch, revision uint64) error {
	// The file transaction, proof and original-deadline enforcement are local
	// safety gates. Broker delivery is independent: renew only a safe installed
	// state first, then let remote readiness expire if ACK transport is down.
	if err := a.renewSafeWatchdogLease(ctx); err != nil {
		return err
	}
	if err := a.emitAck(ctx, model.KindPublicationManifest, uid, epoch, revision, model.AckVerified, time.Time{}, ""); err != nil {
		a.report(fmt.Errorf("publish verified publication ACK %s: %w", uid, err))
		return nil
	}
	// The materialization path has already completed the same staged-file and DNS
	// proof required by a periodic heartbeat. Remember it so finishEvent checks
	// other due publications without immediately repeating this proof.
	a.lastPublicationAck[uid] = a.deps.Clock()
	return nil
}

func (a *Agent) emitServingAck(ctx context.Context, b model.Binding, phase model.AckPhase) error {
	now := a.deps.Clock()
	interval := a.cfg.AckInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	last := a.lastServingAck[b.BindingUID]
	if last.phase == phase && now.Sub(last.at) < interval {
		return nil
	}
	if err := a.emitAck(ctx, model.KindServingSnapshot, b.BindingUID, a.state.ServingFence.Epoch, a.state.ServingFence.Revision, phase, b.Authorization.ValidUntil, ""); err != nil {
		return err
	}
	a.lastServingAck[b.BindingUID] = servingAckState{phase: phase, at: now}
	return nil
}

func (a *Agent) heartbeatPublications(ctx context.Context) error {
	now := a.deps.Clock()
	interval := a.cfg.AckInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	type duePublication struct {
		uid string
		p   publicationState
	}
	var due []duePublication
	for uid, p := range a.state.Publications {
		if !p.Verified || (!p.Manifest.Tombstone && len(a.publicationViews(p)) == 0) {
			continue
		}
		if last := a.lastPublicationAck[uid]; !last.IsZero() && now.Sub(last) < interval {
			continue
		}
		if err := a.verifyPublication(ctx, p); err != nil {
			_ = a.emitAck(ctx, model.KindPublicationManifest, uid, p.Fence.Epoch, p.Fence.Revision, model.AckRejected, time.Time{}, err.Error())
			return fmt.Errorf("reverify publication %s before ACK renewal: %w", uid, err)
		}
		due = append(due, duePublication{uid: uid, p: p})
	}
	// Local watchdog eligibility depends on the complete local verification
	// above, not broker availability. Stop after the first stalled publish so a
	// partition consumes at most one bounded publish timeout per Step. Missing
	// remote ACK leases still expire independently and gate resolver eligibility.
	for _, publication := range due {
		p := publication.p
		if err := a.emitAck(ctx, model.KindPublicationManifest, publication.uid, p.Fence.Epoch, p.Fence.Revision, model.AckVerified, time.Time{}, ""); err != nil {
			a.report(fmt.Errorf("publish verified publication ACK %s: %w", publication.uid, err))
			break
		}
		a.lastPublicationAck[publication.uid] = now
	}
	return nil
}

func hashFiles(files map[string][]byte) string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	size := 0
	for _, key := range keys {
		size += len(key) + 1 + len(files[key])
	}
	all := make([]byte, 0, size)
	for _, k := range keys {
		all = append(all, []byte(k)...)
		all = append(all, 0)
		all = append(all, files[k]...)
	}
	return model.Hash(all)
}

func equalJSON(left, right any) bool {
	a, errA := json.Marshal(left)
	b, errB := json.Marshal(right)
	return errA == nil && errB == nil && string(a) == string(b)
}

func publicationNames(plan model.PublicationPlan) []string {
	seen := map[string]bool{}
	var out []string
	if plan.Apex != "" {
		apex := model.AbsoluteName(plan.Apex)
		seen[apex] = true
		out = append(out, apex)
	}
	for _, owner := range plan.Owners {
		owner = model.AbsoluteName(owner)
		if !seen[owner] {
			seen[owner] = true
			out = append(out, owner)
		}
	}
	for _, rrset := range plan.RRSets {
		owner := model.AbsoluteName(rrset.Name)
		if !seen[owner] {
			seen[owner] = true
			out = append(out, owner)
		}
	}
	sort.Strings(out)
	return out
}

func (a *Agent) validateContributionFences(plan model.PublicationPlan) error {
	for _, observation := range plan.ObservationFences {
		current, ok := a.state.ContributionFences[observation.ContributionUID]
		if !ok {
			continue
		}
		comparison := model.Compare(observation.WriterEpoch, observation.Sequence, current.Epoch, current.Sequence)
		if comparison < 0 {
			return fmt.Errorf("contribution %q regressed from epoch %d sequence %d", observation.ContributionUID, current.Epoch, current.Sequence)
		}
		if comparison == 0 {
			if current.ValidUntil.IsZero() || !current.ValidUntil.Equal(observation.ValidUntil) {
				return fmt.Errorf("contribution %q changed the deadline of an existing fence", observation.ContributionUID)
			}
			if current.GrantUID != "" && current.GrantUID != observation.GrantUID {
				return fmt.Errorf("contribution %q changed the grant of an existing fence", observation.ContributionUID)
			}
		}
	}
	return nil
}

func (a *Agent) commitContributionFences(plan model.PublicationPlan) {
	for _, observation := range plan.ObservationFences {
		next := contributionFence{GrantUID: observation.GrantUID, Epoch: observation.WriterEpoch, Sequence: observation.Sequence, ValidUntil: observation.ValidUntil}
		current := a.state.ContributionFences[observation.ContributionUID]
		comparison := model.Compare(next.Epoch, next.Sequence, current.Epoch, current.Sequence)
		if comparison > 0 || (comparison == 0 && current.GrantUID == "" && !current.ValidUntil.IsZero() && current.ValidUntil.Equal(next.ValidUntil)) {
			a.state.ContributionFences[observation.ContributionUID] = next
		}
	}
}

type watchdogLease struct {
	MemberID   string    `json:"memberID"`
	ReplicaID  string    `json:"replicaID,omitempty"`
	ValidUntil time.Time `json:"validUntil"`
}

func (a *Agent) watchdogPublicationError() error {
	now := a.deps.Clock()
	for uid, p := range a.state.Publications {
		if p.Manifest.Tombstone {
			continue
		}
		current := effectivePlanHash(p.Plan, now)
		if a.cfg.Mode == ModeCombined {
			if len(a.publicationViewsAt(p, now)) == 0 {
				continue
			}
			installed, ok := a.installedPublications[uid]
			if !ok || installed.Identity != publicationIdentity(p) || installed.EffectiveHash != current {
				return fmt.Errorf("publication %s deadline changed after activation; refusing watchdog renewal", uid)
			}
		} else if p.EffectiveHash != current {
			return fmt.Errorf("publication %s deadline changed after cache withdrawal; refusing watchdog renewal", uid)
		}
	}
	return nil
}

// A deadline can cross during reload or DNS proof. Settle that transition
// locally before renewing readiness, under one budget across every retry.
func (a *Agent) renewSafeWatchdogLease(ctx context.Context) error {
	timeout := a.cfg.PublicationDNSProbe.Timeout
	if a.cfg.Mode == ModeResolver {
		timeout = a.cfg.ResolverDNSProbe.Timeout
	}
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	settleCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		if err := settleCtx.Err(); err != nil {
			return errors.Join(fmt.Errorf("settle local publication deadlines: %w", err), a.failClosed(ctx))
		}
		var err error
		if a.cfg.Mode == ModeCombined {
			err = a.expireRecords(settleCtx)
		} else {
			err = a.expireResolverCache(settleCtx)
		}
		if err != nil {
			return errors.Join(err, a.failClosed(ctx))
		}
		if a.watchdogPublicationError() == nil {
			return a.renewWatchdogLease(settleCtx)
		}
		if err := a.reconcile(settleCtx); err != nil {
			return errors.Join(err, a.failClosed(ctx))
		}
	}
}

func (a *Agent) renewWatchdogLease(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(err, a.failClosed(ctx))
	}
	if err := a.watchdogPublicationError(); err != nil {
		return errors.Join(err, a.failClosed(ctx))
	}
	if a.cfg.WatchdogLeasePath == "" {
		return nil
	}
	lease := a.cfg.WatchdogLease
	if lease <= 0 {
		lease = 5 * time.Second
	}
	b, err := json.Marshal(watchdogLease{MemberID: a.cfg.MemberID, ReplicaID: a.cfg.ReplicaID, ValidUntil: a.deps.Clock().UTC().Add(lease)})
	if err != nil {
		return err
	}
	return writeAtomicLease(a.cfg.WatchdogLeasePath, b, 0o600)
}
