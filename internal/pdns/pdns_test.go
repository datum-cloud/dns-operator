package pdns

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

const (
	ns1ExampleNet = "ns1.example.net."
	exampleCom    = "example.com."
)

func TestCreateGetDeleteZoneAndRRSets(t *testing.T) {
	t.Parallel()

	var lastReq struct {
		Method  string
		URL     string
		Body    []byte
		Headers http.Header
	}

	// minimal fake PDNS
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/servers/localhost/zones", func(w http.ResponseWriter, r *http.Request) {
		lastReq.Method = r.Method
		lastReq.URL = r.URL.String()
		lastReq.Headers = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		lastReq.Body = body

		if r.Method == http.MethodPost {
			// Accept creation
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/api/v1/servers/localhost/zones/example.com.", func(w http.ResponseWriter, r *http.Request) {
		lastReq.Method = r.Method
		lastReq.URL = r.URL.String()
		lastReq.Headers = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		lastReq.Body = body

		switch r.Method {
		case http.MethodGet:
			// Return a zone with two rrsets
			resp := zoneResponse{
				Name: exampleCom,
				RRSets: []zoneRRset{
					{Name: exampleCom, Type: "A", TTL: 300, Records: []zoneRRsetRecord{{Content: "1.2.3.4"}}},
					{Name: "www.example.com.", Type: "CNAME", TTL: 300, Records: []zoneRRsetRecord{{Content: "target"}}},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPatch:
			// accept any patch; return 204
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL, "sekret")

	// CreateZone should wrap inputs correctly (trailing dots on zone/ns) and set headers
	if err := c.CreateZone(context.Background(), "example.com", []string{"ns1.example.com", "ns2.example.com."}); err != nil {
		t.Fatalf("CreateZone error: %v", err)
	}
	if lastReq.Method != "POST" || lastReq.URL != "/api/v1/servers/localhost/zones" {
		t.Fatalf("CreateZone wrong request: %s %s", lastReq.Method, lastReq.URL)
	}
	if got := lastReq.Headers.Get("X-API-Key"); got != "sekret" {
		t.Fatalf("missing/incorrect api key header: %q", got)
	}
	var cz createZoneRequest
	if err := json.Unmarshal(lastReq.Body, &cz); err != nil {
		t.Fatalf("CreateZone body decode: %v", err)
	}
	if cz.Name != exampleCom {
		t.Fatalf("CreateZone name: got %q want %q", cz.Name, exampleCom)
	}
	if !reflect.DeepEqual(cz.Nameservers, []string{"ns1.example.com.", "ns2.example.com."}) {
		t.Fatalf("CreateZone nameservers: got %#v", cz.Nameservers)
	}

	// GetZone
	z, err := c.GetZone(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("GetZone error: %v", err)
	}
	if z != exampleCom {
		t.Fatalf("GetZone name: got %q", z)
	}

	// GetZoneRRSets
	rrs, err := c.GetZoneRRSets(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("GetZoneRRSets error: %v", err)
	}
	if len(rrs) != 2 {
		t.Fatalf("GetZoneRRSets len: got %d", len(rrs))
	}

	// DeleteZone
	if err := c.DeleteZone(context.Background(), "example.com"); err != nil {
		t.Fatalf("DeleteZone error: %v", err)
	}
}

func TestBuildRRSets_NormalizationAndFormats(t *testing.T) {
	t.Parallel()

	ttl := int64(120)

	// A
	rsA := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{
				{
					Name: "www",
					TTL:  &ttl,
					A:    &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"},
				},
			},
		},
	}
	rr := buildRRSets("example.com", rsA)
	if len(rr) != 1 || rr[0].Type != "A" || rr[0].Name != "www.example.com." || rr[0].TTL != 120 || rr[0].Records[0].Content != "1.2.3.4" {
		t.Fatalf("A rrset unexpected: %#v", rr)
	}

	// AAAA
	rsAAAA := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeAAAA,
			Records: []dnsv1alpha1.RecordEntry{
				{
					Name: "www",
					TTL:  &ttl,
					AAAA: &dnsv1alpha1.AAAARecordSpec{Content: "2001:db8::1"},
				},
			},
		},
	}
	rr = buildRRSets("example.com", rsAAAA)
	if rr[0].Type != "AAAA" || rr[0].Records[0].Content != "2001:db8::1" {
		t.Fatalf("AAAA rrset unexpected: %#v", rr)
	}

	// CNAME
	rsCNAME := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeCNAME,
			Records: []dnsv1alpha1.RecordEntry{
				{
					Name:  "www",
					TTL:   &ttl,
					CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "alias.example.net."},
				},
			},
		},
	}
	rr = buildRRSets("example.com", rsCNAME)
	if rr[0].Type != "CNAME" || rr[0].Records[0].Content != "alias.example.net." {
		t.Fatalf("CNAME rrset unexpected: %#v", rr)
	}

	// ALIAS (PowerDNS-specific): normalize target to absolute (trailing dot)
	rsALIAS := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeALIAS,
			Records: []dnsv1alpha1.RecordEntry{
				{
					Name:  "@",
					TTL:   &ttl,
					ALIAS: &dnsv1alpha1.ALIASRecordSpec{Content: "target.example.net"},
				},
			},
		},
	}
	rr = buildRRSets("example.com", rsALIAS)
	if rr[0].Type != "ALIAS" || rr[0].Name != "example.com." || rr[0].Records[0].Content != "target.example.net." {
		t.Fatalf("ALIAS rrset unexpected: %#v", rr)
	}

	// TXT: quoted
	rsTXT := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeTXT,
			Records: []dnsv1alpha1.RecordEntry{
				{
					Name: "www",
					TTL:  &ttl,
					TXT:  &dnsv1alpha1.TXTRecordSpec{Content: "hello"},
				},
			},
		},
	}
	rr = buildRRSets("example.com", rsTXT)
	got := rr[0].Records[0].Content
	if got != `"hello"` {
		t.Fatalf("TXT quoting unexpected: %q", got)
	}

	// MX: PDNS payload uses absolute exchange (trailing dot)
	rsMX := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeMX,
			Records: []dnsv1alpha1.RecordEntry{
				{
					Name: "mail",
					TTL:  &ttl,
					MX:   &dnsv1alpha1.MXRecordSpec{Preference: 10, Exchange: "mail.example.com."},
				},
			},
		},
	}
	rr = buildRRSets("example.com", rsMX)
	if rr[0].Type != "MX" || rr[0].Records[0].Content != "10 mail.example.com." {
		t.Fatalf("MX rrset unexpected: %#v", rr)
	}

	// SRV: PDNS payload uses an absolute target with trailing dot
	rsSRV := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeSRV,
			Records: []dnsv1alpha1.RecordEntry{
				{
					Name: "srv",
					TTL:  &ttl,
					SRV:  &dnsv1alpha1.SRVRecordSpec{Priority: 0, Weight: 5, Port: 443, Target: "svc.example.com."},
				},
			},
		},
	}
	rr = buildRRSets("example.com", rsSRV)
	if rr[0].Type != "SRV" || rr[0].Records[0].Content != "0 5 443 svc.example.com." {
		t.Fatalf("SRV rrset unexpected: %#v", rr)
	}

	// CAA: quoted value
	rsCAA := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeCAA,
			Records: []dnsv1alpha1.RecordEntry{
				{
					Name: "www",
					TTL:  &ttl,
					CAA:  &dnsv1alpha1.CAARecordSpec{Flag: 0, Tag: "issue", Value: "letsencrypt.org"},
				},
			},
		},
	}
	rr = buildRRSets("example.com", rsCAA)
	if rr[0].Type != "CAA" || rr[0].Records[0].Content != `0 issue "letsencrypt.org"` {
		t.Fatalf("CAA rrset unexpected: %#v", rr)
	}

	// NS: multiple entries for same owner should group into a single rrset with multiple records
	rsNS := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeNS,
			Records: []dnsv1alpha1.RecordEntry{
				{
					Name: "@",
					TTL:  &ttl,
					NS:   &dnsv1alpha1.NSRecordSpec{Content: ns1ExampleNet},
				},
				{
					Name: "@",
					NS:   &dnsv1alpha1.NSRecordSpec{Content: "ns2.example.net."},
				},
			},
		},
	}
	rr = buildRRSets("example.com", rsNS)
	if len(rr) != 1 {
		t.Fatalf("NS rrset grouping unexpected len: %#v", rr)
	}
	nsGot := []string{rr[0].Records[0].Content, rr[0].Records[1].Content}
	// PDNS payload uses absolute hostnames (with trailing dots)
	if rr[0].Type != "NS" || nsGot[0] != ns1ExampleNet || nsGot[1] != "ns2.example.net." {
		t.Fatalf("NS rrset unexpected: %#v", rr)
	}

	// SOA
	rsSOA := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeSOA,
			Records: []dnsv1alpha1.RecordEntry{
				{
					Name: "@",
					TTL:  &ttl,
					SOA: &dnsv1alpha1.SOARecordSpec{
						MName:  ns1ExampleNet,
						RName:  "hostmaster.example.net.",
						TTL:    3600,
						Serial: 0, // trigger auto
					},
				},
			},
		},
	}
	rr = buildRRSets("example.com", rsSOA)
	if rr[0].Type != "SOA" {
		t.Fatalf("SOA type unexpected: %#v", rr)
	}
	parts := strings.Fields(rr[0].Records[0].Content)

	// PDNS payload uses absolute hostnames (with trailing dots) for mname/rname
	if len(parts) != 7 || parts[0] != ns1ExampleNet || parts[1] != "hostmaster.example.net." {
		t.Fatalf("SOA content unexpected: %q", rr[0].Records[0].Content)
	}

	// serial like yyyymmddNN (we use %s01)
	if len(parts[2]) != 10 {
		t.Fatalf("SOA serial shape unexpected: %q", parts[2])
	}

	// PTR: normalize target to absolute (trailing dot)
	rsPTR := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypePTR,
			Records: []dnsv1alpha1.RecordEntry{
				{
					Name: "ptrhost",
					TTL:  &ttl,
					PTR:  &dnsv1alpha1.PTRRecordSpec{Content: "target.example.net"},
				},
			},
		},
	}
	rr = buildRRSets("example.com", rsPTR)
	if rr[0].Type != "PTR" || rr[0].Records[0].Content != "target.example.net." {
		t.Fatalf("PTR rrset unexpected: %#v", rr)
	}

	// SSHFP
	rsSSHFP := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeSSHFP,
			Records: []dnsv1alpha1.RecordEntry{
				{
					Name:  "ssh",
					TTL:   &ttl,
					SSHFP: &dnsv1alpha1.SSHFPRecordSpec{Algorithm: 1, Type: 1, Fingerprint: "abcdef"},
				},
			},
		},
	}
	rr = buildRRSets("example.com", rsSSHFP)
	if rr[0].Type != "SSHFP" || rr[0].Records[0].Content != "1 1 abcdef" {
		t.Fatalf("SSHFP rrset unexpected: %#v", rr)
	}

	// NAPTR (quoted flags/services/regexp; replacement preserves "." literal)
	rsNAPTR := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeNAPTR,
			Records: []dnsv1alpha1.RecordEntry{
				{
					Name:  "naptr",
					TTL:   &ttl,
					NAPTR: &dnsv1alpha1.NAPTRRecordSpec{Order: 100, Preference: 10, Flags: "U", Services: "E2U+sip", Regexp: "!^.*$!sip:info@example.com!", Replacement: "."},
				},
			},
		},
	}
	rr = buildRRSets("example.com", rsNAPTR)
	if rr[0].Type != "NAPTR" || rr[0].Records[0].Content != `100 10 "U" "E2U+sip" "!^.*$!sip:info@example.com!" .` {
		t.Fatalf("NAPTR rrset unexpected: %#v", rr)
	}

	// TLSA: straight join
	rsTLSA := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeTLSA,
			Records: []dnsv1alpha1.RecordEntry{
				{
					Name: "tlsa",
					TTL:  &ttl,
					TLSA: &dnsv1alpha1.TLSARecordSpec{Usage: 3, Selector: 1, MatchingType: 1, CertData: "ABCD"},
				},
			},
		},
	}
	rr = buildRRSets("example.com", rsTLSA)
	if rr[0].Records[0].Content != "3 1 1 ABCD" {
		t.Fatalf("TLSA rrset unexpected: %#v", rr)
	}

	// HTTPS (SVCB) – alias form only; params ignored for priority 0
	rsHTTPS := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeHTTPS,
			Records: []dnsv1alpha1.RecordEntry{
				{
					Name: "svc",
					TTL:  &ttl,
					HTTPS: &dnsv1alpha1.HTTPSRecordSpec{
						Priority: 0,
						Target:   "alias.example.net.",
						Params:   map[string]string{"no-default-alpn": ""}, // params should be ignored
					},
				},
			},
		},
	}
	rr = buildRRSets("example.com", rsHTTPS)
	if rr[0].Type != "HTTPS" {
		t.Fatalf("HTTPS type unexpected: %#v", rr)
	}
	if len(rr[0].Records) != 1 {
		t.Fatalf("HTTPS rrset expected single record, got %#v", rr[0].Records)
	}
	if got := rr[0].Records[0].Content; got != "0 alias.example.net." {
		t.Fatalf("HTTPS alias form unexpected: %q", got)
	}

	// SVCB mirrors HTTPS behavior for a simple alias record
	rsSVCB := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeSVCB,
			Records: []dnsv1alpha1.RecordEntry{
				{
					Name: "svc",
					TTL:  &ttl,
					SVCB: &dnsv1alpha1.HTTPSRecordSpec{
						Priority: 0,
						Target:   "alias.example.net.",
						Params:   map[string]string{"no-default-alpn": ""},
					},
				},
			},
		},
	}
	rr = buildRRSets("example.com", rsSVCB)
	if rr[0].Type != "SVCB" {
		t.Fatalf("SVCB type unexpected: %#v", rr)
	}
	if got := rr[0].Records[0].Content; got != "0 alias.example.net." {
		t.Fatalf("SVCB alias form unexpected: %q", got)
	}
}

