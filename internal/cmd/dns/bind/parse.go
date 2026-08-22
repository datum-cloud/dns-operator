// SPDX-License-Identifier: AGPL-3.0-only

// Package bind reads and writes BIND master files (RFC 1035 §5) for the
// `datumctl dns` plugin's bulk paths: `zone import`, `zone export` and
// `record apply`.
//
// The package owns zone-file *syntax* only — directives, comments,
// parenthesised continuations, owner-name inheritance, relative versus absolute
// names, and the character-string quoting rules. Every per-type rdata decision
// is delegated to internal/cmd/dns/rdata, which is the CLI's single
// implementation of what an MX or an HTTPS value means. A zone file therefore
// cannot express a record that `record create` would reject, and vice versa.
//
// Types the API does not carry are reported through ParseResult.Unsupported
// rather than dropped: a silent omission during a provider migration is how a
// zone ends up missing its DS records with nobody the wiser.
package bind

import (
	"io"
	"strings"

	"github.com/miekg/dns"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
)

// Record is one parsed resource record, in the shape the API stores.
type Record struct {
	// Name is the owner name relative to the zone Parse was given: "@" for the
	// apex, a bare label otherwise. A name that falls outside the zone keeps
	// its absolute form and raises a warning.
	Name string
	// TTL is the effective TTL: the record's own, else the prevailing $TTL,
	// else the default Parse was given. nil means "let the backend choose".
	TTL *int64
	// Type is the RR type, guaranteed to be one rdata supports.
	Type dnsv1alpha1.RRType
	// Entry carries the parsed rdata with Name and TTL already set, ready to
	// go into DNSRecordSet.spec.records.
	Entry dnsv1alpha1.RecordEntry
	// Line is the 1-based physical line the record starts on.
	Line int
}

// Unsupported is a record the zone file declares and the API cannot store.
type Unsupported struct {
	// Line is the 1-based physical line the record starts on.
	Line int
	// Name is the owner name as written, before normalization.
	Name string
	// Type is the RR type name as written, uppercased.
	Type string
	// Raw is the statement as the user wrote it, comments intact.
	Raw string
}

// ParseResult is everything a zone file yielded.
type ParseResult struct {
	// Origin is the effective origin: the last $ORIGIN in the file, or the one
	// Parse was given when the file declares none. It always ends in a dot.
	Origin string
	// Records are the records the API can store, in file order.
	Records []Record
	// Unsupported are the records it cannot, in file order.
	Unsupported []Unsupported
	// Warnings are the non-fatal advisories Parse produces: an owner name that
	// falls outside the zone, a relative name that already spells the zone out,
	// a duplicate value within one owner, a class other than IN, a directive
	// that was ignored, and a file with nothing to expand relative names
	// against.
	Warnings []string
}

// dnsClasses are the class tokens permitted between the TTL and the type. Only
// IN is meaningful to this API; the rest are accepted so a pasted line parses
// rather than failing on a field nobody reads.
var dnsClasses = map[string]bool{"IN": true, "CH": true, "HS": true, "CS": true}

// Parse reads a BIND master file.
//
// origin is the zone the file is being read into. It seeds the origin used to
// expand relative names, and it is the zone owner names are made relative to,
// so a file whose $ORIGIN is a subdomain still produces names the API accepts.
// defaultTTL is the TTL for records the file gives none and declares no $TTL
// for; nil leaves those records on the backend default.
//
// A malformed statement is a hard error carrying its line number. An
// unsupported RR type is not: it is collected into ParseResult.Unsupported so
// the caller can report it.
func Parse(r io.Reader, origin string, defaultTTL *int64) (ParseResult, error) {
	zone := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(origin)), ".")

	var out ParseResult
	lines, err := scan(r)
	if err != nil {
		return out, err
	}

	// current is the origin relative names expand against; zone is the one they
	// are made relative to on the way out. They differ only when the file
	// declares a $ORIGIN below the zone apex.
	current := zone
	fileTTL := defaultTTL
	sawOrigin := false

	// prevOwner is the owner name of the last record, already qualified against
	// the origin in force when that record was read. Carrying the qualified form
	// rather than the token as written is what makes inheritance survive a
	// mid-file $ORIGIN: RFC 1035 §5.1 inherits the previous record's name, not
	// its spelling, so a blank-owner record after "$ORIGIN sub.example.com."
	// still belongs to the owner above it.
	prevOwner := ""

	for _, ll := range lines {
		if strings.HasPrefix(ll.text, "$") {
			newOrigin, originSet, newTTL, warn, err := directive(ll, current, fileTTL)
			if err != nil {
				return out, err
			}
			if warn != "" {
				out.Warnings = append(out.Warnings, warn)
			}
			if originSet {
				current = newOrigin
				sawOrigin = true
			}
			fileTTL = newTTL
			continue
		}

		rec, uns, owner, warns, err := parseRecord(ll, current, zone, fileTTL, prevOwner)
		if err != nil {
			return out, err
		}
		prevOwner = owner
		out.Warnings = append(out.Warnings, warns...)
		switch {
		case uns != nil:
			out.Unsupported = append(out.Unsupported, *uns)
		case rec != nil:
			out.Records = append(out.Records, *rec)
		}
	}

	out.Origin = absolute(current)
	if !sawOrigin && zone == "" && len(out.Records) > 0 {
		out.Warnings = append(out.Warnings,
			"the file declares no $ORIGIN and no zone was given — relative names were left as written")
	}
	out.Warnings = append(out.Warnings, dedupeWarnings(out.Records)...)
	return out, nil
}

