package pdns

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

func writePDNSAuthWithSQLite(t *testing.T, dir, apiKey string) {
	t.Helper()
	// Minimal authoritative config with API + SQLite backend
	conf := strings.Join([]string{
		"api=yes",
		"api-key=" + apiKey,
		"webserver=yes",
		"webserver-address=0.0.0.0",
		"webserver-port=8081",
		"loglevel=6",

		"webserver-allow-from=0.0.0.0/0,::/0",

		"launch=gsqlite3",
		"gsqlite3-database=/var/lib/powerdns/pdns.sqlite3",
	}, "\n") + "\n"

	if err := os.WriteFile(filepath.Join(dir, "pdns.conf"), []byte(conf), 0o644); err != nil {
		t.Fatalf("write pdns.conf: %v", err)
	}
}

func startPDNS(t *testing.T, apiKey string) (baseURL string, terminate func()) {
	t.Helper()

	ctx := context.Background()
	cfgDir := t.TempDir()
	dataDir := t.TempDir()
	writePDNSAuthWithSQLite(t, cfgDir, apiKey)

	// Use an official-ish PDNS authoritative image that reads /etc/powerdns/pdns.conf.
	// You can pin a specific version if you prefer, e.g. powerdns/pdns-auth-46:latest
	req := tc.GenericContainerRequest{
		ContainerRequest: tc.ContainerRequest{
			Image:        "powerdns/pdns-auth-49:latest",
			ExposedPorts: []string{"8081/tcp"},
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.Mounts = append(hc.Mounts,
					mount.Mount{
						Type:     mount.TypeBind,
						Source:   cfgDir,
						Target:   "/etc/powerdns",
						ReadOnly: true,
					},
					mount.Mount{
						Type:   mount.TypeBind,
						Source: dataDir,
						Target: "/data",
					},
				)
			},
			WaitingFor: wait.ForHTTP("/api/v1/servers/localhost").
				WithPort("8081/tcp").
				WithHeaders(map[string]string{"X-API-Key": apiKey}).
				WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	}
	c, err := tc.GenericContainer(ctx, req)
	if err != nil {
		t.Fatalf("start container: %v", err)
	}

	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(ctx)
		t.Fatalf("host: %v", err)
	}
	mp, err := c.MappedPort(ctx, "8081/tcp")
	if err != nil {
		_ = c.Terminate(ctx)
		t.Fatalf("mapped port: %v", err)
	}

	return fmt.Sprintf("http://%s:%s", host, mp.Port()), func() {
		_ = c.Terminate(context.Background())
	}
}