func TestEncodeSvcbParamsAndLine(t *testing.T) {
	t.Parallel()

	// flags only
	if got := encodeSvcbParams(map[string]string{"no-default-alpn": ""}); got != "no-default-alpn" {
		t.Fatalf("flag encoding: %q", got)
	}
	// quoted + csv
	p := encodeSvcbParams(map[string]string{
		"alpn":     "h2,h3",
		"ipv4hint": "1.2.3.4,5.6.7.8",
		"esnikeys": "abc",
		"port":     "443",
		"unknown":  "v", // default quoted
	})
	// order is deterministic (sorted keys)
	wantParts := []string{`alpn=h2,h3`, `esnikeys="abc"`, `ipv4hint=1.2.3.4,5.6.7.8`, `port=443`, `unknown="v"`}
	for _, w := range wantParts {
		if !strings.Contains(p, w) {
			t.Fatalf("missing part %q in %q", w, p)
		}
	}

	// alias form (priority 0): no params, target normalized
	if got := encodeSvcbLine(0, "name.example.", map[string]string{"alpn": "h2"}); got != "0 name.example." {
		t.Fatalf("alias form wrong: %q", got)
	}

	// service form with "." target and params
	got := encodeSvcbLine(1, ".", map[string]string{"no-default-alpn": "", "alpn": "h2"})
	if got != `1 . no-default-alpn alpn=h2` && got != `1 . alpn=h2 no-default-alpn` {
		t.Fatalf("service form wrong: %q", got)
	}
}

