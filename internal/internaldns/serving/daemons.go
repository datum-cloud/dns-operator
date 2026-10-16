// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// RunDaemons owns query processes independently of the configuration agent.
// Only incomplete or abandoned activation blocks their lifecycle.
func RunDaemons(ctx context.Context, config Config) error {
	if err := os.MkdirAll(config.StateDir, 0o750); err != nil {
		return err
	}
	var processes []*daemonProcess
	addBind := func(bind CoreDNSRenderConfig) {
		processes = append(processes, &daemonProcess{name: "coredns", config: bind.Path, args: []string{"-config", bind.Path}})
	}
	addRouter := func(router DNSDistRenderConfig) {
		processes = append(processes, &daemonProcess{name: "dnsdist", config: router.Path, args: []string{"--supervised", "-C", router.Path}})
	}
	switch config.Render.Role {
	case RenderNode:
		addBind(config.Render.NodeCoreDNS)
		addRouter(config.Render.NodeDNSDist)
	case RenderRegionalCoreDNS:
		addBind(config.Render.ClusterCoreDNS)
	case RenderRegionalDNSDist:
		addRouter(config.Render.ClusterDNSDist)
	case RenderCluster:
		addBind(config.Render.ClusterCoreDNS)
		addRouter(config.Render.ClusterDNSDist)
	default:
		return fmt.Errorf("unsupported serving process role %q", config.Render.Role)
	}
	defer func() {
		for _, p := range processes {
			_ = p.stop()
		}
	}()
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
			if _, err := os.Stat(filepath.Join("/run/internal-dns", "allow-"+p.name)); err != nil {
				continue
			}
			if !activationPending(config.StateDir) {
				state, err := (stateStore{path: filepath.Join(config.StateDir, "checkpoint.json")}).Load()
				if err != nil || state.Active == nil || !installedFilesMatch(state.Active.Files) {
					continue
				}
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
