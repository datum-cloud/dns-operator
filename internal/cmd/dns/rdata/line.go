// SPDX-License-Identifier: AGPL-3.0-only

package rdata

import (
	"strings"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// Line is one parsed zone-file / dig-shaped record line.
type Line struct {
	Name string
	TTL  *int64
	Type dnsv1alpha1.RRType
	// Rdata is the remainder of the line verbatim, quoting intact, ready for
	// ParseValue.
	Rdata string
}

// dnsClasses are the class tokens that may appear between the TTL and the type.
// Only IN is meaningful here; the others are accepted so a pasted line parses.
var dnsClasses = map[string]bool{"IN": true, "CH": true, "HS": true, "CS": true}

// ParseLine parses a whole record line as `dig` prints it and as zone files
// write it: "www 300 IN A 203.0.113.10". The TTL and the class are optional and
// may appear in either order, and a trailing ";" comment is stripped.
func ParseLine(s string) (Line, error) {
	var out Line
	toks, err := tokenize(s, true)
	if err != nil {
		return out, err
	}
	if len(toks) == 0 {
		return out, errf("record line is empty")
	}
	if len(toks) < 3 {
		return out, fixf(
			"a line is \"<name> [ttl] [IN] <type> <rdata>\", as in \"www 300 IN A 203.0.113.10\"",
			"record line %q is missing a type or value", s,
		)
	}

	i := 0
	out.Name = toks[i].text
	i++

	for i < len(toks) {
		up := strings.ToUpper(toks[i].text)
		if dnsClasses[up] {
			i++
			continue
		}
		if out.TTL == nil && looksLikeTTL(toks[i].text) {
			ttl, terr := ParseTTL(toks[i].text)
			if terr != nil {
				return out, terr
			}
			out.TTL = ttl
			i++
			continue
		}
		break
	}
	if i >= len(toks) {
		return out, fixf(
			"a line is \"<name> [ttl] [IN] <type> <rdata>\", as in \"www 300 IN A 203.0.113.10\"",
			"record line %q has no record type", s,
		)
	}

	t, err := ParseRRType(toks[i].text)
	if err != nil {
		return out, err
	}
	out.Type = t
	i++
	if i >= len(toks) {
		return out, errf("record line %q has no %s value", s, t)
	}
	out.Rdata = strings.TrimSpace(rdataFrom(s, toks[i].start))
	if out.Rdata == "" {
		return out, errf("record line %q has no %s value", s, t)
	}
	return out, nil
}

// rdataFrom slices the original line so quoting and escaping survive untouched.
// A ";" comment outside a quoted string is dropped.
func rdataFrom(s string, start int) string {
	rest := s[start:]
	inQuote := false
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case '\\':
			i++
		case '"':
			inQuote = !inQuote
		case ';':
			if !inQuote {
				return rest[:i]
			}
		}
	}
	return rest
}

// looksLikeTTL keeps the TTL slot from swallowing a type or class token: only a
// bare number or a duration counts.
func looksLikeTTL(s string) bool {
	if s == "" {
		return false
	}
	digits := true
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			digits = false
			break
		}
	}
	if digits {
		return true
	}
	if s[0] < '0' || s[0] > '9' {
		return false
	}
	_, err := ParseTTL(s)
	return err == nil
}
