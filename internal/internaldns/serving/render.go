package serving

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	contextdns "go.miloapis.com/dns-operator/internal/internaldns/coredns"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

type DNSDistRenderConfig struct {
	Path            string          `json:"path"`
	ListenAddress   string          `json:"listenAddress"`
	ACLs            []string        `json:"acls"`
	PoolName        string          `json:"poolName"`
	Backends        []model.Backend `json:"backends,omitempty"`
	IdentityKeyFile string          `json:"identityKeyFile,omitempty"`
}

type CoreDNSRenderConfig struct {
	Path            string   `json:"path"`
	ListenAddress   string   `json:"listenAddress"`
	Port            uint16   `json:"port"`
	ProxyPeers      []string `json:"proxyPeers"`
	IdentityKeyFile string   `json:"identityKeyFile,omitempty"`
	PublicUpstreams []string `json:"publicUpstreams,omitempty"`
	MetricsAddress  string   `json:"metricsAddress,omitempty"`
}

type RenderRole string

const (
	RenderNode            RenderRole = "node"
	RenderCluster         RenderRole = "cluster"
	RenderBoth            RenderRole = "both"
	RenderRegionalCoreDNS RenderRole = "regional-coredns"
	RenderRegionalDNSDist RenderRole = "regional-dnsdist"
)

type RenderConfig struct {
	ReadyZoneMembers map[string]map[string]map[string]bool `json:"-"`
	ReadyMembers     map[string]map[string]bool            `json:"-"`
	Role             RenderRole                            `json:"role"`
	NodeDNSDist      DNSDistRenderConfig                   `json:"nodeDNSDist,omitempty"`
	ClusterDNSDist   DNSDistRenderConfig                   `json:"clusterDNSDist,omitempty"`
	NodeCoreDNS      CoreDNSRenderConfig                   `json:"nodeCoreDNS,omitempty"`
	ClusterCoreDNS   CoreDNSRenderConfig                   `json:"clusterCoreDNS,omitempty"`
}

type Rendered struct{ Files map[string][]byte }

func Render(c RenderConfig, snapshot model.ServingSnapshot, active map[string]bool) (Rendered, error) {
	return renderPublications(c, snapshot, active, nil)
}

func renderPublications(c RenderConfig, snapshot model.ServingSnapshot, active map[string]bool, publications map[string]publicationState) (Rendered, error) {
	if err := snapshot.Validate(); err != nil {
		return Rendered{}, err
	}
	if err := validateRenderConfig(c); err != nil {
		return Rendered{}, err
	}
	bindings := append([]model.Binding(nil), snapshot.Bindings...)
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].BindingUID < bindings[j].BindingUID })
	nodeBackends := c.NodeDNSDist.Backends
	clusterBackends := c.ClusterDNSDist.Backends
	if len(nodeBackends) == 0 {
		nodeBackends = collectBackends(bindings, func(b model.Binding) []model.Backend { return b.NodeBackends })
	}
	if len(clusterBackends) == 0 {
		clusterBackends = collectBackends(bindings, func(b model.Binding) []model.Backend { return b.ClusterBackends })
	}
	if err := validateBackends(nodeBackends); err != nil {
		return Rendered{}, fmt.Errorf("node backends: %w", err)
	}
	if err := validateBackends(clusterBackends); err != nil {
		return Rendered{}, fmt.Errorf("cluster backends: %w", err)
	}
	files := map[string][]byte{}
	role := c.Role
	if role == "" {
		role = RenderBoth
	}
	if role == RenderNode || role == RenderBoth {
		files[c.NodeDNSDist.Path] = renderDNSDist(c.NodeDNSDist, nodeBackends, bindings, active, false, nil, c.ReadyZoneMembers)
		files[c.NodeCoreDNS.Path] = renderNodeCoreDNS(c.NodeCoreDNS, bindings)
	}
	if role == RenderCluster || role == RenderBoth || role == RenderRegionalDNSDist {
		files[c.ClusterDNSDist.Path] = renderDNSDist(c.ClusterDNSDist, clusterBackends, bindings, active, true, c.ReadyMembers, c.ReadyZoneMembers)
	}
	if role == RenderCluster || role == RenderBoth || role == RenderRegionalCoreDNS {
		cluster, err := renderClusterCoreDNS(c.ClusterCoreDNS, bindings, publications, files)
		if err != nil {
			return Rendered{}, err
		}
		files[c.ClusterCoreDNS.Path] = cluster
	}
	return Rendered{Files: files}, nil
}

