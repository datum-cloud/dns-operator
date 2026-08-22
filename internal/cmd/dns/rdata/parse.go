// SPDX-License-Identifier: AGPL-3.0-only

package rdata

import (
	"net/netip"
	"strconv"
	"strings"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// ParseValue parses one rdata value in zone-file presentation format and
// returns a RecordEntry with only the field for t populated. Name and TTL are
// never touched — the caller owns those.
//
// Presentation format is accepted for every type, including the structured
// ones, because it is what people paste out of a provider export, a dig answer
// or a "add this record" documentation page.
func ParseValue(t dnsv1alpha1.RRType, value string) (dnsv1alpha1.RecordEntry, error) {
	var e dnsv1alpha1.RecordEntry
	v := strings.TrimSpace(value)
	if v == "" {
		return e, errf("%s record value must not be empty", t)
	}

	switch t {
	case dnsv1alpha1.RRTypeA:
		if err := checkIPv4(v); err != nil {
			return e, err
		}
		e.A = &dnsv1alpha1.ARecordSpec{Content: v}

	case dnsv1alpha1.RRTypeAAAA:
		if err := checkIPv6(v); err != nil {
			return e, err
		}
		e.AAAA = &dnsv1alpha1.AAAARecordSpec{Content: v}

	case dnsv1alpha1.RRTypeCNAME:
		e.CNAME = &dnsv1alpha1.CNAMERecordSpec{Content: strings.ToLower(v)}

	case dnsv1alpha1.RRTypeALIAS:
		e.ALIAS = &dnsv1alpha1.ALIASRecordSpec{Content: strings.ToLower(v)}

	case dnsv1alpha1.RRTypeNS:
		e.NS = &dnsv1alpha1.NSRecordSpec{Content: strings.ToLower(v)}

	case dnsv1alpha1.RRTypePTR:
		e.PTR = &dnsv1alpha1.PTRRecordSpec{Content: strings.ToLower(v)}

	case dnsv1alpha1.RRTypeTXT:
		s, err := parseTXTValue(v)
		if err != nil {
			return e, err
		}
		e.TXT = &dnsv1alpha1.TXTRecordSpec{Content: s}

	case dnsv1alpha1.RRTypeMX:
		toks, err := fields(t, v, 2, 2, "<preference> <exchange>")
		if err != nil {
			return e, err
		}
		pref, err := parseUint16("MX preference", toks[0].text)
		if err != nil {
			return e, err
		}
		e.MX = &dnsv1alpha1.MXRecordSpec{Preference: pref, Exchange: strings.ToLower(hostToken(toks[1]))}

	case dnsv1alpha1.RRTypeSRV:
		toks, err := fields(t, v, 4, 4, "<priority> <weight> <port> <target>")
		if err != nil {
			return e, err
		}
		prio, err := parseUint16("SRV priority", toks[0].text)
		if err != nil {
			return e, err
		}
		weight, err := parseUint16("SRV weight", toks[1].text)
		if err != nil {
			return e, err
		}
		port, err := parseUint16("SRV port", toks[2].text)
		if err != nil {
			return e, err
		}
		e.SRV = &dnsv1alpha1.SRVRecordSpec{
			Priority: prio, Weight: weight, Port: port, Target: strings.ToLower(hostToken(toks[3])),
		}

	case dnsv1alpha1.RRTypeCAA:
		toks, err := fields(t, v, 3, 3, `<flag> <tag> <value>`)
		if err != nil {
			return e, err
		}
		flag, err := parseUint8("CAA flag", toks[0].text)
		if err != nil {
			return e, err
		}
		e.CAA = &dnsv1alpha1.CAARecordSpec{
			Flag: flag, Tag: strings.ToLower(toks[1].text), Value: toks[2].text,
		}

	case dnsv1alpha1.RRTypeTLSA:
		toks, err := fields(t, v, 4, -1, "<usage> <selector> <matchingType> <certData>")
		if err != nil {
			return e, err
		}
		usage, err := parseUint8("TLSA usage", toks[0].text)
		if err != nil {
			return e, err
		}
		sel, err := parseUint8("TLSA selector", toks[1].text)
		if err != nil {
			return e, err
		}
		mt, err := parseUint8("TLSA matching type", toks[2].text)
		if err != nil {
			return e, err
		}
		// Zone files wrap long certificate data across several fields; join
		// them back into one hex string.
		var cert strings.Builder
		for _, tok := range toks[3:] {
			cert.WriteString(tok.text)
		}
		e.TLSA = &dnsv1alpha1.TLSARecordSpec{
			Usage: usage, Selector: sel, MatchingType: mt, CertData: cert.String(),
		}

	case dnsv1alpha1.RRTypeHTTPS, dnsv1alpha1.RRTypeSVCB:
		spec, err := parseSVCB(t, v)
		if err != nil {
			return e, err
		}
		if t == dnsv1alpha1.RRTypeHTTPS {
			e.HTTPS = spec
		} else {
			e.SVCB = spec
		}

	case dnsv1alpha1.RRTypeSOA:
		spec, err := parseSOA(v)
		if err != nil {
			return e, err
		}
		e.SOA = spec

	default:
		return e, errf("unsupported record type %q, must be one of %s", string(t), typeList())
	}
	return e, nil
}

// fields tokenizes v and checks its arity. max of -1 means unbounded.
func fields(t dnsv1alpha1.RRType, v string, min, max int, grammar string) ([]token, error) {
	toks, err := tokenize(v, false)
	if err != nil {
		return nil, err
	}
	if len(toks) < min || (max >= 0 && len(toks) > max) {
		return nil, fixf(
			"the presentation format for "+string(t)+" is "+quoteStr(grammar)+
				", or use the named flags",
			"%s record value %q has %d fields, expected %s", t, v, len(toks), arity(min, max),
		)
	}
	return toks, nil
}

func arity(min, max int) string {
	switch {
	case max < 0:
		return "at least " + strconv.Itoa(min)
	case min == max:
		return strconv.Itoa(min)
	default:
		return strconv.Itoa(min) + " to " + strconv.Itoa(max)
	}
}

func parseSVCB(t dnsv1alpha1.RRType, v string) (*dnsv1alpha1.HTTPSRecordSpec, error) {
	toks, err := fields(t, v, 2, -1, "<priority> <target> [key=value ...]")
	if err != nil {
		return nil, err
	}
	prio, err := parseUint16(string(t)+" priority", toks[0].text)
	if err != nil {
		return nil, err
	}
	spec := &dnsv1alpha1.HTTPSRecordSpec{Priority: prio, Target: strings.ToLower(hostToken(toks[1]))}
	if len(toks) > 2 {
		spec.Params = map[string]string{}
		for _, tok := range toks[2:] {
			k, val, err := parseParam(tok.text)
			if err != nil {
				return nil, err
			}
			if _, dup := spec.Params[k]; dup {
				return nil, errf("%s parameter %q is set more than once", t, k)
			}
			spec.Params[k] = val
		}
	}
	return spec, nil
}

// parseParam splits a SvcParam written as "key=value" or as a bare flag key.
func parseParam(s string) (string, string, error) {
	k, v, found := strings.Cut(s, "=")
	k = strings.ToLower(strings.TrimSpace(k))
	if k == "" {
		return "", "", fixf(
			"parameters are written key=value, as in \"alpn=h3,h2\"",
			"service parameter %q has an empty key", s,
		)
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return "", "", errf("service parameter key %q contains an invalid character %q", k, string(c))
		}
	}
	if !found {
		// A valueless key is a flag parameter such as no-default-alpn.
		return k, "", nil
	}
	// Strip quoting a zone file may have applied to the value.
	if len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
		inner, err := tokenize(v, false)
		if err != nil {
			return "", "", err
		}
		if len(inner) == 1 && inner[0].quoted {
			v = inner[0].text
		}
	}
	return k, v, nil
}

