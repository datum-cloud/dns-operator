// SPDX-License-Identifier: AGPL-3.0-only

// Package runtime wires the internal DNS control plane and fleet processes.
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
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
	"go.miloapis.com/dns-operator/internal/internaldns/watchdog"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/flowcontrol"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	kubeadmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const parentNameExtra = "iam.miloapis.com/parent-name"

// Run starts one of the deliberately separate production roles. Agent and
// watchdog never load Kubernetes credentials.
func Run(ctx context.Context, role string, cfg Config, kubeconfig string) error {
	if err := cfg.Validate(role); err != nil {
		return err
	}
	switch role {
	case roleControlPlane:
		return runControlPlane(ctx, cfg, kubeconfig)
	case roleAgent:
		return runAgent(ctx, cfg)
	case roleWatchdog:
		return (&watchdog.Watchdog{Config: cfg.Watchdog}).Start(ctx)
	default:
		return fmt.Errorf("unsupported role %q", role)
	}
}

type projectRuntime struct {
	config ProjectConfig
	direct client.Client
	start  func(context.Context) error
}

func runControlPlane(ctx context.Context, cfg Config, kubeconfig string) error {
	scheme, err := newScheme()
	if err != nil {
		return err
	}
	platformREST, err := restConfig(kubeconfig)
	if err != nil {
		return fmt.Errorf("load platform kubeconfig: %w", err)
	}
	applyKubeAPILimit(platformREST, cfg.PlatformKubeAPI)
	platformClient, err := client.New(platformREST, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create uncached platform client: %w", err)
	}
	platformClient = &deadlineClient{Client: platformClient, Timeout: cfg.PlatformKubeAPI.requestTimeout()}
	allocator := &platform.Allocator{Client: platformClient, Namespace: cfg.PlatformNamespace, ConsumerPrefix: cfg.ConsumerPrefix, ClusterPrefix: cfg.ClusterPrefix}
	planners := cfg.plannerConfigs()
	regions := make([]controlplane.ServingRegion, 0, len(planners))
	for _, p := range planners {
		regions = append(regions, controlplane.ServingRegion{Region: p.Region, Shard: p.Shard})
	}

	identity := holderIdentity()
	projects := make([]projectRuntime, 0, len(cfg.Projects))
	for _, p := range cfg.Projects {
		projectREST := rest.CopyConfig(platformREST)
		if p.Kubeconfig != "" {
			projectREST, err = restConfig(p.Kubeconfig)
			if err != nil {
				return fmt.Errorf("load project %q kubeconfig: %w", p.Name, err)
			}
		}
		applyKubeAPILimit(projectREST, p.KubeAPI)
		direct, err := client.New(projectREST, client.Options{Scheme: scheme})
		if err != nil {
			return fmt.Errorf("create project %q direct client: %w", p.Name, err)
		}
		direct = &deadlineClient{Client: direct, Timeout: p.KubeAPI.requestTimeout()}
		project := p
		start := func(startCtx context.Context) error {
			mgr, err := ctrl.NewManager(projectREST, ctrl.Options{
				Scheme:                 scheme,
				Cache:                  cache.Options{DefaultNamespaces: map[string]cache.Config{project.Namespace: {}}},
				Metrics:                metricsserver.Options{BindAddress: "0"},
				HealthProbeBindAddress: "0",
				LeaderElection:         false,
			})
			if err != nil {
				return fmt.Errorf("create manager: %w", err)
			}
			// Manager caches deliver watch events only. Every reconciliation read goes
			// to the source API so a partition cannot renew authorization from stale
			// contexts, access bindings, associations, registrations, or producer observations.
			routed := &RoutingClient{Client: platformClient, Source: direct, PlatformNamespace: cfg.PlatformNamespace}
			reconciler := &controlplane.Reconciler{Client: routed, Scheme: scheme, Options: controlplane.ReconcilerOptions{
				ProjectUID: project.ProjectUID, SourceClusterUID: project.SourceClusterUID,
				ProjectNamespace: project.Namespace, PlatformNamespace: cfg.PlatformNamespace,
				Region: cfg.Region, Shard: cfg.Shard, Identity: identity,
				LeaseDuration:    cfg.ownershipLease(),
				AddressAllocator: allocator, ManagedDomainSuffix: cfg.ManagedDomainSuffix,
				PrivateZoneClassName: cfg.PrivateZoneClassName, ServingRegions: regions,
				MaxAccessLease: cfg.Admission.maxAccessLease(),
			}}
			if err := reconciler.SetupWithManager(mgr); err != nil {
				return fmt.Errorf("setup reconciler: %w", err)
			}
			return mgr.Start(startCtx)
		}
		projects = append(projects, projectRuntime{config: project, direct: direct, start: start})
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
			PlatformSubjects:        cfg.Admission.PlatformSubjects,
			IntegrationSubjects:     cfg.Admission.IntegrationSubjects,
			MaxContributionLease:    cfg.Admission.maxLease(),
			MaxAccessLease:          cfg.Admission.maxAccessLease(),
			ResolverContextsEnabled: true,
			ClientForRequest:        projectSelector(projects),
		}
		admissionManager.GetWebhookServer().Register(dnsadmission.Path, &kubeadmission.Webhook{Handler: handler})
	}

	errCh := make(chan error, len(projects)+len(planners)*3+8)
	startProjectSources(ctx, projects)
	if admissionManager != nil {
		go func() { errCh <- admissionManager.Start(ctx) }()
	}

	publicationMembers := regionalMembers(planners)
	publicationRegions := map[string][]model.Member{}
	for _, planner := range planners {
		publicationRegions[planner.Region+"/"+planner.Shard] = append([]model.Member(nil), planner.Members...)
	}
	projectScopes := make([]platform.ProjectScope, 0, len(projects))
	for _, p := range projects {
		projectScopes = append(projectScopes, platform.ProjectScope{Client: p.direct, Namespace: p.config.Namespace, RequestTimeout: p.config.KubeAPI.requestTimeout()})
	}
	sinks := map[string]*platform.AckSink{}
	for _, plannerCfg := range planners {
		pc := plannerCfg
		planner := &platform.Planner{Client: platformClient, Allocator: allocator, Config: pc}
		interval := pc.LeaseDuration / 3
		if interval < time.Second {
			interval = time.Second
		}
		go retryLoop(ctx, interval, "planner "+pc.Region+"/"+pc.Shard, planner.Step, errCh)
		sink := &platform.AckSink{Client: platformClient, Namespace: cfg.PlatformNamespace, Projects: projectScopes,
			PublicationMembers: publicationMembers, PublicationRegions: publicationRegions, Region: pc.Region, Shard: pc.Shard, Members: pc.Members}
		sinks[pc.Region+"/"+pc.Shard] = sink
		go retryLoop(ctx, 2*time.Second, "ack refresh "+pc.Region+"/"+pc.Shard, sink.Refresh, errCh)
	}
	startRegionalWorkers(planners, func(region string, regionalPlanners []platform.PlannerConfig) error {
		return runControlPlaneRegion(ctx, cfg, region, regionalPlanners, platformClient, sinks)
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
		err := project.start(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("source manager stopped unexpectedly")
		}
		log.Error(err, "source manager unavailable; rebuilding and retrying")
		if !waitForRetry(ctx, time.Second) {
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

func runControlPlaneRegion(ctx context.Context, cfg Config, region string, planners []platform.PlannerConfig, platformClient client.Client, sinks map[string]*platform.AckSink) error {
	log := ctrl.LoggerFrom(ctx).WithName("internal-dns-region").WithValues("region", region)
	for ctx.Err() == nil {
		regional, err := connectRegional(ctx, cfg, []string{region})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Error(err, "regional transport setup failed; retrying")
			if !waitForRetry(ctx, time.Second) {
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
			if !waitForRetry(ctx, time.Second) {
				return nil
			}
			continue
		}
		workerCtx, cancel := context.WithCancel(ctx)
		localDone := make(chan error, len(consumers)+1)
		outbox := &platform.Outbox{Client: platformClient, Namespace: cfg.PlatformNamespace, Region: region, Publisher: route.publisher, Bootstrap: route.bootstrap, PendingOnly: true}
		go retryLoop(workerCtx, 500*time.Millisecond, "outbox "+region, outbox.Step, localDone)
		for key, consumer := range consumers {
			go consumeAcks(workerCtx, consumer, sinks[key], localDone)
		}
		<-ctx.Done()
		cancel()
		regional.Close()
		return nil
	}
	return nil
}

func waitForRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func runAgent(ctx context.Context, cfg Config) error {
	regional, err := connectRegional(ctx, cfg, []string{cfg.Agent.Region})
	if err != nil {
		return err
	}
	defer regional.Close()
	route, _ := regional.route(cfg.Agent.Region)
	consumer := func(suffix string, filters ...string) (*transport.Consumer, error) {
		return route.bus.Consumer(ctx, transport.ConsumerConfig{Stream: route.stream.Name,
			Durable: durable(cfg.Agent.MemberID, suffix), FilterSubjects: filters,
			AckWait: 30 * time.Second, MaxAckPending: 64})
	}
	servingSource, err := consumer("serving", model.ServingSubject(cfg.Agent.Region, cfg.Agent.Shard))
	if err != nil {
		return err
	}
	var recordSource, ackSource *transport.Consumer
	if cfg.Agent.Mode == serving.ModeResolver || cfg.Agent.Mode == serving.ModeCombined {
		recordSource, err = consumer("records", model.RecordConsumerFilter(cfg.Agent.Region, cfg.Agent.Shard))
		if err != nil {
			return err
		}
	}
	if cfg.Agent.Mode == serving.ModeResolver || cfg.Agent.Mode == serving.ModeCombined {
		ackSource, err = consumer("acks", ackFilter(cfg.Agent.Region, cfg.Agent.Shard))
		if err != nil {
			return err
		}
	}
	log := ctrl.LoggerFrom(ctx).WithName("internal-dns-agent")
	agent, err := serving.NewAgent(cfg.Agent, serving.Dependencies{
		ServingSource: servingSource, RecordSource: recordSource, AckSource: ackSource,
		Bootstrap:    serving.SnapshotBootstrap{Store: route.store, Region: cfg.Agent.Region, Shard: cfg.Agent.Shard},
		AckPublisher: route.bus,
		OnError:      func(err error) { log.Error(err, "serving agent operation failed; retrying") },
	})
	if err != nil {
		return err
	}
	return agent.Start(ctx)
}

func newScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
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
func applyKubeAPILimit(c *rest.Config, limits KubeAPIClientConfig) {
	c.QPS = limits.QPS
	c.Burst = limits.Burst
	c.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(limits.QPS, limits.Burst)
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

func retryLoop(ctx context.Context, interval time.Duration, name string, step func(context.Context) error, terminal chan<- error) {
	log := ctrl.LoggerFrom(ctx).WithName("internal-dns").WithValues("loop", name)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := step(ctx); err != nil && !errors.Is(err, context.Canceled) {
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

func consumeAcks(ctx context.Context, source *transport.Consumer, sink *platform.AckSink, terminal chan<- error) {
	log := ctrl.LoggerFrom(ctx).WithName("internal-dns-ack-consumer").WithValues("region", sink.Region, "shard", sink.Shard)
	for {
		message, err := source.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				terminal <- nil
				return
			}
			log.Error(err, "read acknowledgement failed; retrying")
			time.Sleep(time.Second)
			continue
		}
		if err := sink.Apply(ctx, message.Subject(), message.Envelope()); err != nil {
			log.Error(err, "apply acknowledgement failed; retrying")
			_ = message.Retry(time.Second)
			continue
		}
		if err := message.Ack(ctx); err != nil {
			log.Error(err, "acknowledgement commit was ambiguous; awaiting redelivery")
		}
	}
}

func projectSelector(projects []projectRuntime) func(context.Context, kubeadmission.Request) (client.Client, string, error) {
	return func(_ context.Context, req kubeadmission.Request) (client.Client, string, error) {
		parents := req.UserInfo.Extra[parentNameExtra]
		if len(parents) == 0 && len(projects) == 1 {
			p := projects[0]
			if req.Namespace != p.config.Namespace {
				return nil, "", fmt.Errorf("request namespace is outside the configured project")
			}
			return p.direct, p.config.SourceClusterUID, nil
		}
		if len(parents) != 1 {
			return nil, "", fmt.Errorf("authenticated project parent is missing or ambiguous")
		}
		for _, p := range projects {
			if p.config.Name == parents[0] && req.Namespace == p.config.Namespace {
				return p.direct, p.config.SourceClusterUID, nil
			}
		}
		return nil, "", fmt.Errorf("authenticated project parent is not configured for this namespace")
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
