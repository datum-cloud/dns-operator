// SPDX-License-Identifier: AGPL-3.0-only

// Package runtime wires the internal DNS control plane and fleet processes.
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	dnsadmission "go.miloapis.com/dns-operator/internal/internaldns/admission"
	"go.miloapis.com/dns-operator/internal/internaldns/controlplane"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"go.miloapis.com/dns-operator/internal/internaldns/platform"
	"go.miloapis.com/dns-operator/internal/internaldns/serving"
	"go.miloapis.com/dns-operator/internal/internaldns/transport"
	resourcemanagerv1alpha1 "go.miloapis.com/milo/pkg/apis/resourcemanager/v1alpha1"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/flowcontrol"
	ctrl "sigs.k8s.io/controller-runtime"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	kubeadmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const parentNameExtra = "iam.miloapis.com/parent-name"

type Options struct {
	Kubeconfig             string
	MetricsBindAddress     string
	HealthProbeBindAddress string
}

// Run starts one of the deliberately separate production roles. Agent and
// readiness never load Kubernetes credentials.
func Run(ctx context.Context, role string, cfg Config, opts Options) error {
	if err := cfg.Validate(role); err != nil {
		return err
	}
	switch role {
	case roleControlPlane:
		return runControlPlane(ctx, cfg, opts)
	case roleAgent:
		startLocalProfiler()
		return runAgent(ctx, cfg)
	case roleReadiness:
		return serving.Readiness(ctx, cfg.Agent)
	case rolePrepare:
		return serving.PrepareDNSDist(cfg.Agent.Render)
	case roleServing:
		return serving.RunDaemons(ctx, cfg.Agent)
	default:
		return fmt.Errorf("unsupported role %q", role)
	}
}

type projectRuntime struct {
	config ProjectConfig
	direct client.Client
	start  func(context.Context) error
}

