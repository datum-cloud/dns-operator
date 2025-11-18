// SPDX-License-Identifier: AGPL-3.0-only
package discovery

import (
	"context"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/projectdiscovery/dnsx/libs/dnsx"
)

func TestDiscoverZoneRecords_ExampleCom(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const domain = "example.com"
	rs, err := DiscoverZoneRecords(ctx, domain)
	if err != nil {
		t.Fatalf("DiscoverZoneRecords(%q) error: %v", domain, err)
	}
	if len(rs) == 0 {
		t.Fatalf("DiscoverZoneRecords(%q) returned no record sets", domain)
	}

	// Log a summary to aid debugging
	for _, s := range rs {
		t.Logf("recordType=%s records=%d", string(s.RecordType), len(s.Records))
	}

	// Validate that at least one of the common types (A/AAAA/TXT/MX/CNAME/SRV/CAA/TLSA/HTTPS/SVCB) is present.
	seen := make(map[string]bool)
	for _, s := range rs {
		seen[string(s.RecordType)] = true
	}
	ok := seen[dns.TypeToString[dns.TypeA]] ||
		seen[dns.TypeToString[dns.TypeAAAA]] ||
		seen[dns.TypeToString[dns.TypeTXT]] ||
		seen[dns.TypeToString[dns.TypeMX]] ||
		seen[dns.TypeToString[dns.TypeCNAME]] ||
		seen[dns.TypeToString[dns.TypeSRV]] ||
		seen[dns.TypeToString[dns.TypeCAA]] ||
		seen[dns.TypeToString[dns.TypeTLSA]] ||
		seen[dns.TypeToString[dns.TypeHTTPS]] ||
		seen[dns.TypeToString[dns.TypeSVCB]]
	if !ok {
		t.Fatalf("no expected RR types found in discovery for %q; got types: %#v", domain, seen)
	}
}

// This diagnostic test queries via dnsx directly and logs raw RRs so we can see
// what the resolver returns on this environment (helps debug mapping/filters).
func TestDnsxRaw_ExampleCom_Diagnostics(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = ctx

	const domain = "example.com"
	opts := dnsx.DefaultOptions
	opts.QueryAll = true
	opts.QuestionTypes = []uint16{
		dns.TypeA,
		dns.TypeAAAA,
		dns.TypeCNAME,
		dns.TypeTXT,
		dns.TypeMX,
		dns.TypeSRV,
		dns.TypeCAA,
		dns.TypeTLSA,
		dns.TypeHTTPS,
		dns.TypeSVCB,
	}
	client, err := dnsx.New(opts)
	if err != nil {
		t.Fatalf("dnsx init: %v", err)
	}
	resp, err := client.QueryMultiple(domain)
	if err != nil {
		t.Fatalf("dnsx query multiple: %v", err)
	}
	if resp == nil || resp.RawResp == nil {
		t.Fatalf("dnsx response empty for %s", domain)
	}
	t.Logf("raw answer count: %d", len(resp.RawResp.Answer))
	for i, rr := range resp.RawResp.Answer {
		t.Logf("answer[%d]: %s", i, rr.String())
	}
}
