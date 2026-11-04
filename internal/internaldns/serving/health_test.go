package serving

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func freeTCPAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	_ = l.Close()
	return address
}

func portOpen(address string) bool {
	conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func eventually(t *testing.T, within time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %s", what, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHealthPortOpenOnlyWhileReady(t *testing.T) {
	address := freeTCPAddress(t)
	var ready atomic.Bool
	h := startHealthPort(address, 10*time.Millisecond, func(context.Context) error {
		if ready.Load() {
			return nil
		}
		return errors.New("not ready")
	})
	defer h.Close()

	time.Sleep(50 * time.Millisecond)
	if portOpen(address) {
		t.Fatal("health port open before the member is ready")
	}
	ready.Store(true)
	eventually(t, time.Second, "health port opening", func() bool { return portOpen(address) })
	ready.Store(false)
	eventually(t, time.Second, "health port closing", func() bool { return !portOpen(address) })
	ready.Store(true)
	eventually(t, time.Second, "health port reopening", func() bool { return portOpen(address) })
	h.Close()
	if portOpen(address) {
		t.Fatal("health port open after Close returned")
	}
}

func TestRunDaemonsClosesHealthPortThenDrains(t *testing.T) {
	cfg := regionalTestConfig(t)
	address := freeTCPAddress(t)
	cfg.HealthCheck = HealthCheckConfig{Address: address, DrainSeconds: 1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- runDaemons(ctx, cfg, func(context.Context) error { return nil }) }()

	eventually(t, 2*time.Second, "health port opening", func() bool { return portOpen(address) })
	cancel()
	stopped := time.Now()
	eventually(t, 500*time.Millisecond, "health port closing on shutdown", func() bool { return !portOpen(address) })
	select {
	case err := <-result:
		t.Fatalf("runDaemons returned %v before the drain time", err)
	case <-time.After(700 * time.Millisecond):
	}
	if portOpen(address) {
		t.Fatal("health port reopened during the drain")
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(stopped); elapsed < time.Second {
			t.Fatalf("runDaemons returned after %s, want at least the 1s drain", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runDaemons did not return after the drain time")
	}
}

func TestRunDaemonsWithoutDrainStopsPromptly(t *testing.T) {
	cfg := regionalTestConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- runDaemons(ctx, cfg, func(context.Context) error { return nil }) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("runDaemons did not stop promptly without a drain time")
	}
}

func TestRunDaemonsKeepsServingWithUnusableHealthAddress(t *testing.T) {
	cfg := regionalTestConfig(t)
	cfg.HealthCheck = HealthCheckConfig{Address: "${UNSET_HEALTH_ADDRESS}:8053", DrainSeconds: 30}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- runDaemons(ctx, cfg, func(context.Context) error { return nil }) }()
	select {
	case err := <-result:
		t.Fatalf("runDaemons returned %v with an unusable health address", err)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runDaemons drained although its health port never opened")
	}
}

func TestHealthCheckListenAddress(t *testing.T) {
	t.Setenv("LISTENER_ADDRESS", "fd00::53")
	for _, tc := range []struct {
		address, want string
		invalid       bool
	}{
		{address: "", want: ""},
		{address: "${LISTENER_ADDRESS}:8053", want: "[fd00::53]:8053"},
		{address: "[${LISTENER_ADDRESS}]:8053", want: "[fd00::53]:8053"},
		{address: "192.0.2.1:8053", want: "192.0.2.1:8053"},
		{address: "[::]:8053", invalid: true},
		{address: ":8053", invalid: true},
		{address: "${LISTENER_ADDRESS}:0", invalid: true},
		{address: "${LISTENER_ADDRESS}", invalid: true},
	} {
		got, err := HealthCheckConfig{Address: tc.address}.listenAddress()
		if (err != nil) != tc.invalid || got != tc.want {
			t.Errorf("listenAddress(%q) = %q, %v; want %q, invalid %v", tc.address, got, err, tc.want, tc.invalid)
		}
	}
}

func TestHealthCheckValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		health  HealthCheckConfig
		role    RenderRole
		noDNS   bool
		invalid bool
	}{
		{name: "off"},
		{name: "address and drain", health: HealthCheckConfig{Address: "${LISTENER_ADDRESS}:8053", DrainSeconds: 15}},
		{name: "drain at limit", health: HealthCheckConfig{DrainSeconds: maxDrainSeconds}},
		{name: "negative drain", health: HealthCheckConfig{DrainSeconds: -1}, invalid: true},
		{name: "drain above limit", health: HealthCheckConfig{DrainSeconds: maxDrainSeconds + 1}, invalid: true},
		{name: "address without DNS verification", health: HealthCheckConfig{Address: "${LISTENER_ADDRESS}:8053"}, noDNS: true, invalid: true},
		{name: "wildcard address", health: HealthCheckConfig{Address: "[::]:8053"}, invalid: true},
		{name: "address without port", health: HealthCheckConfig{Address: "192.0.2.1"}, invalid: true},
		{name: "regional dnsdist has no serving role", health: HealthCheckConfig{DrainSeconds: 15}, role: RenderRegionalDNSDist, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := regionalTestConfig(t)
			if tc.role != "" {
				cfg.Render.Role = tc.role
				cfg.Mode = ModeResolver
			}
			cfg.UnsafeAllowNoDNSVerify = tc.noDNS
			cfg.HealthCheck = tc.health
			if err := cfg.Validate(); (err != nil) != tc.invalid {
				t.Fatalf("Validate() = %v, want invalid %v", err, tc.invalid)
			}
		})
	}
}
