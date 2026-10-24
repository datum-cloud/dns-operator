// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	contextdns "go.miloapis.com/dns-operator/internal/internaldns/coredns"
)

const DNSDistKeyPath = "/run/internal-dns/dnsdist.key"
const DNSDistImage = "powerdns/dnsdist-20:2.0.10@sha256:24ac720af997aff6b98cb5b50d748754f815428e07b5368dfb0eccd5aa8d07e8"

func dnsdistConsole(path string) string {
	if strings.Contains(filepath.Base(path), "node") {
		return "127.0.0.1:5199"
	}
	return "127.0.0.1:5299"
}

func PrepareDNSDist(config RenderConfig) error {
	if err := validateRenderConfig(config); err != nil {
		return err
	}
	var routers []DNSDistRenderConfig
	switch config.Role {
	case RenderNode:
		routers = append(routers, config.NodeDNSDist)
	case RenderRegionalDNSDist:
		routers = append(routers, config.ClusterDNSDist)
	case RenderRegionalCoreDNS:
		return nil
	default:
		return fmt.Errorf("unsupported Kubernetes fleet role %q", config.Role)
	}
	if err := prepareDNSDistKey(DNSDistKeyPath); err != nil {
		return err
	}
	for _, router := range routers {
		if router.IdentityKeyFile != "" {
			if err := prepareIdentityKey(router.IdentityKeyFile, config.Role == RenderNode); err != nil {
				return err
			}
		}
		if err := prepareDNSDistRouter(router); err != nil {
			return err
		}
	}
	return nil
}

func prepareDNSDistRouter(router DNSDistRenderConfig) error {
	if len(router.Backends) == 0 {
		return errors.New("dnsdist startup requires fixed shared backends")
	}
	if err := os.MkdirAll(filepath.Dir(router.Path), 0750); err != nil {
		return err
	}
	startup := renderDNSDist(router, router.Backends, nil, nil, false, nil, nil)
	retained, err := os.ReadFile(router.Path)
	if err == nil {
		startup, err = refreshDNSDistConfiguration(router, retained)
		if err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary := router.Path + ".bootstrap"
	if err := writeDurable(temporary, startup, 0644); err != nil {
		return err
	}
	if err := os.Rename(temporary, router.Path); err != nil {
		return err
	}
	if err := syncDirs([]string{router.Path}); err != nil {
		return err
	}
	return nil
}

// Refresh immutable process settings while retaining accepted tenant rules.
func refreshDNSDistConfiguration(router DNSDistRenderConfig, retained []byte) ([]byte, error) {
	boundary := []byte(dnsdistRulesBoundary)
	retainedHeader, rules, ok := bytes.Cut(retained, boundary)
	if !ok || !bytes.HasSuffix(rules, []byte("setRules(rules)\nend\n")) {
		return nil, errors.New("retained dnsdist configuration has no complete rules section")
	}
	startup := renderDNSDist(router, router.Backends, nil, nil, false, nil, nil)
	header, _, _ := bytes.Cut(startup, boundary)
	if dnsdistTopology(retainedHeader) != dnsdistTopology(header) {
		return startup, nil
	}
	rules, ok = withDNSDistReadinessCanary(router, rules)
	if !ok {
		return startup, nil
	}
	return append(append(header, boundary...), rules...), nil
}

func prepareDNSDistKey(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err == nil {
		key, decodeErr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if decodeErr != nil || len(key) != 32 {
			return errors.New("invalid dnsdist control key")
		}
		return os.Chmod(path, 0640)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeRandomKey(path)
}

func prepareIdentityKey(path string, generate bool) error {
	_, err := contextdns.LoadIdentityKey(path)
	if generate && errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
			return err
		}
		return writeRandomKey(path)
	}
	if err != nil {
		return fmt.Errorf("identity key: %w", err)
	}
	return nil
}

func writeRandomKey(path string) error {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := writeDurable(temporary, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0640); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func dnsdistTopology(header []byte) string {
	_, rest, _ := bytes.Cut(header, []byte("internalDNSTopology = "))
	value, _, _ := bytes.Cut(rest, []byte("\n"))
	return string(value)
}
