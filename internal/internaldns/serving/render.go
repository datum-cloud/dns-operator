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

	contextdns "go.miloapis.com/dns-operator/internal/internaldns/coredns"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

type DNSDistRenderConfig struct {
	Path          string          `json:"path"`
	ListenAddress string          `json:"listenAddress"`
	ACLs          []string        `json:"acls"`
	PoolName      string          `json:"poolName"`
	Backends      []model.Backend `json:"backends,omitempty"`
}

type CoreDNSRenderConfig struct {
	Path            string   `json:"path"`
	ListenAddress   string   `json:"listenAddress"`
	Port            uint16   `json:"port"`
	ProxyPeers      []string `json:"proxyPeers"`
	PublicUpstreams []string `json:"publicUpstreams,omitempty"`
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
		cfg := contextdns.Config{ListenAddress: net.JoinHostPort(b.ListenAddress, strconv.Itoa(int(b.Port))), ProxyPeers: b.ProxyPeers, PublicUpstreams: b.PublicUpstreams}
		if err := cfg.Validate(); err != nil {
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

func renderDNSDist(c DNSDistRenderConfig, backends []model.Backend, bindings []model.Binding, active map[string]bool, cluster bool, eligible map[string]map[string]bool, zones map[string]map[string]map[string]bool) []byte {
	var out bytes.Buffer
	fmt.Fprintf(&out, "setLocal(%q)\n", c.ListenAddress)
	fmt.Fprint(&out, "setACL({")
	for i, acl := range c.ACLs {
		if i > 0 {
			fmt.Fprint(&out, ",")
		}
		fmt.Fprintf(&out, "%q", acl)
	}
	fmt.Fprint(&out, "})\nsetSecurityPollSuffix(\"\")\n\n")
	for _, b := range backends {
		fmt.Fprintf(&out, "local server = newServer({address=%q, name=%q, pool=%q, useProxyProtocol=true, tcpOnly=true, checkTCP=true, checkName=\".\", checkType=\"SOA\", mustResolve=false})\n", net.JoinHostPort(b.Address, strconv.Itoa(int(b.Port))), "b_"+model.OpaqueToken(b.MemberID), c.PoolName)
		if cluster {
			for _, binding := range bindings {
				for _, zone := range binding.Zones {
					if zones != nil && active[binding.BindingUID] && zones[binding.BindingUID][zone.ZoneUID][b.MemberID] {
						fmt.Fprintf(&out, "server:addPool(%q)\n", binding.ViewName()+"_"+model.OpaqueToken(zone.ZoneUID))
					}
				}
				if !binding.ConfigurationPending && active[binding.BindingUID] && (eligible == nil || eligible[binding.BindingUID][b.MemberID]) {
					fmt.Fprintf(&out, "server:addPool(%q)\n", binding.ViewName())
				}
			}
		}
	}
	fmt.Fprint(&out, "\nlocal bindings = {\n")
	for _, b := range bindings {
		addr := b.ConsumerAddress
		if cluster {
			addr = b.ClusterAddress
		}
		fmt.Fprintf(&out, "  [%q] = {context=%q, ready=%t},\n", addr, b.ViewName(), !b.ConfigurationPending && active[b.BindingUID])
	}
	fmt.Fprint(&out, "}\n\nfunction classifyContext(dq)\n  local binding = bindings[dq.localaddr:toString()]\n  if binding == nil then\n    return DNSAction.Refused, \"\"\n  end\n  dq:setTag(\"dns-context\", binding.context)\n  if not binding.ready then dq:setTag(\"context-unready\", \"true\") end\n  return DNSAction.None, \"\"\nend\n\n")
	fmt.Fprint(&out, "addAction(AllRule(), LuaAction(classifyContext))\n")
	fmt.Fprint(&out, "addAction(TagRule(\"context-unready\", \"true\"), RCodeAction(DNSRCode.SERVFAIL))\n")
	for _, binding := range bindings {
		attached := append([]model.ZoneAttachment(nil), binding.Zones...)
		sort.Slice(attached, func(i, j int) bool { return len(attached[i].Apex) > len(attached[j].Apex) })
		for _, zone := range attached {
			rule := fmt.Sprintf("AndRule({TagRule(\"dns-context\",%q),QNameSuffixRule(%q)})", binding.ViewName(), model.AbsoluteName(zone.Apex))
			if zone.Pending || zones != nil && len(zones[binding.BindingUID][zone.ZoneUID]) == 0 {
				fmt.Fprintf(&out, "addAction(%s,RCodeAction(DNSRCode.SERVFAIL))\n", rule)
			} else if cluster && zones != nil {
				pool := binding.ViewName() + "_" + model.OpaqueToken(zone.ZoneUID)
				fmt.Fprintf(&out, "getPool(%q)\naddAction(AndRule({%s,NotRule(PoolAvailableRule(%q))}),RCodeAction(DNSRCode.SERVFAIL))\naddAction(%s,PoolAction(%q))\n", pool, rule, pool, rule, pool)
			}
		}
	}
	if cluster {
		for _, binding := range bindings {
			fmt.Fprintf(&out, "getPool(%q)\naddAction(AndRule({TagRule(\"dns-context\",%q),NotRule(PoolAvailableRule(%q))}), RCodeAction(DNSRCode.SERVFAIL))\naddAction(TagRule(\"dns-context\",%q),PoolAction(%q))\n", binding.ViewName(), binding.ViewName(), binding.ViewName(), binding.ViewName(), binding.ViewName())
		}
		fmt.Fprint(&out, "addAction(AllRule(),RCodeAction(DNSRCode.SERVFAIL))\n")
	} else {
		fmt.Fprintf(&out, "addAction(NotRule(PoolAvailableRule(%q)), RCodeAction(DNSRCode.SERVFAIL))\naddAction(AllRule(),PoolAction(%q))\n", c.PoolName, c.PoolName)
	}
	return out.Bytes()
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
	return contextdns.Config{ListenAddress: net.JoinHostPort(c.ListenAddress, strconv.Itoa(int(c.Port))), ProxyPeers: c.ProxyPeers, PublicUpstreams: c.PublicUpstreams, Contexts: []contextdns.Context{}}
}
