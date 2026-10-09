// SPDX-License-Identifier: AGPL-3.0-only

// Package watchdog supervises a serving agent from an independent process. An
// agent that is killed or hung cannot renew this lease; the supervisor gates its
// DNS processes without depending on Kubernetes or a remote control plane.
package watchdog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/serving"
)

type Config struct {
	LeasePath           string            `json:"leasePath"`
	MemberID            string            `json:"memberID"`
	ReplicaID           string            `json:"replicaID,omitempty"`
	MaxLeaseSeconds     int               `json:"maxLeaseSeconds,omitempty"`
	StartupGraceSeconds int               `json:"startupGraceSeconds,omitempty"`
	IntervalMillis      int               `json:"intervalMillis,omitempty"`
	FailClosed          []serving.Command `json:"failClosed"`
}

type Lease struct {
	MemberID   string    `json:"memberID"`
	ReplicaID  string    `json:"replicaID,omitempty"`
	ValidUntil time.Time `json:"validUntil"`
}

type Watchdog struct {
	Config   Config
	Runner   serving.Runner
	Now      func() time.Time
	ReadFile func(string) ([]byte, error)
}

func (c Config) Validate() error {
	if !filepath.IsAbs(c.LeasePath) || c.MemberID == "" || len(c.FailClosed) == 0 {
		return errors.New("watchdog requires an absolute lease path, member identity, and fail-closed commands")
	}
	if c.MaxLeaseSeconds < 0 || c.MaxLeaseSeconds > 30 || c.StartupGraceSeconds < 0 || c.StartupGraceSeconds > 60 {
		return errors.New("watchdog lease and startup grace exceed platform bounds")
	}
	if c.IntervalMillis < 0 || c.IntervalMillis > 1000 {
		return errors.New("watchdog checks must run at least once per second")
	}
	for _, command := range c.FailClosed {
		if command.Path == "" {
			return errors.New("watchdog fail-closed command path is required")
		}
	}
	return nil
}

func (w *Watchdog) Check(now time.Time) error {
	readFile := w.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	var lease Lease
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		b, err := readFile(w.Config.LeasePath)
		if err != nil {
			lastErr = fmt.Errorf("read serving lease: %w", err)
		} else if len(b) > 4096 {
			// A decoded identity or deadline is never retried, and neither is an
			// oversized file: only a transient read or partial JSON observation
			// can be repaired by the writer's atomic replacement becoming visible.
			return errors.New("serving lease exceeds size limit")
		} else {
			lease = Lease{}
			if err := json.Unmarshal(b, &lease); err != nil {
				lastErr = fmt.Errorf("decode serving lease: %w", err)
			} else {
				lastErr = nil
				break
			}
		}
		if attempt < 2 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if lastErr != nil {
		return lastErr
	}
	if lease.MemberID != w.Config.MemberID || w.Config.ReplicaID != "" && lease.ReplicaID != w.Config.ReplicaID {
		return errors.New("serving lease belongs to another member")
	}
	if w.Now != nil {
		// A successful retry may observe a lease written after Check's caller
		// sampled now. Validate that replacement against the current clock so a
		// fresh maximum-length lease is not misclassified as overlong.
		now = w.Now()
	}
	max := w.Config.MaxLeaseSeconds
	if max == 0 {
		max = 5
	}
	if lease.ValidUntil.IsZero() || !lease.ValidUntil.After(now) {
		return errors.New("serving lease expired")
	}
	if lease.ValidUntil.After(now.Add(time.Duration(max) * time.Second)) {
		return errors.New("serving lease deadline exceeds the watchdog bound")
	}
	return nil
}

func (w *Watchdog) Start(ctx context.Context) error {
	if err := w.Config.Validate(); err != nil {
		return err
	}
	if w.Runner == nil {
		w.Runner = serving.ExecRunner{}
	}
	if w.Now == nil {
		w.Now = time.Now
	}
	start := w.Now()
	last := start
	interval := w.Config.IntervalMillis
	if interval == 0 {
		interval = 250
	}
	ticker := time.NewTicker(time.Duration(interval) * time.Millisecond)
	defer ticker.Stop()
	for {
		now := w.Now()
		err := w.Check(now)
		if now.Before(last.Add(-time.Second)) {
			err = errors.New("watchdog clock moved backwards")
		}
		last = now
		// Grace only applies to a new member without any checkpoint. An existing
		// expired or malformed lease is gated immediately even during startup.
		if err != nil && errors.Is(err, os.ErrNotExist) && now.Sub(start) < time.Duration(w.Config.StartupGraceSeconds)*time.Second {
			err = nil
		}
		if err != nil {
			var failures []error
			for _, command := range w.Config.FailClosed {
				commandCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				out, runErr := w.Runner.Run(commandCtx, command)
				cancel()
				if runErr != nil {
					failures = append(failures, fmt.Errorf("fail closed %s: %w: %s", command.Path, runErr, out))
				}
			}
			if len(failures) == 0 {
				return fmt.Errorf("serving member gated: %w", err)
			}
			// A failed gate is retried; returning success or letting a remote
			// controller handle it would allow stale local DNS to keep serving.
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
