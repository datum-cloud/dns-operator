// SPDX-License-Identifier: AGPL-3.0-only

package rdata

import (
	"io"
	"os"
	"strings"

	"github.com/spf13/pflag"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// Flag names. Structured types are taught with these because
// "10 5 5060 sipserver.example.com." is opaque to whoever reads it back six
// months later. Flat types have nothing to disambiguate and stay positional,
// with the single exception of TXT, whose value is awkward enough to shell-quote
// that it earns --data.
const (
	FlagData         = "data"
	FlagPreference   = "preference"
	FlagExchange     = "exchange"
	FlagPriority     = "priority"
	FlagWeight       = "weight"
	FlagPort         = "port"
	FlagTarget       = "target"
	FlagFlag         = "flag"
	FlagTag          = "tag"
	FlagValue        = "value"
	FlagUsage        = "usage"
	FlagSelector     = "selector"
	FlagMatchingType = "matching-type"
	FlagCertData     = "cert-data"
	FlagParam        = "param"
	FlagMName        = "mname"
	FlagRName        = "rname"
	FlagSerial       = "serial"
	FlagRefresh      = "refresh"
	FlagRetry        = "retry"
	FlagExpire       = "expire"
	// FlagMinimum is SOARecordSpec.TTL, the SOA minimum field. It is not the
	// record's own TTL — that is --ttl, a RecordEntry field — and the flag is
	// named for what the RFC calls it so the two are never confused.
	FlagMinimum = "minimum"
)

// FlagNames returns the type-specific flags RegisterFlags would register for t,
// so callers can report "--preference is not a flag for A records" rather than
// letting pflag report an unknown flag.
func FlagNames(t dnsv1alpha1.RRType) []string {
	switch t {
	case dnsv1alpha1.RRTypeTXT:
		return []string{FlagData}
	case dnsv1alpha1.RRTypeMX:
		return []string{FlagPreference, FlagExchange}
	case dnsv1alpha1.RRTypeSRV:
		return []string{FlagPriority, FlagWeight, FlagPort, FlagTarget}
	case dnsv1alpha1.RRTypeCAA:
		return []string{FlagFlag, FlagTag, FlagValue}
	case dnsv1alpha1.RRTypeTLSA:
		return []string{FlagUsage, FlagSelector, FlagMatchingType, FlagCertData}
	case dnsv1alpha1.RRTypeHTTPS, dnsv1alpha1.RRTypeSVCB:
		return []string{FlagPriority, FlagTarget, FlagParam}
	case dnsv1alpha1.RRTypeSOA:
		return []string{FlagMName, FlagRName, FlagSerial, FlagRefresh, FlagRetry, FlagExpire, FlagMinimum}
	default:
		return nil
	}
}

// RegisterFlags adds the named rdata flags for t to fs, and only those. Flat
// types (A, AAAA, CNAME, ALIAS, NS, PTR) register nothing: their value is
// positional.
func RegisterFlags(fs *pflag.FlagSet, t dnsv1alpha1.RRType) {
	switch t {
	case dnsv1alpha1.RRTypeTXT:
		fs.String(FlagData, "", "text data; @path reads a file, - reads stdin")

	case dnsv1alpha1.RRTypeMX:
		fs.Uint16(FlagPreference, 0, "mail exchange preference (0-65535, lower wins)")
		fs.String(FlagExchange, "", "mail server, fully qualified (e.g. mail.example.com.)")

	case dnsv1alpha1.RRTypeSRV:
		fs.Uint16(FlagPriority, 0, "service priority (0-65535, lower wins)")
		fs.Uint16(FlagWeight, 0, "relative weight among equal priorities (0-65535)")
		fs.Uint16(FlagPort, 0, "port the service listens on (0-65535)")
		fs.String(FlagTarget, "", "service host, fully qualified (e.g. sip.example.com.)")

	case dnsv1alpha1.RRTypeCAA:
		fs.Uint8(FlagFlag, 0, "issuer critical flag (0 or 128)")
		fs.String(FlagTag, "", "property tag (issue, issuewild, iodef)")
		fs.String(FlagValue, "", "property value (e.g. letsencrypt.org)")

	case dnsv1alpha1.RRTypeTLSA:
		fs.Uint8(FlagUsage, 0, "certificate usage (0-3)")
		fs.Uint8(FlagSelector, 0, "selector: 0 full certificate, 1 public key")
		fs.Uint8(FlagMatchingType, 0, "matching type: 0 exact, 1 SHA-256, 2 SHA-512")
		fs.String(FlagCertData, "", "certificate association data, hexadecimal")

	case dnsv1alpha1.RRTypeHTTPS, dnsv1alpha1.RRTypeSVCB:
		fs.Uint16(FlagPriority, 0, "service priority; 0 selects alias mode")
		fs.String(FlagTarget, "", "target host, fully qualified, or . for this name")
		fs.StringArray(FlagParam, nil, "service parameter key=value; repeatable")

	case dnsv1alpha1.RRTypeSOA:
		fs.String(FlagMName, "", "primary nameserver, fully qualified")
		fs.String(FlagRName, "", "responsible mailbox in dot notation (e.g. hostmaster.example.com.)")
		fs.Uint32(FlagSerial, 0, "zone serial; omit for the backend default")
		fs.Uint32(FlagRefresh, 0, "refresh interval in seconds; omit for 10800")
		fs.Uint32(FlagRetry, 0, "retry interval in seconds; omit for 3600")
		fs.Uint32(FlagExpire, 0, "expire interval in seconds; omit for 604800")
		fs.Uint32(FlagMinimum, 0, "SOA minimum (negative cache) TTL in seconds; omit for 3600")
	}
}

// FromFlags builds a RecordEntry from the named flags registered for t. Only
// the type-specific field is set; Name and TTL belong to the caller.
//
// anySet is false when the user supplied none of the type-specific flags, which
// is how the caller knows to fall back to positional rdata. Mixing the two is a
// usage error the caller reports, not a merge performed here.
func FromFlags(fs *pflag.FlagSet, t dnsv1alpha1.RRType) (dnsv1alpha1.RecordEntry, bool, error) {
	var e dnsv1alpha1.RecordEntry
	names := FlagNames(t)
	anySet := false
	for _, n := range names {
		if f := fs.Lookup(n); f != nil && f.Changed {
			anySet = true
			break
		}
	}
	if !anySet {
		return e, false, nil
	}

	switch t {
	case dnsv1alpha1.RRTypeTXT:
		s, err := fs.GetString(FlagData)
		if err != nil {
			return e, true, err
		}
		// Through the same decoder ParseValue uses, so the two notations
		// really do produce identical entries: a --data value that is a
		// well-formed quoted character-string is presentation format, and
		// anything else is taken literally. Assigning the flag verbatim would
		// have let `--data '"a" b'` reach the backend as malformed
		// presentation data while the identical text was rejected as a
		// positional.
		content, err := parseTXTValue(s)
		if err != nil {
			return e, true, err
		}
		e.TXT = &dnsv1alpha1.TXTRecordSpec{Content: content}

	case dnsv1alpha1.RRTypeMX:
		pref, err := fs.GetUint16(FlagPreference)
		if err != nil {
			return e, true, err
		}
		exch, err := fs.GetString(FlagExchange)
		if err != nil {
			return e, true, err
		}
		e.MX = &dnsv1alpha1.MXRecordSpec{
			Preference: pref,
			Exchange:   strings.ToLower(strings.TrimSpace(exch)),
		}

	case dnsv1alpha1.RRTypeSRV:
		prio, err := fs.GetUint16(FlagPriority)
		if err != nil {
			return e, true, err
		}
		weight, err := fs.GetUint16(FlagWeight)
		if err != nil {
			return e, true, err
		}
		port, err := fs.GetUint16(FlagPort)
		if err != nil {
			return e, true, err
		}
		target, err := fs.GetString(FlagTarget)
		if err != nil {
			return e, true, err
		}
		e.SRV = &dnsv1alpha1.SRVRecordSpec{
			Priority: prio, Weight: weight, Port: port,
			Target: strings.ToLower(strings.TrimSpace(target)),
		}

	case dnsv1alpha1.RRTypeCAA:
		flag, err := fs.GetUint8(FlagFlag)
		if err != nil {
			return e, true, err
		}
		tag, err := fs.GetString(FlagTag)
		if err != nil {
			return e, true, err
		}
		value, err := fs.GetString(FlagValue)
		if err != nil {
			return e, true, err
		}
		e.CAA = &dnsv1alpha1.CAARecordSpec{
			Flag:  flag,
			Tag:   strings.ToLower(strings.TrimSpace(tag)),
			Value: unquoteOuter(strings.TrimSpace(value)),
		}

	case dnsv1alpha1.RRTypeTLSA:
		usage, err := fs.GetUint8(FlagUsage)
		if err != nil {
			return e, true, err
		}
		sel, err := fs.GetUint8(FlagSelector)
		if err != nil {
			return e, true, err
		}
		mt, err := fs.GetUint8(FlagMatchingType)
		if err != nil {
			return e, true, err
		}
		cert, err := fs.GetString(FlagCertData)
		if err != nil {
			return e, true, err
		}
		e.TLSA = &dnsv1alpha1.TLSARecordSpec{
			Usage: usage, Selector: sel, MatchingType: mt,
			CertData: strings.Join(strings.Fields(cert), ""),
		}

	case dnsv1alpha1.RRTypeHTTPS, dnsv1alpha1.RRTypeSVCB:
		prio, err := fs.GetUint16(FlagPriority)
		if err != nil {
			return e, true, err
		}
		target, err := fs.GetString(FlagTarget)
		if err != nil {
			return e, true, err
		}
		raw, err := fs.GetStringArray(FlagParam)
		if err != nil {
			return e, true, err
		}
		spec := &dnsv1alpha1.HTTPSRecordSpec{
			Priority: prio,
			Target:   strings.ToLower(strings.TrimSpace(target)),
		}
		if len(raw) > 0 {
			spec.Params = map[string]string{}
			for _, p := range raw {
				k, v, perr := parseParam(p)
				if perr != nil {
					return e, true, perr
				}
				if _, dup := spec.Params[k]; dup {
					return e, true, errf("--param %s is given more than once", k)
				}
				spec.Params[k] = v
			}
		}
		if t == dnsv1alpha1.RRTypeHTTPS {
			e.HTTPS = spec
		} else {
			e.SVCB = spec
		}

	case dnsv1alpha1.RRTypeSOA:
		mname, err := fs.GetString(FlagMName)
		if err != nil {
			return e, true, err
		}
		rname, err := fs.GetString(FlagRName)
		if err != nil {
			return e, true, err
		}
		spec := &dnsv1alpha1.SOARecordSpec{
			MName: strings.ToLower(strings.TrimSpace(mname)),
			RName: strings.ToLower(strings.TrimSpace(rname)),
		}
		nums := []struct {
			flag  string
			field string
			dst   *uint32
		}{
			{FlagSerial, "serial", &spec.Serial},
			{FlagRefresh, "refresh", &spec.Refresh},
			{FlagRetry, "retry", &spec.Retry},
			{FlagExpire, "expire", &spec.Expire},
			{FlagMinimum, "minimum", &spec.TTL},
		}
		for _, n := range nums {
			f := fs.Lookup(n.flag)
			if f == nil || !f.Changed {
				continue
			}
			v, verr := fs.GetUint32(n.flag)
			if verr != nil {
				return e, true, verr
			}
			if v == 0 {
				// Same trap as the presentation parser: the API stores these
				// as non-pointer uint32, so 0 and "unset" are the same value
				// and the backend substitutes its default for both.
				return e, true, fixf(
					"the API cannot express a literal 0 for this field — omit --"+n.flag+
						" to accept the backend default ("+soaDefaultText(n.field)+")",
					"--%s may not be 0", n.flag,
				)
			}
			*n.dst = v
		}
		e.SOA = spec

	default:
		return e, false, errf("%s records take their value positionally, not as flags", t)
	}
	return e, true, nil
}

// ResolveTXTData expands the indirections --data accepts: "@path" reads a file
// and "-" reads stdin, because SPF and DKIM values are where shell quoting
// bites hardest. Any other value is returned unchanged. A trailing newline from
// a file or a pipe is stripped.
func ResolveTXTData(v string, stdin io.Reader) (string, error) {
	switch {
	case v == "-":
		if stdin == nil {
			stdin = os.Stdin
		}
		b, err := io.ReadAll(stdin)
		if err != nil {
			return "", errf("reading TXT data from stdin: %v", err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	case strings.HasPrefix(v, "@"):
		path := v[1:]
		if path == "" {
			return "", errf("--data @ needs a file path")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return "", errf("reading TXT data from %q: %v", path, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	default:
		return v, nil
	}
}

// unquoteOuter drops one layer of surrounding quotes, which is what a user
// pasting a CAA value out of a zone file will have.
func unquoteOuter(s string) string {
	if len(s) >= 2 && strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		toks, err := tokenize(s, false)
		if err == nil && len(toks) == 1 && toks[0].quoted {
			return toks[0].text
		}
	}
	return s
}
