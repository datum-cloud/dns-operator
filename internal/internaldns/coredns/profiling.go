// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime/pprof"
	"strconv"
	"time"
)

const maxProfileSeconds = 30

func ValidateProfileAddress(address string) error {
	if address == "" {
		return nil
	}
	addr, err := netip.ParseAddrPort(address)
	if err != nil || !addr.Addr().Unmap().IsLoopback() || addr.Addr().Zone() != "" || addr.Port() == 0 {
		return fmt.Errorf("profile address %q must be a loopback IP literal and nonzero port", address)
	}
	return nil
}

func serveProfiling(ctx context.Context, address string) (<-chan error, error) {
	if address == "" {
		return nil, nil
	}
	if err := ValidateProfileAddress(address); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	server := &http.Server{
		Handler:           profilingHandler(),
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      (maxProfileSeconds + 5) * time.Second,
		IdleTimeout:       10 * time.Second,
		MaxHeaderBytes:    4096,
	}
	done := make(chan error, 1)
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	return done, nil
}

type profileHandler struct {
	active chan struct{}
}

func profilingHandler() *profileHandler {
	return &profileHandler{active: make(chan struct{}, 1)}
}

func (h *profileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "GET required", http.StatusMethodNotAllowed)
		return
	}
	var name string
	switch r.URL.Path {
	case "/debug/pprof/profile":
		name = "cpu"
	case "/debug/pprof/heap":
		name = "heap"
	case "/debug/pprof/allocs":
		name = "allocs"
	default:
		http.NotFound(w, r)
		return
	}
	seconds := 10
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		http.Error(w, "invalid query parameters", http.StatusBadRequest)
		return
	}
	for key, values := range query {
		if name != "cpu" || key != "seconds" || len(values) != 1 {
			http.Error(w, "only CPU profiles accept a single seconds parameter", http.StatusBadRequest)
			return
		}
		seconds, err = strconv.Atoi(values[0])
	}
	if err != nil || seconds < 1 || seconds > maxProfileSeconds {
		http.Error(w, "seconds must be an integer from 1 to 30", http.StatusBadRequest)
		return
	}
	select {
	case h.active <- struct{}{}:
		defer func() { <-h.active }()
	default:
		http.Error(w, "a profile capture is already active", http.StatusTooManyRequests)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if name != "cpu" {
		_ = pprof.Lookup(name).WriteTo(w, 0)
		return
	}
	if err := pprof.StartCPUProfile(w); err != nil {
		http.Error(w, "CPU profiler is unavailable", http.StatusConflict)
		return
	}
	defer pprof.StopCPUProfile()
	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-r.Context().Done():
	}
}
