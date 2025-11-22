// SPDX-License-Identifier: AGPL-3.0-only
package discovery

import (
	"strings"

	"github.com/miekg/dns"
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// mapAnswersToEntries converts a slice of dns.RR answers of a single RR type into
// a list of RecordEntry where each entry represents exactly one owner + one
// value for the given RecordType.
//
// It sets typed fields on RecordEntry wherever supported by the API types.
// Unsupported/unknown types (including PTR, since there is no typed PTR field)
// are skipped.
func mapAnswersToEntries(zoneFQDN string, answers []dns.RR) []dnsv1alpha1.RecordEntry {
	out := make([]dnsv1alpha1.RecordEntry, 0, len(answers))

	for _, rr := range answers {
		name := ownerToRelative(rr.Header().Name, zoneFQDN)
		ttl := int64(rr.Header().Ttl)

		entry := dnsv1alpha1.RecordEntry{
			Name: name,
			TTL:  &ttl,
		}

		switch r := rr.(type) {
		case *dns.A:
			entry.A = &dnsv1alpha1.ARecordSpec{
				Content: r.A.String(),
			}

		case *dns.AAAA:
			entry.AAAA = &dnsv1alpha1.AAAARecordSpec{
				Content: r.AAAA.String(),
			}

		case *dns.NS:
			entry.NS = &dnsv1alpha1.NSRecordSpec{
				Content: ensureTrailingDot(r.Ns),
			}

		case *dns.TXT:
			// Join fragments into a single logical TXT string
			entry.TXT = &dnsv1alpha1.TXTRecordSpec{
				Content: strings.Join(r.Txt, ""),
			}

		case *dns.CNAME:
			entry.CNAME = &dnsv1alpha1.CNAMERecordSpec{
				Content: ensureTrailingDot(r.Target),
			}

		case *dns.SOA:
			entry.SOA = &dnsv1alpha1.SOARecordSpec{
				MName:   ensureTrailingDot(r.Ns),
				RName:   ensureTrailingDot(r.Mbox),
				Serial:  r.Serial,
				Refresh: r.Refresh,
				Retry:   r.Retry,
				Expire:  r.Expire,
				TTL:     r.Minttl,
			}

		case *dns.CAA:
			entry.CAA = &dnsv1alpha1.CAARecordSpec{
				Flag:  r.Flag,
				Tag:   r.Tag,
				Value: r.Value,
			}

		case *dns.MX:
			entry.MX = &dnsv1alpha1.MXRecordSpec{
				Preference: r.Preference,
				Exchange:   ensureTrailingDot(r.Mx),
			}

		case *dns.SRV:
			entry.SRV = &dnsv1alpha1.SRVRecordSpec{
				Priority: r.Priority,
				Weight:   r.Weight,
				Port:     r.Port,
				Target:   ensureTrailingDot(r.Target),
			}

		case *dns.TLSA:
			entry.TLSA = &dnsv1alpha1.TLSARecordSpec{
				Usage:        r.Usage,
				Selector:     r.Selector,
				MatchingType: r.MatchingType,
				CertData:     r.Certificate,
			}

		case *dns.HTTPS:
			entry.HTTPS = &dnsv1alpha1.HTTPSRecordSpec{
				Priority: r.Priority,
				Target:   ensureTrailingDot(r.Target),
				Params:   svcbParamsToMap(r.Value),
			}

		case *dns.SVCB:
			entry.SVCB = &dnsv1alpha1.HTTPSRecordSpec{
				Priority: r.Priority,
				Target:   ensureTrailingDot(r.Target),
				Params:   svcbParamsToMap(r.Value),
			}

		default:
			// No typed representation for this RR type (e.g. PTR) -> skip.
			continue
		}

		out = append(out, entry)
	}

	return out
}

func ensureTrailingDot(s string) string {
	if s == "" || strings.HasSuffix(s, ".") {
		return s
	}
	return s + "."
}

func ownerToRelative(owner, zoneFQDN string) string {
	o := strings.TrimSuffix(owner, ".")
	z := strings.TrimSuffix(zoneFQDN, ".")
	if strings.EqualFold(o, z) {
		return "@"
	}
	if strings.HasSuffix(strings.ToLower(o), strings.ToLower("."+z)) {
		return strings.TrimSuffix(o, "."+z)
	}
	return o
}

func svcbParamsToMap(values []dns.SVCBKeyValue) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for _, v := range values {
		// v.String() renders "key=value" or "flag" (no value)
		kv := v.String()
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 {
			out[parts[0]] = parts[1]
		} else if len(parts) == 1 {
			out[parts[0]] = ""
		}
	}
	return out
}