func TestApplyRecordSetAuthoritative_PatchIncludesDeletes(t *testing.T) {
	t.Parallel()

	// Zone has an existing A rrset for "old.example.com."
	existing := zoneResponse{
		Name: exampleCom,
		RRSets: []zoneRRset{
			{Name: "old.example.com.", Type: "A", TTL: 300, Records: []zoneRRsetRecord{{Content: "9.9.9.9"}}},
			{Name: "keep.example.com.", Type: "TXT", TTL: 300, Records: []zoneRRsetRecord{{Content: `"ok"`}}},
		},
	}

	var capturedPatch patchZoneRequest

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/servers/localhost/zones/example.com.", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(existing)
		case http.MethodPatch:
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &capturedPatch)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	s := httptest.NewServer(mux)
	defer s.Close()

	c := NewClient(s.URL, "k")
	ttl := int64(60)
	// desired: A rrset for "new.example.com." only
	rs := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "new", TTL: &ttl, A: &dnsv1alpha1.ARecordSpec{Content: "1.1.1.1"}},
			},
		},
	}

	if err := c.ApplyRecordSetAuthoritative(context.Background(), "example.com", rs); err != nil {
		t.Fatalf("ApplyRecordSetAuthoritative error: %v", err)
	}

	// Patch must contain:
	//  - REPLACE for new.example.com. A
	//  - DELETE for old.example.com. A
	// (TXT rrset must be untouched)
	var hasReplaceNew, hasDeleteOld bool
	for _, r := range capturedPatch.RRSets {
		if r.Type == "A" && r.Name == "new.example.com." && r.ChangeType == "REPLACE" {
			hasReplaceNew = true
			if len(r.Records) != 1 || r.Records[0].Content != "1.1.1.1" {
				t.Fatalf("replace content wrong: %#v", r)
			}
		}
		if r.Type == "A" && r.Name == "old.example.com." && r.ChangeType == "DELETE" {
			hasDeleteOld = true
		}
	}
	if !hasReplaceNew || !hasDeleteOld {
		t.Fatalf("patch missing expected operations: %#v", capturedPatch.RRSets)
	}
}

