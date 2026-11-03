// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	multiclusterproviders "go.miloapis.com/milo/pkg/multicluster-runtime"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/config"
	"go.miloapis.com/dns-operator/internal/controller"
	"go.miloapis.com/dns-operator/internal/display"
	dnswebhook "go.miloapis.com/dns-operator/internal/webhook"
)

const (
	testProject   = "p1"
	testZone      = "zone"
	testNamespace = "default"
	testDomain    = "example.com"
)

// projectControlPlanePath is the path Milo serves each project's control plane
// under; the provider appends it to the project kubeconfig's host.
var projectControlPlanePath = regexp.MustCompile(
	`^/apis/resourcemanager\.miloapis\.com/v1alpha1/projects/[^/]+/control-plane`)

// TestNonLeaderReplica runs the replicator as a replica that never wins the
// lease, with the Milo provider production uses and a webhook server that
// never serves. One replica serves both subtests: the provider registers a
// process-wide controller name, so a second provider in this process would
// not start.
func TestNonLeaderReplica(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probe := freeAddress(t)
	p := startPlanes(t)
	mcmgr := startReplica(ctx, t, p, true, newCluster(t, p.deployment), ctrl.Options{
		HealthProbeBindAddress: probe,
		WebhookServer:          neverServing{webhook.NewServer(webhook.Options{})},
	}, setupReplicatorWebhook)

	// The webhook must read the record set's DNSZone from the project's
	// control plane and stamp the display annotations.
	t.Run("its webhook reads the project's zone", func(t *testing.T) {
		// The same construction SetupDNSRecordSetWebhook registers.
		mutator := &dnswebhook.DNSRecordSetMutator{Manager: mcmgr, Client: mcmgr.GetLocalManager().GetClient()}
		if !annotatedWithin(ctx, t, mutator, 60*time.Second) {
			t.Fatal("a replica that is not the leader admitted a DNSRecordSet without its display annotations: " +
				"it never engaged the project, so its webhook read the DNSZone from the deployment cluster")
		}
	})

	// The webhook fails closed, so a replica must not take traffic before its
	// webhook server serves, nor before it has registered the projects it
	// found at start. So /readyz must fail on the webhook while the projects
	// check passes.
	t.Run("it is ready only when its webhook serves", func(t *testing.T) {
		status, body := readyzOnceProjectsRegistered(t, probe, 60*time.Second)
		if status == http.StatusOK || !strings.Contains(body, "[-]webhook failed") {
			t.Errorf("readyz = %d, want a failure with the webhook check failing, while the webhook server does not serve:\n%s",
				status, body)
		}
		if !strings.Contains(body, "[+]projects ok") {
			t.Errorf("readyz never reported the projects registered:\n%s", body)
		}
	})
	requireNotLeader(t, mcmgr)
}

// readyzOnceProjectsRegistered reads the replica's verbose readiness until it
// reports the projects registered, or until timeout, and returns the last read.
func readyzOnceProjectsRegistered(t *testing.T, address string, timeout time.Duration) (int, string) {
	t.Helper()
	status, body := readyz(t, address)
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(time.Second) {
		if status, body = readyz(t, address); strings.Contains(body, "[+]projects ok") {
			break
		}
	}
	return status, body
}

const ownProcessEnv = "DNS_OPERATOR_TEST_OWN_PROCESS"

func TestSingleReplicaWithoutLeaderElection(t *testing.T) {
	// The Milo provider registers a process-wide controller name, and
	// TestNonLeaderReplica starts one, so this test runs in a process of its own.
	if os.Getenv(ownProcessEnv) != t.Name() {
		runInOwnProcess(t)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := startPlanes(t)
	downstream := newCluster(t, p.deployment)
	zoneReplicator := func(mcmgr mcmanager.Manager) error {
		return (&controller.DNSZoneReplicator{DownstreamClient: downstream.GetClient()}).SetupWithManager(mcmgr, downstream)
	}
	mcmgr := startReplica(ctx, t, p, false, downstream, ctrl.Options{
		HealthProbeBindAddress: freeAddress(t),
		WebhookServer:          neverServing{webhook.NewServer(webhook.Options{})},
	}, zoneReplicator, setupReplicatorWebhook)

	if !finalizedWithin(ctx, t, newClient(t, p.project), 60*time.Second) {
		t.Fatal("the replicator never reconciled the project's DNSZone")
	}
	mutator := &dnswebhook.DNSRecordSetMutator{Manager: mcmgr, Client: mcmgr.GetLocalManager().GetClient()}
	if !annotatedWithin(ctx, t, mutator, 60*time.Second) {
		t.Fatal("the webhook admitted a DNSRecordSet without its display annotations")
	}
}

func runInOwnProcess(t *testing.T) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = append(os.Environ(), ownProcessEnv+"="+t.Name())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s in its own process: %v\n%s", t.Name(), err, out)
	}
}