func runControlPlane(ctx context.Context, cfg Config, opts Options) error {
	scheme, err := newScheme()
	if err != nil {
		return err
	}
	platformREST, err := restConfig(opts.Kubeconfig)
	if err != nil {
		return fmt.Errorf("load platform kubeconfig: %w", err)
	}
	applyKubeAPILimit(platformREST, cfg.PlatformKubeAPI, "platform")
	platformClient, err := client.New(platformREST, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create uncached platform client: %w", err)
	}
	platformClient = &deadlineClient{Client: platformClient, Timeout: cfg.PlatformKubeAPI.requestTimeout()}
	allocator := &platform.Allocator{Client: platformClient, Namespace: cfg.PlatformNamespace, ClusterPrefix: cfg.ClusterPrefix}
	planners := cfg.plannerConfigs()
	regions := make([]controlplane.ServingRegion, 0, len(planners))
	for _, p := range planners {
		regions = append(regions, controlplane.ServingRegion{Region: p.Region, Shard: p.Shard})
	}

	identity := holderIdentity()
	var discoveryReader client.Reader
	passSlots := make(chan struct{}, maxConcurrentProjectPasses)
	makeProject := func(p ProjectConfig, sourceREST *rest.Config) (projectRuntime, error) {
		projectREST := rest.CopyConfig(sourceREST)
		if p.Kubeconfig != "" {
			loaded, loadErr := restConfig(p.Kubeconfig)
			if loadErr != nil {
				return projectRuntime{}, fmt.Errorf("load project %q kubeconfig: %w", p.Name, loadErr)
			}
			projectREST = loaded
		}
		applyKubeAPILimit(projectREST, p.KubeAPI, "project")
		direct, err := client.New(projectREST, client.Options{Scheme: scheme})
		if err != nil {
			return projectRuntime{}, fmt.Errorf("create project %q direct client: %w", p.Name, err)
		}
		direct = &deadlineClient{Client: direct, Timeout: p.KubeAPI.requestTimeout()}
		project := p
		start := func(startCtx context.Context) error {
			mgr, err := ctrl.NewManager(projectREST, ctrl.Options{
				Scheme:                 scheme,
				Cache:                  projectCache(project.Namespace),
				Metrics:                metricsserver.Options{BindAddress: "0"},
				HealthProbeBindAddress: "0",
				LeaderElection:         false,
			})
			if err != nil {
				return fmt.Errorf("create manager: %w", err)
			}
			// Manager caches deliver watch events only. Every reconciliation read goes
			// to the source API so a partition cannot refresh access mappings from stale
			// contexts, access bindings, associations, registrations, or producer observations.
			routed := &RoutingClient{Client: platformClient, Source: direct, PlatformNamespace: cfg.PlatformNamespace}
			reconciler := &controlplane.Reconciler{Client: routed, APIReader: direct, Scheme: scheme, Options: controlplane.ReconcilerOptions{
				ProjectUID: project.ProjectUID, PassSlots: passSlots,
				VerifyProjectIdentity: func(ctx context.Context) error {
					if discoveryReader == nil {
						return nil
					}
					return verifyProjectIdentity(ctx, discoveryReader, project.Name, project.ProjectUID)
				},
				ProjectNamespace: project.Namespace, PlatformNamespace: cfg.PlatformNamespace,
				Region: cfg.Region, Shard: cfg.Shard, Identity: identity,
				LeaseDuration:        cfg.ownershipLease(),
				AddressAllocator:     allocator,
				PrivateZoneClassName: cfg.PrivateZoneClassName, ServingRegions: regions,
				ManagedDomainSuffixes: cfg.ManagedDomainSuffixes,
			}}
			if err := reconciler.SetupWithManager(mgr); err != nil {
				return fmt.Errorf("setup reconciler: %w", err)
			}
			return mgr.Start(startCtx)
		}
		return projectRuntime{config: project, direct: direct, start: start}, nil
	}
	projects := make([]projectRuntime, 0, len(cfg.Projects))
	for _, p := range cfg.Projects {
		project, err := makeProject(p, platformREST)
		if err != nil {
			return err
		}
		projects = append(projects, project)
	}
	directory := newProjectDirectory(projects)
	var discovery *projectDiscovery
	if cfg.ProjectDiscovery != nil {
		discoveryREST, loadErr := restConfig(cfg.ProjectDiscovery.Kubeconfig)
		if loadErr != nil {
			return fmt.Errorf("load project discovery kubeconfig: %w", loadErr)
		}
		applyKubeAPILimit(discoveryREST, cfg.ProjectDiscovery.KubeAPI, "discovery")
		discoveryClient, clientErr := client.New(discoveryREST, client.Options{Scheme: scheme})
		if clientErr != nil {
			return fmt.Errorf("create project discovery client: %w", clientErr)
		}
		discoveryClient = &deadlineClient{Client: discoveryClient, Timeout: cfg.ProjectDiscovery.KubeAPI.requestTimeout()}
		discoveryReader = discoveryClient
		discovery = &projectDiscovery{client: discoveryClient, directory: directory, rest: discoveryREST, kubeAPI: cfg.ProjectDiscovery.KubeAPI, makeProject: makeProject}
	}

	var admissionManager ctrl.Manager
	if cfg.Admission.Enabled {
		admissionManager, err = ctrl.NewManager(platformREST, ctrl.Options{
			Scheme:                 scheme,
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			LeaderElection:         false,
			WebhookServer:          webhook.NewServer(webhook.Options{Port: cfg.Admission.Port, CertDir: cfg.Admission.CertDir}),
		})
		if err != nil {
			return fmt.Errorf("create admission manager: %w", err)
		}
		handler := &dnsadmission.Handler{
			ClientForRequest: directory.selectProject,
		}
		admissionManager.GetWebhookServer().Register(dnsadmission.Path, &kubeadmission.Webhook{Handler: handler})
		admissionManager.GetWebhookServer().Register("/readyz", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	}

	errCh := make(chan error, len(projects)+len(planners)*3+8)
	if address := metricsBindAddress(cfg, opts); address != "" && address != "0" {
		server, err := metricsserver.NewServer(metricsserver.Options{BindAddress: address}, platformREST, nil)
		if err != nil {
			return fmt.Errorf("create metrics server: %w", err)
		}
		go func() { errCh <- server.Start(ctx) }()
	}
	live := newLoopLiveness()
	discovered := &firstSuccess{}
	if opts.HealthProbeBindAddress != "" && opts.HealthProbeBindAddress != "0" {
		ready := map[string]healthz.Checker{"projects": discovered.check}
		if admissionManager != nil {
			ready["webhook"] = admissionManager.GetWebhookServer().StartedChecker()
		}
		server, err := newProbeServer(opts.HealthProbeBindAddress, map[string]healthz.Checker{"loops": live.check}, ready)
		if err != nil {
			return err
		}
		go func() { errCh <- server.Start(ctx) }()
	}
	startProjectSources(ctx, projects)
	if discovery == nil {
		discovered.done()
	} else {
		discovery.passed = discovered.done
		go retryLoop(ctx, time.Duration(cfg.ProjectDiscovery.IntervalSeconds)*time.Second, loopID{Loop: "project discovery"}, live, discovery.step, errCh)
	}
	if admissionManager != nil {
		go func() { errCh <- admissionManager.Start(ctx) }()
	}

	publicationMembers := regionalMembers(planners)
	publicationRegions := map[string][]model.Member{}
	for _, planner := range planners {
		publicationRegions[planner.Region+"/"+planner.Shard] = append([]model.Member(nil), planner.Members...)
	}

	sinks := map[string]*platform.AckSink{}
	for _, plannerCfg := range planners {
		pc := plannerCfg
		planner := &platform.Planner{Client: platformClient, Allocator: allocator, Config: pc}
		interval := min(pc.LeaseDuration/3, maxPlannerInterval)
		if interval < time.Second {
			interval = time.Second
		}
		go retryLoop(ctx, interval, loopID{Loop: "planner", Region: pc.Region, Shard: pc.Shard}, live, planner.Step, errCh)
		sink := &platform.AckSink{Client: platformClient, Namespace: cfg.PlatformNamespace, ProjectScopes: directory.scopes,
			PublicationMembers: publicationMembers, PublicationRegions: publicationRegions, Region: pc.Region, Shard: pc.Shard, Members: pc.Members}
		sinks[pc.Region+"/"+pc.Shard] = sink
		go retryLoop(ctx, 2*time.Second, loopID{Loop: "ack refresh", Region: pc.Region, Shard: pc.Shard}, live, sink.Refresh, errCh)
	}
	startRegionalWorkers(planners, func(region string, regionalPlanners []platform.PlannerConfig) error {
		return runControlPlaneRegion(ctx, cfg, region, regionalPlanners, platformClient, sinks, live)
	}, errCh)

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		if err == nil || errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
}

// projectStarts paces how fast project managers start, each listing and
// watching its project's DNS resources, so a restart with hundreds of projects
// does not open every watch against the API server in the same second.
var projectStarts = rate.NewLimiter(rate.Limit(10), 10)

// maxConcurrentProjectPasses bounds how many project passes one control-plane
// replica runs at once. Every project starts a full pass when the process
// starts; unbounded, those passes hit the API server together.
const maxConcurrentProjectPasses = 16

// maxPlannerInterval bounds how long a new or changed resolver binding waits
// for the shard planner, independent of the ownership lease that otherwise
// sets the planner's cadence.
const maxPlannerInterval = 10 * time.Second

func startProjectSources(ctx context.Context, projects []projectRuntime) {
	for _, project := range projects {
		go runProjectSource(ctx, project)
	}
}

func runProjectSource(ctx context.Context, project projectRuntime) {
	name := project.config.Name
	if name == "" {
		name = string(project.config.ProjectUID)
	}
	log := ctrl.LoggerFrom(ctx).WithName("internal-dns-project-source").WithValues("project", name)
	for ctx.Err() == nil {
		if projectStarts.Wait(ctx) != nil {
			return
		}
		err := project.start(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("source manager stopped unexpectedly")
		}
		log.Error(err, "source manager unavailable; rebuilding and retrying")
		if !waitForRetry(ctx) {
			return
		}
	}
}

// startRegionalWorkers starts every transport domain independently. In
// particular, it does not wait for one region's initial NATS connection before
// starting source controllers, planners, ACK expiry, or another region.
func startRegionalWorkers(planners []platform.PlannerConfig, run func(string, []platform.PlannerConfig) error, terminal chan<- error) {
	byRegion := map[string][]platform.PlannerConfig{}
	for _, planner := range planners {
		byRegion[planner.Region] = append(byRegion[planner.Region], planner)
	}
	for region, regionalPlanners := range byRegion {
		go func() { terminal <- run(region, regionalPlanners) }()
	}
}

func runControlPlaneRegion(ctx context.Context, cfg Config, region string, planners []platform.PlannerConfig, platformClient client.Client, sinks map[string]*platform.AckSink, live *loopLiveness) error {
	log := ctrl.LoggerFrom(ctx).WithName("internal-dns-region").WithValues("region", region)
	for ctx.Err() == nil {
		regional, err := connectRegional(ctx, cfg, []string{region})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Error(err, "regional transport setup failed; retrying")
			if !waitForRetry(ctx) {
				return nil
			}
			continue
		}
		route, err := regional.route(region)
		if err != nil {
			regional.Close()
			return err
		}
		consumers := make(map[string]*transport.Consumer, len(planners))
		setupFailed := false
		for _, planner := range planners {
			consumer, consumerErr := route.bus.Consumer(ctx, transport.ConsumerConfig{
				Stream:         route.stream.Name,
				Durable:        durable(roleControlPlane, planner.Region, planner.Shard, "acks"),
				FilterSubjects: []string{ackFilter(planner.Region, planner.Shard)},
				AckWait:        30 * time.Second, MaxAckPending: 128,
				NewDurableReplay: ackReplay, Prefetch: ackPrefetch, AsyncAck: true,
			})
			if consumerErr != nil {
				log.Error(consumerErr, "regional ACK consumer setup failed; retrying", "shard", planner.Shard)
				setupFailed = true
				break
			}
			consumers[planner.Region+"/"+planner.Shard] = consumer
		}
		if setupFailed {
			regional.Close()
			if !waitForRetry(ctx) {
				return nil
			}
			continue
		}
		workerCtx, cancel := context.WithCancel(ctx)
		const ackWorkers = 4
		localDone := make(chan error, len(consumers)*ackWorkers+1)
		outbox := &platform.Outbox{Client: platformClient, Namespace: cfg.PlatformNamespace, Region: region, Publisher: route.publisher, Bootstrap: route.bootstrap, PendingOnly: true}
		go retryLoop(workerCtx, 500*time.Millisecond, loopID{Loop: "outbox", Region: region}, live, outbox.Step, localDone)
		for key, consumer := range consumers {
			for range ackWorkers {
				go consumeAcks(workerCtx, consumer, sinks[key], localDone)
			}
		}
		<-ctx.Done()
		cancel()
		regional.Close()
		return nil
	}
	return nil
}

