// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/pprof/profile"
)

func TestProfileAddressIsOptInAndLoopbackOnly(t *testing.T) {
	for _, address := range []string{"", "127.0.0.1:6060", "[::1]:6060"} {
		if err := ValidateProfileAddress(address); err != nil {
			t.Errorf("%q: %v", address, err)
		}
	}
	for _, address := range []string{":6060", "0.0.0.0:6060", "[::]:6060", "10.0.0.1:6060", "[fd00::1]:6060", "localhost:6060", "${POD_IP}:6060", "127.0.0.1:0", "127.0.0.1:65536", "[::1%lo]:6060", "[::ffff:0.0.0.0]:6060"} {
		if err := ValidateProfileAddress(address); err == nil {
			t.Errorf("accepted profile address %q", address)
		}
		if done, err := serveProfiling(context.Background(), address); done != nil || err == nil {
			t.Errorf("listener accepted profile address %q", address)
		}
	}
	if done, err := serveProfiling(context.Background(), ""); done != nil || err != nil {
		t.Fatalf("disabled profiler = %v, %v", done, err)
	}
}

func TestProfileRequestsAreBounded(t *testing.T) {
	handler := profilingHandler()
	for _, path := range []string{
		"/debug/pprof/profile?seconds=0", "/debug/pprof/profile?seconds=-1",
		"/debug/pprof/profile?seconds=31", "/debug/pprof/profile?seconds=1.5",
		"/debug/pprof/profile?seconds=no", "/debug/pprof/profile?seconds=",
		"/debug/pprof/profile?seconds=1&seconds=30", "/debug/pprof/profile?unknown=1",
		"/debug/pprof/profile?seconds=%zz", "/debug/pprof/profile?seconds=1;seconds=30",
		"/debug/pprof/heap?gc=1", "/debug/pprof/heap?debug=1",
		"/debug/pprof/allocs?seconds=1",
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		// Malformed query strings must be rejected rather than silently dropped.
		i := 0
		for i < len(path) && path[i] != '?' {
			i++
		}
		r.URL.Path, r.URL.RawQuery = path[:i], path[i+1:]
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s returned %d", path, w.Code)
		}
	}
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/cmdline", "/debug/pprof/trace", "/debug/pprof/goroutine", "/metrics"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s returned %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/debug/pprof/heap", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", w.Code)
	}
}

func TestHeapAndAllocationCapturesArePprofProfiles(t *testing.T) {
	handler := profilingHandler()
	for _, name := range []string{"heap", "allocs"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/debug/pprof/"+name, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d", name, w.Code)
		}
		p, err := profile.ParseData(w.Body.Bytes())
		if err != nil {
			t.Fatalf("%s invalid profile: %v", name, err)
		}
		if len(p.SampleType) != 4 {
			t.Fatalf("%s missing allocation/in-use sample types: %v", name, p.SampleType)
		}
	}
}

func TestCPUProfileCancellationReleasesCapture(t *testing.T) {
	handler := profilingHandler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cpu := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(cpu, httptest.NewRequest(http.MethodGet, "/debug/pprof/profile?seconds=30", nil).WithContext(ctx))
	}()
	deadline := time.Now().Add(2 * time.Second)
	for len(handler.active) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("CPU capture did not start")
		}
		time.Sleep(time.Millisecond)
	}
	busy := httptest.NewRecorder()
	handler.ServeHTTP(busy, httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil))
	if busy.Code != http.StatusTooManyRequests {
		t.Fatalf("concurrent capture = %d", busy.Code)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled CPU profile retained profiler")
	}
	if cpu.Code != http.StatusOK {
		t.Fatalf("CPU profile = %d: %s", cpu.Code, cpu.Body.String())
	}
	if _, err := profile.ParseData(cpu.Body.Bytes()); err != nil {
		t.Fatalf("invalid CPU profile: %v", err)
	}
	w := httptest.NewRecorder()
	started := time.Now()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/debug/pprof/profile?seconds=1", nil))
	elapsed := time.Since(started)
	// The runtime profile writer starts asynchronously, so the encoded profile
	// can begin after the handler's timer. Measure the requested capture window
	// at the handler boundary rather than requiring identical profiler timing.
	if elapsed < time.Second || elapsed > 3*time.Second {
		t.Fatalf("one-second capture elapsed time = %s", elapsed)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("next CPU profile = %d: %s", w.Code, w.Body.String())
	}
	p, err := profile.ParseData(w.Body.Bytes())
	if err != nil {
		t.Fatalf("invalid subsequent CPU profile: %v", err)
	}
	if p.DurationNanos <= 0 || p.DurationNanos > int64(3*time.Second) {
		t.Fatalf("one-second capture duration = %s", time.Duration(p.DurationNanos))
	}
}

func TestProfileListenerLifecycleAndMetricsSeparation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	address := freeAddress(t)
	done, err := serveProfiling(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Get("http://" + address + "/debug/pprof/heap")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := profile.ParseData(data); err != nil {
		t.Fatal(err)
	}
	metrics := freeAddress(t)
	metricsDone, err := serveMetrics(ctx, metrics, &ContextPlugin{})
	if err != nil {
		t.Fatal(err)
	}
	response, err = http.Get("http://" + metrics + "/debug/pprof/heap")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("profile exposed on metrics: %d", response.StatusCode)
	}
	cancel()
	for _, stopped := range []<-chan error{done, metricsDone} {
		select {
		case err := <-stopped:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("listener did not stop")
		}
	}
	if response, err := http.Get("http://" + address + "/debug/pprof/heap"); err == nil {
		_ = response.Body.Close()
		t.Fatal("profiling listener survived cancellation")
	}
}

func TestProfilingPreservesQueryService(t *testing.T) {
	for _, occupied := range []bool{false, true} {
		t.Run(map[bool]string{false: "enabled", true: "bind-failure"}[occupied], func(t *testing.T) {
			cfg := fixture()
			cfg.ListenAddress = freeAddress(t)
			address := freeAddress(t)
			if occupied {
				listener, err := net.Listen("tcp", address)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = listener.Close() }()
			}
			path := filepath.Join(t.TempDir(), "contexts.json")
			data, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- (Process{ProfileAddress: address}).Run(ctx, path) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Error("DNS process did not stop")
				}
			})
			deadline := time.Now().Add(3 * time.Second)
			for {
				answer, err := proxyQuery("tcp", cfg.ListenAddress, "fd53::b")
				if err == nil && len(answer.Answer) > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("query with profiling occupied=%v: %v %v", occupied, answer, err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !occupied {
				response, err := http.Get("http://" + address + "/debug/pprof/heap")
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(response.Body)
				_ = response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := profile.ParseData(data); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