// directive handles the control entries. $ORIGIN and $TTL are honoured;
// $INCLUDE and $GENERATE are refused rather than skipped, because skipping one
// drops records the user believes were imported.
func directive(
	ll logicalLine, current string, ttl *int64,
) (newOrigin string, originSet bool, newTTL *int64, warn string, err error) {
	toks := splitFields(ll.text)
	name := strings.ToUpper(toks[0].text)

	switch name {
	case "$ORIGIN":
		if len(toks) < 2 {
			return "", false, ttl, "", atf(ll.line, "$ORIGIN needs a domain name")
		}
		o := strings.ToLower(toks[1].text)
		if !strings.HasSuffix(o, ".") {
			// A relative $ORIGIN is expanded against the prevailing one, which
			// is what BIND does and what nested-subdomain files rely on.
			if current == "" {
				return "", false, ttl, "", atFix(ll.line,
					"write the origin as an absolute name, as in \"$ORIGIN example.com.\"",
					"$ORIGIN %q is relative and there is no origin to expand it against", toks[1].text)
			}
			o = o + "." + current
		}
		// "$ORIGIN ." is the DNS root, which trims to the empty string — the
		// same value that means "unset" everywhere else here. originSet is what
		// keeps the two apart, so a file that reparents itself to the root is
		// honoured rather than silently ignored.
		return strings.TrimSuffix(o, "."), true, ttl, "", nil

	case "$TTL":
		if len(toks) < 2 {
			return "", false, ttl, "", atf(ll.line, "$TTL needs a value")
		}
		v, terr := parseTTL(toks[1].text)
		if terr != nil {
			return "", false, ttl, "", at(ll.line, terr)
		}
		return "", false, ttlPtr(v), "", nil

	case "$INCLUDE":
		return "", false, ttl, "", atFix(ll.line,
			"inline the included file, or import each file separately",
			"$INCLUDE is not supported — it would read a file the command was not given")

	case "$GENERATE":
		return "", false, ttl, "", atFix(ll.line,
			"expand the range into explicit records",
			"$GENERATE is not supported")
	}
	return "", false, ttl,
		"ignored unrecognised directive " + quote(name) + " on line " + itoa(ll.line), nil
}

