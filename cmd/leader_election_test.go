// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// testLeaderElection is the flags' defaults with leader election on.
var testLeaderElection = leaderElection{
	enabled: true, leaseDuration: 10 * time.Second, renewDeadline: 3 * time.Second, retryPeriod: 2 * time.Second,
}

// TestEachRoleLeadsItsOwnLease starts a downstream manager and a replicator
// manager in one namespace, as production runs them, and requires both to
// lead. With one lease between them, one of the two would wait for ever.
func TestEachRoleLeadsItsOwnLease(t *testing.T) {
	cfg := startEnvtest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, name := range []string{downstreamLeaseName, replicatorLeaseName} {
		opts := withLeaderElection(ctrl.Options{
			Scheme:                  scheme,
			Metrics:                 metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress:  "0",
			LeaderElectionNamespace: testNamespace,
		}, cfg, name, testLeaderElection)
		mgr, err := ctrl.NewManager(cfg, opts)
		if err != nil {
			t.Fatalf("manager for %s: %v", name, err)
		}
		go func() { _ = mgr.Start(ctx) }()
		select {
		case <-mgr.Elected():
		case <-time.After(15 * time.Second):
			t.Fatalf("the manager for %s did not lead its lease within 15 s", name)
		}
	}
}

func TestWithLeaderElectionTakesTheFlags(t *testing.T) {
	le := leaderElection{
		enabled: true, leaseDuration: time.Minute, renewDeadline: 40 * time.Second, retryPeriod: 5 * time.Second,
	}
	base := &rest.Config{Host: "https://example.invalid"}

	opts := withLeaderElection(ctrl.Options{}, base, replicatorLeaseName, le)

	if !opts.LeaderElection || opts.LeaderElectionID != replicatorLeaseName {
		t.Errorf("got election %v on %q, want it on %q", opts.LeaderElection, opts.LeaderElectionID, replicatorLeaseName)
	}
	if *opts.LeaseDuration != le.leaseDuration || *opts.RenewDeadline != le.renewDeadline ||
		*opts.RetryPeriod != le.retryPeriod {
		t.Errorf("got timings %v/%v/%v, want 1m0s/40s/5s", *opts.LeaseDuration, *opts.RenewDeadline, *opts.RetryPeriod)
	}
	if !opts.LeaderElectionReleaseOnCancel {
		t.Error("a stopping leader keeps its lease until it expires")
	}
	if opts.LeaderElectionConfig == nil || opts.LeaderElectionConfig.Dial == nil {
		t.Error("the lease shares the manager's client")
	}
}

type remoteAddrRecorder struct {
	mu    sync.Mutex
	addrs []string
}

func (r *remoteAddrRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.addrs = append(r.addrs, req.RemoteAddr)
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

func (r *remoteAddrRecorder) last() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.addrs[len(r.addrs)-1]
}

func restClientFor(t *testing.T, cfg *rest.Config) *rest.RESTClient {
	t.Helper()
	cfg = rest.CopyConfig(cfg)
	cfg.GroupVersion = &schema.GroupVersion{Group: "coordination.k8s.io", Version: "v1"}
	cfg.APIPath = "/apis"
	cfg.NegotiatedSerializer = serializer.NewCodecFactory(scheme).WithoutConversion()
	c, err := rest.RESTClientFor(cfg)
	if err != nil {
		t.Fatalf("building REST client: %v", err)
	}
	return c
}

func getLease(ctx context.Context, c *rest.RESTClient) error {
	return c.Get().Namespace(testNamespace).Resource("leases").Name(replicatorLeaseName).Do(ctx).Error()
}

func TestLeaderElectionRestConfigDoesNotMutateBase(t *testing.T) {
	limiter := flowcontrol.NewTokenBucketRateLimiter(1, 1)
	base := &rest.Config{Host: "https://example.invalid", QPS: 50, Burst: 100, RateLimiter: limiter}

	cfg := leaderElectionRestConfig(base)

	if cfg.RateLimiter != nil {
		t.Errorf("leader election config reuses the base rate limiter")
	}
	if cfg.QPS != leaderElectionQPS || cfg.Burst != leaderElectionBurst {
		t.Errorf("got QPS %v Burst %d, want %v and %d", cfg.QPS, cfg.Burst, leaderElectionQPS, leaderElectionBurst)
	}
	if cfg.Dial == nil {
		t.Errorf("leader election config has no dialer of its own")
	}
	if base.RateLimiter != limiter || base.QPS != 50 || base.Burst != 100 || base.Dial != nil {
		t.Errorf("base config was mutated: %+v", base)
	}
}

func TestLeaderElectionIgnoresSaturatedMainLimiter(t *testing.T) {
	srv := httptest.NewServer(&remoteAddrRecorder{})
	defer srv.Close()

	base := &rest.Config{Host: srv.URL, RateLimiter: flowcontrol.NewTokenBucketRateLimiter(0.001, 1)}
	mainClient := restClientFor(t, base)

	if err := getLease(context.Background(), mainClient); err != nil {
		t.Fatalf("first request on main client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := getLease(ctx, mainClient); err == nil {
		t.Fatalf("expected the main client to be throttled")
	}

	leaderClient := restClientFor(t, leaderElectionRestConfig(base))
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := getLease(ctx, leaderClient); err != nil {
		t.Fatalf("leader election request was throttled by the main limiter: %v", err)
	}
}

func TestLeaderElectionUsesItsOwnConnection(t *testing.T) {
	rec := &remoteAddrRecorder{}
	srv := httptest.NewServer(rec)
	defer srv.Close()

	base := &rest.Config{Host: srv.URL, QPS: -1}
	ctx := context.Background()

	if err := getLease(ctx, restClientFor(t, base)); err != nil {
		t.Fatalf("main client request: %v", err)
	}
	mainAddr := rec.last()
	if err := getLease(ctx, restClientFor(t, base)); err != nil {
		t.Fatalf("second main client request: %v", err)
	}
	if rec.last() != mainAddr {
		t.Fatalf("clients built from the same config did not share a connection, so this test cannot tell connections apart")
	}

	if err := getLease(ctx, restClientFor(t, leaderElectionRestConfig(base))); err != nil {
		t.Fatalf("leader election request: %v", err)
	}
	if rec.last() == mainAddr {
		t.Errorf("leader election reused the main client's connection %s", mainAddr)
	}
}
