package pdns

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	dnserrors "go.miloapis.com/dns-operator/internal/dns/errors"
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

// writePDNSAuthWithLMDB configures the backend the Datum control plane runs:
// LMDB in LightningStream mode, which is what makes a test here see what the
// SQLite tests above cannot.
func writePDNSAuthWithLMDB(t *testing.T, dir, apiKey string) {
	t.Helper()
	conf := strings.Join([]string{
		"api=yes",
		"api-key=" + apiKey,
		"webserver=yes",
		"webserver-address=0.0.0.0",
		"webserver-port=8081",
		"loglevel=6",

		"webserver-allow-from=0.0.0.0/0,::/0",

		"load-modules=liblmdbbackend.so",
		"launch=lmdb",
		"lmdb-filename=/var/lib/powerdns/db",
		"lmdb-shards=1",
		"lmdb-lightning-stream=yes",
		"lmdb-flag-deleted=yes",
		// The zone cache would answer the API from a snapshot taken before the
		// zone existed.
		"zone-cache-refresh-interval=0",
	}, "\n") + "\n"

	if err := os.WriteFile(filepath.Join(dir, "pdns.conf"), []byte(conf), 0o644); err != nil {
		t.Fatalf("write pdns.conf: %v", err)
	}
}

func startPDNS(t *testing.T, apiKey string) (baseURL string, terminate func()) {
	t.Helper()
	// An official-ish PDNS authoritative image that reads /etc/powerdns/pdns.conf.
	return startPDNSWith(t, "powerdns/pdns-auth-49:latest", apiKey, writePDNSAuthWithSQLite)
}

// startPDNSLMDB starts the version and the backend the control plane runs.
// Both are needed to see datum-cloud/dns-operator#158.
func startPDNSLMDB(t *testing.T) (baseURL, apiKey string, terminate func()) {
	t.Helper()
	apiKey = "itest-key"
	baseURL, terminate = startPDNSWith(t, "powerdns/pdns-auth-51:5.1.4", apiKey, writePDNSAuthWithLMDB)
	return baseURL, apiKey, terminate
}

