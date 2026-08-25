// SPDX-License-Identifier: AGPL-3.0-only

package rdata

import (
	"fmt"
	"regexp"
	"strings"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// maxTXTLength is a CLI-side cap. TXTRecordSpec.Content carries no kubebuilder
// validation whatsoever; 2048 bytes per record is the limit the portal enforces
// and the one most providers publish.
const maxTXTLength = 2048

var (
	caaTagPattern = regexp.MustCompile(`^[a-z0-9]+$`)
	hexPattern    = regexp.MustCompile(`^[0-9A-Fa-f]+$`)
	// svcbKeyPattern also admits the generic keyNNNNN spelling from RFC 9460.
	svcbGenericKey = regexp.MustCompile(`^key[0-9]{1,5}$`)
)

// Validate checks that e is a well-formed value of type t and that the DNS
// backend will actually write it.
//
// This is the reason the package exists. There is no validating webhook and no
// CEL rule tying the typed field to spec.recordType, so an entry whose cname is
// set under recordType: A is admitted, then skipped by internal/pdns while it
// builds the RRset — the user gets a record that does not exist and no
// condition saying why. Every check below either mirrors a kubebuilder marker,
// fills a gap where a field has no marker at all, or rejects input the backend
// would silently drop.
func Validate(t dnsv1alpha1.RRType, e dnsv1alpha1.RecordEntry) error {
	return ValidateInZone(t, e, "")
}

// ValidateInZone is Validate with the zone known, which lets the fix line on a
// missing trailing dot name the domain the user probably meant.
func ValidateInZone(t dnsv1alpha1.RRType, e dnsv1alpha1.RecordEntry, zone string) error {
	if _, err := ParseRRType(string(t)); err != nil {
		return err
	}
	if err := validateName(t, e.Name, zone); err != nil {
		return err
	}
	if err := validateTTL(e.TTL); err != nil {
		return err
	}
	if err := validateTypedField(t, e); err != nil {
		return err
	}
	return validateValue(t, e, zone)
}

// validateName checks the owner name, and — when the zone is known — checks it
// the way the backend will read it rather than the way it is spelled.
//
// Two of the three checks below are wrong without the zone. pdns.QualifyOwner
// resolves "@", "" and an absolute name equal to the zone all to the same RRset
// name, so an apex test that compares the literal string against "@" both
// admits a CNAME at "example.com." (bypassing the structural guard that keeps a
// CNAME off the apex) and rejects an SOA at "example.com." (which IS the apex).
// And a relative name that already spells out the zone gets the zone appended a
// second time, producing an RRset name that cannot resolve.
//
// NormalizeNameWithWarnings already encodes all of that. Validate calls it
// rather than reimplementing it, so the two cannot drift.
func validateName(t dnsv1alpha1.RRType, name, zone string) error {
	if name == "" {
		return fixf("use \"@\" for the zone apex", "record name is empty")
	}

	apex := IsApex(name)
	if zone != "" {
		norm, warns, err := NormalizeNameWithWarnings(name, zone)
		if err != nil {
			return err
		}
		if len(warns) > 0 {
			// An out-of-zone name is not merely unusual, so it is not merely a
			// warning here. A DNSRecordSet PATCH carries every owner name in
			// the (zone, type) bucket, and PowerDNS rejects the whole PATCH
			// when one name is outside the zone — so a single pasted
			// out-of-zone name does not fail on its own, it takes down every
			// other record written in the same call, with a backend error
			// instead of a CLI one.
			return fixf(
				"remove it, or use a name inside "+quoteStr(strings.TrimSuffix(strings.ToLower(zone), "."))+
					" — the DNS backend rejects the entire record set when one name is out of zone",
				"record name %q is outside zone %q", name, zone,
			)
		}
		apex = IsApex(norm)
	} else {
		if !namePattern.MatchString(name) {
			return errf("record name %q is not a valid owner name", name)
		}
		if !apex {
			if err := checkOwnerLabels(name, strings.TrimSuffix(name, ".")); err != nil {
				return err
			}
		}
	}

	switch t {
	case dnsv1alpha1.RRTypeCNAME:
		if apex {
			return fixf(
				"use an ALIAS record, which resolves at the apex the way a CNAME cannot",
				"a CNAME record may not exist at the zone apex",
			)
		}
	case dnsv1alpha1.RRTypeSOA:
		if !apex {
			return fixf("use \"@\" — a zone has exactly one SOA, at its apex",
				"an SOA record may not exist at %q", name)
		}
	}
	return nil
}

// validateTTL bounds a field the CRD leaves unbounded.
func validateTTL(ttl *int64) error {
	if ttl == nil {
		return nil
	}
	return checkTTLRange(*ttl, fmt.Sprintf("%d", *ttl))
}

func validateTypedField(t dnsv1alpha1.RRType, e dnsv1alpha1.RecordEntry) error {
	set := setFields(e)
	want := jsonFieldFor(t)
	switch len(set) {
	case 0:
		return fixf(
			"set the "+quoteStr(want)+" field, or change the record type to match the value you have",
			"record %q of type %s has no %s value", e.Name, t, t,
		)
	case 1:
		if set[0] == want {
			return nil
		}
		other, err := ParseRRType(set[0])
		otherName := set[0]
		if err == nil {
			otherName = string(other)
		}
		return fixf(
			"the value must match the record type — either write it as "+string(t)+
				" data, or create it as a "+otherName+" record instead",
			"record %q of type %s carries %s data, which the DNS backend silently discards",
			e.Name, t, otherName,
		)
	default:
		return fixf(
			"a record entry holds exactly one value — set only "+quoteStr(want),
			"record %q of type %s sets %d type-specific fields (%s)",
			e.Name, t, len(set), strings.Join(set, ", "),
		)
	}
}

func validateValue(t dnsv1alpha1.RRType, e dnsv1alpha1.RecordEntry, zone string) error {
	switch t {
	case dnsv1alpha1.RRTypeA:
		return checkIPv4(strings.TrimSpace(e.A.Content))

	case dnsv1alpha1.RRTypeAAAA:
		return checkIPv6(strings.TrimSpace(e.AAAA.Content))

	case dnsv1alpha1.RRTypeCNAME:
		return checkTargetHost("CNAME target", e.CNAME.Content, zone, relaxedHost, false)

	case dnsv1alpha1.RRTypeALIAS:
		return checkTargetHost("ALIAS target", e.ALIAS.Content, zone, relaxedHost, false)

	case dnsv1alpha1.RRTypeNS:
		return checkTargetHost("NS content", e.NS.Content, zone, strictHost, false)

	case dnsv1alpha1.RRTypePTR:
		return checkTargetHost("PTR content", e.PTR.Content, zone, relaxedHost, false)

	case dnsv1alpha1.RRTypeTXT:
		return validateTXT(e.TXT.Content)

	case dnsv1alpha1.RRTypeMX:
		// RFC 7505 "null MX": preference 0 with an exchange of "." says the
		// domain accepts no mail. Any other "." is a mistake.
		if strings.TrimSpace(e.MX.Exchange) == "." {
			if e.MX.Preference != 0 {
				return fixf("a null MX is written \"0 .\"",
					"MX exchange \".\" declares that the domain accepts no mail, so its preference must be 0, not %d",
					e.MX.Preference)
			}
			return nil
		}
		return checkTargetHost("MX exchange", e.MX.Exchange, zone, strictHost, false)

	case dnsv1alpha1.RRTypeSRV:
		// RFC 2782: a target of "." means the service is not available here.
		return checkTargetHost("SRV target", e.SRV.Target, zone, relaxedHost, true)

	case dnsv1alpha1.RRTypeCAA:
		return validateCAA(*e.CAA)

	case dnsv1alpha1.RRTypeTLSA:
		return validateTLSA(*e.TLSA)

	case dnsv1alpha1.RRTypeHTTPS:
		return validateSVCB(t, *e.HTTPS, zone)

	case dnsv1alpha1.RRTypeSVCB:
		return validateSVCB(t, *e.SVCB, zone)

	case dnsv1alpha1.RRTypeSOA:
		return validateSOA(*e.SOA, zone)
	}
	return nil
}

// checkTargetHost applies the shared rules for every rdata field that names
// another host: non-empty, a syntactically valid host name at the appropriate
// strictness, and — because internal/pdns absolutizes it by appending a dot
// rather than the zone — fully qualified.
func checkTargetHost(field, value, zone string, underscores, allowRoot bool) error {
	v := strings.TrimSpace(value)
	if v == "" {
		return errf("%s must not be empty", field)
	}
	if v == "@" {
		return fixf("write the target out in full, as in \"example.com.\"",
			"%s may not be %q — the zone apex has no shorthand inside rdata", field, v)
	}
	if v == "." {
		if allowRoot {
			return nil
		}
		return errf("%s may not be the DNS root %q", field, v)
	}
	if err := checkHostname(field, v, underscores); err != nil {
		return err
	}
	return requireFQDN(field, v, zone)
}

// validateTXT fills a total gap: TXTRecordSpec.Content has no markers at all.
//
// The length cap applies to the LOGICAL value. Escaping and chunking are a
// serialization concern and inflate the byte count — a 2040-byte value encodes
// to 2063 — so measuring the wire form would reject a record the CLI itself
// created and make it uneditable.
func validateTXT(raw string) error {
	content := txtLogical(raw)
	if content == "" {
		return errf("TXT data must not be empty")
	}
	if len(content) > maxTXTLength {
		return errf("TXT data is %d bytes, the maximum is %d", len(content), maxTXTLength)
	}
	// Control characters are NOT rejected. They used to be, on the grounds that
	// presentation format could not carry them — which stopped being true when
	// quoteTXT learned the RFC 1035 §5.1 \DDD form. Rejecting them now would
	// break the export/apply loop for any record created elsewhere: the CLI
	// would export it correctly and then refuse to read its own file back.
	// They are unusual enough to warn about, and warning is entryWarnings' job.
	return nil
}

func validateCAA(c dnsv1alpha1.CAARecordSpec) error {
	if c.Tag == "" {
		return errf("CAA tag must not be empty")
	}
	if !caaTagPattern.MatchString(c.Tag) {
		return fixf("tags are lowercase letters and digits, usually \"issue\", \"issuewild\" or \"iodef\"",
			"CAA tag %q must match [a-z0-9]+", c.Tag)
	}
	if strings.TrimSpace(c.Value) == "" {
		return errf("CAA value must not be empty")
	}
	// pdns.quoteIfNeeded escapes only semicolons when it wraps a CAA value, so
	// a quote or a backslash in the value produces a zone-file line that says
	// something other than what was typed. Rejecting them keeps the rendered
	// value and the written value identical.
	if strings.ContainsAny(c.Value, "\"\n\\") {
		return errf("CAA value %q contains a quote, newline or backslash, which the DNS backend cannot encode", c.Value)
	}
	return nil
}

// validateTLSA fills another total gap: no TLSA field carries a marker, so the
// API accepts usage 200 and non-hex certificate data.
func validateTLSA(t dnsv1alpha1.TLSARecordSpec) error {
	if t.Usage > 3 {
		return fixf("usage is 0 (PKIX-TA), 1 (PKIX-EE), 2 (DANE-TA) or 3 (DANE-EE)",
			"TLSA usage %d is out of range", t.Usage)
	}
	if t.Selector > 1 {
		return fixf("selector is 0 (full certificate) or 1 (subject public key info)",
			"TLSA selector %d is out of range", t.Selector)
	}
	if t.MatchingType > 2 {
		return fixf("matching type is 0 (exact), 1 (SHA-256) or 2 (SHA-512)",
			"TLSA matching type %d is out of range", t.MatchingType)
	}
	if t.CertData == "" {
		return errf("TLSA certificate data must not be empty")
	}
	if !hexPattern.MatchString(t.CertData) {
		return errf("TLSA certificate data %q is not hexadecimal", t.CertData)
	}
	if len(t.CertData)%2 != 0 {
		return errf("TLSA certificate data has an odd number of hex digits (%d)", len(t.CertData))
	}
	switch t.MatchingType {
	case 1:
		if len(t.CertData) != 64 {
			return fixf("a SHA-256 digest is 64 hex digits",
				"TLSA matching type 1 needs a SHA-256 digest, got %d hex digits", len(t.CertData))
		}
	case 2:
		if len(t.CertData) != 128 {
			return fixf("a SHA-512 digest is 128 hex digits",
				"TLSA matching type 2 needs a SHA-512 digest, got %d hex digits", len(t.CertData))
		}
	}
	return nil
}

// validateSVCB covers HTTPSRecordSpec, whose Target and Params carry no
// markers, and rejects the alias-mode combinations internal/pdns drops.
func validateSVCB(t dnsv1alpha1.RRType, s dnsv1alpha1.HTTPSRecordSpec, zone string) error {
	target := strings.TrimSpace(s.Target)
	if target == "" {
		return fixf("use \".\" for service mode at this owner name, or a fully qualified target",
			"%s target must not be empty", t)
	}
	if target != "." {
		if err := checkHostname(string(t)+" target", target, relaxedHost); err != nil {
			return err
		}
		if err := requireFQDN(string(t)+" target", target, zone); err != nil {
			return err
		}
	}
	if s.Priority == 0 {
		// Alias form. internal/pdns emits only "0 <target>", so parameters set
		// here never reach the zone.
		if target == "." {
			return fixf("give the alias target, or use a priority of 1 or more for service mode",
				"%s priority 0 selects alias mode, which needs a real target rather than %q", t, ".")
		}
		if len(s.Params) > 0 {
			return fixf("drop the parameters, or use a priority of 1 or more for service mode",
				"%s priority 0 selects alias mode, which carries no parameters — %s would be discarded",
				t, strings.Join(sortedKeys(s.Params), ", "))
		}
		return nil
	}
	for _, k := range sortedKeys(s.Params) {
		if k != strings.ToLower(k) {
			return errf("%s parameter key %q must be lowercase", t, k)
		}
		v := s.Params[k]
		if strings.ContainsAny(v, " \t\n") {
			return fixf("parameter values are written without spaces, as in \"alpn=h3,h2\"",
				"%s parameter %q has a value containing whitespace", t, k)
		}
		if _, isFlag := svcbFlagKeys[k]; !isFlag && strings.TrimSpace(v) == "" {
			return fixf("write it as "+quoteStr(k+"=value")+", or drop it",
				"%s parameter %q has no value", t, k)
		}
	}
	return nil
}

func validateSOA(s dnsv1alpha1.SOARecordSpec, zone string) error {
	if err := checkTargetHost("SOA mname", s.MName, zone, strictHost, false); err != nil {
		return err
	}
	rname := strings.TrimSpace(s.RName)
	if rname == "" {
		return errf("SOA rname must not be empty")
	}
	if strings.Contains(rname, "@") {
		return fixf("write the address in dot notation — \"admin@example.com\" becomes \"admin.example.com.\"",
			"SOA rname %q contains \"@\"", s.RName)
	}
	if err := checkHostname("SOA rname", rname, relaxedHost); err != nil {
		return err
	}
	if len(splitDNSLabels(strings.TrimSuffix(rname, "."))) < 3 {
		return fixf("it is a mailbox in dot notation, as in \"hostmaster.example.com.\"",
			"SOA rname %q is not a mailbox address", s.RName)
	}
	return requireFQDN("SOA rname", rname, zone)
}

// ValidateEntries validates every entry and additionally checks the constraints
// that only exist across a whole RRset: single-valued types, and duplicate
// values that PowerDNS rejects with a 422 for the entire set.
func ValidateEntries(t dnsv1alpha1.RRType, entries []dnsv1alpha1.RecordEntry) error {
	return ValidateEntriesInZone(t, entries, "")
}

// ValidateEntriesInZone is ValidateEntries with the zone known.
func ValidateEntriesInZone(t dnsv1alpha1.RRType, entries []dnsv1alpha1.RecordEntry, zone string) error {
	if len(entries) == 0 {
		return errf("a %s record set needs at least one value", t)
	}
	for _, e := range entries {
		if err := ValidateInZone(t, e, zone); err != nil {
			return err
		}
	}
	type ownerState struct {
		count int
		seen  map[string]bool
	}
	owners := map[string]*ownerState{}
	for _, e := range entries {
		// Grouped by the qualified name, which folds case. Two entries at
		// "www" and "WWW" are therefore one owner here, and a second CNAME
		// among them is a violation — deliberately. DNS is case-insensitive,
		// but buildRRSets would key two case-distinct rrsets off those
		// spellings and hand PowerDNS a name that already has a CNAME. Do not
		// "fix" this by comparing the raw names.
		owner := FQDN(e.Name, zone)
		st := owners[owner]
		if st == nil {
			st = &ownerState{seen: map[string]bool{}}
			owners[owner] = st
		}
		st.count++
		if IsSingleValued(t) && st.count > 1 {
			fix := "keep one value — the DNS backend writes the first and discards the rest"
			if t == dnsv1alpha1.RRTypeSOA {
				fix = "keep one value — the DNS backend writes the last and discards the rest"
			}
			if t == dnsv1alpha1.RRTypeCNAME {
				fix = "a name may have exactly one CNAME (RFC 1034) — use several A or ALIAS records instead"
			}
			return fixf(fix, "%s record %q has %d values but is single-valued", t, e.Name, st.count)
		}
		k := Key(t, e)
		if st.seen[k] {
			return fixf("remove the duplicate — PowerDNS rejects the whole record set when a value repeats",
				"%s record %q has a duplicate value %q", t, e.Name, Render(t, e))
		}
		st.seen[k] = true
	}
	return nil
}

// Warnings returns non-fatal advisories for one or more entries of type t:
// things that are legal, that the CLI will happily submit, and that the user
// probably still wants to know about. Cross-entry warnings are produced only
// when more than one entry is passed.
func Warnings(t dnsv1alpha1.RRType, entries ...dnsv1alpha1.RecordEntry) []string {
	return WarningsInZone(t, "", entries...)
}

// WarningsInZone is Warnings with the zone known, which it needs to group
// entries the way the backend does. pdns.QualifyOwner collapses "www" and
// "www.example.com." onto one RRset; grouping by the raw name instead would
// miss a TTL disagreement between those two spellings entirely and let one of
// the TTLs vanish without a word.
func WarningsInZone(t dnsv1alpha1.RRType, zone string, entries ...dnsv1alpha1.RecordEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, entryWarnings(t, e)...)
	}
	if len(entries) > 1 {
		out = append(out, ttlWarnings(zone, entries)...)
	}
	return out
}