func finalizedWithin(ctx context.Context, t *testing.T, c client.Client, timeout time.Duration) bool {
	t.Helper()
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(time.Second) {
		var zone dnsv1alpha1.DNSZone
		err := c.Get(ctx, client.ObjectKey{Namespace: testNamespace, Name: testZone}, &zone)
		if err == nil && slices.Contains(zone.Finalizers, "dns.networking.miloapis.com/finalize-dnszone") {
			return true
		}
	}
	return false
}

// planes stand in for production's two API servers: the deployment cluster,
// which holds the Projects, the Lease and downstream DNSZones, and a project
// control plane, which holds the project's DNSZone. A proxy in front of the
// second strips the project path, as Milo does. So a webhook that never
// engaged the project falls back to the deployment cluster, finds no zone
// there, and admits the record set without annotations.
type planes struct{ deployment, project *rest.Config }

func startPlanes(t *testing.T) planes {
	t.Helper()
	dnsCRDs := filepath.Join("..", "config", "crd", "bases")
	return planes{
		deployment: startEnvtest(t, dnsCRDs, miloProjectCRDs(t)),
		project:    startEnvtest(t, dnsCRDs, nsoDomainCRD(t)),
	}
}

// startReplica builds the replicator the way main does, with the Milo
// provider production uses, and starts it. A contended replica runs with
// leader election on while another replica holds the lease, so it cannot win
// it; otherwise leader election is off, as production runs it.
func startReplica(
	ctx context.Context, t *testing.T, p planes, contended bool, downstream cluster.Cluster,
	opts ctrl.Options, setups ...func(mcmanager.Manager) error,
) mcmanager.Manager {
	t.Helper()
	deploymentClient := newClient(t, p.deployment)
	if contended {
		holdLease(ctx, t, deploymentClient)
	}
	createReadyProject(ctx, t, deploymentClient)
	createZone(ctx, t, newClient(t, p.project))

	serverConfig := config.DNSOperator{Discovery: config.DiscoveryConfig{
		Mode:                    multiclusterproviders.ProviderMilo,
		DiscoveryKubeconfigPath: writeKubeconfig(t, p.deployment),
		ProjectKubeconfigPath:   writeKubeconfig(t, &rest.Config{Host: projectControlPlaneProxy(t, p.project)}),
	}}
	opts.Scheme = scheme
	opts.Metrics = metricsserver.Options{BindAddress: "0"}
	le := testLeaderElection
	le.enabled = contended
	opts = withLeaderElection(opts, p.deployment, replicatorLeaseName, le)
	opts.LeaderElectionNamespace = testNamespace
	runnables, mcmgr, err := newReplicatorManager(serverConfig, p.deployment, newCluster(t, p.deployment), opts)
	if err != nil {
		t.Fatalf("newReplicatorManager: %v", err)
	}
	for _, setup := range setups {
		if err := setup(mcmgr); err != nil {
			t.Fatalf("setting up the replica: %v", err)
		}
	}
	goUntilCleanup(t, func() error { return runReplicator(ctx, runnables, downstream, mcmgr) })
	return mcmgr
}

// requireNotLeader fails the test if the replica won the lease another holder
// keeps, since then it did not run a replica that is not the leader.
func requireNotLeader(t *testing.T, mcmgr mcmanager.Manager) {
	t.Helper()
	select {
	case <-mcmgr.Elected():
		t.Fatal("the replica won the lease another holder keeps, so this test did not run a non-leader")
	default:
	}
}

// neverServing is a webhook server that registers handlers and never serves.
type neverServing struct{ webhook.Server }

func (neverServing) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// readyz reads the replica's verbose readiness from its health probe address.
func readyz(t *testing.T, address string) (int, string) {
	t.Helper()
	resp, err := http.Get("http://" + address + "/readyz?verbose")
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// freeAddress returns a loopback address with a port nothing listens on.
func freeAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String()
}

// annotatedWithin admits a DNSRecordSet in the test project until the webhook
// stamps its display annotations, and reports whether it did before timeout.
func annotatedWithin(ctx context.Context, t *testing.T, m *dnswebhook.DNSRecordSetMutator, timeout time.Duration) bool {
	t.Helper()
	projectCtx := mccontext.WithCluster(ctx, testProject)
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(time.Second) {
		rs := &dnsv1alpha1.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "www", Namespace: testNamespace}}
		rs.Spec.DNSZoneRef.Name = testZone
		rs.Spec.Records = []dnsv1alpha1.RecordEntry{{Name: "www"}}
		if err := m.Default(projectCtx, rs); err != nil {
			t.Logf("webhook: %v", err)
			continue
		}
		if rs.Annotations[display.AnnotationDisplayName] == "www."+testDomain {
			return true
		}
	}
	return false
}

// goUntilCleanup runs run in the background and, when the test ends, waits for
// it to return. The caller cancels run's context before cleanup. A manager
// still running when envtest stops its API server keeps watches and lease
// renewals open, and the API server then outlasts envtest's stop timeout.
// Cleanups run in reverse order, so every caller after startEnvtest is waited
// for before its API server stops.
func goUntilCleanup(t *testing.T, run func() error) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = run()
	}()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("a manager did not stop within 30 s of its context ending")
		}
	})
}