func validateRenderConfig(c RenderConfig) error {
	role := c.Role
	if role == "" {
		role = RenderBoth
	}
	if role != RenderNode && role != RenderCluster && role != RenderBoth && role != RenderRegionalCoreDNS && role != RenderRegionalDNSDist {
		return fmt.Errorf("unsupported render role %q", role)
	}

	dists := map[string]DNSDistRenderConfig{}
	binds := map[string]CoreDNSRenderConfig{}
	if role == RenderNode || role == RenderBoth {
		dists["node dnsdist"] = c.NodeDNSDist
		binds["node CoreDNS"] = c.NodeCoreDNS
	}
	if role == RenderCluster || role == RenderBoth || role == RenderRegionalDNSDist {
		dists["cluster dnsdist"] = c.ClusterDNSDist
	}
	if role == RenderCluster || role == RenderBoth || role == RenderRegionalCoreDNS {
		binds["cluster CoreDNS"] = c.ClusterCoreDNS
	}
	for label, d := range dists {
		if !filepath.IsAbs(d.Path) || d.PoolName == "" || model.SafeToken(d.PoolName) != d.PoolName {
			return fmt.Errorf("%s path and safe pool name are required", label)
		}
		if _, _, err := net.SplitHostPort(d.ListenAddress); err != nil {
			return fmt.Errorf("%s invalid listen address: %w", label, err)
		}
		for _, acl := range d.ACLs {
			if _, err := netip.ParsePrefix(acl); err != nil {
				return fmt.Errorf("%s invalid ACL %q", label, acl)
			}
		}
		if err := validateBackends(d.Backends); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if d.IdentityKeyFile != "" && !filepath.IsAbs(d.IdentityKeyFile) {
			return fmt.Errorf("%s identity key file must be an absolute path", label)
		}
	}
	if (role == RenderNode || role == RenderBoth) && c.NodeDNSDist.IdentityKeyFile != c.NodeCoreDNS.IdentityKeyFile {
		return errors.New("node dnsdist and node CoreDNS require the same identity key file")
	}
	if (role == RenderCluster || role == RenderBoth) && c.ClusterDNSDist.IdentityKeyFile != c.ClusterCoreDNS.IdentityKeyFile {
		return errors.New("cluster dnsdist and cluster CoreDNS require the same identity key file")
	}
	for label, b := range binds {
		if !filepath.IsAbs(b.Path) || b.Port == 0 {
			return fmt.Errorf("%s absolute path and port are required", label)
		}
		if addr, err := netip.ParseAddr(b.ListenAddress); err != nil || addr.IsUnspecified() {
			return fmt.Errorf("%s invalid listen address", label)
		}
		for _, peer := range b.ProxyPeers {
			if _, err := netip.ParseAddr(peer); err != nil {
				return fmt.Errorf("%s invalid proxy peer %q", label, peer)
			}
		}
		if b.MetricsAddress != "" && !strings.Contains(b.MetricsAddress, "$") {
			if _, err := metricsListenAddress(b.MetricsAddress); err != nil {
				return fmt.Errorf("%s metrics address: %w", label, err)
			}
		}
		if err := coreDNSConfig(b).Validate(); err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}
	return nil
}

