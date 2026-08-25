// SPDX-License-Identifier: AGPL-3.0-only

package rdata

import (
	"regexp"
	"strings"
)

// namePattern is the kubebuilder pattern on RecordEntry.Name.
var namePattern = regexp.MustCompile(`^(@|[A-Za-z0-9*._-]+)$`)

// IsApex reports whether name is one of the two literal spellings of the apex,
// "@" and "".
//
// Use it for display and for genuinely zone-less contexts. **Using it to gate
// behaviour is a bug** — reach for IsApexIn instead.
//
// The reason is that it is not the whole answer. pdns.QualifyOwner resolves
// "@", "" and an absolute name equal to the zone to the same RRset, and the API
// pattern on RecordEntry.Name permits all three, so a record stored as
// "example.com." is at the apex and this function says it is not. Four separate
// guards were written on this test and all four failed open on that spelling:
// `zone import`'s platformOwned dropped the platform's apex NS on --replace,
// `record apply`'s classify pruned it, the apex CNAME-to-ALIAS rewrite did not
// fire, and platformRisk withheld its warning. Each was fixed at its own call
// site before the pattern was recognised as one root cause.
func IsApex(name string) bool { return name == "@" || name == "" }

// IsApexIn reports whether name refers to the apex of zone, whichever of the
// legal spellings it is written in: "@", "", "example.com." and "EXAMPLE.COM."
// are all the apex of example.com.
//
// This is the test to use anywhere the answer decides what happens to a record
// — a platform-managed guard, a prune exclusion, a rewrite, a warning. Both
// sides go through FQDN, so the trailing-dot and case rules cannot drift from
// the ones pdns.QualifyOwner applies, which is the only definition that
// ultimately matters.
//
// A relative name that spells out the zone is deliberately NOT the apex:
// "example.com" without a trailing dot qualifies to "example.com.example.com.",
// which is the doubling trap NormalizeName rejects rather than a name at the
// apex. An empty zone degrades to the literal test, since without a zone there
// is nothing an absolute name could be compared against.
func IsApexIn(name, zone string) bool {
	return FQDN(name, zone) == FQDN("@", zone)
}

// FQDN returns the absolute name pdns.QualifyOwner will key the RRset on,
// lowercased. It mirrors that function exactly: a trailing dot means the name
// is already absolute, anything else is suffixed with the zone.
func FQDN(name, zone string) string {
	zone = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(zone)), ".")
	name = strings.ToLower(strings.TrimSpace(name))
	if IsApex(name) {
		return zone + "."
	}
	if strings.HasSuffix(name, ".") {
		return name
	}
	return name + "." + zone + "."
}

// NormalizeName canonicalises a user-supplied owner name to the zone-relative
// form the CLI teaches: "@" for the apex, a bare label otherwise, lowercased.
//
// It rejects the trap that pdns.QualifyOwner sets: a relative name that already
// spells out the zone ("www.example.com" in example.com) is suffixed with the
// zone again by the backend, producing "www.example.com.example.com." An
// explicit trailing dot is honoured as absolute; an absolute name inside the
// zone is reduced to its relative form, and one outside the zone is returned
// unchanged with a warning.
func NormalizeName(name, zone string) (string, error) {
	out, _, err := NormalizeNameWithWarnings(name, zone)
	return out, err
}

// NormalizeNameWithWarnings is NormalizeName plus the non-fatal advisories the
// CLI prints — currently, an absolute name that falls outside the zone.
func NormalizeNameWithWarnings(name, zone string) (string, []string, error) {
	zone = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(zone)), ".")
	raw := strings.TrimSpace(name)
	lower := strings.ToLower(raw)

	if IsApex(lower) {
		return "@", nil, nil
	}
	if strings.ContainsAny(raw, " \t") {
		return "", nil, errf("record name %q contains whitespace", name)
	}
	if !namePattern.MatchString(lower) {
		return "", nil, fixf(
			"names may contain letters, digits, \"-\", \"_\", \".\" and \"*\", or be \"@\" for the apex",
			"record name %q is not a valid owner name", name,
		)
	}

	absolute := strings.HasSuffix(lower, ".")
	bare := strings.TrimSuffix(lower, ".")
	if bare == "" {
		return "@", nil, nil
	}
	if err := checkOwnerLabels(name, bare); err != nil {
		return "", nil, err
	}

	if absolute {
		if zone == "" {
			return bare + ".", nil, nil
		}
		if bare == zone {
			return "@", nil, nil
		}
		if strings.HasSuffix(bare, "."+zone) {
			return strings.TrimSuffix(bare, "."+zone), nil, nil
		}
		return bare + ".", []string{
			"name " + quoteStr(bare+".") + " is outside zone " + quoteStr(zone) +
				" — the DNS backend will reject it as out-of-zone",
		}, nil
	}

	if zone != "" {
		if bare == zone {
			return "", nil, fixf(
				"names are relative to the zone — use \"@\" for the apex",
				"record name %q is the zone itself", name,
			)
		}
		if strings.HasSuffix(bare, "."+zone) {
			short := strings.TrimSuffix(bare, "."+zone)
			return "", nil, fixf(
				"names are relative to the zone — use "+quoteStr(short)+", or "+
					quoteStr(bare+".")+" with a trailing dot to force an absolute name",
				"record name %q already includes the zone domain", name,
			)
		}
		if len(bare)+1+len(zone) > maxNameLength {
			return "", nil, errf("record name %q is too long for zone %q, the maximum total length is %d",
				name, zone, maxNameLength)
		}
	}
	return bare, nil, nil
}

// checkOwnerLabels applies label rules the CRD pattern does not: it accepts the
// character set but not the shape, so "a..b" and a 70-character label both pass
// admission today.
func checkOwnerLabels(orig, bare string) error {
	if len(bare) > maxNameLength {
		return errf("record name %q is %d characters, the maximum is %d", orig, len(bare), maxNameLength)
	}
	labels := strings.Split(bare, ".")
	for i, l := range labels {
		if l == "" {
			return errf("record name %q has an empty label", orig)
		}
		if len(l) > 63 {
			return errf("record name %q has a label longer than 63 characters", orig)
		}
		if strings.Contains(l, "*") {
			if l != "*" {
				return fixf(
					"a wildcard must be a whole label, as in \"*\" or \"*.dev\"",
					"record name %q has a partial wildcard label %q", orig, l,
				)
			}
			if i != 0 {
				return fixf(
					"a wildcard must be the leftmost label, as in \"*.dev\"",
					"record name %q has a wildcard that is not the leftmost label", orig,
				)
			}
		}
	}
	return nil
}