// knownCAATags is advisory only. The API pattern is [a-z0-9]+ and the CLI
// honours that range rather than narrowing to the portal's three-value enum:
// RFC 8657 added contactemail and contactphone after the portal shipped, and
// refusing a tag the server accepts would make the CLI the more restrictive
// client for no gain. Unknown tags warn instead.
var knownCAATags = map[string]bool{
	"issue": true, "issuewild": true, "iodef": true,
	"issuemail": true, "contactemail": true, "contactphone": true,
}

var knownSVCBKeys = map[string]bool{
	"mandatory": true, "alpn": true, "no-default-alpn": true, "port": true,
	"ipv4hint": true, "ech": true, "ipv6hint": true, "dohpath": true,
	"ohttp": true, "esnikeys": true,
}

func entryWarnings(t dnsv1alpha1.RRType, e dnsv1alpha1.RecordEntry) []string {
	var out []string
	switch t {
	case dnsv1alpha1.RRTypeCAA:
		if e.CAA == nil {
			return nil
		}
		if !knownCAATags[e.CAA.Tag] {
			out = append(out, fmt.Sprintf("CAA tag %q is not one of the tags defined by RFC 8659/8657 (%s)",
				e.CAA.Tag, "issue, issuewild, iodef, issuemail, contactemail, contactphone"))
		}
		if e.CAA.Flag != 0 && e.CAA.Flag != 128 {
			out = append(out, fmt.Sprintf("CAA flag %d is unusual — 0 (non-critical) and 128 (critical) are the defined values",
				e.CAA.Flag))
		}
	case dnsv1alpha1.RRTypeSRV:
		if e.SRV != nil && e.SRV.Port == 0 {
			out = append(out, "SRV port 0 is reserved — set the port the service listens on")
		}
	case dnsv1alpha1.RRTypeHTTPS, dnsv1alpha1.RRTypeSVCB:
		spec := e.HTTPS
		if t == dnsv1alpha1.RRTypeSVCB {
			spec = e.SVCB
		}
		if spec == nil {
			return nil
		}
		for _, k := range sortedKeys(spec.Params) {
			if !knownSVCBKeys[k] && !svcbGenericKey.MatchString(k) {
				out = append(out, fmt.Sprintf("%s parameter %q is not a registered service parameter key", t, k))
			}
		}
	case dnsv1alpha1.RRTypeSOA:
		if e.SOA == nil {
			return nil
		}
		if e.SOA.Refresh != 0 && e.SOA.Refresh < 1200 {
			out = append(out, fmt.Sprintf("SOA refresh %d is below the recommended minimum of 1200 seconds", e.SOA.Refresh))
		}
		if e.SOA.Retry != 0 && e.SOA.Retry < 600 {
			out = append(out, fmt.Sprintf("SOA retry %d is below the recommended minimum of 600 seconds", e.SOA.Retry))
		}
		if e.SOA.Expire != 0 && e.SOA.Expire < 604800 {
			out = append(out, fmt.Sprintf("SOA expire %d is below the recommended minimum of 604800 seconds", e.SOA.Expire))
		}
	}
	if t == dnsv1alpha1.RRTypeTXT && e.TXT != nil {
		if i := strings.IndexFunc(txtLogical(e.TXT.Content), func(r rune) bool {
			return r < 0x20 || r == 0x7f
		}); i >= 0 {
			out = append(out, fmt.Sprintf(
				"TXT data contains a control character at byte %d — it is written as a \\DDD escape and round-trips, but a newline here is usually a paste accident",
				i))
		}
	}
	if e.TTL != nil && *e.TTL == 0 {
		out = append(out, "TTL 0 disables caching entirely, which most resolvers treat as a very short TTL")
	}
	return out
}

// ttlWarnings reports entries for one owner name that disagree on TTL. TTL is
// per-RRset in DNS but per-entry in this API, and internal/pdns takes the first
// entry's TTL for an owner and ignores the rest.
func ttlWarnings(zone string, entries []dnsv1alpha1.RecordEntry) []string {
	first := map[string]*int64{}
	seen := map[string]bool{}
	warned := map[string]bool{}
	var out []string
	for _, e := range entries {
		owner := FQDN(e.Name, zone)
		if !seen[owner] {
			seen[owner] = true
			first[owner] = e.TTL
			continue
		}
		if !warned[owner] && FormatTTL(first[owner]) != FormatTTL(e.TTL) {
			out = append(out, fmt.Sprintf(
				"values for %q disagree on TTL (%s and %s) — the DNS backend applies the first one, %s, to the whole record set",
				e.Name, FormatTTL(first[owner]), FormatTTL(e.TTL), FormatTTL(first[owner])))
			// One warning per owner is enough.
			warned[owner] = true
		}
	}
	return out
}