func validateBackends(backends []model.Backend) error {
	members := map[string]string{}
	endpoints := map[string]string{}
	for _, b := range backends {
		if b.MemberID == "" || b.Port == 0 {
			return errors.New("incomplete backend")
		}
		if a, err := netip.ParseAddr(b.Address); err != nil || a.IsUnspecified() {
			return fmt.Errorf("invalid backend %q", b.Address)
		}
		endpoint := net.JoinHostPort(b.Address, strconv.Itoa(int(b.Port)))
		if _, ok := members[b.MemberID]; ok {
			return fmt.Errorf("duplicate backend member %q", b.MemberID)
		}
		if old, ok := endpoints[endpoint]; ok {
			return fmt.Errorf("backend endpoint %q belongs to both %q and %q", endpoint, old, b.MemberID)
		}
		members[b.MemberID] = endpoint
		endpoints[endpoint] = b.MemberID
	}
	return nil
}

func collectBackends(bindings []model.Binding, get func(model.Binding) []model.Backend) []model.Backend {
	seen := map[string]model.Backend{}
	for _, binding := range bindings {
		for _, b := range get(binding) {
			key := b.MemberID + "\x00" + net.JoinHostPort(b.Address, strconv.Itoa(int(b.Port)))
			seen[key] = b
		}
	}
	out := make([]model.Backend, 0, len(seen))
	for _, b := range seen {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MemberID < out[j].MemberID })
	return out
}

const dnsdistRulesBoundary = "local rules = {}\n"

