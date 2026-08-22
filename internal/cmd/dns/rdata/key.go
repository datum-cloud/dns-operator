// SPDX-License-Identifier: AGPL-3.0-only

package rdata

import (
	"fmt"
	"net/netip"
	"strings"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// Key returns a stable identity for the *value* of e — its owner name and TTL
// are deliberately excluded, so `record delete <zone> www A 203.0.113.10` can
// match the value the user named without knowing how it was spelled.
//
// The canonical form folds the differences that do not change what the record
// resolves to: host names lose their trailing dot and their case, IP addresses
// are reduced to their canonical text, hex is uppercased, and service
// parameters are sorted.
func Key(t dnsv1alpha1.RRType, e dnsv1alpha1.RecordEntry) string {
	switch t {
	case dnsv1alpha1.RRTypeA:
		if e.A == nil {
			return ""
		}
		return "A|" + canonIP(e.A.Content)

	case dnsv1alpha1.RRTypeAAAA:
		if e.AAAA == nil {
			return ""
		}
		return "AAAA|" + canonIP(e.AAAA.Content)

	case dnsv1alpha1.RRTypeCNAME:
		if e.CNAME == nil {
			return ""
		}
		return "CNAME|" + canonHost(e.CNAME.Content)

	case dnsv1alpha1.RRTypeALIAS:
		if e.ALIAS == nil {
			return ""
		}
		return "ALIAS|" + canonHost(e.ALIAS.Content)

	case dnsv1alpha1.RRTypeNS:
		if e.NS == nil {
			return ""
		}
		return "NS|" + canonHost(e.NS.Content)

	case dnsv1alpha1.RRTypePTR:
		if e.PTR == nil {
			return ""
		}
		return "PTR|" + canonHost(e.PTR.Content)

	case dnsv1alpha1.RRTypeTXT:
		if e.TXT == nil {
			return ""
		}
		// TXT data is opaque and case-sensitive. Decoded first, so a value
		// read back from the API in wire form matches the same value typed on
		// the command line — without this, delete-by-value silently matches
		// nothing.
		return "TXT|" + txtLogical(e.TXT.Content)

	case dnsv1alpha1.RRTypeMX:
		if e.MX == nil {
			return ""
		}
		return fmt.Sprintf("MX|%d|%s", e.MX.Preference, canonHost(e.MX.Exchange))

	case dnsv1alpha1.RRTypeSRV:
		if e.SRV == nil {
			return ""
		}
		return fmt.Sprintf("SRV|%d|%d|%d|%s",
			e.SRV.Priority, e.SRV.Weight, e.SRV.Port, canonHost(e.SRV.Target))

	case dnsv1alpha1.RRTypeCAA:
		if e.CAA == nil {
			return ""
		}
		return fmt.Sprintf("CAA|%d|%s|%s", e.CAA.Flag, strings.ToLower(e.CAA.Tag), e.CAA.Value)

	case dnsv1alpha1.RRTypeTLSA:
		if e.TLSA == nil {
			return ""
		}
		return fmt.Sprintf("TLSA|%d|%d|%d|%s",
			e.TLSA.Usage, e.TLSA.Selector, e.TLSA.MatchingType,
			strings.ToUpper(strings.TrimSpace(e.TLSA.CertData)))

	case dnsv1alpha1.RRTypeHTTPS:
		if e.HTTPS == nil {
			return ""
		}
		return "HTTPS|" + svcbKey(*e.HTTPS)

	case dnsv1alpha1.RRTypeSVCB:
		if e.SVCB == nil {
			return ""
		}
		return "SVCB|" + svcbKey(*e.SVCB)

	case dnsv1alpha1.RRTypeSOA:
		if e.SOA == nil {
			return ""
		}
		s := *e.SOA
		// The effective values are compared, so an entry that leaves a timer
		// unset matches one that spells out the backend's default.
		return fmt.Sprintf("SOA|%s|%s|%d|%d|%d|%d",
			canonHost(s.MName), canonHost(s.RName),
			orDefault(s.Refresh, soaDefaultRefresh),
			orDefault(s.Retry, soaDefaultRetry),
			orDefault(s.Expire, soaDefaultExpire),
			orDefault(s.TTL, soaDefaultMinimum))
	}
	return ""
}

// Equal reports whether a and b denote the same value of type t, ignoring owner
// name, TTL, trailing dots and host-name case.
func Equal(t dnsv1alpha1.RRType, a, b dnsv1alpha1.RecordEntry) bool {
	ka := Key(t, a)
	if ka == "" {
		return false
	}
	return ka == Key(t, b)
}

func canonHost(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "." {
		return "."
	}
	return strings.TrimSuffix(s, ".")
}

func canonIP(s string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return strings.ToLower(strings.TrimSpace(s))
	}
	return addr.String()
}

func svcbKey(s dnsv1alpha1.HTTPSRecordSpec) string {
	target := canonHost(s.Target)
	if target == "" {
		target = "."
	}
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "%d|%s", s.Priority, target)
	// Alias form carries no parameters, so two alias records that differ only
	// in discarded parameters are the same record.
	if s.Priority != 0 {
		for _, k := range sortedKeys(s.Params) {
			_, _ = fmt.Fprintf(&b, "|%s=%s", k, strings.TrimSpace(s.Params[k]))
		}
	}
	return b.String()
}
