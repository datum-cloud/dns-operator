// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type daemonProcess struct {
	name   string
	config string
	args   []string
	cmd    *exec.Cmd
	done   chan error
}

func (p *daemonProcess) start() error {
	p.cmd = exec.Command(p.name, p.args...)
	p.cmd.Stdout, p.cmd.Stderr = os.Stdout, os.Stderr
	if err := p.cmd.Start(); err != nil {
		p.cmd = nil
		return err
	}
	p.done = make(chan error, 1)
	go func(cmd *exec.Cmd, done chan error) { done <- cmd.Wait() }(p.cmd, p.done)
	return nil
}

func (p *daemonProcess) stop() error {
	if p.cmd == nil {
		return nil
	}
	_ = p.cmd.Process.Signal(unix.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(time.Second):
		_ = p.cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(time.Second):
			return fmt.Errorf("DNS process %s did not stop", p.name)
		}
	}
	p.cmd, p.done = nil, nil
	return nil
}

const daemonRuntimeDir = "/run/internal-dns"

// A gate persists as an activation marker, so only an approved, fully
// installed configuration with no activation pending regains start permission.
func restoreDaemonPermissions(stateDir, runtimeDir string, names []string) error {
	start, _, release := daemonPermission(stateDir)
	defer release()
	if !start || activationPending(stateDir) || !approvedConfigurationInstalled(stateDir) {
		return nil
	}
	for _, name := range names {
		if err := writeDurable(filepath.Join(runtimeDir, "allow-"+name), nil, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func approvedConfigurationInstalled(stateDir string) bool {
	state, err := (stateStore{path: filepath.Join(stateDir, "checkpoint.json")}).Load()
	return err == nil && state.Active != nil && approvedFilesInstalled(state.Active.Files)
}

// RunDaemons owns query processes independently of the configuration agent.
// Only incomplete or abandoned activation blocks their lifecycle.
func RunDaemons(ctx context.Context, config Config) error {
	if err := os.MkdirAll(config.StateDir, 0o750); err != nil {
		return err
	}
	var processes []*daemonProcess
	var bindErr error
	addBind := func(bind CoreDNSRenderConfig) {
		args, err := coreDNSArgs(bind)
		bindErr = err
		processes = append(processes, &daemonProcess{name: "coredns", config: bind.Path, args: args})
	}

	switch config.Render.Role {
	case RenderNode:
		addBind(config.Render.NodeCoreDNS)
	case RenderRegionalCoreDNS:
		addBind(config.Render.ClusterCoreDNS)
	case RenderCluster:
		addBind(config.Render.ClusterCoreDNS)
	default:
		return fmt.Errorf("unsupported serving process role %q", config.Render.Role)
	}
	if bindErr != nil {
		return bindErr
	}
	defer func() {
		for _, p := range processes {
			_ = p.stop()
		}
	}()
	names := make([]string, 0, len(processes))
	for _, p := range processes {
		names = append(names, p.name)
	}
	if err := restoreDaemonPermissions(config.StateDir, daemonRuntimeDir, names); err != nil {
		fmt.Fprintf(os.Stderr, "restore approved DNS process permission: %v\n", err)
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		start, retain, release := daemonPermission(config.StateDir)
		for _, p := range processes {
			if p.cmd != nil {
				select {
				case <-p.done:
					p.cmd, p.done = nil, nil
				default:
				}
			}
			if !retain {
				if err := p.stop(); err != nil {
					release()
					return err
				}
				continue
			}
			if p.cmd != nil || !start {
				continue
			}
			if _, err := os.Stat(filepath.Join(daemonRuntimeDir, "allow-"+p.name)); err != nil {
				continue
			}
			if !activationPending(config.StateDir) && !approvedConfigurationInstalled(config.StateDir) {
				continue
			}
			if err := p.start(); err != nil && !errors.Is(err, os.ErrNotExist) {
				release()
				return err
			}
		}
		release()
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func coreDNSArgs(bind CoreDNSRenderConfig) ([]string, error) {
	args := []string{"-config", bind.Path, "-identity-key-file=" + bind.IdentityKeyFile}
	if bind.MetricsAddress == "" {
		return args, nil
	}
	address, err := metricsListenAddress(os.ExpandEnv(bind.MetricsAddress))
	if err != nil {
		return nil, fmt.Errorf("CoreDNS metrics address %q: %w", bind.MetricsAddress, err)
	}
	return append(args, "-metrics-address", address), nil
}

// metricsListenAddress accepts an IP literal and port, with or without
// brackets, so an expanded IPv6 pod address needs none. An unspecified
// address would also listen on the producer network.
func metricsListenAddress(address string) (string, error) {
	i := strings.LastIndex(address, ":")
	if i < 0 {
		return "", fmt.Errorf("%q is not address:port", address)
	}
	ip, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(address[:i], "["), "]"))
	ip = ip.Unmap()
	if err != nil || ip.IsUnspecified() {
		return "", fmt.Errorf("%q must name the pod's IP address", address)
	}
	if port, err := strconv.ParseUint(address[i+1:], 10, 16); err != nil || port == 0 {
		return "", fmt.Errorf("%q has an invalid port", address)
	}
	return net.JoinHostPort(ip.String(), address[i+1:]), nil
}