func TestHelpers(t *testing.T) {
	t.Parallel()

	if got := quoteIfNeeded("x"); got != `"x"` {
		t.Fatalf("quoteIfNeeded: %q", got)
	}
	if got := quoteIfNeeded(`"x"`); got != `"x"` {
		t.Fatalf("quoteIfNeeded pass-through: %q", got)
	}
	if got := qualifyOwner("@", "example.com"); got != exampleCom {
		t.Fatalf("qualifyOwner @: %q", got)
	}
	if got := qualifyOwner("www", "example.com"); got != "www.example.com." {
		t.Fatalf("qualifyOwner rel: %q", got)
	}
	if got := qualifyOwner("abs.example.", "example.com"); got != "abs.example." {
		t.Fatalf("qualifyOwner abs: %q", got)
	}
}

func TestNewFromEnv(t *testing.T) {
	t.Setenv("PDNS_API_URL", "")
	t.Setenv("PDNS_API_KEY", "")
	t.Setenv("PDNS_API_KEY_FILE", "")

	// missing creds => error
	if _, err := NewFromEnv(); err == nil {
		t.Fatal("expected error when no API key provided")
	}

	t.Setenv("PDNS_API_URL", "http://pdns:8081")
	t.Setenv("PDNS_API_KEY", "abc123")
	cli, err := NewFromEnv()
	if err != nil {
		t.Fatalf("NewFromEnv error: %v", err)
	}
	if cli.BaseURL != "http://pdns:8081" || cli.APIKey != "abc123" {
		t.Fatalf("NewFromEnv result bad: %#v", cli)
	}
}