func renderDNSDist(c DNSDistRenderConfig, backends []model.Backend, bindings []model.Binding, active map[string]bool, cluster bool, eligible map[string]map[string]bool, zones map[string]map[string]map[string]bool) []byte {
	var out bytes.Buffer
	endpoint := dnsdistConsole(c.Path)
	fmt.Fprint(&out, "local client = inClientStartup()\nif client or not internalDNSInitialized then\n")
	fmt.Fprintf(&out, "  local key\n  if os.getenv(\"INTERNAL_DNS_VALIDATE\") == \"1\" then key = string.rep(\"A\",43)..\"=\" else\n    local handle = assert(io.open(%q,\"r\"))\n    key = handle:read(\"*l\"); handle:close()\n  end\n  setKey(key)\n  controlSocket(%q)\n  setConsoleACL({\"127.0.0.1/32\"})\nend\nif not client then\n", DNSDistKeyPath, endpoint)
	topology, _ := json.Marshal(struct {
		Listen    string
		ACLs      []string
		Backends  []model.Backend
		Pool      string
		Transport string
	}{c.ListenAddress, c.ACLs, backends, c.PoolName, dnsdistTransport(c)})
	fingerprint := model.OpaqueToken(string(topology))
	fmt.Fprint(&out, "if not internalDNSInitialized then\n")
	fmt.Fprintf(&out, "setLocal(%q)\nsetACL({", c.ListenAddress)
	for i, acl := range c.ACLs {
		if i > 0 {
			fmt.Fprint(&out, ",")
		}
		fmt.Fprintf(&out, "%q", acl)
	}
	fmt.Fprint(&out, "})\nsetSecurityPollSuffix(\"\")\ninternalDNSServers = {}\ninternalDNSPools = {}\n")
	proxy := "useProxyProtocol=true,"
	if c.IdentityKeyFile != "" {
		proxy = ""
		fmt.Fprintf(&out, "if os.getenv(\"INTERNAL_DNS_VALIDATE\") == \"1\" then internalDNSIdentityKey = string.rep(\"A\",43)..\"=\" else\n  local handle = assert(io.open(%q,\"r\"))\n  internalDNSIdentityKey = assert(handle:read(\"*l\")):match(\"^%%s*(.-)%%s*$\"); handle:close()\nend\n", c.IdentityKeyFile)
	}
	for _, backend := range backends {
		fmt.Fprintf(&out, "internalDNSServers[%q] = newServer({address=%q,name=%q,pool=%q,%stcpOnly=true,checkTCP=true,checkName=\".\",checkType=\"SOA\",mustResolve=false,maxCheckFailures=3,rise=2})\n", backend.MemberID, net.JoinHostPort(backend.Address, strconv.Itoa(int(backend.Port))), "b_"+model.OpaqueToken(backend.MemberID), c.PoolName, proxy)
	}
	fmt.Fprintf(&out, "internalDNSTopology = %q\ninternalDNSInitialized = true\nend\nassert(internalDNSTopology == %q,\"dnsdist topology requires a replacement member\")\n", fingerprint, fingerprint)
	fmt.Fprint(&out, dnsdistRulesBoundary+"local function appendRule(rule,action) rules[#rules+1] = newRuleAction(rule,action) end\n")
	if cluster {
		pools := map[string]map[string]bool{}
		for _, binding := range bindings {
			if eligible != nil {
				pools[eligiblePool(backends, eligible[binding.BindingUID])] = eligible[binding.BindingUID]
			}
			for _, zone := range binding.Zones {
				if zones != nil {
					pools[eligiblePool(backends, zones[binding.BindingUID][zone.ZoneUID])] = zones[binding.BindingUID][zone.ZoneUID]
				}
			}
		}
		names := make([]string, 0, len(pools))
		for name := range pools {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(&out, "if not internalDNSPools[%q] then\ngetPool(%q)\n", name, name)
			for _, backend := range backends {
				if pools[name][backend.MemberID] {
					fmt.Fprintf(&out, "internalDNSServers[%q]:addPool(%q)\n", backend.MemberID, name)
				}
			}
			fmt.Fprintf(&out, "internalDNSPools[%q] = true\nend\n", name)
		}
	}
	fmt.Fprint(&out, "local function destination(address) local nmg = newNMG(); nmg:addMask(address); return NetmaskGroupRule(nmg, false, true) end\n\n")
	for _, b := range bindings {
		addr := b.ConsumerAddress
		if cluster {
			addr = b.ClusterAddress
		}
		fmt.Fprintf(&out, "appendRule(destination(%q), SetTagAction(\"dns-context\", %q))\n", addr, b.ViewName())
		if b.ConfigurationPending || !active[b.BindingUID] {
			fmt.Fprintf(&out, "appendRule(destination(%q), SetTagAction(\"context-unready\", \"true\"))\n", addr)
		}
	}
	fmt.Fprint(&out, "appendRule(NotRule(TagRule(\"dns-context\")), RCodeAction(DNSRCode.REFUSED))\n")
	fmt.Fprint(&out, "appendRule(TagRule(\"context-unready\", \"true\"), RCodeAction(DNSRCode.SERVFAIL))\n")
	if c.IdentityKeyFile != "" {
		for _, b := range bindings {
			addr := b.ConsumerAddress
			if cluster {
				addr = b.ClusterAddress
			}
			fmt.Fprintf(&out, "appendRule(destination(%q), SetEDNSOptionAction(%d, internalDNSIdentityKey..%q))\n", addr, contextdns.IdentityOption, addr)
		}
	}
	for _, binding := range bindings {
		attached := append([]model.ZoneAttachment(nil), binding.Zones...)
		sort.Slice(attached, func(i, j int) bool { return len(attached[i].Apex) > len(attached[j].Apex) })
		for _, zone := range attached {
			rule := fmt.Sprintf("AndRule({TagRule(\"dns-context\",%q),QNameSuffixRule(%q)})", binding.ViewName(), model.AbsoluteName(zone.Apex))
			if zone.Pending || zones != nil && len(zones[binding.BindingUID][zone.ZoneUID]) == 0 {
				fmt.Fprintf(&out, "appendRule(%s,RCodeAction(DNSRCode.SERVFAIL))\n", rule)
			} else if cluster && zones != nil {
				pool := eligiblePool(backends, zones[binding.BindingUID][zone.ZoneUID])
				fmt.Fprintf(&out, "getPool(%q)\nappendRule(AndRule({%s,NotRule(PoolAvailableRule(%q))}),RCodeAction(DNSRCode.SERVFAIL))\nappendRule(%s,PoolAction(%q))\n", pool, rule, pool, rule, pool)
			}
		}
	}
	if cluster {
		for _, binding := range bindings {
			pool := eligiblePool(backends, eligible[binding.BindingUID])
			if eligible == nil {
				pool = c.PoolName
			}
			fmt.Fprintf(&out, "getPool(%q)\nappendRule(AndRule({TagRule(\"dns-context\",%q),NotRule(PoolAvailableRule(%q))}), RCodeAction(DNSRCode.SERVFAIL))\nappendRule(TagRule(\"dns-context\",%q),PoolAction(%q))\n", pool, binding.ViewName(), pool, binding.ViewName(), pool)
		}
		fmt.Fprint(&out, "appendRule(AllRule(),RCodeAction(DNSRCode.SERVFAIL))\n")
	} else {
		fmt.Fprintf(&out, "appendRule(NotRule(PoolAvailableRule(%q)), RCodeAction(DNSRCode.SERVFAIL))\nappendRule(AllRule(),PoolAction(%q))\n", c.PoolName, c.PoolName)
	}
	fmt.Fprint(&out, "setRules(rules)\nend\n")
	return out.Bytes()
}