func startPDNSWith(t *testing.T, image, apiKey string, writeConf func(t *testing.T, dir, apiKey string)) (baseURL string, terminate func()) {
	t.Helper()

	ctx := context.Background()
	cfgDir := t.TempDir()
	dataDir := t.TempDir()
	writeConf(t, cfgDir, apiKey)

	req := tc.GenericContainerRequest{
		ContainerRequest: tc.ContainerRequest{
			Image:        image,
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
	// ALIAS
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
	// // PTR
	apply(dnsv1alpha1.RRTypePTR,
		dnsv1alpha1.RecordEntry{Name: "ptrhost", TTL: &ttl, PTR: &dnsv1alpha1.PTRRecordSpec{Content: "target." + zone + "."}},
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
		return index[key{typ, QualifyOwner(owner, zone)}]
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

	// // PTR
	if got := get("PTR", "ptrhost"); len(got) != 1 || got[0] != "target."+zone+"." {
		t.Fatalf("PTR ptrhost got=%v", got)
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
			return index[[2]string{typ, QualifyOwner(owner, zone)}]
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
	nsBefore := len(indexBefore[[2]string{"NS", QualifyOwner("@", zone)}])
	soaBefore := len(indexBefore[[2]string{"SOA", QualifyOwner("@", zone)}])

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
	if got := len(indexAfter[[2]string{"NS", QualifyOwner("@", zone)}]); got != nsBefore {
		t.Fatalf("NS rrset count changed: before=%d after=%d", nsBefore, got)
	}
	if got := len(indexAfter[[2]string{"SOA", QualifyOwner("@", zone)}]); got != soaBefore {
		t.Fatalf("SOA rrset count changed: before=%d after=%d", soaBefore, got)
	}
}

// TestPDNS_LMDB_DeleteLeavesNoComments proves the fix on the backend the
// control plane runs. datum-cloud/dns-operator#158.
//
// The control runs first and it is the point of the test: a bare DELETE leaves
// all three comments behind. Every other test in this file runs SQLite, which
// does clear them, so without the control a green run here would not even show
// the defect was reachable.
func TestPDNS_LMDB_DeleteLeavesNoComments(t *testing.T) {
	// No t.Parallel(): container + real PDNS.
	baseURL, apiKey, stop := startPDNSLMDB(t)
	defer stop()

	client := NewClient(baseURL, apiKey)
	ctx := context.Background()
	const zoneName = "clear.test"
	zone := dnsv1alpha1.DNSZone{Spec: dnsv1alpha1.DNSZoneSpec{DomainName: zoneName}}

	if err := client.CreateZone(ctx, zoneName, []string{"ns1.example.net", "ns2.example.net"}); err != nil {
		t.Fatalf("CreateZone: %v", err)
	}

	countsOf := func(t *testing.T, owner, recordType string) (records, comments int) {
		t.Helper()
		records, comments, _ = rrsetCounts(ctx, t, client, zoneName, owner, recordType)
		return records, comments
	}
	counts := func(t *testing.T, owner string) (records, comments int) {
		t.Helper()
		return countsOf(t, owner, "A")
	}

	// replaceComments is scoped by type as well as by name. If the clear ever
	// named the wrong type, this sibling is what would disappear.
	sibling := dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "txt", Namespace: "default", UID: types.UID("uid-txt"), Generation: 1},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeTXT,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "fixed", TXT: &dnsv1alpha1.TXTRecordSpec{Content: "keep me"}},
			},
		},
	}
	if _, err := client.EnsureRecordSet(ctx, zone, sibling); err != nil {
		t.Fatalf("EnsureRecordSet(TXT): %v", err)
	}

	// No comments at all: it must delete it cleanly rather than fail on
	// having nothing to remove.
	if err := sendRaw(ctx, t, client, http.MethodPatch, zoneName,
		`{"rrsets":[{"name":"`+QualifyOwner("bare", zoneName)+`","type":"A","ttl":300,"changetype":"REPLACE","records":[{"content":"8.8.8.8","disabled":false}]}]}`); err != nil {
		t.Fatalf("write the uncommented RRset: %v", err)
	}

	// The operator stamps three ownership comments on every RRset it writes.
	if _, err := client.EnsureRecordSet(ctx, zone, aRecordSet(1, "control", "fixed")); err != nil {
		t.Fatalf("EnsureRecordSet: %v", err)
	}
	for _, owner := range []string{"control", "fixed"} {
		if records, comments := counts(t, owner); records != 1 || comments != 3 {
			t.Fatalf("%s starts at records=%d comments=%d, want 1 and 3", owner, records, comments)
		}
	}

	// The control: the DELETE on its own, as the API reference describes it.
	if err := sendRaw(ctx, t, client, http.MethodPatch, zoneName,
		fmt.Sprintf(`{"rrsets":[{"name":%q,"type":"A","changetype":"DELETE","comments":[]}]}`,
			QualifyOwner("control", zoneName))); err != nil {
		t.Fatalf("the control DELETE changed nothing, so it proves nothing: %v", err)
	}
	if records, comments := counts(t, "control"); records != 0 || comments != 3 {
		t.Fatalf("control left records=%d comments=%d, want 0 and 3: this backend does not show the defect, "+
			"so the reading below is not evidence", records, comments)
	}

	// The fix: the client's own delete, which clears the comments behind it.
	if err := client.DeleteRRSet(ctx, zoneName, "A", "fixed"); err != nil {
		t.Fatalf("DeleteRRSet: %v", err)
	}
	if records, comments := counts(t, "fixed"); records != 0 || comments != 0 {
		t.Fatalf("after DeleteRRSet, records=%d comments=%d, want nothing left", records, comments)
	}

	// The clear addresses one RRset: the other name keeps its comments.
	if records, comments := counts(t, "control"); records != 0 || comments != 3 {
		t.Fatalf("deleting one name changed another: control now records=%d comments=%d", records, comments)
	}

	// And one type: the TXT at that same name is untouched.
	if records, comments := countsOf(t, "fixed", "TXT"); records != 1 || comments != 3 {
		t.Fatalf("deleting the A at fixed changed its TXT: records=%d comments=%d, want 1 and 3", records, comments)
	}

	// An RRset with no comments deletes cleanly.
	if err := client.DeleteRRSet(ctx, zoneName, "A", "bare"); err != nil {
		t.Fatalf("DeleteRRSet on an RRset with no comments: %v", err)
	}
	if records, comments := counts(t, "bare"); records != 0 || comments != 0 {
		t.Fatalf("after deleting the uncommented RRset, records=%d comments=%d", records, comments)
	}

	// #158 asks for "no shell and no comments", and a zero-record shell reads
	// as zero rows. So check the listing itself, not only the counts.
	for _, owner := range []string{"fixed", "bare"} {
		if _, _, listed := rrsetCounts(ctx, t, client, zoneName, owner, "A"); listed {
			t.Fatalf("%s is still listed in the zone after its delete, as a shell", owner)
		}
	}
	// The control was deleted without a clear, so its shell must still be there.
	// Without this the check above would pass against a zone that lists nothing.
	if _, _, listed := rrsetCounts(ctx, t, client, zoneName, "control", "A"); !listed {
		t.Fatal("the control shell is gone, so the listing check above proves nothing")
	}
}