func TestPDNS_EndToEnd_AllTypes(t *testing.T) {
	// No t.Parallel(): we’re booting a container.
	const apiKey = "itest-key"
	baseURL, stop := startPDNS(t, apiKey)
	defer stop()

	client := NewClient(baseURL, apiKey)

	zone := "example.test"
	// Create the zone with some NS via the API create call
	if err := client.CreateZone(context.Background(), zone, []string{"ns1.example.net", "ns2.example.net"}); err != nil {
		t.Fatalf("CreateZone: %v", err)
	}
	if got, err := client.GetZone(context.Background(), zone); err != nil || got != zone+"." {
		t.Fatalf("GetZone: got=%q err=%v", got, err)
	}

	// Build and apply records for each type
	var ttl int64 = 300

	apply := func(rt dnsv1alpha1.RRType, recs ...dnsv1alpha1.RecordEntry) {
		rs := dnsv1alpha1.DNSRecordSet{
			Spec: dnsv1alpha1.DNSRecordSetSpec{
				RecordType: rt,
				Records:    recs,
			},
		}
		if err := client.ApplyRecordSetAuthoritative(context.Background(), zone, rs); err != nil {
			t.Fatalf("ApplyRecordSetAuthoritative(%s): %v", rt, err)
		}
	}

	// A
	apply(dnsv1alpha1.RRTypeA,
		dnsv1alpha1.RecordEntry{Name: "@", TTL: &ttl, A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"}},
		dnsv1alpha1.RecordEntry{Name: "www", TTL: &ttl, A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.5"}},
	)
	// AAAA
	apply(dnsv1alpha1.RRTypeAAAA,
		dnsv1alpha1.RecordEntry{Name: "v6", TTL: &ttl, AAAA: &dnsv1alpha1.AAAARecordSpec{Content: "2001:db8::1"}},
	)
	// CNAME
	apply(dnsv1alpha1.RRTypeCNAME,
		dnsv1alpha1.RecordEntry{Name: "alias", TTL: &ttl, CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "www." + zone + "."}},
	)
	// ALIAS (PowerDNS-specific)
	apply(dnsv1alpha1.RRTypeALIAS,
		dnsv1alpha1.RecordEntry{Name: "@", TTL: &ttl, ALIAS: &dnsv1alpha1.ALIASRecordSpec{Content: "www." + zone + "."}},
	)
	// TXT (quoted)
	apply(dnsv1alpha1.RRTypeTXT,
		dnsv1alpha1.RecordEntry{Name: "txt", TTL: &ttl, TXT: &dnsv1alpha1.TXTRecordSpec{Content: "hello world"}},
	)
	// MX
	apply(dnsv1alpha1.RRTypeMX,
		dnsv1alpha1.RecordEntry{Name: "@", TTL: &ttl, MX: &dnsv1alpha1.MXRecordSpec{Preference: 10, Exchange: "mail." + zone + "."}},
	)
	// SRV
	apply(dnsv1alpha1.RRTypeSRV,
		dnsv1alpha1.RecordEntry{Name: "_https._tcp", TTL: &ttl, SRV: &dnsv1alpha1.SRVRecordSpec{Priority: 1, Weight: 0, Port: 443, Target: "www." + zone + "."}},
	)
	// CAA
	apply(dnsv1alpha1.RRTypeCAA,
		dnsv1alpha1.RecordEntry{Name: "@", TTL: &ttl, CAA: &dnsv1alpha1.CAARecordSpec{Flag: 0, Tag: "issue", Value: "letsencrypt.org"}},
	)
	// NS
	apply(dnsv1alpha1.RRTypeNS,
		dnsv1alpha1.RecordEntry{Name: "@", TTL: &ttl, NS: &dnsv1alpha1.NSRecordSpec{Content: "ns1.example.net."}},
		dnsv1alpha1.RecordEntry{Name: "@", TTL: &ttl, NS: &dnsv1alpha1.NSRecordSpec{Content: "ns2.example.net."}},
	)
	// SOA (normalize mname/rname; serial auto)
	apply(dnsv1alpha1.RRTypeSOA,
		dnsv1alpha1.RecordEntry{
			Name: "@",
			TTL:  &ttl,
			SOA:  &dnsv1alpha1.SOARecordSpec{MName: "ns1.example.net.", RName: "hostmaster.example.net."},
		},
	)
	// PTR (we’ll add it under a label in the same zone; PDNS doesn’t enforce reverse-zone semantics)
	apply(dnsv1alpha1.RRTypePTR,
		dnsv1alpha1.RecordEntry{Name: "ptrhost", TTL: &ttl, PTR: &dnsv1alpha1.PTRRecordSpec{Content: "target." + zone + "."}},
	)
	// SSHFP
	apply(dnsv1alpha1.RRTypeSSHFP,
		dnsv1alpha1.RecordEntry{Name: "ssh", TTL: &ttl, SSHFP: &dnsv1alpha1.SSHFPRecordSpec{Algorithm: 1, Type: 1, Fingerprint: "abcdef"}},
	)
	// NAPTR
	apply(dnsv1alpha1.RRTypeNAPTR,
		dnsv1alpha1.RecordEntry{Name: "naptr", TTL: &ttl, NAPTR: &dnsv1alpha1.NAPTRRecordSpec{Order: 100, Preference: 10, Flags: "U", Services: "E2U+sip", Regexp: "!^.*$!sip:info@" + zone + "!", Replacement: "."}},
	)
	// TLSA
	apply(dnsv1alpha1.RRTypeTLSA,
		dnsv1alpha1.RecordEntry{Name: "_443._tcp", TTL: &ttl, TLSA: &dnsv1alpha1.TLSARecordSpec{Usage: 3, Selector: 1, MatchingType: 1, CertData: "ABCD"}},
	)
	// HTTPS: alias-form (prio 0) + service-form (.)
	apply(dnsv1alpha1.RRTypeHTTPS,
		dnsv1alpha1.RecordEntry{
			Name: "https-alias",
			TTL:  &ttl,
			HTTPS: &dnsv1alpha1.HTTPSRecordSpec{
				Priority: 0, Target: "www." + zone + ".", // alias form
			},
		},
		dnsv1alpha1.RecordEntry{
			Name: "https",
			TTL:  &ttl,
			HTTPS: &dnsv1alpha1.HTTPSRecordSpec{
				Priority: 1, Target: ".", Params: map[string]string{
					"alpn":            "h2,h3",
					"ipv4hint":        "1.2.3.4,5.6.7.8",
					"no-default-alpn": "",
				},
			},
		},
	)
	// SVCB mirrors HTTPS behavior
	apply(dnsv1alpha1.RRTypeSVCB,
		dnsv1alpha1.RecordEntry{
			Name: "svcb",
			TTL:  &ttl,
			SVCB: &dnsv1alpha1.HTTPSRecordSpec{
				Priority: 1, Target: ".", Params: map[string]string{"alpn": "h2", "port": "8443"},
			},
		},
	)

	// Fetch rrsets back and build an index type->name->[]content
	sets, err := client.GetZoneRRSets(context.Background(), zone)
	if err != nil {
		t.Fatalf("GetZoneRRSets: %v", err)
	}
	type key struct{ typ, name string }
	index := make(map[key][]string)
	for _, s := range sets {
		k := key{s.Type, s.Name}
		for _, r := range s.Records {
			index[k] = append(index[k], r.Content)
		}
		// stable order for deterministic checks
		sort.Strings(index[k])
	}

	// helper for asserts with normalization
	get := func(typ, owner string) []string {
		return index[key{typ, qualifyOwner(owner, zone)}]
	}
	stripq := func(s string) string {
		if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
			return s[1 : len(s)-1]
		}
		return s
	}

	// A
	if got := get("A", "@"); len(got) != 1 || got[0] != "1.2.3.4" {
		t.Fatalf("A @ got=%v", got)
	}
	if got := get("A", "www"); len(got) != 1 || got[0] != "1.2.3.5" {
		t.Fatalf("A www got=%v", got)
	}

	// AAAA
	if got := get("AAAA", "v6"); len(got) != 1 || got[0] != "2001:db8::1" {
		t.Fatalf("AAAA v6 got=%v", got)
	}

	// CNAME
	if got := get("CNAME", "alias"); len(got) != 1 || stripTrailingDot(got[0]) != "www."+zone {
		t.Fatalf("CNAME alias got=%v", got)
	}

	// ALIAS
	if got := get("ALIAS", "@"); len(got) != 1 || stripTrailingDot(got[0]) != "www."+zone {
		t.Fatalf("ALIAS @ got=%v", got)
	}

	// TXT (we compare without quotes)
	if got := get("TXT", "txt"); len(got) != 1 || stripq(got[0]) != "hello world" {
		t.Fatalf("TXT txt got=%v", got)
	}

	// MX
	if got := get("MX", "@"); len(got) != 1 || got[0] != "10 mail."+zone+"." {
		t.Fatalf("MX @ got=%v", got)
	}

	// SRV
	if got := get("SRV", "_https._tcp"); len(got) != 1 || !strings.HasSuffix(got[0], " www."+zone+".") {
		t.Fatalf("SRV _https._tcp got=%v", got)
	}

	// CAA (quoted value)
	if got := get("CAA", "@"); len(got) != 1 || stripq(strings.TrimPrefix(got[0], "0 issue ")) != "letsencrypt.org" {
		t.Fatalf("CAA @ got=%v", got)
	}

	// NS
	if got := get("NS", "@"); len(got) != 2 {
		t.Fatalf("NS @ got=%v", got)
	} else {
		sort.Strings(got)
		if got[0] != "ns1.example.net." || got[1] != "ns2.example.net." {
			t.Fatalf("NS @ got=%v", got)
		}
	}

	// SOA (check mname/rname; serial shape often managed by PDNS)
	if got := get("SOA", "@"); len(got) != 1 {
		t.Fatalf("SOA @ missing")
	} else {
		fields := strings.Fields(got[0])
		if len(fields) < 2 || fields[0] != "ns1.example.net." || fields[1] != "hostmaster.example.net." {
			t.Fatalf("SOA content: %q", got[0])
		}
	}

	// PTR
	if got := get("PTR", "ptrhost"); len(got) != 1 || got[0] != "target."+zone+"." {
		t.Fatalf("PTR ptrhost got=%v", got)
	}

	// SSHFP
	if got := get("SSHFP", "ssh"); len(got) != 1 || got[0] != "1 1 abcdef" {
		t.Fatalf("SSHFP ssh got=%v", got)
	}

	// NAPTR (we expect provider-quoted fields)
	if got := get("NAPTR", "naptr"); len(got) != 1 || got[0] != `100 10 "U" "E2U+sip" "!^.*$!sip:info@`+zone+`!" .` {
		t.Fatalf("NAPTR naptr got=%v", got)
	}

	// TLSA
	if got := get("TLSA", "_443._tcp"); len(got) != 1 || got[0] != "3 1 1 abcd" {
		t.Fatalf("TLSA _443._tcp got=%v", got)
	}

	// HTTPS alias form
	if got := get("HTTPS", "https-alias"); len(got) != 1 || !strings.HasPrefix(got[0], "0 ") || !strings.Contains(got[0], " www."+zone) {
		t.Fatalf("HTTPS alias got=%v", got)
	}
	// HTTPS service form (dot target + params)
	if got := get("HTTPS", "https"); len(got) != 1 || !strings.HasPrefix(got[0], "1 . ") || !strings.Contains(got[0], `alpn=h2,h3`) || !strings.Contains(got[0], "ipv4hint=1.2.3.4,5.6.7.8") {
		t.Fatalf("HTTPS service got=%v", got)
	}

	// SVCB service form
	if got := get("SVCB", "svcb"); len(got) != 1 || !strings.HasPrefix(got[0], "1 . ") || !strings.Contains(got[0], `alpn=h2`) || !strings.Contains(got[0], "port=8443") {
		t.Fatalf("SVCB service got=%v", got)
	}
}

