// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/sys/unix"
)

const activationMarkerName = "activation.pending"

func activationMarkerPath(stateDir string) string {
	return filepath.Join(stateDir, activationMarkerName)
}

func activationPending(stateDir string) bool {
	_, err := os.Stat(activationMarkerPath(stateDir))
	return !errors.Is(err, os.ErrNotExist)
}

// Measured from the first write of a pending marker, so back-to-back updates
// that never commit cannot keep a member ready.
const activationInstallGrace = time.Minute

// activationOwned reports whether a live installer holds the transaction lock.
// The serving supervisor takes the lock briefly on every pass, so ownership is
// only concluded when the lock stays unavailable across several attempts.
func activationOwned(stateDir string) bool {
	lock, err := os.Open(activationMarkerPath(stateDir) + ".lock")
	if err != nil {
		return false
	}
	defer func() { _ = lock.Close() }()
	for attempt := 0; attempt < 5; attempt++ {
		err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
			return false
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

func routineInstallation(stateDir string, info os.FileInfo, now time.Time) error {
	if !activationOwned(stateDir) {
		return errors.New("configuration installation is incomplete")
	}
	since := info.ModTime()
	if state, err := readActivation(activationMarkerPath(stateDir)); err == nil && !state.Since.IsZero() {
		since = state.Since
	}
	if age := now.Sub(since); age > activationInstallGrace {
		return fmt.Errorf("configuration installation has not finished after %s", age.Truncate(time.Second))
	}
	return nil
}

// installationReadiness reports whether a routine installation is in
// progress. Each marker write is a new file, so a marker that changed while
// its owner was checked belongs to a later write and is checked again; an
// abandoned marker never changes.
func installationReadiness(stateDir string, now time.Time) (bool, error) {
	marker := activationMarkerPath(stateDir)
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		before, statErr := os.Stat(marker)
		if statErr != nil {
			return false, nil
		}
		if err = routineInstallation(stateDir, before, now); err == nil {
			return true, nil
		}
		after, statErr := os.Stat(marker)
		if statErr != nil {
			return false, nil
		}
		if os.SameFile(before, after) {
			return true, err
		}
	}
	return true, err
}

func installedFilesMatch(files map[string][]byte) bool {
	if len(files) == 0 {
		return false
	}
	for path, expected := range files {
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, expected) {
			return false
		}
	}
	return true
}

// approvedFilesInstalled accepts a dnsdist file whose process settings were
// refreshed at startup, provided its routing rules are the approved ones.
func approvedFilesInstalled(files map[string][]byte) bool {
	if len(files) == 0 {
		return false
	}
	for path, expected := range files {
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, expected) && !sameDNSDistRules(actual, expected) {
			return false
		}
	}
	return true
}

func sameDNSDistRules(a, b []byte) bool {
	boundary := []byte(dnsdistRulesBoundary)
	_, rulesA, okA := bytes.Cut(a, boundary)
	_, rulesB, okB := bytes.Cut(b, boundary)
	return okA && okB && bytes.Equal(rulesA, rulesB)
}

// Readiness verifies the approved files and live DNS processes using local state.
// It does not require the configuration agent or state delivery to be available.
// While a live agent installs an update, the daemons keep serving and the
// installed files are expected to differ from the approved checkpoint. An
// abandoned or gated activation is never ready.
func Readiness(ctx context.Context, config Config) error {
	installing, err := installationReadiness(config.StateDir, time.Now())
	if err != nil {
		return err
	}
	state, err := (stateStore{path: filepath.Join(config.StateDir, "checkpoint.json")}).Load()
	if err != nil {
		return err
	}
	if state.Active == nil {
		return errors.New("no approved configuration is active")
	}
	if !installing && !installedFilesMatch(state.Active.Files) {
		if installing, err = installationReadiness(config.StateDir, time.Now()); err != nil {
			return err
		}
		if !installing {
			return errors.New("installed configuration does not match the approved checkpoint")
		}
	}
	if err := state.Active.Snapshot.Validate(); err != nil {
		return fmt.Errorf("approved snapshot: %w", err)
	}
	if config.UnsafeAllowNoDNSVerify {
		return errors.New("readiness requires live DNS query verification")
	}
	return (&Agent{cfg: config}).probeLocalDaemons(ctx)
}

func (a *Agent) probeLocalDaemons(ctx context.Context) error {
	if a.cfg.UnsafeAllowNoDNSVerify {
		return nil
	}
	role := a.cfg.Render.Role
	if role == "" {
		role = RenderBoth
	}
	var binds []CoreDNSRenderConfig
	var dists []DNSDistRenderConfig
	if role == RenderNode || role == RenderBoth {
		binds = append(binds, a.cfg.Render.NodeCoreDNS)
		dists = append(dists, a.cfg.Render.NodeDNSDist)
	}
	if role == RenderCluster || role == RenderBoth || role == RenderRegionalCoreDNS {
		binds = append(binds, a.cfg.Render.ClusterCoreDNS)
	}
	if role == RenderCluster || role == RenderBoth || role == RenderRegionalDNSDist {
		dists = append(dists, a.cfg.Render.ClusterDNSDist)
	}
	for _, bind := range binds {
		server := net.JoinHostPort(bind.ListenAddress, strconv.Itoa(int(bind.Port)))
		view := PublicationView{DestinationAddress: bind.ListenAddress, DestinationPort: bind.Port}
		answer, err := exchangePublication(ctx, networkTCP, server, ".", view, 200*time.Millisecond)
		if err := servingAnswer(answer, err); err != nil {
			return fmt.Errorf("local CoreDNS health %s: %w", server, err)
		}
		if bind.IdentityKeyFile != "" {
			if err := probeIdentityTransport(ctx, server); err != nil {
				return fmt.Errorf("local CoreDNS identity transport %s: %w", server, err)
			}
		}
	}
	for _, dist := range dists {
		for _, network := range []string{networkUDP, networkTCP} {
			answer, err := exchangeResolverSOA(ctx, network, dist.ListenAddress, ".", 200*time.Millisecond)
			if err := servingAnswer(answer, err); err != nil {
				return fmt.Errorf("local dnsdist health %s: %w", dist.ListenAddress, err)
			}
		}
	}
	return nil
}

// servingAnswer rejects SERVFAIL: CoreDNS without loaded state and a gated
// dnsdist both answer every query that way.
func servingAnswer(answer *dns.Msg, err error) error {
	if err != nil || answer == nil || !answer.Response {
		return errors.Join(err, errors.New("DNS response required"))
	}
	if answer.Rcode == dns.RcodeServerFailure {
		return errors.New("daemon answered SERVFAIL")
	}
	return nil
}

// dnsdist reaches a keyed CoreDNS without a PROXY header, which a CoreDNS
// running without the key refuses at the connection.
func probeIdentityTransport(ctx context.Context, server string) error {
	query := new(dns.Msg)
	query.SetQuestion(".", dns.TypeSOA)
	answer, _, err := (&dns.Client{Net: networkTCP, Timeout: 200 * time.Millisecond}).ExchangeContext(ctx, query, server)
	return servingAnswer(answer, err)
}
