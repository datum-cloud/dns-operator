// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"errors"
	"log"
	"net"
	"os"
	"time"
)

// HealthCheckConfig exposes member readiness as a plain TCP port for network
// load balancers, and keeps the member answering for a while after shutdown
// begins so they can move traffic away first.
type HealthCheckConfig struct {
	Address      string `json:"address,omitempty"`
	DrainSeconds int    `json:"drainSeconds,omitempty"`
}

const maxDrainSeconds = 300

const healthCheckInterval = time.Second

func (c HealthCheckConfig) validate() error {
	if c.DrainSeconds < 0 || c.DrainSeconds > maxDrainSeconds {
		return errors.New("healthCheck.drainSeconds must be between zero and 300")
	}
	return nil
}

func (c HealthCheckConfig) drain() time.Duration {
	return time.Duration(c.DrainSeconds) * time.Second
}

// listenAddress expands environment references so a deployment can name the
// member's interface address.
func (c HealthCheckConfig) listenAddress() (string, error) {
	if c.Address == "" {
		return "", nil
	}
	return metricsListenAddress(os.ExpandEnv(c.Address))
}

// healthPort listens on address only while ready succeeds. A closed port
// refuses connections, which is the signal load balancers act on.
type healthPort struct {
	address  string
	interval time.Duration
	ready    func(context.Context) error
	stop     context.CancelFunc
	done     chan struct{}
}

func startHealthPort(address string, interval time.Duration, ready func(context.Context) error) *healthPort {
	ctx, cancel := context.WithCancel(context.Background())
	h := &healthPort{address: address, interval: interval, ready: ready, stop: cancel, done: make(chan struct{})}
	go h.run(ctx)
	return h
}

// Close stops checking and closes the port before it returns.
func (h *healthPort) Close() {
	if h == nil {
		return
	}
	h.stop()
	<-h.done
}

func (h *healthPort) run(ctx context.Context) {
	defer close(h.done)
	var listener net.Listener
	defer func() {
		if listener != nil {
			_ = listener.Close()
			log.Printf("health port %s closed", h.address)
		}
	}()
	var lastErr string
	for {
		err := h.ready(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil && listener == nil {
			listener, err = net.Listen("tcp", h.address)
			if err == nil {
				go acceptAndClose(listener)
				log.Printf("health port %s open", h.address)
			}
		} else if err != nil && listener != nil {
			_ = listener.Close()
			listener = nil
			log.Printf("health port %s closed", h.address)
		}
		message := ""
		if err != nil {
			message = err.Error()
			if message != lastErr {
				log.Printf("health port %s not ready: %v", h.address, err)
			}
		}
		lastErr = message
		select {
		case <-ctx.Done():
			return
		case <-time.After(h.interval):
		}
	}
}

func acceptAndClose(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		_ = conn.Close()
	}
}