// TestPDNS_LMDB_AMixedCaseNameSurvivesTheNextReconcile is #173. PowerDNS stores
// a name lowercased, and a record set that spells it in uppercase must still find
// it there on the next reconcile rather than prune it as surplus.
func TestPDNS_LMDB_AMixedCaseNameSurvivesTheNextReconcile(t *testing.T) {
	// No t.Parallel(): container + real PDNS.
	baseURL, apiKey, stop := startPDNSLMDB(t)
	defer stop()

	client := NewClient(baseURL, apiKey)
	ctx := context.Background()
	const zoneName = "case.test"
	zone := dnsv1alpha1.DNSZone{Spec: dnsv1alpha1.DNSZoneSpec{DomainName: zoneName}}

	if err := client.CreateZone(ctx, zoneName, []string{"ns1.example.net", "ns2.example.net"}); err != nil {
		t.Fatalf("CreateZone: %v", err)
	}

	// "lower" was never affected, so it shows the fold changed nothing that
	// already worked.
	recordSet := aRecordSet(1, "WWW", "lower")
	for call := 1; call <= 2; call++ {
		if _, err := client.EnsureRecordSet(ctx, zone, recordSet); err != nil {
			t.Fatalf("EnsureRecordSet, call %d: %v", call, err)
		}
		for _, owner := range []string{"www", "lower"} {
			if records, comments, _ := rrsetCounts(ctx, t, client, zoneName, owner, "A"); records != 1 || comments != 3 {
				t.Fatalf("after call %d, %s holds records=%d comments=%d, want 1 and 3", call, owner, records, comments)
			}
		}
	}

	if err := client.DeleteRecordSet(ctx, zone, recordSet); err != nil {
		t.Fatalf("DeleteRecordSet: %v", err)
	}
	if got := zoneCommentTotal(ctx, t, client, zoneName); got != 0 {
		t.Fatalf("the zone holds %d comments after the record set was deleted, want 0", got)
	}
}

// rrsetCounts reports the records and comments PowerDNS holds for one RRset,
// and whether the zone lists it at all. A shell left behind is listed with no
// records, which is the state #158 is about.
func rrsetCounts(ctx context.Context, t *testing.T, client *Client, zoneName, owner, recordType string) (records, comments int, listed bool) {
	t.Helper()
	sets, err := client.GetZoneRRSets(ctx, zoneName)
	if err != nil {
		t.Fatalf("GetZoneRRSets: %v", err)
	}
	for _, set := range sets {
		if set.Type == recordType && set.Name == QualifyOwner(owner, zoneName) {
			return len(set.Records), len(set.Comments), true
		}
	}
	return 0, 0, false
}

