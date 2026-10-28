package serving

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	contextdns "go.miloapis.com/dns-operator/internal/internaldns/coredns"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

type DNSDistRenderConfig struct {
	Path            string          `json:"path"`
	ListenAddress   string          `json:"listenAddress"`
	ListenerCount   int             `json:"listenerCount,omitempty"`
	ACLs            []string        `json:"acls"`
	PoolName        string          `json:"poolName"`
	Backends        []model.Backend `json:"backends,omitempty"`
	IdentityKeyFile string          `json:"identityKeyFile,omitempty"`
	// BackendTransport defaults to TCP. UDP uses UDP for incoming UDP queries
	// while retaining TCP for TCP clients, and requires per-query keyed identity.
	BackendTransport model.Transport `json:"backendTransport,omitempty"`
	MetricsAddress   string          `json:"metricsAddress,omitempty"`
	MetricsACLs      []string        `json:"metricsACLs,omitempty"`
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
	ReadyZoneMembers  map[string]map[string]map[string]bool `json:"-"`
	ReadyMembers      map[string]map[string]bool            `json:"-"`
	Role              RenderRole                            `json:"role"`
	NodeDNSDist       DNSDistRenderConfig                   `json:"nodeDNSDist,omitempty"`
	ClusterDNSDist    DNSDistRenderConfig                   `json:"clusterDNSDist,omitempty"`
	NodeCoreDNS       CoreDNSRenderConfig                   `json:"nodeCoreDNS,omitempty"`
	ClusterCoreDNS    CoreDNSRenderConfig                   `json:"clusterCoreDNS,omitempty"`
	NodeAuthoritative bool                                  `json:"-"`
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
		node, err := renderNodeCoreDNS(c.NodeCoreDNS, bindings, publications, c.NodeAuthoritative)
		if err != nil {
			return Rendered{}, err
		}
		files[c.NodeCoreDNS.Path] = node
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
		if d.ListenerCount < 0 || d.ListenerCount > 64 {
			return fmt.Errorf("%s listener count must be between 0 and 64", label)
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
		transport := model.Transport(strings.ToUpper(string(d.BackendTransport)))
		if transport != "" && transport != model.TransportTCP && transport != model.TransportUDP {
			return fmt.Errorf("%s backend transport must be tcp or udp", label)
		}
		if transport == model.TransportUDP && d.IdentityKeyFile == "" {
			return fmt.Errorf("%s UDP backend requires an identity key", label)
		}
		if err := validateDNSDistMetrics(d); err != nil {
			return fmt.Errorf("%s %w", label, err)
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

var envReference = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

func validateDNSDistMetrics(d DNSDistRenderConfig) error {
	if d.MetricsAddress == "" {
		if len(d.MetricsACLs) > 0 {
			return errors.New("metrics ACLs require a metrics address")
		}
		return nil
	}
	if _, err := metricsListenAddress(envReference.ReplaceAllString(d.MetricsAddress, "192.0.2.1")); err != nil {
		return fmt.Errorf("metrics address %q: environment references may stand only for the host: %w", d.MetricsAddress, err)
	}
	if len(d.MetricsACLs) == 0 {
		return errors.New("metrics address requires metrics ACLs naming the scrapers")
	}
	for _, acl := range d.MetricsACLs {
		prefix, err := netip.ParsePrefix(acl)
		if err != nil {
			return fmt.Errorf("invalid metrics ACL %q", acl)
		}
		if prefix.Bits() == 0 {
			return fmt.Errorf("metrics ACL %q admits every address", acl)
		}
	}
	return nil
}

// Expands ${VAR} references when dnsdist starts, and turns metrics off rather
// than failing startup when the result is not a specific IP address and port.
const dnsdistMetricsAddressLua = `local function internalDNSMetricsAddress(raw)
  local address = raw:gsub("%${([%w_]+)}", function(name) return os.getenv(name) or "" end)
  local host, port = address:match("^%[(.*)%]:(%d+)$")
  if not host then host, port = address:match("^(.*):(%d+)$") end
  port = tonumber(port)
  if not host or host == "" or not port or port == 0 or port > 65535 then return nil, address end
  local ok, ca = pcall(newCA, host)
  if not ok then return nil, address end
  host = ca:toString():lower()
  if host == "0.0.0.0" or host == "::" or host:match("^::ffff:0%.0%.0%.0$") or host:match("^::ffff:0:0$") then return nil, address end
  if host:find(":") then return "[" .. host .. "]:" .. port, address end
  return host .. ":" .. port, address
end
`

// The web server gets a random password and API key nobody knows: dnsdist
// accepts any credentials when none are set, which would open its API.
const dnsdistWebSecretLua = `local handle = assert(io.open("/dev/urandom", "rb"))
local secret = handle:read(32)
handle:close()
internalDNSWebSecret = hashPassword((secret:gsub(".", function(c) return string.format("%02x", c:byte()) end)))
`

func renderDNSDistMetricsStart(out *bytes.Buffer, c DNSDistRenderConfig) {
	if c.MetricsAddress == "" {
		return
	}
	out.WriteString("if os.getenv(\"INTERNAL_DNS_VALIDATE\") ~= \"1\" then\n" + dnsdistMetricsAddressLua)
	fmt.Fprintf(out, "local address, raw = internalDNSMetricsAddress(%q)\nif address then\n", c.MetricsAddress)
	out.WriteString(dnsdistWebSecretLua + "webserver(address)\n")
	fmt.Fprint(out, "else\nerrlog(\"internal DNS: metrics address \" .. raw .. \" is not a specific IP address and port; metrics are off\")\nend\nend\n")
}

// Runs on every reload so ACL changes apply without a restart.
func renderDNSDistMetricsAccess(out *bytes.Buffer, c DNSDistRenderConfig) {
	if c.MetricsAddress == "" {
		return
	}
	fmt.Fprintf(out, "if internalDNSWebSecret then\nsetWebserverConfig({acl=%q,password=internalDNSWebSecret,apiKey=internalDNSWebSecret,statsRequireAuthentication=false,apiRequiresAuthentication=true,dashboardRequiresAuthentication=true})\nend\n", strings.Join(c.MetricsACLs, ","))
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
	listeners := c.ListenerCount
	if listeners == 0 {
		listeners = 1
	}
	fmt.Fprint(&out, "local client = inClientStartup()\nif client or not internalDNSInitialized then\n")
	fmt.Fprintf(&out, "  local key\n  if os.getenv(\"INTERNAL_DNS_VALIDATE\") == \"1\" then key = string.rep(\"A\",43)..\"=\" else\n    local handle = assert(io.open(%q,\"r\"))\n    key = handle:read(\"*l\"); handle:close()\n  end\n  setKey(key)\n  controlSocket(%q)\n  setConsoleACL({\"127.0.0.1/32\"})\nend\nif not client then\n", DNSDistKeyPath, endpoint)
	topology, _ := json.Marshal(struct {
		Listen    string
		ACLs      []string
		Backends  []model.Backend
		Pool      string
		Transport string
		Listeners int
	}{c.ListenAddress, c.ACLs, backends, c.PoolName, dnsdistTransport(c), listeners})
	fingerprint := model.OpaqueToken(string(topology))
	fmt.Fprint(&out, "if not internalDNSInitialized then\n")
	if listeners == 1 {
		fmt.Fprintf(&out, "setLocal(%q)\n", c.ListenAddress)
	} else {
		fmt.Fprintf(&out, "setLocal(%q,{reusePort=true})\n", c.ListenAddress)
		for range listeners - 1 {
			fmt.Fprintf(&out, "addLocal(%q,{reusePort=true})\n", c.ListenAddress)
		}
	}
	fmt.Fprint(&out, "setACL({")
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
		tcpOnly := !strings.EqualFold(string(c.BackendTransport), string(model.TransportUDP))
		fmt.Fprintf(&out, "internalDNSServers[%q] = newServer({address=%q,name=%q,pool=%q,%stcpOnly=%t,checkTCP=%t,checkName=\".\",checkType=\"SOA\",mustResolve=false,maxCheckFailures=3,rise=2})\n", backend.MemberID, net.JoinHostPort(backend.Address, strconv.Itoa(int(backend.Port))), "b_"+model.OpaqueToken(backend.MemberID), c.PoolName, proxy, tcpOnly, tcpOnly)
	}
	renderDNSDistMetricsStart(&out, c)
	fmt.Fprintf(&out, "internalDNSTopology = %q\ninternalDNSInitialized = true\nend\nassert(internalDNSTopology == %q,\"dnsdist topology requires a replacement member\")\n", fingerprint, fingerprint)
	renderDNSDistMetricsAccess(&out, c)
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
	out.WriteString(dnsdistDestinationHelper)
	out.Write(renderDNSDistReadinessCanary(c))
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

const dnsdistDestinationHelper = "local function destination(address) local nmg = newNMG(); nmg:addMask(address); return NetmaskGroupRule(nmg, false, true) end\n\n"

const (
	dnsdistCanaryStart = "-- readiness canary\n"
	dnsdistCanaryEnd   = "-- end readiness canary\n"
)

// The canary is accepted only from loopback and the listen address, the
// sources a local probe can have, so tenants cannot reach the backend pool
// without being classified into a context.
func renderDNSDistReadinessCanary(c DNSDistRenderConfig) []byte {
	var out bytes.Buffer
	out.WriteString(dnsdistCanaryStart)
	sources := []string{"127.0.0.0/8", "::1/128"}
	if host, _, err := net.SplitHostPort(c.ListenAddress); err == nil {
		if addr, err := netip.ParseAddr(host); err == nil && !addr.IsUnspecified() && !addr.IsLoopback() {
			addr = addr.Unmap()
			sources = append(sources, netip.PrefixFrom(addr, addr.BitLen()).String())
		}
	}
	out.WriteString("local function readinessCanary() local nmg = newNMG();")
	for _, source := range sources {
		fmt.Fprintf(&out, " nmg:addMask(%q);", source)
	}
	fmt.Fprintf(&out, " return AndRule({NetmaskGroupRule(nmg, true, true),QClassRule(DNSClass.CHAOS),QTypeRule(DNSQType.TXT),QNameRule(%q)}) end\n", contextdns.ReadinessCanaryName)
	fmt.Fprintf(&out, "getPool(%q)\nappendRule(AndRule({readinessCanary(),NotRule(PoolAvailableRule(%q))}),RCodeAction(DNSRCode.SERVFAIL))\n", c.PoolName, c.PoolName)
	if c.IdentityKeyFile != "" {
		fmt.Fprintf(&out, "appendRule(readinessCanary(),SetEDNSOptionAction(%d, internalDNSIdentityKey..\"::1\"))\n", contextdns.IdentityOption)
	}
	fmt.Fprintf(&out, "appendRule(readinessCanary(),PoolAction(%q))\n", c.PoolName)
	out.WriteString(dnsdistCanaryEnd)
	return out.Bytes()
}

// withDNSDistReadinessCanary replaces the canary in retained rules, or adds it
// to rules accepted before the canary existed. It reports false when neither
// is possible.
func withDNSDistReadinessCanary(c DNSDistRenderConfig, rules []byte) ([]byte, bool) {
	canary := renderDNSDistReadinessCanary(c)
	if before, rest, ok := bytes.Cut(rules, []byte(dnsdistCanaryStart)); ok {
		_, after, ok := bytes.Cut(rest, []byte(dnsdistCanaryEnd))
		if !ok {
			return nil, false
		}
		return append(append(append([]byte(nil), before...), canary...), after...), true
	}
	before, after, ok := bytes.Cut(rules, []byte(dnsdistDestinationHelper))
	if !ok {
		return nil, false
	}
	return append(append(append(append([]byte(nil), before...), dnsdistDestinationHelper...), canary...), after...), true
}

func withoutDNSDistReadinessCanary(rules []byte) []byte {
	before, rest, ok := bytes.Cut(rules, []byte(dnsdistCanaryStart))
	if !ok {
		return rules
	}
	_, after, ok := bytes.Cut(rest, []byte(dnsdistCanaryEnd))
	if !ok {
		return rules
	}
	return append(append([]byte(nil), before...), after...)
}

func dnsdistTransport(c DNSDistRenderConfig) string {
	if c.IdentityKeyFile != "" {
		if strings.EqualFold(string(c.BackendTransport), string(model.TransportUDP)) {
			return "identity-option-udp"
		}
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

func renderNodeCoreDNS(c CoreDNSRenderConfig, bindings []model.Binding, publications map[string]publicationState, authoritative bool) ([]byte, error) {
	cfg := coreDNSConfig(c)
	for _, b := range bindings {
		context := contextdns.Context{ID: b.ViewName(), Destination: b.ConsumerAddress, Pending: b.ConfigurationPending}
		if !authoritative {
			context.Upstreams = []string{net.JoinHostPort(b.ClusterAddress, strconv.Itoa(int(b.Port)))}
		}
		for _, z := range b.Zones {
			zone := contextdns.Zone{Apex: model.AbsoluteName(z.Apex)}
			p, ok := publications[z.ZoneUID]
			if !authoritative {
				zone.Pending = z.Pending
			} else if z.Pending || !ok || p.Manifest.Tombstone || model.Compare(p.Fence.Epoch, p.Fence.Revision, z.RequiredPublicationEpoch, z.RequiredPublicationRevision) < 0 {
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
	data, _ := json.MarshalIndent(cfg, "", "  ")
	return data, nil
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
