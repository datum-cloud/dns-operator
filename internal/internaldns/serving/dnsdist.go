// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	if _, err := os.Stat(router.Path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary := router.Path + ".bootstrap"
	if err := writeDurable(temporary, renderDNSDist(router, router.Backends, nil, nil, false, nil, nil), 0644); err != nil {
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
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := writeDurable(temporary, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