// zoneCommentTotal counts every comment in the zone, which is the denominator
// #158 states its acceptance in.
func zoneCommentTotal(ctx context.Context, t *testing.T, client *Client, zoneName string) int {
	t.Helper()
	sets, err := client.GetZoneRRSets(ctx, zoneName)
	if err != nil {
		t.Fatalf("GetZoneRRSets: %v", err)
	}
	total := 0
	for _, set := range sets {
		total += len(set.Comments)
	}
	return total
}

// sendRaw sends a request for a zone straight to PowerDNS, so a test can set up
// state the client itself would never write.
func sendRaw(ctx context.Context, t *testing.T, client *Client, method, zoneName, body string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, method,
		client.BaseURL+"/api/v1/servers/localhost/zones/"+zoneName+".", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", client.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("%s returned %d", method, resp.StatusCode)
	}
	return nil
}

// TestPDNS_LMDB_AFailedPatchAppliesNothing proves the property the fix rests
// on: no half of the pair is ever visible.
//
// The patch below deletes one RRset, clears its comments, and then submits a
// record PowerDNS must reject. What it observes is that nothing landed, since
// uncommitted state is invisible through the API.
func TestPDNS_LMDB_AFailedPatchAppliesNothing(t *testing.T) {
	// No t.Parallel(): container + real PDNS.
	baseURL, apiKey, stop := startPDNSLMDB(t)
	defer stop()

	client := NewClient(baseURL, apiKey)
	ctx := context.Background()
	const zoneName = "rollback.test"
	zone := dnsv1alpha1.DNSZone{Spec: dnsv1alpha1.DNSZoneSpec{DomainName: zoneName}}

	if err := client.CreateZone(ctx, zoneName, []string{"ns1.example.net", "ns2.example.net"}); err != nil {
		t.Fatalf("CreateZone: %v", err)
	}
	if _, err := client.EnsureRecordSet(ctx, zone, aRecordSet(1, "kept")); err != nil {
		t.Fatalf("EnsureRecordSet: %v", err)
	}

	counts := func(t *testing.T, owner string) (records, comments int) {
		t.Helper()
		records, comments, _ = rrsetCounts(ctx, t, client, zoneName, owner, "A")
		return records, comments
	}
	if records, comments := counts(t, "kept"); records != 1 || comments != 3 {
		t.Fatalf("kept starts at records=%d comments=%d, want 1 and 3", records, comments)
	}

	// "zz" sorts after "kept", so the pair is applied before PowerDNS reaches
	// the record it refuses. Sorted the other way the patch would abort first
	// and the test would pass having proved nothing.
	err := client.applyRRSetPatch(ctx, zoneName, []rrset{
		newDeleteRRSet(QualifyOwner("kept", zoneName), "A"),
		{
			Name:       QualifyOwner("zz", zoneName),
			Type:       "A",
			TTL:        300,
			ChangeType: changeTypeReplace,
			Records:    []rrsetRecord{{Content: "not-an-address"}},
		},
	})
	if err == nil {
		t.Fatal("PowerDNS accepted an unparseable A record, so this patch proves nothing")
	}

	if records, comments := counts(t, "kept"); records != 1 || comments != 3 {
		t.Fatalf("the failed patch left records=%d comments=%d, want the RRset untouched at 1 and 3: "+
			"a clear committed without its delete is the state this design assumes cannot exist", records, comments)
	}

	// Positive control: a reader blind to deletes would report 1 and 3 anyway.
	if err := client.DeleteRRSet(ctx, zoneName, "A", "kept"); err != nil {
		t.Fatalf("DeleteRRSet: %v", err)
	}
	if records, comments := counts(t, "kept"); records != 0 || comments != 0 {
		t.Fatalf("a delete that must land left records=%d comments=%d, so the reading above was blind", records, comments)
	}
}