func waitForRetry(ctx context.Context) bool {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func runAgent(ctx context.Context, cfg Config) error {
	log := ctrl.LoggerFrom(ctx).WithName("internal-dns-agent")
	publisher := &agentPublisher{}
	agent, err := serving.NewAgent(cfg.Agent, serving.Dependencies{AckPublisher: publisher, OnError: func(err error) { log.Error(err, "serving agent operation failed; retrying") }})
	if err != nil {
		return err
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- agent.Start(workerCtx) }()
	go func() {
		for workerCtx.Err() == nil {
			err := runAgentTransport(workerCtx, cfg, agent, publisher)
			if workerCtx.Err() != nil {
				return
			}
			if err != nil {
				log.Error(err, "DNS state delivery unavailable; retrying")
			}
			if !waitForRetry(workerCtx) {
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
		return <-done
	case err := <-done:
		return err
	}
}

// agentPublisher does not block local activation while the transport connects.
type agentPublisher struct {
	mu       sync.RWMutex
	delegate transport.ExportPublisher
}

func (p *agentPublisher) set(delegate transport.ExportPublisher) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.delegate = delegate
}
func (p *agentPublisher) Publish(ctx context.Context, subject string, env model.Envelope) (transport.PublishAck, error) {
	p.mu.RLock()
	delegate := p.delegate
	p.mu.RUnlock()
	if delegate == nil {
		return transport.PublishAck{}, errors.New("DNS state transport unavailable")
	}
	return delegate.Publish(ctx, subject, env)
}
func runAgentTransport(ctx context.Context, cfg Config, agent *serving.Agent, publisher *agentPublisher) error {
	regional, err := connectRegional(ctx, cfg, []string{cfg.Agent.Region})
	if err != nil {
		return err
	}
	defer regional.Close()
	route, _ := regional.route(cfg.Agent.Region)
	var sources []serving.EventSource
	for _, feed := range []struct {
		suffix, filter string
		replay         time.Duration
		prefetch       int
		asyncAck       bool
	}{{"serving", model.ServingSubject(cfg.Agent.Region, cfg.Agent.Shard), 0, 0, false}, {"records", model.RecordConsumerFilter(cfg.Agent.Region, cfg.Agent.Shard), 0, 0, false}, {"acks", ackFilter(cfg.Agent.Region, cfg.Agent.Shard), ackReplay, ackPrefetch, true}} {
		source, err := route.bus.Consumer(ctx, transport.ConsumerConfig{Stream: route.stream.Name, Durable: durable(cfg.Agent.MemberID, feed.suffix), FilterSubjects: []string{feed.filter}, AckWait: 30 * time.Second, MaxAckPending: 64, NewDurableReplay: feed.replay, Prefetch: feed.prefetch, AsyncAck: feed.asyncAck})
		if err != nil {
			return err
		}
		sources = append(sources, source)
	}
	publisher.set(route.bus)
	defer publisher.set(nil)
	return agent.RunTransport(ctx, serving.SnapshotBootstrap{Store: route.store, Region: cfg.Agent.Region, Shard: cfg.Agent.Shard}, sources...)
}

func newScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := resourcemanagerv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return scheme, nil
}

func restConfig(path string) (*rest.Config, error) {
	if path == "" {
		return ctrl.GetConfig()
	}
	if !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("kubeconfig path must be absolute: %s", path)
	}
	return clientcmd.BuildConfigFromFlags("", path)
}

