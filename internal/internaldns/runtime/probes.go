package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

// metricsBindAddress keeps the deprecated configuration key working for
// deployments that still set it; otherwise the flag applies.
func metricsBindAddress(cfg Config, opts Options) string {
	if cfg.MetricsBindAddress != "" {
		return cfg.MetricsBindAddress
	}
	return opts.MetricsBindAddress
}

type firstSuccess struct{ ok atomic.Bool }

func (f *firstSuccess) done() { f.ok.Store(true) }

func (f *firstSuccess) check(*http.Request) error {
	if !f.ok.Load() {
		return errors.New("project discovery has not completed a pass")
	}
	return nil
}

type probeServer struct {
	listener net.Listener
	server   *http.Server
}

func newProbeServer(address string, live, ready map[string]healthz.Checker) (*probeServer, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen for health probes on %s: %w", address, err)
	}
	mux := http.NewServeMux()
	for path, checks := range map[string]map[string]healthz.Checker{"/healthz": live, "/readyz": ready} {
		handler := http.StripPrefix(path, &healthz.Handler{Checks: checks})
		mux.Handle(path, handler)
		mux.Handle(path+"/", handler)
	}
	return &probeServer{listener: listener, server: &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}}, nil
}

func (s *probeServer) Start(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.server.Shutdown(shutdown)
	}()
	if err := s.server.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve health probes: %w", err)
	}
	return nil
}
