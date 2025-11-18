// SPDX-License-Identifier: AGPL-3.0-only
package discovery

import (
	"context"
	"fmt"

	"github.com/miekg/dns"
	"github.com/projectdiscovery/dnsx/libs/dnsx"
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// DiscoverZoneRecords performs best-effort discovery of common RR types for the given domain
// and returns RecordSets grouped by RecordType, with typed fields populated where available.
func DiscoverZoneRecords(ctx context.Context, domain string) ([]dnsv1alpha1.DiscoveredRecordSet, error) {
	fmt.Printf("starting discovery for domain=%q\n", domain)
	// Exclude NS and SOA per requirements.
	options := dnsx.DefaultOptions
	qtypes := []uint16{
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
	options.QuestionTypes = qtypes
	options.QueryAll = true

	fmt.Println("creating dnsx client")
	client, err := dnsx.New(options)
	if err != nil {
		return nil, err
	}
	fmt.Println("querying multiple record types")
	resp, err := client.QueryMultiple(domain)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("dnsx returned nil response")
	}
	// Print quick summary for debugging
	fmt.Printf("resolver status=%s answers=%d types=%v\n", resp.StatusCode, len(resp.AllRecords), qtypes)

	// Build RRs from textual records since QueryMultiple does not populate RawResp
	typeToRRs := make(map[uint16][]dns.RR)
	for _, rec := range resp.AllRecords {
		rr, perr := dns.NewRR(rec)
		if perr != nil || rr == nil {
			continue
		}
		rt := rr.Header().Rrtype
		typeToRRs[rt] = append(typeToRRs[rt], rr)
	}

	typeToEntries := make(map[dnsv1alpha1.RRType][]dnsv1alpha1.RecordEntry)
	for _, qt := range qtypes {
		answers := typeToRRs[qt]
		if len(answers) == 0 {
			continue
		}
		entries := mapAnswersToEntries(domain, answers, qt)
		if len(entries) == 0 {

			continue
		}
		if rt, ok := mapQtypeToRRType(qt); ok {
			typeToEntries[rt] = append(typeToEntries[rt], entries...)
		}
	}

	out := make([]dnsv1alpha1.DiscoveredRecordSet, 0, len(typeToEntries))
	for rt, recs := range typeToEntries {
		out = append(out, dnsv1alpha1.DiscoveredRecordSet{
			RecordType: rt,
			Records:    recs,
		})
	}
	return out, nil
}

func mapQtypeToRRType(qt uint16) (dnsv1alpha1.RRType, bool) {
	switch qt {
	case dns.TypeA:
		return dnsv1alpha1.RRTypeA, true
	case dns.TypeAAAA:
		return dnsv1alpha1.RRTypeAAAA, true
	case dns.TypeCNAME:
		return dnsv1alpha1.RRTypeCNAME, true
	case dns.TypeTXT:
		return dnsv1alpha1.RRTypeTXT, true
	case dns.TypeMX:
		return dnsv1alpha1.RRTypeMX, true
	case dns.TypeSRV:
		return dnsv1alpha1.RRTypeSRV, true
	case dns.TypeNS:
		return dnsv1alpha1.RRTypeNS, true
	case dns.TypeSOA:
		return dnsv1alpha1.RRTypeSOA, true
	case dns.TypeCAA:
		return dnsv1alpha1.RRTypeCAA, true
	case dns.TypeTLSA:
		return dnsv1alpha1.RRTypeTLSA, true
	case dns.TypeHTTPS:
		return dnsv1alpha1.RRTypeHTTPS, true
	case dns.TypeSVCB:
		return dnsv1alpha1.RRTypeSVCB, true
	default:
		return "", false
	}
}