func dnsdistTransport(c DNSDistRenderConfig) string {
	if c.IdentityKeyFile != "" {
		return "identity-option-tcp"
	}
	return "proxy-v2-tcp"
}

func eligiblePool(backends []model.Backend, allowed map[string]bool) string {
	members := make([]string, 0, len(allowed))
	for _, backend := range backends {
		if allowed[backend.MemberID] {
			members = append(members, backend.MemberID)
		}
	}
	sort.Strings(members)
	return "ready_" + model.OpaqueToken(strings.Join(members, "\x00"))
}

func renderNodeCoreDNS(c CoreDNSRenderConfig, bindings []model.Binding) []byte {
	cfg := coreDNSConfig(c)
	for _, b := range bindings {
		context := contextdns.Context{ID: b.ViewName(), Destination: b.ConsumerAddress, Pending: b.ConfigurationPending, Upstreams: []string{net.JoinHostPort(b.ClusterAddress, strconv.Itoa(int(b.Port)))}}
		for _, zone := range b.Zones {
			context.Zones = append(context.Zones, contextdns.Zone{Apex: model.AbsoluteName(zone.Apex), Pending: zone.Pending})
		}
		cfg.Contexts = append(cfg.Contexts, context)
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	return data
}

func renderClusterCoreDNS(c CoreDNSRenderConfig, bindings []model.Binding, publications map[string]publicationState, _ map[string][]byte) ([]byte, error) {
	cfg := coreDNSConfig(c)
	for _, b := range bindings {
		context := contextdns.Context{ID: b.ViewName(), Destination: b.ClusterAddress, Pending: b.ConfigurationPending}
		for _, z := range b.Zones {
			zone := contextdns.Zone{Apex: model.AbsoluteName(z.Apex)}
			p, ok := publications[z.ZoneUID]
			if z.Pending || !ok || p.Manifest.Tombstone || model.Compare(p.Fence.Epoch, p.Fence.Revision, z.RequiredPublicationEpoch, z.RequiredPublicationRevision) < 0 {
				zone.Pending = true
			} else {
				data, err := renderZone(p)
				if err != nil {
					return nil, err
				}
				zone.Data = string(data)
			}
			context.Zones = append(context.Zones, zone)
		}
		cfg.Contexts = append(cfg.Contexts, context)
	}
	return json.MarshalIndent(cfg, "", "  ")
}

func coreDNSConfig(c CoreDNSRenderConfig) contextdns.Config {
	return contextdns.Config{ListenAddress: net.JoinHostPort(c.ListenAddress, strconv.Itoa(int(c.Port))), ProxyPeers: c.ProxyPeers, IdentityKeyFile: c.IdentityKeyFile, PublicUpstreams: c.PublicUpstreams, Contexts: []contextdns.Context{}}
}
