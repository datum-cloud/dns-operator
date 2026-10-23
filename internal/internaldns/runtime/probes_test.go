package runtime

import (
	"context"
	"net/http"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

func TestProbeServerWaitsForProjectDiscovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	discovered := &firstSuccess{}
	live := newLoopLiveness()
	server, err := newProbeServer("127.0.0.1:0", map[string]healthz.Checker{"loops": live.check}, map[string]healthz.Checker{"projects": discovered.check})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Start(ctx) }()
	base := "http://" + server.listener.Addr().String()
	status := func(path string) int {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := status("/healthz"); got != http.StatusOK {
		t.Fatalf("/healthz = %d", got)
	}
	if got := status("/readyz"); got != http.StatusInternalServerError {
		t.Fatalf("/readyz before discovery = %d", got)
	}
	discovered.done()
	if got := status("/readyz"); got != http.StatusOK {
		t.Fatalf("/readyz after discovery = %d", got)
	}
	if got := status("/readyz/projects"); got != http.StatusOK {
		t.Fatalf("/readyz/projects = %d", got)
	}
}

func TestConfiguredMetricsAddressOverridesFlag(t *testing.T) {
	if got := metricsBindAddress(Config{MetricsBindAddress: ":9000"}, Options{MetricsBindAddress: ":8080"}); got != ":9000" {
		t.Fatalf("got %q", got)
	}
	if got := metricsBindAddress(Config{}, Options{MetricsBindAddress: ":8080"}); got != ":8080" {
		t.Fatalf("got %q", got)
	}
}