func TestSOASerialAutoChangesPerDay(t *testing.T) {
	t.Parallel()

	// Set a fixed time base by checking only the prefix (YYYYMMDD)
	// Build an SOA with no explicit Serial -> auto serial "yyyymmdd01"
	rs := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeSOA,
			Records: []dnsv1alpha1.RecordEntry{{
				Name: "@",
				SOA: &dnsv1alpha1.SOARecordSpec{
					MName: ns1ExampleNet,
					RName: "hostmaster.example.net.",
				},
			}},
		},
	}
	rrs := buildRRSets("example.com", rs)
	if len(rrs) != 1 || len(rrs[0].Records) != 1 {
		t.Fatalf("unexpected SOA rrsets: %#v", rrs)
	}
	serial := strings.Fields(rrs[0].Records[0].Content)[2]
	// serial like "yyyymmdd01"
	if len(serial) != 10 {
		t.Fatalf("unexpected serial: %q", serial)
	}
	today := time.Now().Format("20060102")
	if !strings.HasPrefix(serial, today) {
		t.Fatalf("serial not using today's date: %q (want prefix %s)", serial, today)
	}
}

// optional: ensures ApplyRecordSetAuthoritative uses PATCH path properly
func TestApplyRecordSetAuthoritative_PathAndHeaders(t *testing.T) {
	t.Parallel()

	// capture path and method for PATCH; respond to GET with empty rrsets
	var gotMethod, gotPath, gotAPIKey string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(zoneResponse{Name: exampleCom, RRSets: nil})
		case http.MethodPatch:
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotAPIKey = r.Header.Get("X-API-Key")
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer ts.Close()

	c := NewClient(ts.URL, "k3y")
	rs := dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "@", A: &dnsv1alpha1.ARecordSpec{Content: "8.8.8.8"}},
			},
		},
	}
	if err := c.ApplyRecordSetAuthoritative(context.Background(), "example.com", rs); err != nil {
		t.Fatalf("ApplyRecordSetAuthoritative error: %v", err)
	}
	if gotMethod != http.MethodPatch || gotPath != "/api/v1/servers/localhost/zones/example.com." || gotAPIKey != "k3y" {
		t.Fatalf("unexpected patch request: method=%s path=%s key=%s", gotMethod, gotPath, gotAPIKey)
	}
}

// sanity: makeSimpleRRSet keeps values verbatim (used after we normalize)
func TestMakeSimpleRRSet(t *testing.T) {
	t.Parallel()
	rr := makeSimpleRRSet("x.example.", "TXT", 300, []string{`"a"`, `"b"`})
	if rr.Name != "x.example." || rr.Type != "TXT" || rr.TTL != 300 || len(rr.Records) != 2 || rr.Records[0].Content != `"a"` {
		t.Fatalf("unexpected rrset: %#v", rr)
	}
}
