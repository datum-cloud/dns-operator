// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"fmt"
	"sort"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// row is one record as a user thinks of it: a single value at a single name.
// The DNSRecordSet it came from is retained because status, provenance and the
// object name all live there.
type row struct {
	name   string
	rrType dnsv1alpha1.RRType
	ttl    *int64
	value  string
	status string
	detail string
	prov   provenance
	set    *dnsv1alpha1.DNSRecordSet
	entry  dnsv1alpha1.RecordEntry
}

// flatten turns the zone's type buckets into one row per value. This is the
// whole read-side translation: everything downstream — filters, sorting, the
// footer tally — works on records, not on sets.
func flatten(sets []dnsv1alpha1.DNSRecordSet, zone *dnsv1alpha1.DNSZone) []row {
	var rows []row
	for i := range sets {
		rs := &sets[i]
		t := rs.Spec.RecordType
		for _, raw := range rs.Spec.Records {
			entry := canonicalEntry(t, raw)
			// Zone-aware, so "@" and "example.com." — and a relative label
			// and its fully qualified form — resolve to the same owner. The
			// zone-less form can only fold case and the trailing dot.
			word, detail := util.RecordStatusInZone(rs, raw.Name, zone.Spec.DomainName)
			rows = append(rows, row{
				name:   displayName(entry.Name),
				rrType: t,
				ttl:    entry.TTL,
				value:  rdata.Render(t, entry),
				status: word,
				detail: detail,
				prov:   classify(rs, raw, zone.Spec.DomainName),
				set:    rs,
				entry:  entry,
			})
		}
	}
	sortRows(rows)
	return rows
}

// sortRows orders by name, then type, then value, with the apex first: a zone's
// own records belong at the top of its listing, not filed under "@" wherever
// punctuation happens to sort.
func sortRows(rows []row) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].name != rows[j].name {
			return lessName(rows[i].name, rows[j].name)
		}
		if rows[i].rrType != rows[j].rrType {
			return rows[i].rrType < rows[j].rrType
		}
		return rows[i].value < rows[j].value
	})
}

func lessName(a, b string) bool {
	switch {
	case a == "@":
		return true
	case b == "@":
		return false
	default:
		return a < b
	}
}

// displayName renders a stored owner name in the zone-relative form the CLI
// teaches. The API accepts "" for the apex; the CLI never shows a blank cell.
//
// This is one of the two places rdata.IsApex is still the right call: it is
// display, and it deliberately does NOT fold an absolutely-spelled apex like
// "example.com." into "@". Those really are two different strings in
// spec.records, a user looking at this column is often looking precisely
// because something is stored in an unexpected spelling, and rewriting it here
// would hide the evidence. What matters is that nothing DECIDES anything on
// this value — classification, guarding and matching all go through the
// qualified comparison instead.
func displayName(name string) string {
	if rdata.IsApex(name) {
		return "@"
	}
	return name
}

// canonicalEntry returns an entry in its logical form, the one everything in
// this package holds and compares.
//
// rdata's read paths decode defensively, so this is not needed to make Render,
// Key or Equal behave. It is called where the logical value itself is wanted —
// the value handed to Fields, or echoed back after a write — so that the
// storage encoding never leaks into anything a person reads. The encode/decode
// pair belongs to rdata; nothing here quotes or chunks by hand.
func canonicalEntry(t dnsv1alpha1.RRType, e dnsv1alpha1.RecordEntry) dnsv1alpha1.RecordEntry {
	return rdata.EntryFromAPI(t, e)
}

// apiEntry converts an entry to the form the API must store, and is called once
// on each value immediately before it is submitted.
//
// It delegates wholesale rather than special-casing TXT: TXT is the only type
// with a wire form today, and a write path that says "encode this entry" keeps
// working if that stops being true, where one that names TXT does not.
//
// Entries already on the server are left exactly as they are — this command was
// asked to change one record, not to rewrite its neighbours.
func apiEntry(t dnsv1alpha1.RRType, e dnsv1alpha1.RecordEntry) dnsv1alpha1.RecordEntry {
	return rdata.EntryForAPI(t, e)
}

// entriesEqual compares two values of the same type through the canonical form,
// so a TXT record entered as a bare string matches the quoted one stored for it.
func entriesEqual(t dnsv1alpha1.RRType, a, b dnsv1alpha1.RecordEntry) bool {
	return rdata.Equal(t, canonicalEntry(t, a), canonicalEntry(t, b))
}

// statusOrder fixes the footer's column order so a tally reads the same way
// every time, best outcome first.
//
// util.StatusUnknown is deliberately absent. RecordStatus returns it only for a
// nil record set, and no path in this package can produce one — flatten and
// describe both hold a real object. Listing it would advertise a filter token
// that can never match anything.
var statusOrder = []string{
	util.StatusProgrammed,
	util.StatusPending,
	util.StatusConflict,
	util.StatusNotOwner,
	util.StatusError,
	util.StatusRejected,
}

// tally counts rows by status word, returning the known statuses in
// statusOrder and any reason the server invented after them, alphabetically.
// Zero-count categories are omitted: a footer that lists what did not happen
// buries what did.
func tally(rows []row) []string {
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.status]++
	}

	var parts []string
	seen := map[string]bool{}
	for _, s := range statusOrder {
		seen[s] = true
		if counts[s] > 0 {
			parts = append(parts, countOf(counts[s], s))
		}
	}

	var extra []string
	for s := range counts {
		if !seen[s] {
			extra = append(extra, s)
		}
	}
	sort.Strings(extra)
	for _, s := range extra {
		parts = append(parts, countOf(counts[s], s))
	}
	return parts
}

// countOf renders a count with a word that is already in the right form, for
// the status tally where "3 Programmed" must not become "3 Programmeds".
func countOf(n int, word string) string {
	return fmt.Sprintf("%d %s", n, word)
}

// pluralize returns the singular or plural form of a word for a count. It does
// not include the count itself; pair it with countOf when both are wanted.
func pluralize(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}