func TestPDNS_ApplyRecordSetAuthoritative_CleansRemovedOwners(t *testing.T) {
	// No t.Parallel(): container + real PDNS.
	const apiKey = "itest-key"
	baseURL, stop := startPDNS(t, apiKey)
	defer stop()

	client := NewClient(baseURL, apiKey)
	ctx := context.Background()
	zone := "cleanup.test"

	if err := client.CreateZone(ctx, zone, []string{"ns1.example.net", "ns2.example.net"}); err != nil {
		t.Fatalf("CreateZone: %v", err)
	}

	var ttl int64 = 300

	// Helper to index rrsets by (type, owner).
	buildIndex := func(t *testing.T) (map[[2]string][]string, func(typ, owner string) []string) {
		t.Helper()
		sets, err := client.GetZoneRRSets(ctx, zone)
		if err != nil {
			t.Fatalf("GetZoneRRSets: %v", err)
		}
		index := make(map[[2]string][]string)
		for _, s := range sets {
			k := [2]string{s.Type, s.Name}
			for _, r := range s.Records {
				index[k] = append(index[k], r.Content)
			}
			sort.Strings(index[k])
		}
		get := func(typ, owner string) []string {
			return index[[2]string{typ, qualifyOwner(owner, zone)}]
		}
		return index, get
	}

	// Initial: three A owners.
	initial := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "@", TTL: &ttl, A: &dnsv1alpha1.ARecordSpec{Content: "1.1.1.1"}},
				{Name: "www", TTL: &ttl, A: &dnsv1alpha1.ARecordSpec{Content: "1.1.1.2"}},
				{Name: "api", TTL: &ttl, A: &dnsv1alpha1.ARecordSpec{Content: "1.1.1.3"}},
			},
		},
	}
	if err := client.ApplyRecordSetAuthoritative(ctx, zone, initial); err != nil {
		t.Fatalf("ApplyRecordSetAuthoritative(initial): %v", err)
	}

	_, get := buildIndex(t)

	// Sanity: all three are present.
	if got := get("A", "@"); len(got) != 1 || got[0] != "1.1.1.1" {
		t.Fatalf("before: A @ got=%v", got)
	}
	if got := get("A", "www"); len(got) != 1 || got[0] != "1.1.1.2" {
		t.Fatalf("before: A www got=%v", got)
	}
	if got := get("A", "api"); len(got) != 1 || got[0] != "1.1.1.3" {
		t.Fatalf("before: A api got=%v", got)
	}

	// Capture NS/SOA count before we mutate A records, to verify we don't touch other types.
	indexBefore, _ := buildIndex(t)
	nsBefore := len(indexBefore[[2]string{"NS", qualifyOwner("@", zone)}])
	soaBefore := len(indexBefore[[2]string{"SOA", qualifyOwner("@", zone)}])

	// Updated: drop "www", change @ and api.
	updated := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "@", TTL: &ttl, A: &dnsv1alpha1.ARecordSpec{Content: "2.2.2.2"}},
				{Name: "api", TTL: &ttl, A: &dnsv1alpha1.ARecordSpec{Content: "2.2.2.3"}},
			},
		},
	}
	if err := client.ApplyRecordSetAuthoritative(ctx, zone, updated); err != nil {
		t.Fatalf("ApplyRecordSetAuthoritative(updated): %v", err)
	}

	indexAfter, get := buildIndex(t)

	// Expect: @ and api updated…
	if got := get("A", "@"); len(got) != 1 || got[0] != "2.2.2.2" {
		t.Fatalf("after: A @ got=%v", got)
	}
	if got := get("A", "api"); len(got) != 1 || got[0] != "2.2.2.3" {
		t.Fatalf("after: A api got=%v", got)
	}

	// …and www removed entirely (no rrset of type A at that owner).
	if got := get("A", "www"); len(got) != 0 {
		t.Fatalf("after: expected A www to be deleted, got=%v", got)
	}

	// Verify we did not touch NS/SOA rrsets (ApplyRecordSetAuthoritative is per-type).
	if got := len(indexAfter[[2]string{"NS", qualifyOwner("@", zone)}]); got != nsBefore {
		t.Fatalf("NS rrset count changed: before=%d after=%d", nsBefore, got)
	}
	if got := len(indexAfter[[2]string{"SOA", qualifyOwner("@", zone)}]); got != soaBefore {
		t.Fatalf("SOA rrset count changed: before=%d after=%d", soaBefore, got)
	}
}