// startEnvtest starts an API server with the CRDs in dirs and stops it when
// the test ends.
func startEnvtest(t *testing.T, dirs ...string) *rest.Config {
	t.Helper()
	env := &envtest.Environment{CRDDirectoryPaths: dirs, ErrorIfCRDPathMissing: true}
	if dir := firstEnvTestBinaryDir(t); dir != "" {
		env.BinaryAssetsDirectory = dir
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v (run 'task setup-envtest' or set KUBEBUILDER_ASSETS)", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stopping envtest: %v", err)
		}
	})
	return cfg
}

// miloProjectCRDs returns the directory of Milo's resourcemanager CRDs, the
// Project among them, in the module this repository pins.
func miloProjectCRDs(t *testing.T) string {
	t.Helper()
	return filepath.Join(moduleDir(t, "go.miloapis.com/milo"), "config", "crd", "bases", "resourcemanager")
}

func nsoDomainCRD(t *testing.T) string {
	t.Helper()
	return filepath.Join(moduleDir(t, "go.datum.net/network-services-operator"),
		"config", "crd", "bases", "networking.datumapis.com_domains.yaml")
}

func moduleDir(t *testing.T, module string) string {
	t.Helper()
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", module).Output()
	if err != nil {
		t.Fatalf("locating the %s module: %v", module, err)
	}
	return strings.TrimSpace(string(out))
}

// projectControlPlaneProxy serves the project control plane at Milo's project
// path, forwarding to cfg's API server, and returns its URL.
func projectControlPlaneProxy(t *testing.T, cfg *rest.Config) string {
	t.Helper()
	transport, err := rest.TransportFor(cfg)
	if err != nil {
		t.Fatalf("project transport: %v", err)
	}
	target, err := http.NewRequest(http.MethodGet, cfg.Host, nil)
	if err != nil {
		t.Fatalf("project host: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target.URL)
	proxy.Transport = transport
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = projectControlPlanePath.ReplaceAllString(r.URL.Path, "")
		r.URL.RawPath = ""
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// writeKubeconfig writes cfg as a kubeconfig file and returns its path.
func writeKubeconfig(t *testing.T, cfg *rest.Config) string {
	t.Helper()
	kubeconfig := clientcmdapi.NewConfig()
	kubeconfig.Clusters["c"] = &clientcmdapi.Cluster{Server: cfg.Host, CertificateAuthorityData: cfg.CAData}
	kubeconfig.AuthInfos["u"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData, Token: cfg.BearerToken,
	}
	kubeconfig.Contexts["c"] = &clientcmdapi.Context{Cluster: "c", AuthInfo: "u"}
	kubeconfig.CurrentContext = "c"
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*kubeconfig, path); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return path
}

// holdLease records another replica as the holder of the replicator's lease
// for an hour, so the replica under test cannot win it.
func holdLease(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	holder, seconds, now := "another-replica", int32(3600), metav1.NewMicroTime(time.Now())
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: replicatorLeaseName, Namespace: testNamespace},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: &holder, LeaseDurationSeconds: &seconds, AcquireTime: &now, RenewTime: &now,
		},
	}
	if err := c.Create(ctx, lease); err != nil {
		t.Fatalf("creating the held lease: %v", err)
	}
}

// createReadyProject creates the test Project in the deployment cluster and
// marks it Ready, which is what makes the Milo provider register it.
func createReadyProject(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	project := &unstructured.Unstructured{}
	project.SetAPIVersion("resourcemanager.miloapis.com/v1alpha1")
	project.SetKind("Project")
	project.SetName(testProject)
	project.Object["spec"] = map[string]any{"ownerRef": map[string]any{"kind": "Organization", "name": "org"}}
	if err := c.Create(ctx, project); err != nil {
		t.Fatalf("creating the project: %v", err)
	}
	project.Object["status"] = map[string]any{"conditions": []any{map[string]any{
		"type": "Ready", "status": "True", "reason": "Ready", "message": "ready",
		"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
	}}}
	if err := c.Status().Update(ctx, project); err != nil {
		t.Fatalf("marking the project ready: %v", err)
	}
}

// createZone creates the record set's DNSZone in the project control plane.
func createZone(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: testZone, Namespace: testNamespace}}
	zone.Spec.DomainName = testDomain
	zone.Spec.DNSZoneClassName = "test"
	if err := c.Create(ctx, zone); err != nil {
		t.Fatalf("creating the zone: %v", err)
	}
}

func newClient(t *testing.T, cfg *rest.Config) client.Client {
	t.Helper()
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	return c
}

func newCluster(t *testing.T, cfg *rest.Config) cluster.Cluster {
	t.Helper()
	cl, err := cluster.New(cfg, func(o *cluster.Options) { o.Scheme = scheme })
	if err != nil {
		t.Fatalf("creating cluster: %v", err)
	}
	return cl
}
