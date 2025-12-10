// SPDX-License-Identifier: AGPL-3.0-only
package discovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/projectdiscovery/dnsx/libs/dnsx"
	"github.com/projectdiscovery/retryabledns"
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

const (
	discoveryLookupConcurrency = 8
	discoveryLookupTimeout     = 5 * time.Second
)

var queryRecordsFunc = queryRecordsForName

// commonDiscoverySubdomains captures a curated list of host/service labels that we want
// to probe in addition to the zone apex. The goal is to cover the most frequent records
// seen across customer zones (web/mobile properties, mail/exchange, voice/video, etc.).
// The list is intentionally opinionated and can grow as we observe additional patterns.
var commonDiscoverySubdomains = []string{
	"", // zone apex

	// Web/mobile properties
	"www",
	"m",
	"api",
	"app",
	"beta",
	"dev",
	"stage",
	"staging",
	"test",
	"preview",
	"admin",
	"portal",
	"dashboard",
	"login",
	"auth",
	"sso",
	"cdn",
	"static",
	"assets",
	"media",
	"img",
	"files",
	"support",
	"help",
	"status",

	// Remote access / infra
	"vpn",
	"remote",
	"intranet",
	"edge",

	// Email / collaboration
	"mail",
	"smtp",
	"imap",
	"pop",
	"pop3",
	"autodiscover",
	"_autodiscover._tcp",
	"_dmarc",
	"_mta-sts",

	// SIP/voice and chat services
	"_sip._tcp",
	"_sipfederationtls._tcp",
	"_sipinternaltls._tcp",
	"_xmpp-client._tcp",
	"_xmpp-server._tcp",

	// Secure mail submission / IMAP
	"_imap._tcp",
	"_imaps._tcp",
	"_submission._tcp",

	// Legacy transfer
	"ftp",
	"sftp",
}

// DiscoverZoneRecords performs best-effort discovery of common RR types for the given domain
// and returns RecordSets grouped by RecordType, with typed fields populated where available.
func DiscoverZoneRecords(ctx context.Context, domain string) ([]dnsv1alpha1.DiscoveredRecordSet, error) {
	fmt.Printf("starting discovery for domain=%q\n", domain)
	candidateNames := candidateDiscoveryNames(domain)
	if len(candidateNames) == 0 {
		return nil, fmt.Errorf("no discovery candidates produced for domain %q", domain)
	}
	fmt.Printf("querying %d candidate names for domain=%q\n", len(candidateNames), domain)
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

	typeToRRs := make(map[uint16][]dns.RR)
	var mu sync.Mutex
	mergeResponses := func(rrs map[uint16][]dns.RR) {
		if len(rrs) == 0 {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for rt, entries := range rrs {
			typeToRRs[rt] = append(typeToRRs[rt], entries...)
		}
	}

	apexRRs, err := queryRecordsFunc(ctx, candidateNames[0], client, qtypes)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			fmt.Printf("timed out querying apex fqdn=%q after %s; treating as empty\n", candidateNames[0], discoveryLookupTimeout)
		} else {
			return nil, err
		}
	} else {
		mergeResponses(apexRRs)
	}

	if len(candidateNames) > 1 {
		var wg sync.WaitGroup
		sem := make(chan struct{}, discoveryLookupConcurrency)

		for _, name := range candidateNames[1:] {
			name := name
			wg.Add(1)
			go func() {
				defer wg.Done()

				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					fmt.Printf("context canceled before querying fqdn=%q\n", name)
					return
				}
				defer func() { <-sem }()

				rrs, qerr := queryRecordsFunc(ctx, name, client, qtypes)
				if qerr != nil {
					switch {
					case errors.Is(qerr, context.DeadlineExceeded):
						fmt.Printf("timed out querying fqdn=%q after %s; treating as empty\n", name, discoveryLookupTimeout)
					case errors.Is(qerr, context.Canceled):
						fmt.Printf("context canceled before completing lookup for fqdn=%q\n", name)
					default:
						fmt.Printf("skipping fqdn=%q due to query error: %v\n", name, qerr)
					}
					return
				}
				mergeResponses(rrs)
			}()
		}

		wg.Wait()
	}

	typeToEntries := make(map[dnsv1alpha1.RRType][]dnsv1alpha1.RecordEntry)
	for _, qt := range qtypes {
		answers := typeToRRs[qt]
		if len(answers) == 0 {
			continue
		}
		entries := mapAnswersToEntries(domain, answers)
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

func queryRecordsForName(ctx context.Context, name string, client *dnsx.DNSX, qtypes []uint16) (map[uint16][]dns.RR, error) {
	fmt.Printf("querying multiple record types for fqdn=%q\n", name)

	type queryResult struct {
		resp *retryabledns.DNSData
		err  error
	}

	resCh := make(chan queryResult, 1)
	go func() {
		resp, err := client.QueryMultiple(name)
		resCh <- queryResult{resp: resp, err: err}
	}()

	timeoutCtx, cancel := context.WithTimeout(ctx, discoveryLookupTimeout)
	defer cancel()

	select {
	case <-timeoutCtx.Done():
		return nil, timeoutCtx.Err()
	case res := <-resCh:
		if res.err != nil {
			return nil, res.err
		}
		if res.resp == nil {
			return nil, fmt.Errorf("dnsx returned nil response")
		}
		fmt.Printf("resolver status=%s answers=%d fqdn=%s types=%v\n", res.resp.StatusCode, len(res.resp.AllRecords), name, qtypes)

		typeToRRs := make(map[uint16][]dns.RR)
		for _, rec := range res.resp.AllRecords {
			rr, perr := dns.NewRR(rec)
			if perr != nil || rr == nil {
				continue
			}
			rt := rr.Header().Rrtype
			typeToRRs[rt] = append(typeToRRs[rt], rr)
		}
		return typeToRRs, nil
	}
}

func candidateDiscoveryNames(domain string) []string {
	base := strings.TrimSpace(domain)
	base = strings.TrimSuffix(base, ".")
	if base == "" {
		return nil
	}

	seen := make(map[string]struct{}, len(commonDiscoverySubdomains))
	names := make([]string, 0, len(commonDiscoverySubdomains))
	for _, label := range commonDiscoverySubdomains {
		var fqdn string
		switch label {
		case "", "@":
			fqdn = base
		default:
			fqdn = fmt.Sprintf("%s.%s", label, base)
		}
		if _, exists := seen[fqdn]; exists {
			continue
		}
		seen[fqdn] = struct{}{}
		names = append(names, fqdn)
	}
	return names
}
