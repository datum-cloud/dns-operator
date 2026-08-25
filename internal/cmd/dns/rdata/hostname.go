// SPDX-License-Identifier: AGPL-3.0-only

package rdata

import "strings"

// Hostname strictness is tiered the same way the portal's zod schemas tier it:
//
//   - strict  — RFC 1123 letters, digits and interior hyphens only. Used for
//     MX exchange, NS content and SOA mname, which name real hosts.
//   - relaxed — additionally allows "_" anywhere in a label. Used for CNAME,
//     ALIAS, PTR content and SRV target, where names like
//     _domainconnect.gd.domaincontrol.com are common in practice.
//
// SVCB/HTTPS targets use relaxed plus a bare "." (RFC 9460 service form).
const (
	strictHost  = false
	relaxedHost = true
)

const maxNameLength = 253

// checkHostname validates the presentation form of a hostname. A trailing dot
// is permitted and ignored; callers that require one check separately with
// requireFQDN.
func checkHostname(field, h string, underscores bool) error {
	if h == "" {
		return errf("%s must not be empty", field)
	}
	name := strings.TrimSuffix(h, ".")
	if name == "" {
		return errf("%s %q is the DNS root, which is not a valid host name here", field, h)
	}
	if len(name) > maxNameLength {
		return errf("%s %q is %d characters, the maximum is %d", field, h, len(name), maxNameLength)
	}
	for _, label := range splitDNSLabels(name) {
		if err := checkLabel(field, h, label, underscores); err != nil {
			return err
		}
	}
	return nil
}

// splitDNSLabels splits a presentation-format name on its UNESCAPED dots. A
// backslash-escaped dot belongs to the label it sits in — "first\.last" is one
// label, the local part of an SOA mailbox — and splitting on it would validate
// (and count) a different name than the one the user wrote.
func splitDNSLabels(name string) []string {
	var out []string
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] == '\\' && i+1 < len(name) {
			b.WriteByte(name[i])
			b.WriteByte(name[i+1])
			i++
			continue
		}
		if name[i] == '.' {
			out = append(out, b.String())
			b.Reset()
			continue
		}
		b.WriteByte(name[i])
	}
	out = append(out, b.String())
	return out
}

func checkLabel(field, whole, label string, underscores bool) error {
	if label == "" {
		return errf("%s %q has an empty label", field, whole)
	}
	if len(label) > 63 {
		return errf("%s %q has a label longer than 63 characters", field, whole)
	}
	if label[0] == '-' || label[len(label)-1] == '-' {
		return errf("%s %q has a label that starts or ends with a hyphen", field, whole)
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		// A backslash escape stands for the character after it, whatever it
		// is; the pair counts as one character of the label.
		if c == '\\' && i+1 < len(label) {
			i++
			continue
		}
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
		case c == '_' && underscores:
		case c == '_':
			return fixf(
				"only CNAME, ALIAS, PTR and SRV targets may contain underscores",
				"%s %q contains an underscore, which is not allowed in a host name", field, whole,
			)
		default:
			return errf("%s %q contains an invalid character %q", field, whole, string(c))
		}
	}
	return nil
}

// requireFQDN enforces the trailing dot on a target field.
//
// internal/pdns absolutizes every target by appending a dot and nothing else
// (qualifyIfNeeded), so a relative "mail" becomes the root-relative "mail."
// rather than "mail.<zone>.". There is no spelling of a zone-relative target
// that behaves the way a user expects, so the CLI insists on the absolute form.
// zone may be empty, in which case the fix omits the zone-qualified suggestion.
//
// The suggestion depends on how many labels the value already has, and getting
// that wrong is worse than saying nothing. A bare label is almost always a name
// inside the zone — "mail" means "mail.<zone>." — but a value that already has
// a dot in it is a name the user wrote out, and appending the zone to it would
// propose "lb.example.net.example.com.": the very doubling that the owner-name
// rule next door exists to prevent. A Fix line that walks the user into the bug
// it is fixing is worse than no Fix line.
func requireFQDN(field, value, zone string) error {
	if strings.HasSuffix(value, ".") {
		return nil
	}
	const lead = "targets are absolute, not zone-relative — "
	fix := lead + "add a trailing dot"
	switch {
	case len(splitDNSLabels(value)) > 1:
		// Already spelled out; it just needs terminating.
		fix = lead + "did you mean " + quoteStr(value+".") + "?"
	case zone != "":
		// A single label, which reads as a name inside the zone.
		fix = lead + "did you mean " +
			quoteStr(value+"."+strings.TrimSuffix(strings.ToLower(zone), ".")+".") + "?"
	}
	return fixf(fix, "%s %q is not a fully qualified domain name", field, value)
}

func quoteStr(s string) string { return "\"" + s + "\"" }
