// SPDX-License-Identifier: AGPL-3.0-only

package watchdog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/serving"
)

type recordingRunner struct{ calls int }

func (r *recordingRunner) Run(context.Context, serving.Command) ([]byte, error) {
	r.calls++
	return nil, nil
}

func TestDeadOrForgedAgentIsGatedWithoutRemoteState(t *testing.T) {
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		lease Lease
		valid bool
	}{
		{"fresh", Lease{MemberID: "auth-1", ValidUntil: now.Add(5 * time.Second)}, true},
		{"expired", Lease{MemberID: "auth-1", ValidUntil: now.Add(-time.Second)}, false},
		{"another member", Lease{MemberID: "auth-2", ValidUntil: now.Add(time.Second)}, false},
		{"overlong lease", Lease{MemberID: "auth-1", ValidUntil: now.Add(time.Minute)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lease.json")
			b, _ := json.Marshal(tc.lease)
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			w := Watchdog{Config: Config{LeasePath: path, MemberID: "auth-1", FailClosed: []serving.Command{{Path: "stop-private-dns"}}}, Now: func() time.Time { return now }}
			if got := w.Check(now) == nil; got != tc.valid {
				t.Fatalf("valid=%v want=%v", got, tc.valid)
			}
			if !tc.valid {
				runner := &recordingRunner{}
				w.Runner = runner
				if err := w.Start(context.Background()); err == nil || runner.calls != 1 {
					t.Fatalf("dead agent was not gated: calls=%d err=%v", runner.calls, err)
				}
			}
		})
	}
}

func TestExpiredCheckpointCannotUseStartupGrace(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "lease.json")
	b, _ := json.Marshal(Lease{MemberID: "auth-1", ValidUntil: now.Add(-time.Second)})
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	w := Watchdog{Config: Config{LeasePath: path, MemberID: "auth-1", StartupGraceSeconds: 60, FailClosed: []serving.Command{{Path: "stop"}}}, Runner: runner, Now: func() time.Time { return now }}
	if err := w.Start(context.Background()); err == nil || runner.calls != 1 {
		t.Fatal("expired checkpoint survived restart")
	}
}

func TestCheckRereadsTransientPartialLease(t *testing.T) {
	now := time.Now().UTC()
	valid, err := json.Marshal(Lease{MemberID: "auth-1", ValidUntil: now.Add(5 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	w := Watchdog{
		Config: Config{LeasePath: "/lease.json", MemberID: "auth-1", MaxLeaseSeconds: 5},
		Now:    func() time.Time { return now },
		ReadFile: func(string) ([]byte, error) {
			reads++
			if reads == 1 {
				return []byte{}, nil
			}
			return valid, nil
		},
	}
	if err := w.Check(now); err != nil {
		t.Fatalf("transient partial lease was gated: %v", err)
	}
	if reads != 2 {
		t.Fatalf("lease reads = %d, want 2", reads)
	}
}

func TestCheckRetriesOnlyReadAndDecodeFailures(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		name  string
		lease Lease
	}{
		{name: "wrong identity", lease: Lease{MemberID: "other", ValidUntil: now.Add(time.Second)}},
		{name: "expired", lease: Lease{MemberID: "auth-1", ValidUntil: now.Add(-time.Second)}},
		{name: "overlong", lease: Lease{MemberID: "auth-1", ValidUntil: now.Add(6 * time.Second)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal(test.lease)
			if err != nil {
				t.Fatal(err)
			}
			reads := 0
			w := Watchdog{
				Config: Config{LeasePath: "/lease.json", MemberID: "auth-1", MaxLeaseSeconds: 5},
				Now:    func() time.Time { return now },
				ReadFile: func(string) ([]byte, error) {
					reads++
					return payload, nil
				},
			}
			if err := w.Check(now); err == nil {
				t.Fatal("unsafe decoded lease was accepted")
			}
			if reads != 1 {
				t.Fatalf("unsafe decoded lease was retried %d times", reads)
			}
		})
	}

	for _, failure := range []struct {
		name string
		read func(string) ([]byte, error)
	}{
		{name: "read", read: func(string) ([]byte, error) { return nil, errors.New("short read") }},
		{name: "decode", read: func(string) ([]byte, error) { return []byte("{"), nil }},
	} {
		t.Run("persistent "+failure.name, func(t *testing.T) {
			reads := 0
			w := Watchdog{
				Config: Config{LeasePath: "/lease.json", MemberID: "auth-1", MaxLeaseSeconds: 5},
				Now:    func() time.Time { return now },
				ReadFile: func(path string) ([]byte, error) {
					reads++
					return failure.read(path)
				},
			}
			if err := w.Check(now); err == nil {
				t.Fatalf("persistent %s failure was accepted", failure.name)
			}
			if reads != 3 {
				t.Fatalf("persistent %s failure reads = %d, want 3", failure.name, reads)
			}
		})
	}
}