// TestPDNS_LMDB_ARecordSetLeavesTheCommentCountWhereItFoundIt is #158's
// acceptance criterion stated the way the issue states it: "Creating a record
// set and deleting it leaves the zone's comment count unchanged."
//
// It counts the whole zone rather than one name, so a shell stranded at any
// name fails it, including one this test never mentions.
func TestPDNS_LMDB_ARecordSetLeavesTheCommentCountWhereItFoundIt(t *testing.T) {
	// No t.Parallel(): container + real PDNS.
	baseURL, apiKey, stop := startPDNSLMDB(t)
	defer stop()

	client := NewClient(baseURL, apiKey)
	ctx := context.Background()
	const zoneName = "count.test"
	zone := dnsv1alpha1.DNSZone{Spec: dnsv1alpha1.DNSZoneSpec{DomainName: zoneName}}

	if err := client.CreateZone(ctx, zoneName, []string{"ns1.example.net", "ns2.example.net"}); err != nil {
		t.Fatalf("CreateZone: %v", err)
	}
	before := zoneCommentTotal(ctx, t, client, zoneName)

	recordSet := aRecordSet(1, "www", "api", "mail")
	if _, err := client.EnsureRecordSet(ctx, zone, recordSet); err != nil {
		t.Fatalf("EnsureRecordSet: %v", err)
	}

	// The record set must actually put comments in the zone, or deleting it
	// would leave the count unchanged for the wrong reason.
	during := zoneCommentTotal(ctx, t, client, zoneName)
	if during <= before {
		t.Fatalf("the zone holds %d comments after writing the record set and held %d before, "+
			"so this test cannot tell a clean delete from a write that never happened", during, before)
	}

	if err := client.DeleteRecordSet(ctx, zone, recordSet); err != nil {
		t.Fatalf("DeleteRecordSet: %v", err)
	}

	if after := zoneCommentTotal(ctx, t, client, zoneName); after != before {
		t.Fatalf("the zone holds %d comments after the record set was created and deleted, and held %d "+
			"before it existed; %d were stranded", after, before, after-before)
	}
}

// TestPDNS_LMDB_ARecreatedZoneComesBackWithoutComments is #172's acceptance
// criterion: a zone deleted and created again under the same name comes back
// with no comments on it.
func TestPDNS_LMDB_ARecreatedZoneComesBackWithoutComments(t *testing.T) {
	// No t.Parallel(): container + real PDNS.
	baseURL, apiKey, stop := startPDNSLMDB(t)
	defer stop()

	client := NewClient(baseURL, apiKey)
	ctx := context.Background()
	nameservers := []string{"ns1.example.net", "ns2.example.net"}

	// recreate writes one record set into a new zone, deletes the zone the way
	// it is told to, and creates it again. It returns the comments the new zone
	// holds before anything has been written to it.
	recreate := func(t *testing.T, zoneName string, deleteZone func(dnsv1alpha1.DNSZone) error) int {
		t.Helper()
		zone := dnsv1alpha1.DNSZone{Spec: dnsv1alpha1.DNSZoneSpec{DomainName: zoneName}}
		if err := client.CreateZone(ctx, zoneName, nameservers); err != nil {
			t.Fatalf("CreateZone: %v", err)
		}
		if _, err := client.EnsureRecordSet(ctx, zone, aRecordSet(1, "www", "shell")); err != nil {
			t.Fatalf("EnsureRecordSet: %v", err)
		}
		// A bare DELETE leaves a shell: no records and all three comments, which
		// is what a zone delete meets in practice.
		if err := sendRaw(ctx, t, client, http.MethodPatch, zoneName,
			fmt.Sprintf(`{"rrsets":[{"name":%q,"type":"A","changetype":"DELETE"}]}`,
				QualifyOwner("shell", zoneName))); err != nil {
			t.Fatalf("leave a shell: %v", err)
		}
		if got := zoneCommentTotal(ctx, t, client, zoneName); got != 6 {
			t.Fatalf("%s holds %d comments before its delete, want 6", zoneName, got)
		}
		if err := deleteZone(zone); err != nil {
			t.Fatalf("deleting %s: %v", zoneName, err)
		}
		// CreateZone accepts a zone that already exists, so a delete that never
		// happened would otherwise read as a clean recreate.
		if _, err := client.GetZone(ctx, zoneName); !errors.Is(err, dnserrors.ErrZoneNotFound) {
			t.Fatalf("%s is still there after its delete: GetZone returned %v", zoneName, err)
		}
		if err := client.CreateZone(ctx, zoneName, nameservers); err != nil {
			t.Fatalf("CreateZone again: %v", err)
		}
		return zoneCommentTotal(ctx, t, client, zoneName)
	}

	// The control: the zone DELETE on its own, as the API reference describes it.
	bare := func(zone dnsv1alpha1.DNSZone) error {
		return sendRaw(ctx, t, client, http.MethodDelete, zone.Spec.DomainName, "")
	}
	if got := recreate(t, "control.test", bare); got != 6 {
		t.Fatalf("the control came back with %d comments, want 6: this backend does not show the defect, "+
			"so the reading below is not evidence", got)
	}

	viaClient := func(zone dnsv1alpha1.DNSZone) error { return client.DeleteZone(ctx, zone) }
	if got := recreate(t, "fixed.test", viaClient); got != 0 {
		t.Fatalf("the zone came back with %d comments left by the zone deleted before it", got)
	}
}

