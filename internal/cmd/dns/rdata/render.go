// SPDX-License-Identifier: AGPL-3.0-only

package rdata

import (
	"fmt"
	"sort"
	"strings"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// nowFunc is time.Now, indirected so the SOA serial default is testable.
var nowFunc = time.Now

// SOA field defaults substituted by internal/pdns when the stored value is 0.
const (
	soaDefaultRefresh = 10800
	soaDefaultRetry   = 3600
	soaDefaultExpire  = 604800
	soaDefaultMinimum = 3600
)

// Render returns the zone-file presentation form of e's value for type t.
//
// The output is the line internal/pdns will hand to PowerDNS, so it doubles as
// the confirmation echoed back after a flag-driven mutation and as the VALUE
// column in `record list`. Where the backend substitutes a default for an unset
// field — the SOA timers, an empty SVCB target — Render shows the effective
// value rather than the blank, so describe never hides what will be written.
func Render(t dnsv1alpha1.RRType, e dnsv1alpha1.RecordEntry) string {
	switch t {
	case dnsv1alpha1.RRTypeA:
		if e.A == nil {
			return ""
		}
		return strings.TrimSpace(e.A.Content)

	case dnsv1alpha1.RRTypeAAAA:
		if e.AAAA == nil {
			return ""
		}
		return strings.TrimSpace(e.AAAA.Content)

	case dnsv1alpha1.RRTypeCNAME:
		if e.CNAME == nil {
			return ""
		}
		return qualify(e.CNAME.Content)

	case dnsv1alpha1.RRTypeALIAS:
		if e.ALIAS == nil {
			return ""
		}
		return qualify(e.ALIAS.Content)

	case dnsv1alpha1.RRTypeNS:
		if e.NS == nil {
			return ""
		}
		return qualify(e.NS.Content)

	case dnsv1alpha1.RRTypePTR:
		if e.PTR == nil {
			return ""
		}
		return qualify(e.PTR.Content)

	case dnsv1alpha1.RRTypeTXT:
		if e.TXT == nil {
			return ""
		}
		return renderTXT(e.TXT.Content)

	case dnsv1alpha1.RRTypeMX:
		if e.MX == nil {
			return ""
		}
		return fmt.Sprintf("%d %s", e.MX.Preference, qualify(e.MX.Exchange))

	case dnsv1alpha1.RRTypeSRV:
		if e.SRV == nil {
			return ""
		}
		return fmt.Sprintf("%d %d %d %s",
			e.SRV.Priority, e.SRV.Weight, e.SRV.Port, qualify(e.SRV.Target))

	case dnsv1alpha1.RRTypeCAA:
		if e.CAA == nil {
			return ""
		}
		return fmt.Sprintf("%d %s %s", e.CAA.Flag, e.CAA.Tag, quoteTXT(e.CAA.Value))

	case dnsv1alpha1.RRTypeTLSA:
		if e.TLSA == nil {
			return ""
		}
		return fmt.Sprintf("%d %d %d %s",
			e.TLSA.Usage, e.TLSA.Selector, e.TLSA.MatchingType, e.TLSA.CertData)

	case dnsv1alpha1.RRTypeHTTPS:
		if e.HTTPS == nil {
			return ""
		}
		return renderSVCB(*e.HTTPS)

	case dnsv1alpha1.RRTypeSVCB:
		if e.SVCB == nil {
			return ""
		}
		return renderSVCB(*e.SVCB)

	case dnsv1alpha1.RRTypeSOA:
		if e.SOA == nil {
			return ""
		}
		s := *e.SOA
		return fmt.Sprintf("%s %s %s %d %d %d %d",
			qualify(s.MName), qualify(s.RName), soaSerial(s.Serial),
			orDefault(s.Refresh, soaDefaultRefresh),
			orDefault(s.Retry, soaDefaultRetry),
			orDefault(s.Expire, soaDefaultExpire),
			orDefault(s.TTL, soaDefaultMinimum))
	}
	return ""
}

func soaSerial(serial uint32) string {
	if serial != 0 {
		return fmt.Sprintf("%d", serial)
	}
	return nowFunc().Format("20060102") + "01"
}

func orDefault(v, def uint32) uint32 {
	if v == 0 {
		return def
	}
	return v
}

// qualify mirrors pdns.qualifyIfNeeded: a target is absolutized by appending a
// dot and nothing else.
func qualify(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasSuffix(s, ".") {
		return s
	}
	return s + "."
}

func renderSVCB(s dnsv1alpha1.HTTPSRecordSpec) string {
	t := strings.TrimSpace(s.Target)
	switch t {
	case ".":
	case "":
		t = "."
	default:
		t = qualify(t)
	}
	// Alias form (priority 0) carries no parameters; internal/pdns drops any
	// that are set, so Render shows the same.
	if s.Priority == 0 {
		return fmt.Sprintf("%d %s", s.Priority, t)
	}
	if p := renderSVCBParams(s.Params); p != "" {
		return fmt.Sprintf("%d %s %s", s.Priority, t, p)
	}
	return fmt.Sprintf("%d %s", s.Priority, t)
}

// svcbFlagKeys, svcbUnquotedCSV, svcbQuotedKeys and svcbKeyRank mirror
// internal/pdns so Render reproduces the backend's parameter spelling and
// ordering byte for byte.
var (
	svcbFlagKeys    = map[string]struct{}{"no-default-alpn": {}}
	svcbUnquotedCSV = map[string]struct{}{"alpn": {}, "ipv4hint": {}, "ipv6hint": {}, "port": {}}
	svcbQuotedKeys  = map[string]struct{}{"esnikeys": {}, "ech": {}}
)

func svcbKeyRank(k string) int {
	switch k {
	case "alpn":
		return 10
	case "no-default-alpn":
		return 20
	case "port":
		return 30
	case "esnikeys", "ech":
		return 40
	case "ipv4hint":
		return 50
	case "ipv6hint":
		return 60
	default:
		return 1000
	}
}

func renderSVCBParams(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := sortedKeys(m)
	sort.SliceStable(keys, func(i, j int) bool {
		return svcbKeyRank(keys[i]) < svcbKeyRank(keys[j])
	})
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v := strings.TrimSpace(m[k])
		if _, isFlag := svcbFlagKeys[k]; isFlag {
			parts = append(parts, k)
			continue
		}
		if v == "" {
			continue
		}
		if _, unq := svcbUnquotedCSV[k]; unq {
			parts = append(parts, k+"="+v)
			continue
		}
		parts = append(parts, k+"="+quoteTXT(v))
	}
	return strings.Join(parts, " ")
}
