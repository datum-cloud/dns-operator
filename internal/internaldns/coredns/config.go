// SPDX-License-Identifier: AGPL-3.0-only

package coredns

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/coredns/coredns/plugin/file"
	"github.com/miekg/dns"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

type Config struct {
	ListenAddress   string    `json:"listenAddress"`
	ProxyPeers      []string  `json:"proxyPeers"`
	IdentityKeyFile string    `json:"identityKeyFile,omitempty"`
	PublicUpstreams []string  `json:"publicUpstreams,omitempty"`
	Contexts        []Context `json:"contexts"`
}

type Context struct {
	ID          string   `json:"id"`
	Destination string   `json:"destination"`
	Pending     bool     `json:"pending,omitempty"`
	Upstreams   []string `json:"upstreams,omitempty"`
	Zones       []Zone   `json:"zones,omitempty"`
}

type Zone struct {
	Apex    string `json:"apex"`
	Pending bool   `json:"pending,omitempty"`
	Data    string `json:"data,omitempty"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return Decode(data)
}

func Decode(data []byte) (Config, error) {
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return Config{}, err
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if err := endpoint(c.ListenAddress); err != nil {
		return fmt.Errorf("CoreDNS listener: %w", err)
	}
	if len(c.ProxyPeers) == 0 {
		return errors.New("CoreDNS requires trusted proxy peers")
	}
	for _, peer := range c.ProxyPeers {
		if _, err := netip.ParseAddr(peer); err != nil {
			return fmt.Errorf("CoreDNS proxy peer: %w", err)
		}
	}
	if c.IdentityKeyFile != "" && !filepath.IsAbs(c.IdentityKeyFile) {
		return errors.New("CoreDNS identity key file must be an absolute path")
	}
	for _, upstream := range c.PublicUpstreams {
		if err := publicEndpoint(upstream); err != nil {
			return fmt.Errorf("CoreDNS public upstream: %w", err)
		}
	}
	seen := map[string]bool{}
	listenerHost, _, _ := net.SplitHostPort(c.ListenAddress)
	for _, context := range c.Contexts {
		destination, err := netip.ParseAddr(context.Destination)
		if err != nil {
			return err
		}
		key := destination.Unmap().String()
		if key == netip.MustParseAddr(listenerHost).Unmap().String() {
			return errors.New("CoreDNS management listener cannot identify a tenant context")
		}
		if context.ID == "" || seen[key] {
			return errors.New("CoreDNS contexts require an identity and a unique destination")
		}
		seen[key] = true
		for _, upstream := range context.Upstreams {
			if err := endpoint(upstream); err != nil {
				return err
			}
		}
		apices := map[string]bool{}
		for _, zone := range context.Zones {
			apex := model.AbsoluteName(zone.Apex)
			if apex == "." || apices[apex] {
				return errors.New("CoreDNS context zones require unique private apices")
			}
			apices[apex] = true
			if zone.Pending || len(context.Upstreams) > 0 {
				continue
			}
			if _, err := file.Parse(strings.NewReader(zone.Data), apex, "publication", -1); err != nil {
				return fmt.Errorf("CoreDNS zone %s: %w", apex, err)
			}
		}
	}
	return nil
}

func publicEndpoint(value string) error {
	if err := endpoint(value); err == nil {
		return nil
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return err
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return endpoint(value)
	}
	_, valid := dns.IsDomainName(host)
	portNumber, portErr := strconv.ParseUint(port, 10, 16)
	if !valid || host == "" || host == "." || portErr != nil || portNumber == 0 {
		return fmt.Errorf("public DNS upstream requires a hostname or IP and port: %s", value)
	}
	return nil
}

func endpoint(value string) error {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return err
	}
	addr, err := netip.ParseAddr(host)
	portNumber, portErr := strconv.ParseUint(port, 10, 16)
	if err != nil || addr.IsUnspecified() || portErr != nil || portNumber == 0 {
		return fmt.Errorf("concrete IP address and port required: %s", value)
	}
	return nil
}