// parseRecord turns one statement into a Record, an Unsupported, or an error.
//
// It returns the record's owner name already qualified against the prevailing
// origin, because that — not the token as written — is what the next statement
// inherits when it begins with whitespace.
func parseRecord(
	ll logicalLine, current, zone string, defaultTTL *int64, prevOwner string,
) (*Record, *Unsupported, string, []string, error) {
	toks := splitFields(ll.text)
	if len(toks) == 0 {
		return nil, nil, prevOwner, nil, nil
	}

	i := 0
	written := ""
	owner := prevOwner
	if !ll.inherits {
		written = toks[0].text
		owner = qualifyOwner(written, current)
		i = 1
	} else if owner == "" {
		return nil, nil, "", nil, atFix(ll.line,
			"give the record an owner name, or \"@\" for the zone apex",
			"record starts with whitespace but there is no previous record to take an owner name from")
	}

	// TTL and class may appear in either order, and either may be absent.
	var (
		ttl   *int64
		warns []string
	)
	for i < len(toks) {
		tok := toks[i].text
		if class := strings.ToUpper(tok); dnsClasses[class] {
			if class != "IN" {
				// The API has no class field, so the record is stored as IN
				// whatever the file said. That is the only thing the CLI can do
				// with it, but it is not what the file asked for.
				warns = append(warns, "line "+itoa(ll.line)+": class "+quote(class)+
					" is not supported and the record was imported as IN")
			}
			i++
			continue
		}
		if ttl == nil && looksLikeTTL(tok) {
			v, err := parseTTL(tok)
			if err != nil {
				return nil, nil, owner, nil, at(ll.line, err)
			}
			ttl = ttlPtr(v)
			i++
			continue
		}
		break
	}
	if i >= len(toks) {
		return nil, nil, owner, warns, atFix(ll.line,
			"a record is \"<name> [ttl] [IN] <type> <rdata>\", as in \"www 300 IN A 203.0.113.10\"",
			"record %q has no type", ll.raw)
	}

	typeTok := strings.ToUpper(toks[i].text)
	rrType, typeErr := rdata.ParseRRType(typeTok)
	if typeErr != nil {
		if looksLikeBadTTL(toks[i].text) {
			// A negative or malformed number in the TTL slot is a broken TTL,
			// not a record type nobody has heard of. Saying so is the whole
			// difference between a one-character fix and a puzzle.
			return nil, nil, owner, warns, at(ll.line, badTTL(toks[i].text))
		}
		if !knownRRType(typeTok) {
			return nil, nil, owner, warns, atFix(ll.line,
				"a record is \"<name> [ttl] [IN] <type> <rdata>\", as in \"www 300 IN A 203.0.113.10\"",
				"%q is not a DNS record type", toks[i].text)
		}
		return nil, &Unsupported{
			Line: ll.line, Name: displayOwner(written, owner), Type: typeTok, Raw: ll.raw,
		}, owner, warns, nil
	}
	i++
	if i >= len(toks) {
		return nil, nil, owner, warns, atf(ll.line, "%s record %q has no value", rrType, ll.raw)
	}

	value := strings.TrimSpace(ll.text[toks[i].start:])
	entry, err := rdata.ParseValue(rrType, value)
	if err != nil {
		return nil, nil, owner, warns, at(ll.line, err)
	}
	// A zone file writes targets relative to $ORIGIN; the API stores them
	// absolute, because internal/pdns absolutizes a relative target by
	// appending a bare dot rather than the zone.
	absolutizeTargets(rrType, &entry, current)

	name, nameWarns, err := ownerName(owner, written, zone)
	if err != nil {
		return nil, nil, owner, warns, at(ll.line, err)
	}
	entry.Name = name
	if ttl == nil {
		ttl = defaultTTL
	}
	entry.TTL = copyTTL(ttl)

	for _, w := range nameWarns {
		warns = append(warns, "line "+itoa(ll.line)+": "+w)
	}
	return &Record{
		Name:  name,
		TTL:   copyTTL(ttl),
		Type:  rrType,
		Entry: entry,
		Line:  ll.line,
	}, nil, owner, warns, nil
}

// qualifyOwner expands an owner name as written into the absolute name it
// denotes under origin, following RFC 1035 §5.1: "@" is the origin itself, a
// trailing dot means the name is already absolute, and anything else is
// suffixed with the origin.
//
// With no origin to expand against — no zone argument and no $ORIGIN — the name
// is returned as written. That is the honest answer, and Parse warns about it
// once rather than guessing per record.
func qualifyOwner(owner, origin string) string {
	o := strings.ToLower(strings.TrimSpace(owner))
	switch {
	case o == "@":
		if origin == "" {
			return "@"
		}
		return origin + "."
	case strings.HasSuffix(o, "."):
		return o
	case origin == "":
		return o
	}
	return o + "." + origin + "."
}

// ownerName reduces an already-qualified owner to the zone-relative form the
// API stores. written is the token the file used, empty when the owner was
// inherited, and is only consulted to warn about a spelling.
func ownerName(qualified, written, zone string) (string, []string, error) {
	name, warns, err := rdata.NormalizeNameWithWarnings(qualified, zone)
	if err != nil {
		return "", nil, err
	}
	// A relative owner that already spells out the zone is legal BIND — it
	// expands to "www.example.com.example.com." — and is almost never what the
	// author meant. The CLI's own name grammar rejects it outright; here the
	// file's semantics win and the reader gets told what they bought.
	if raw := strings.ToLower(strings.TrimSpace(written)); zone != "" && raw != "" &&
		raw != "@" && !strings.HasSuffix(raw, ".") && strings.HasSuffix(raw, "."+zone) {
		warns = append(warns, "owner name "+quote(written)+" is relative to the origin, so it means "+
			quote(raw+"."+zone+".")+" — add a trailing dot if you meant the name literally")
	}
	return name, warns, nil
}

// displayOwner is the owner name to show back to the user: what they wrote when
// they wrote something, and the inherited qualified name when they did not.
func displayOwner(written, qualified string) string {
	if written != "" {
		return written
	}
	return qualified
}