// applyKubeAPILimit installs one token bucket on the REST config before that
// config is shared by a manager and its uncached client. client-go otherwise
// creates a separate default 5 QPS limiter for every constructed client.
func applyKubeAPILimit(c *rest.Config, limits KubeAPIClientConfig, name string) {
	c.QPS = limits.QPS
	c.Burst = limits.Burst
	c.RateLimiter = &timedRateLimiter{RateLimiter: flowcontrol.NewTokenBucketRateLimiter(limits.QPS, limits.Burst), wait: clientThrottle.WithLabelValues(name)}
}

func connectWithRetry(ctx context.Context, cfg transport.Config) (*transport.JetStream, error) {
	log := ctrl.LoggerFrom(ctx).WithName("nats")
	for {
		js, err := transport.Connect(cfg)
		if err == nil {
			return js, nil
		}
		log.Error(err, "NATS unavailable; retrying")
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func retryLoop(ctx context.Context, interval time.Duration, id loopID, live *loopLiveness, step func(context.Context) error, terminal chan<- error) {
	log := ctrl.LoggerFrom(ctx).WithName("internal-dns").WithValues("loop", id.operation())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	live.start(id, interval)
	defer live.stop(id)
	for {
		started := time.Now()
		err := step(ctx)
		live.tick(id)
		if !errors.Is(err, context.Canceled) {
			observeLoop(id, started, err)
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Error(err, "operation failed; retrying")
		}
		select {
		case <-ctx.Done():
			terminal <- nil
			return
		case <-ticker.C:
		}
	}
}

// ackReplay covers the longest serving lease an acknowledgement may carry;
// anything older can no longer change readiness.
const ackReplay = 2 * time.Minute

const ackPrefetch = 32

func consumeAcks(ctx context.Context, source *transport.Consumer, sink *platform.AckSink, terminal chan<- error) {
	log := ctrl.LoggerFrom(ctx).WithName("internal-dns-ack-consumer").WithValues("region", sink.Region, "shard", sink.Shard)
	read := loopID{Loop: "ack read", Region: sink.Region, Shard: sink.Shard}
	apply := loopID{Loop: "ack apply", Region: sink.Region, Shard: sink.Shard}
	commit := loopID{Loop: "ack commit", Region: sink.Region, Shard: sink.Shard}
	for {
		message, err := source.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				terminal <- nil
				return
			}
			countLoop(read, err)
			log.Error(err, "read acknowledgement failed; retrying")
			time.Sleep(time.Second)
			continue
		}
		countLoop(read, nil)
		started := time.Now()
		err = sink.Apply(ctx, message.Subject(), message.Envelope())
		observeLoop(apply, started, err)
		if err != nil {
			log.Error(err, "apply acknowledgement failed; retrying")
			_ = message.Retry(time.Second)
			continue
		}
		started = time.Now()
		err = message.Ack(ctx)
		observeLoop(commit, started, err)
		if err != nil {
			log.Error(err, "acknowledgement commit was ambiguous; awaiting redelivery")
		}
	}
}

