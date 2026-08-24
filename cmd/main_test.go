// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	multiclusterproviders "go.miloapis.com/milo/pkg/multicluster-runtime"

	"go.miloapis.com/dns-operator/internal/config"
)

// TestReplicatorProviderLifecycle exercises the replicator startup path from
// cmd/main.go against envtest: initializeClusterDiscovery builds the single
// cluster discovery provider, and runReplicator starts everything.
//
// It asserts that the multicluster manager engages the cluster on its own -
// there is no explicit provider start call anymore - by requiring that a
// controller registered through the multicluster builder actually receives a
// reconcile for an object created in the cluster, under the expected cluster
// name. It then asserts that cancelling the context shuts everything down.
func TestReplicatorProviderLifecycle(t *testing.T) {
	testEnv := &envtest.Environment{}
	if dir := firstEnvTestBinaryDir(t); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("starting envtest: %v (run 'make setup-envtest' or set KUBEBUILDER_ASSETS)", err)
	}
	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stopping envtest: %v", err)
		}
	})

	deploymentCluster, err := cluster.New(cfg, func(o *cluster.Options) { o.Scheme = scheme })
	if err != nil {
		t.Fatalf("creating deployment cluster: %v", err)
	}

	// The downstream cluster is a separate cluster.Cluster in production; the
	// same API server is fine here, runReplicator only has to start it.
	downstreamCluster, err := cluster.New(cfg, func(o *cluster.Options) { o.Scheme = scheme })
	if err != nil {
		t.Fatalf("creating downstream cluster: %v", err)
	}

	serverConfig := config.DNSOperator{
		Discovery: config.DiscoveryConfig{Mode: multiclusterproviders.ProviderSingle},
	}
	runnables, provider, err := initializeClusterDiscovery(serverConfig, deploymentCluster, scheme)
	if err != nil {
		t.Fatalf("initializeClusterDiscovery: %v", err)
	}
	if provider == nil {
		t.Fatal("initializeClusterDiscovery returned a nil provider")
	}

	mcmgr, err := mcmanager.New(cfg, provider, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		t.Fatalf("creating multicluster manager: %v", err)
	}

	// A controller registered through the multicluster builder only starts
	// watching once its cluster is engaged, so a reconcile arriving here proves
	// engagement happened.
	reconciled := make(chan mcreconcile.Request, 16)
	err = mcbuilder.ControllerManagedBy(mcmgr).
		For(&corev1.ConfigMap{}).
		Named("provider-lifecycle-test").
		Complete(reconcile.TypedFunc[mcreconcile.Request](
			func(_ context.Context, req mcreconcile.Request) (reconcile.Result, error) {
				select {
				case reconciled <- req:
				default:
				}
				return reconcile.Result{}, nil
			}))
	if err != nil {
		t.Fatalf("registering test controller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Note: no provider start call here, matching cmd/main.go.
	runErr := make(chan error, 1)
	go func() { runErr <- runReplicator(ctx, runnables, downstreamCluster, mcmgr) }()

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("creating client: %v", err)
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "engagement-probe", Namespace: "default"},
	}
	if err := k8sClient.Create(ctx, cm); err != nil {
		t.Fatalf("creating ConfigMap: %v", err)
	}

	deadline := time.After(90 * time.Second)
	for {
		gotIt := false
		select {
		case req := <-reconciled:
			if req.Name != cm.Name || req.Namespace != cm.Namespace {
				continue // unrelated ConfigMap (e.g. kube-root-ca.crt)
			}
			if want := "single"; req.ClusterName.String() != want {
				t.Fatalf("reconcile cluster name = %q, want %q", req.ClusterName, want)
			}
			gotIt = true
		case err := <-runErr:
			t.Fatalf("runReplicator returned before the object was reconciled: %v", err)
		case <-deadline:
			t.Fatal("timed out waiting for a reconcile: the cluster was never engaged, " +
				"so mcmgr.Start did not run the discovery provider")
		}
		if gotIt {
			break
		}
	}

	// Shutdown: cancelling the context must stop everything runReplicator owns.
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("runReplicator returned an error on shutdown: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("runReplicator did not return within 60s of context cancellation")
	}
}

// firstEnvTestBinaryDir locates envtest binaries under ./bin/k8s so the test can
// run from an IDE without KUBEBUILDER_ASSETS being set. Run 'make setup-envtest'
// to populate it.
func firstEnvTestBinaryDir(t *testing.T) string {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") != "" {
		return ""
	}
	basePath := filepath.Join("..", "bin", "k8s")
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(basePath, entry.Name())
		}
	}
	return ""
}