func parseSOA(v string) (*dnsv1alpha1.SOARecordSpec, error) {
	toks, err := tokenize(v, false)
	if err != nil {
		return nil, err
	}
	if len(toks) != 2 && len(toks) != 7 {
		return nil, fixf(
			"the presentation format for SOA is \"<mname> <rname> <serial> <refresh> <retry> <expire> <minimum>\"; "+
				"give just \"<mname> <rname>\" to accept the backend defaults",
			"SOA record value %q has %d fields, expected 2 or 7", v, len(toks),
		)
	}
	spec := &dnsv1alpha1.SOARecordSpec{
		MName: strings.ToLower(hostToken(toks[0])),
		RName: strings.ToLower(hostToken(toks[1])),
	}
	if len(toks) == 2 {
		return spec, nil
	}
	names := []string{"serial", "refresh", "retry", "expire", "minimum"}
	vals := make([]uint32, len(names))
	for i, n := range names {
		u, err := parseSOAUint32(n, toks[2+i].text)
		if err != nil {
			return nil, err
		}
		vals[i] = u
	}
	spec.Serial, spec.Refresh, spec.Retry, spec.Expire, spec.TTL = vals[0], vals[1], vals[2], vals[3], vals[4]
	return spec, nil
}

// parseSOAUint32 rejects a literal 0. SOARecordSpec stores these fields as
// non-pointer uint32, so internal/pdns cannot tell "zero" from "unset" and
// substitutes its default for both — a literal 0 simply is not expressible
// through this API, and silently becoming 10800 is worse than an error.
func parseSOAUint32(field, s string) (uint32, error) {
	u, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, errf("SOA %s %q is not a number between 0 and 4294967295", field, s)
	}
	if u == 0 {
		return 0, fixf(
			"the API cannot express a literal 0 for this field — omit it to accept the backend default ("+
				soaDefaultText(field)+"), or give a non-zero value",
			"SOA %s may not be 0", field,
		)
	}
	return uint32(u), nil
}

func soaDefaultText(field string) string {
	switch field {
	case "serial":
		return "today's date plus 01"
	case "refresh":
		return "10800"
	case "retry":
		return "3600"
	case "expire":
		return "604800"
	case "minimum":
		return "3600"
	}
	return "the backend default"
}

func parseUint16(field, s string) (uint16, error) {
	u, err := strconv.ParseUint(s, 10, 16)
	if err != nil {
		return 0, errf("%s %q is not a number between 0 and 65535", field, s)
	}
	return uint16(u), nil
}

func parseUint8(field, s string) (uint8, error) {
	u, err := strconv.ParseUint(s, 10, 8)
	if err != nil {
		return 0, errf("%s %q is not a number between 0 and 255", field, s)
	}
	return uint8(u), nil
}

func checkIPv4(v string) error {
	addr, err := netip.ParseAddr(v)
	if err != nil || !addr.Is4() {
		return fixf("an A record holds a single IPv4 address, as in \"203.0.113.10\"",
			"%q is not a valid IPv4 address", v)
	}
	return nil
}

func checkIPv6(v string) error {
	addr, err := netip.ParseAddr(v)
	if err != nil || addr.Is4() || addr.Zone() != "" {
		return fixf("an AAAA record holds a single IPv6 address, as in \"2001:db8::1\"",
			"%q is not a valid IPv6 address", v)
	}
	return nil
}