func projectSelector(projects []projectRuntime) func(context.Context, kubeadmission.Request) (client.Client, error) {
	return func(_ context.Context, req kubeadmission.Request) (client.Client, error) {
		parents := req.UserInfo.Extra[parentNameExtra]
		if len(parents) == 0 && len(projects) == 1 {
			p := projects[0]
			if req.Namespace != p.config.Namespace {
				return nil, fmt.Errorf("request namespace is outside the configured project")
			}
			return p.direct, nil
		}
		if len(parents) != 1 {
			return nil, fmt.Errorf("authenticated project parent is missing or ambiguous")
		}
		for _, p := range projects {
			if p.config.Name == parents[0] && req.Namespace == p.config.Namespace {
				return p.direct, nil
			}
		}
		return nil, fmt.Errorf("authenticated project parent is not configured for this namespace")
	}
}

func (c Config) plannerConfigs() []platform.PlannerConfig {
	configs := append([]platform.PlannerConfig(nil), c.Shards...)
	if len(configs) == 0 {
		configs = []platform.PlannerConfig{{Region: c.Region, Shard: c.Shard}}
	}
	for i := range configs {
		p := &configs[i]
		if p.Namespace == "" {
			p.Namespace = c.PlatformNamespace
		}
		if p.Identity == "" {
			p.Identity = holderIdentity() + "-" + model.SafeToken(p.Region) + "-" + model.SafeToken(p.Shard)
		}
		if p.LeaseDuration <= 0 {
			p.LeaseDuration = c.ownershipLease()
		}
		if len(p.NodeBackends) == 0 {
			p.NodeBackends = append([]model.Backend(nil), c.NodeBackends...)
		}
		if len(p.ClusterBackends) == 0 {
			p.ClusterBackends = append([]model.Backend(nil), c.ClusterBackends...)
		}

		if len(p.Members) == 0 {
			p.Members = append([]model.Member(nil), c.Members...)
		}
	}
	return configs
}