// looksLikeBadTTL reports whether a token in the type slot is a broken number
// rather than an unknown type. looksLikeTTL requires a leading digit, so "-5"
// and "+5" fall through to the type slot and would otherwise be reported as
// record types nobody has heard of.
func looksLikeBadTTL(tok string) bool {
	if len(tok) < 2 || (tok[0] != '-' && tok[0] != '+') {
		return false
	}
	for i := 1; i < len(tok); i++ {
		if tok[i] < '0' || tok[i] > '9' {
			return false
		}
	}
	return true
}

// absolutizeTargets rewrites every rdata field that names a host from the
// zone-file convention (relative to $ORIGIN, "@" for the origin) to the
// absolute form rdata.Validate requires and the backend stores.
func absolutizeTargets(t dnsv1alpha1.RRType, e *dnsv1alpha1.RecordEntry, origin string) {
	switch t {
	case dnsv1alpha1.RRTypeCNAME:
		e.CNAME.Content = expand(e.CNAME.Content, origin)
	case dnsv1alpha1.RRTypeALIAS:
		e.ALIAS.Content = expand(e.ALIAS.Content, origin)
	case dnsv1alpha1.RRTypeNS:
		e.NS.Content = expand(e.NS.Content, origin)
	case dnsv1alpha1.RRTypePTR:
		e.PTR.Content = expand(e.PTR.Content, origin)
	case dnsv1alpha1.RRTypeMX:
		e.MX.Exchange = expand(e.MX.Exchange, origin)
	case dnsv1alpha1.RRTypeSRV:
		e.SRV.Target = expand(e.SRV.Target, origin)
	case dnsv1alpha1.RRTypeHTTPS:
		e.HTTPS.Target = expandSVCB(e.HTTPS.Target, origin)
	case dnsv1alpha1.RRTypeSVCB:
		e.SVCB.Target = expandSVCB(e.SVCB.Target, origin)
	case dnsv1alpha1.RRTypeSOA:
		e.SOA.MName = expand(e.SOA.MName, origin)
		e.SOA.RName = expand(e.SOA.RName, origin)
	}
}

// expand makes one host field absolute against origin. A value that is already
// absolute, or that cannot be expanded because there is no origin, is left
// alone — rdata.Validate then reports the missing trailing dot with its own
// fix line rather than the parser inventing a name.
func expand(v, origin string) string {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return v
	case v == "@":
		if origin == "" {
			return v
		}
		return origin + "."
	case strings.HasSuffix(v, "."):
		return v
	case origin == "":
		return v
	}
	return v + "." + origin + "."
}

// expandSVCB is expand with RFC 9460's service form preserved: a bare "." is
// the target "use the owner name", not a relative label.
func expandSVCB(v, origin string) string {
	if strings.TrimSpace(v) == "." {
		return "."
	}
	return expand(v, origin)
}

// knownRRType reports whether name is a registered RR type this API does not
// carry, as opposed to a typo. The distinction matters: an unsupported type is
// reported and the import continues, a typo is an error the user must fix.
//
// dns.StringToType is the IANA registry as miekg/dns tracks it, which is a
// fact rather than a list this package would have to keep current.
func knownRRType(name string) bool {
	if _, ok := dns.StringToType[strings.ToUpper(name)]; ok {
		return true
	}
	// RFC 3597 generic type spelling, TYPE65534.
	if strings.HasPrefix(strings.ToUpper(name), "TYPE") {
		rest := name[4:]
		if rest == "" {
			return false
		}
		for i := 0; i < len(rest); i++ {
			if rest[i] < '0' || rest[i] > '9' {
				return false
			}
		}
		return true
	}
	return false
}

// dedupeWarnings reports values repeated within one (name, type), which
// PowerDNS rejects with a 422 for the whole RRset rather than ignoring.
func dedupeWarnings(records []Record) []string {
	seen := map[string]int{}
	var out []string
	for _, r := range records {
		k := string(r.Type) + "\x00" + r.Name + "\x00" + rdata.Key(r.Type, r.Entry)
		if first, dup := seen[k]; dup {
			out = append(out, "line "+itoa(r.Line)+": duplicate of the "+string(r.Type)+
				" record for "+quote(r.Name)+" on line "+itoa(first))
			continue
		}
		seen[k] = r.Line
	}
	return out
}

func copyTTL(t *int64) *int64 {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

func absolute(zone string) string {
	if zone == "" {
		return ""
	}
	if strings.HasSuffix(zone, ".") {
		return zone
	}
	return zone + "."
}

func quote(s string) string { return "\"" + s + "\"" }
