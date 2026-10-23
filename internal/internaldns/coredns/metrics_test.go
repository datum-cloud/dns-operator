// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestQueriesAreCountedByOutcome(t *testing.T) {
	p := &ContextPlugin{}
	defer p.Close()
	before := map[string]float64{}
	for _, outcome := range []string{outcomeServfailNoState, outcomeServedZone, outcomeRefusedUnknownContext, outcomeServfailZone} {
		before[outcome] = testutil.ToFloat64(contextQueries.WithLabelValues(outcome))
	}
	if _, code := query(t, p, "fd53::a", "app.datum.internal."); code != dns.RcodeServerFailure {
		t.Fatalf("query without a snapshot = %d", code)
	}
	c := fixture()
	if err := p.Replace(c); err != nil {
		t.Fatal(err)
	}
	query(t, p, "fd53::a", "app.datum.internal.")
	query(t, p, "fd53::c", "app.datum.internal.")
	c.Contexts[0].Zones[0].Pending = true
	if err := p.Replace(c); err != nil {
		t.Fatal(err)
	}
	query(t, p, "fd53::a", "app.datum.internal.")
	for outcome, want := range map[string]float64{outcomeServfailNoState: 1, outcomeServedZone: 1, outcomeRefusedUnknownContext: 1, outcomeServfailZone: 1} {
		if got := testutil.ToFloat64(contextQueries.WithLabelValues(outcome)) - before[outcome]; got != want {
			t.Errorf("%s queries = %v, want %v", outcome, got, want)
		}
	}
}

func TestSnapshotReloadsAreCounted(t *testing.T) {
	p := &ContextPlugin{}
	defer p.Close()
	applied := testutil.ToFloat64(snapshotReloads.WithLabelValues("applied"))
	rejected := testutil.ToFloat64(snapshotReloads.WithLabelValues("rejected"))
	c := fixture()
	c.Contexts[1].Pending = true
	if err := p.Replace(c); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(snapshotContexts.WithLabelValues("pending")); got != 1 {
		t.Errorf("pending contexts = %v", got)
	}
	if got := testutil.ToFloat64(snapshotContexts.WithLabelValues("ready")); got != 1 {
		t.Errorf("ready contexts = %v", got)
	}
	c.Contexts[0].Zones[0].Data = "invalid"
	if err := p.Replace(c); err == nil {
		t.Fatal("invalid zone was accepted")
	}
	if got := testutil.ToFloat64(snapshotReloads.WithLabelValues("applied")) - applied; got != 1 {
		t.Errorf("applied reloads = %v", got)
	}
	if got := testutil.ToFloat64(snapshotReloads.WithLabelValues("rejected")) - rejected; got != 1 {
		t.Errorf("rejected reloads = %v", got)
	}
}

func TestMetricsListenerReportsReadiness(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	address := freeAddress(t)
	p := &ContextPlugin{}
	defer p.Close()
	done, err := serveMetrics(ctx, address, p)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get("http://" + address + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if code, _ := get("/health"); code != http.StatusOK {
		t.Fatalf("/health = %d", code)
	}
	if code, _ := get("/ready"); code != http.StatusServiceUnavailable {
		t.Fatalf("/ready without a snapshot = %d", code)
	}
	if err := p.Replace(fixture()); err != nil {
		t.Fatal(err)
	}
	if code, _ := get("/ready"); code != http.StatusOK {
		t.Fatalf("/ready with a snapshot = %d", code)
	}
	if _, body := get("/metrics"); !strings.Contains(body, "internal_dns_context_queries_total") {
		t.Fatal("/metrics is missing the context query counter")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("metrics listener stopped with %v", err)
	}
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().String()
}

func TestQueryMetricsKeepContextSelection(t *testing.T) {
	c := fixture()
	c.ListenAddress = freeAddress(t)
	metricsAddress := freeAddress(t)
	path := filepath.Join(t.TempDir(), "contexts.json")
	data, _ := json.Marshal(c)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, path, metricsAddress) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(7 * time.Second):
			t.Error("CoreDNS did not stop")
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		m, err := proxyQuery("tcp", c.ListenAddress, "fd53::b")
		if err == nil && address(m) == "192.0.2.20" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("context b behind the metrics plugin: %v %v", m, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	resp, err := http.Get("http://" + metricsAddress + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	for _, family := range []string{"coredns_dns_requests_total", "coredns_dns_responses_total", "process_start_time_seconds", `internal_dns_context_queries_total{outcome="served_zone"}`} {
		if !strings.Contains(string(body), family) {
			t.Errorf("/metrics is missing %s", family)
		}
	}
}