// TestPDNS_LMDB_AZoneDeleteThatFailsKeepsTheRecords covers the one state that
// clearing first creates: the comments are gone and the zone is not.
//
// A proxy in front of PowerDNS refuses the zone DELETE, so the clear lands and
// the delete does not. The records must survive it, and the retry must find
// nothing left to clear.
func TestPDNS_LMDB_AZoneDeleteThatFailsKeepsTheRecords(t *testing.T) {
	// No t.Parallel(): container + real PDNS.
	baseURL, apiKey, stop := startPDNSLMDB(t)
	defer stop()

	backend, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse %s: %v", baseURL, err)
	}
	forward := httputil.NewSingleHostReverseProxy(backend)
	var refuseDelete atomic.Bool
	var patches atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && refuseDelete.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.Method == http.MethodPatch {
			patches.Add(1)
		}
		forward.ServeHTTP(w, r)
	}))
	defer proxy.Close()

	client := NewClient(proxy.URL, apiKey)
	ctx := context.Background()
	const zoneName = "refused.test"
	zone := dnsv1alpha1.DNSZone{Spec: dnsv1alpha1.DNSZoneSpec{DomainName: zoneName}}

	if err := client.CreateZone(ctx, zoneName, []string{"ns1.example.net", "ns2.example.net"}); err != nil {
		t.Fatalf("CreateZone: %v", err)
	}
	if _, err := client.EnsureRecordSet(ctx, zone, aRecordSet(1, "www")); err != nil {
		t.Fatalf("EnsureRecordSet: %v", err)
	}

	refuseDelete.Store(true)
	patches.Store(0)
	if err := client.DeleteZone(ctx, zone); err == nil {
		t.Fatal("DeleteZone reported success for a zone DELETE the proxy refused")
	}
	if got := patches.Load(); got == 0 {
		t.Fatal("DeleteZone sent no clear before the refused delete, so the reading below proves nothing")
	}
	if records, comments, _ := rrsetCounts(ctx, t, client, zoneName, "www", "A"); records != 1 || comments != 0 {
		t.Fatalf("after the clear and a refused delete, www holds records=%d comments=%d, want 1 and 0",
			records, comments)
	}

	refuseDelete.Store(false)
	patches.Store(0)
	if err := client.DeleteZone(ctx, zone); err != nil {
		t.Fatalf("DeleteZone on the retry: %v", err)
	}
	if got := patches.Load(); got != 0 {
		t.Fatalf("the retry sent %d PATCHes to a zone with no comments left, want none", got)
	}
	if _, err := client.GetZone(ctx, zoneName); !errors.Is(err, dnserrors.ErrZoneNotFound) {
		t.Fatalf("the zone is still there after the retry: GetZone returned %v", err)
	}
}
