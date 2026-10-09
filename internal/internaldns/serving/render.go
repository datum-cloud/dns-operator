package serving

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

type DNSDistRenderConfig struct {
	Path          string          `json:"path"`
	ListenAddress string          `json:"listenAddress"`
	ACLs          []string        `json:"acls"`
	PoolName      string          `json:"poolName"`
	Backends      []model.Backend `json:"backends,omitempty"`
}

type BINDRenderConfig struct {
	Path                       string   `json:"path"`
	ListenAddress              string   `json:"listenAddress"`
	Port                       uint16   `json:"port"`
	ProxyPeers                 []string `json:"proxyPeers"`
	DefaultCacheSize           string   `json:"defaultCacheSize,omitempty"`
	MaxNegativeCacheTTLSeconds uint32   `json:"maxNegativeCacheTTLSeconds,omitempty"`
	DNSSECValidation           string   `json:"dnssecValidation,omitempty"`
	RNDCIncludePath            string   `json:"rndcIncludePath,omitempty"`
	ControlAddress             string   `json:"controlAddress,omitempty"`
	ControlPort                uint16   `json:"controlPort,omitempty"`
	ControlKeyName             string   `json:"controlKeyName,omitempty"`
}

type RenderRole string

const (
	RenderNode            RenderRole = "node"
	RenderCluster         RenderRole = "cluster"
	RenderBoth            RenderRole = "both"
	RenderRegionalBIND    RenderRole = "regional-bind"
	RenderRegionalDNSDist RenderRole = "regional-dnsdist"
)

type RenderConfig struct {
	ReadyMembers   map[string]map[string]bool `json:"-"`
	Role           RenderRole                 `json:"role"`
	NodeDNSDist    DNSDistRenderConfig        `json:"nodeDNSDist,omitempty"`
	ClusterDNSDist DNSDistRenderConfig        `json:"clusterDNSDist,omitempty"`
	NodeBIND       BINDRenderConfig           `json:"nodeBIND,omitempty"`
	ClusterBIND    BINDRenderConfig           `json:"clusterBIND,omitempty"`
}

type Rendered struct{ Files map[string][]byte }

func Render(c RenderConfig, snapshot model.ServingSnapshot, active map[string]bool) (Rendered, error) {
	return renderPublications(c, snapshot, active, nil, time.Now())
}

func renderPublications(c RenderConfig, snapshot model.ServingSnapshot, active map[string]bool, publications map[string]publicationState, now time.Time) (Rendered, error) {
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
		files[c.NodeDNSDist.Path] = renderDNSDist(c.NodeDNSDist, nodeBackends, bindings, active, false, nil)
		files[c.NodeBIND.Path] = renderNodeBIND(c.NodeBIND, bindings)
	}
	if role == RenderCluster || role == RenderBoth || role == RenderRegionalDNSDist {
		files[c.ClusterDNSDist.Path] = renderDNSDist(c.ClusterDNSDist, clusterBackends, bindings, active, true, c.ReadyMembers)
	}
	if role == RenderCluster || role == RenderBoth || role == RenderRegionalBIND {
		cluster, err := renderClusterBIND(c.ClusterBIND, bindings, publications, now, files)
		if err != nil {
			return Rendered{}, err
		}
		files[c.ClusterBIND.Path] = cluster
	}
	return Rendered{Files: files}, nil
}

