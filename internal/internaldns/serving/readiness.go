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
)

const activationMarkerName = "activation.pending"

func activationMarkerPath(stateDir string) string {
	return filepath.Join(stateDir, activationMarkerName)
}

func activationPending(stateDir string) bool {
	_, err := os.Stat(activationMarkerPath(stateDir))
	return !errors.Is(err, os.ErrNotExist)
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

// Readiness verifies the approved files and live DNS processes using local state.
// It does not require the configuration agent or state delivery to be available.
func Readiness(ctx context.Context, config Config) error {
	if activationPending(config.StateDir) {
		return errors.New("configuration installation is incomplete")
	}
	state, err := (stateStore{path: filepath.Join(config.StateDir, "checkpoint.json")}).Load()
	if err != nil {
		return err
	}
	if state.Active == nil || !installedFilesMatch(state.Active.Files) {
		return errors.New("installed configuration does not match the approved checkpoint")
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
		if err != nil || answer == nil || !answer.Response {
			return fmt.Errorf("local CoreDNS health %s: %w", server, errors.Join(err, errors.New("DNS response required")))
		}
	}
	for _, dist := range dists {
		for _, network := range []string{networkUDP, networkTCP} {
			answer, err := exchangeResolverSOA(ctx, network, dist.ListenAddress, ".", 200*time.Millisecond)
			if err != nil || answer == nil || !answer.Response {
				return fmt.Errorf("local dnsdist health %s: %w", dist.ListenAddress, errors.Join(err, errors.New("DNS response required")))
			}
		}
	}
	return nil
}