func regionalMembers(configs []platform.PlannerConfig) []model.Member {
	var result []model.Member
	for _, p := range configs {
		for _, m := range p.Members {
			if m.Role == memberRoleRegional || m.Role == memberRoleCluster {
				result = append(result, m)
			}
		}
	}
	return result
}

func ackFilter(region, shard string) string {
	return "dns.private.acks." + model.SafeToken(region) + "." + model.SafeToken(shard) + ".>"
}

func durable(parts ...string) string {
	for i := range parts {
		parts[i] = model.SafeToken(parts[i])
	}
	return strings.Join(parts, "-")
}

var (
	processIdentityOnce sync.Once
	processIdentity     string
)

func holderIdentity() string {
	processIdentityOnce.Do(func() {
		processIdentity = newProcessIdentity()
	})
	return processIdentity
}

// newProcessIdentity distinguishes concurrently running control-plane
// processes even when they share a host. holderIdentity caches one value for
// the process lifetime so every project reconciler and shard planner in this
// process uses the same stable lease identity.
func newProcessIdentity() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "internal-dns"
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err == nil {
		return host + "-" + hex.EncodeToString(random)
	}
	// crypto/rand failure is exceptional. PID and startup time still prevent
	// same-host processes from silently sharing a lease identity.
	return fmt.Sprintf("%s-%d-%d", host, os.Getpid(), time.Now().UTC().UnixNano())
}