func validateRenderConfig(c RenderConfig) error {
	role := c.Role
	if role == "" {
		role = RenderBoth
	}
	if role != RenderNode && role != RenderCluster && role != RenderBoth && role != RenderRegionalBIND && role != RenderRegionalDNSDist {
		return fmt.Errorf("unsupported render role %q", role)
	}

	dists := map[string]DNSDistRenderConfig{}
	binds := map[string]BINDRenderConfig{}
	if role == RenderNode || role == RenderBoth {
		dists["node dnsdist"] = c.NodeDNSDist
		binds["node BIND"] = c.NodeBIND
	}
	if role == RenderCluster || role == RenderBoth || role == RenderRegionalDNSDist {
		dists["cluster dnsdist"] = c.ClusterDNSDist
	}
	if role == RenderCluster || role == RenderBoth || role == RenderRegionalBIND {
		binds["cluster BIND"] = c.ClusterBIND
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
		if b.DefaultCacheSize == "" {
			b.DefaultCacheSize = "16M"
		}
		if !safeNamedAtom(b.DefaultCacheSize) {
			return fmt.Errorf("%s unsafe cache size", label)
		}
		if b.MaxNegativeCacheTTLSeconds > 60 {
			return fmt.Errorf("%s maximum negative cache TTL may not exceed 60 seconds", label)
		}
		if b.DNSSECValidation != "" && b.DNSSECValidation != "auto" && b.DNSSECValidation != "yes" && b.DNSSECValidation != "no" {
			return fmt.Errorf("%s invalid DNSSEC validation mode", label)
		}
		if b.RNDCIncludePath != "" && (!filepath.IsAbs(b.RNDCIncludePath) || strings.ContainsAny(b.RNDCIncludePath, "\"\n\r")) {
			return fmt.Errorf("%s unsafe RNDC include", label)
		}
		if b.ControlAddress != "" {
			if _, err := netip.ParseAddr(b.ControlAddress); err != nil || b.ControlPort == 0 || !safeNamedAtom(b.ControlKeyName) {
				return fmt.Errorf("%s invalid controls", label)
			}
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

func renderDNSDist(c DNSDistRenderConfig, backends []model.Backend, bindings []model.Binding, active map[string]bool, cluster bool, eligible map[string]map[string]bool) []byte {
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
		fmt.Fprintf(&out, "local server = newServer({address=%q, name=%q, pool=%q, useProxyProtocol=true, checkName=\".\", checkType=\"SOA\", mustResolve=false})\n", net.JoinHostPort(b.Address, strconv.Itoa(int(b.Port))), "b_"+model.OpaqueToken(b.MemberID), c.PoolName)
		if cluster {
			for _, binding := range bindings {
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
		fmt.Fprintf(&out, "  [%q] = {context=%q, validUntil=%d, ready=%t},\n", addr, b.ViewName(), b.Authorization.ValidUntil.Unix(), !b.ConfigurationPending && active[b.BindingUID])
	}
	fmt.Fprint(&out, "}\n\nfunction classifyVPC(dq)\n  local binding = bindings[dq.localaddr:toString()]\n  if binding == nil or os.time() >= binding.validUntil then\n    return DNSAction.Refused, \"\"\n  end\n  dq:setTag(\"vpc-context\", binding.context)\n  if not binding.ready then dq:setTag(\"context-unready\", \"true\") end\n  return DNSAction.None, \"\"\nend\n\n")
	fmt.Fprint(&out, "addAction(AllRule(), LuaAction(classifyVPC))\n")
	fmt.Fprint(&out, "addAction(TagRule(\"context-unready\", \"true\"), RCodeAction(DNSRCode.SERVFAIL))\n")
	if cluster {
		for _, binding := range bindings {
			fmt.Fprintf(&out, "getPool(%q)\naddAction(AndRule({TagRule(\"vpc-context\",%q),NotRule(PoolAvailableRule(%q))}), RCodeAction(DNSRCode.SERVFAIL))\naddAction(TagRule(\"vpc-context\",%q),PoolAction(%q))\n", binding.ViewName(), binding.ViewName(), binding.ViewName(), binding.ViewName(), binding.ViewName())
		}
		fmt.Fprint(&out, "addAction(AllRule(),RCodeAction(DNSRCode.SERVFAIL))\n")
	} else {
		fmt.Fprintf(&out, "addAction(NotRule(PoolAvailableRule(%q)), RCodeAction(DNSRCode.SERVFAIL))\naddAction(AllRule(),PoolAction(%q))\n", c.PoolName, c.PoolName)
	}
	return out.Bytes()
}

func renderOptions(out *bytes.Buffer, c BINDRenderConfig) {
	fmt.Fprint(out, "options {\n  directory \"/tmp\";\n")
	directive := "listen-on"
	if netip.MustParseAddr(c.ListenAddress).Is6() {
		directive = "listen-on-v6"
	}
	fmt.Fprintf(out, "  %s port %d proxy plain { %s; };\n", directive, c.Port, c.ListenAddress)
	if directive == "listen-on" {
		fmt.Fprint(out, "  listen-on-v6 { none; };\n")
	} else {
		fmt.Fprint(out, "  listen-on { none; };\n")
	}
	fmt.Fprint(out, "  allow-proxy { ")
	for _, p := range c.ProxyPeers {
		fmt.Fprintf(out, "%s; ", p)
	}
	fmt.Fprint(out, "};\n")
	fmt.Fprintf(out, "  allow-proxy-on { %s; };\n", c.ListenAddress)
	dnssec := c.DNSSECValidation
	if dnssec == "" {
		dnssec = "auto"
	}
	id := model.OpaqueToken(c.Path)
	fmt.Fprintf(out, "  dnssec-validation %s;\n", dnssec)
	fmt.Fprint(out, "  recursion yes;\n  empty-zones-enable no;\n  stale-answer-enable no;\n  stale-cache-enable no;\n")
	fmt.Fprintf(out, "  pid-file \"/tmp/internal-dns-%s.pid\";\n  session-keyfile \"/tmp/internal-dns-%s-session.key\";\n};\n\n", id, id)
	if c.RNDCIncludePath != "" {
		fmt.Fprintf(out, "include %q;\n", c.RNDCIncludePath)
	}
	if c.ControlAddress != "" {
		fmt.Fprintf(out, "controls { inet %s port %d allow { %s; } keys { %q; }; };\n", c.ControlAddress, c.ControlPort, c.ControlAddress, c.ControlKeyName)
	}
}

func renderNodeBIND(c BINDRenderConfig, bindings []model.Binding) []byte {
	var out bytes.Buffer
	renderOptions(&out, c)
	for _, b := range bindings {
		renderViewStart(&out, b, b.ConsumerAddress, c.DefaultCacheSize, maxNegativeCacheTTL(c))
		fmt.Fprint(&out, "  forward only;\n  forwarders { ")
		renderAddrPort(&out, b.ClusterAddress, b.Port)
		fmt.Fprint(&out, " };\n};\n\n")
	}
	renderManagementAndReject(&out, c.ListenAddress)
	return out.Bytes()
}

func renderClusterBIND(c BINDRenderConfig, bindings []model.Binding, publications map[string]publicationState, now time.Time, files map[string][]byte) ([]byte, error) {
	var out bytes.Buffer
	renderOptions(&out, c)
	for _, b := range bindings {
		renderViewStart(&out, b, b.ClusterAddress, c.DefaultCacheSize, maxNegativeCacheTTL(c))
		for _, z := range b.Zones {
			p, ok := publications[z.ZoneUID]
			if !ok || p.Manifest.Tombstone || model.Compare(p.Fence.Epoch, p.Fence.Revision, z.RequiredPublicationEpoch, z.RequiredPublicationRevision) < 0 {
				// Never recurse toward the public namespace when a private apex is missing.
				// The front gate returns immediate SERVFAIL; direct BIND queries fail too.
				fmt.Fprintf(&out, "  zone %q { type forward; forward only; forwarders { 127.0.0.1 port 9; }; };\n", strings.TrimSuffix(model.AbsoluteName(z.Apex), "."))
				continue
			}
			data, err := renderZone(p, now)
			if err != nil {
				return nil, err
			}
			path := filepath.Join(filepath.Dir(c.Path), "zone-"+model.OpaqueToken(z.ZoneUID)+"-"+model.Hash(data)[:16]+".db")
			files[path] = data
			fmt.Fprintf(&out, "  zone %q { type primary; file %q; allow-transfer { none; }; notify no; };\n", strings.TrimSuffix(model.AbsoluteName(z.Apex), "."), path)
		}
		fmt.Fprint(&out, "};\n\n")
	}
	renderManagementAndReject(&out, c.ListenAddress)
	return out.Bytes(), nil
}

func renderViewStart(out *bytes.Buffer, b model.Binding, dest, defaultCache string, maxNegativeCacheTTL uint32) {
	if b.ConfigurationPending {
		fmt.Fprintf(out, "view %q {\n  match-clients { any; };\n  match-destinations { %s; };\n  recursion no;\n  allow-query { none; };\n  allow-recursion { none; };\n  allow-query-cache { none; };\n", b.ViewName(), dest)
		return
	}
	cache := defaultCache
	if cache == "" {
		cache = "16M"
	}
	fmt.Fprintf(out, "view %q {\n  match-clients { any; };\n  match-destinations { %s; };\n  allow-query { any; };\n  allow-recursion { any; };\n  allow-query-cache { any; };\n  max-cache-size %s;\n  max-ncache-ttl %d;\n", b.ViewName(), dest, cache, maxNegativeCacheTTL)
	// Private authoritative zones are intentionally unsigned and have no DS in
	// the public root chain. Keep DNSSEC validation enabled for public recursion,
	// but exclude every private apex attached to this isolated view.
	apices := make([]string, 0, len(b.Zones))
	seen := map[string]bool{}
	for _, zone := range b.Zones {
		apex := strings.TrimSuffix(model.AbsoluteName(zone.Apex), ".")
		if !seen[apex] {
			seen[apex] = true
			apices = append(apices, apex)
		}
	}
	if len(apices) > 0 {
		sort.Strings(apices)
		fmt.Fprint(out, "  validate-except { ")
		for _, apex := range apices {
			fmt.Fprintf(out, "%q; ", apex)
		}
		fmt.Fprint(out, "};\n")
	}
}

func maxNegativeCacheTTL(c BINDRenderConfig) uint32 {
	if c.MaxNegativeCacheTTLSeconds == 0 {
		return 5
	}
	return c.MaxNegativeCacheTTLSeconds
}

func renderManagementAndReject(out *bytes.Buffer, listen string) {
	fmt.Fprintf(out, "view \"management\" {\n  match-clients { any; };\n  match-destinations { %s; };\n  recursion no;\n  allow-query { any; };\n  allow-query-cache { none; };\n};\n\n", listen)
	fmt.Fprint(out, "view \"reject\" {\n  match-clients { any; };\n  match-destinations { any; };\n  recursion no;\n  allow-query { none; };\n  allow-query-cache { none; };\n};\n")
}
func renderAddrPort(out *bytes.Buffer, addr string, port uint16) {
	fmt.Fprint(out, addr)
	if port != 53 {
		fmt.Fprintf(out, " port %d", port)
	}
	fmt.Fprint(out, "; ")
}
func safeNamedAtom(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}
