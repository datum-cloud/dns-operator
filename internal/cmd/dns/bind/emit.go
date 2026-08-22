// SPDX-License-Identifier: AGPL-3.0-only

package bind

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
)

// defaultEmitTTL is the $TTL written when the caller supplies none. It matches
// internal/pdns's own default for an entry with no TTL, so an exported file
// says out loud what the backend was doing silently.
const defaultEmitTTL = 300

// emitOrder is the order type groups are written in: the zone's own metadata
// first, then addresses, then the rest in the order the CRD enum declares them.
// Convention, not requirement — but an export that opens with SOA and NS reads
// like every other zone file the user has seen.
var emitOrder = []dnsv1alpha1.RRType{
	dnsv1alpha1.RRTypeSOA,
	dnsv1alpha1.RRTypeNS,
	dnsv1alpha1.RRTypeA,
	dnsv1alpha1.RRTypeAAAA,
	dnsv1alpha1.RRTypeALIAS,
	dnsv1alpha1.RRTypeCNAME,
	dnsv1alpha1.RRTypeMX,
	dnsv1alpha1.RRTypeTXT,
	dnsv1alpha1.RRTypeSRV,
	dnsv1alpha1.RRTypeCAA,
	dnsv1alpha1.RRTypePTR,
	dnsv1alpha1.RRTypeTLSA,
	dnsv1alpha1.RRTypeHTTPS,
	dnsv1alpha1.RRTypeSVCB,
}

// Emit writes records as a BIND master file.
//
// The output is what Parse reads: `$ORIGIN`, `$TTL`, then `<name> [TTL] IN
// <type> <rdata>` grouped by type and sorted within each group. Rdata comes
// from rdata.Render, so a TXT string longer than a character-string is chunked
// per RFC 1035 and every target carries its trailing dot.
//
// A record whose TTL is nil is written without one and therefore adopts the
// emitted $TTL; that is the only sense in which Emit is lossy, and it is the
// same convention every zone file uses.
func Emit(w io.Writer, origin string, defaultTTL int64, records []Record) error {
	zone := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(origin)), ".")
	if zone == "" {
		return errf("cannot emit a zone file without an origin")
	}
	if defaultTTL <= 0 || defaultTTL > maxTTL {
		defaultTTL = defaultEmitTTL
	}

	byType := map[dnsv1alpha1.RRType][]Record{}
	for _, r := range records {
		if rdata.Render(r.Type, r.Entry) == "" {
			return errf("%s record %q carries no %s value",
				r.Type, displayName(r.Name), strings.ToLower(string(r.Type)))
		}
		byType[r.Type] = append(byType[r.Type], r)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "$ORIGIN %s.\n", zone)
	fmt.Fprintf(&b, "$TTL %d\n", defaultTTL)

	for _, t := range emitOrder {
		group := byType[t]
		if len(group) == 0 {
			continue
		}
		delete(byType, t)
		writeGroup(&b, t, group)
	}
	// A type outside emitOrder can only appear if the enum grows; write it
	// rather than dropping records on the floor.
	for _, t := range sortedTypes(byType) {
		writeGroup(&b, t, byType[t])
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// writeGroup writes one type's records as an aligned block. Alignment is
// cosmetic — the parser splits on whitespace — but a zone file people edit by
// hand is worth keeping readable.
//
// The columns are padded by hand rather than through text/tabwriter, and the
// value is appended after them without passing through any padding at all.
// tabwriter reads a tab in its input as a column separator, so a value that
// contained one would come out as spaces: the exported file would no longer
// describe the zone, and export → edit → apply would then write the corrupted
// value back. rdata escapes control characters today, so no tab can reach here
// — but "no field will ever contain a tab" is not an invariant worth resting a
// silent data loss on.
func writeGroup(b *strings.Builder, t dnsv1alpha1.RRType, group []Record) {
	rendered := make([][4]string, 0, len(group))
	for _, r := range group {
		ttl := ""
		if r.TTL != nil {
			ttl = strconv.FormatInt(*r.TTL, 10)
		}
		rendered = append(rendered, [4]string{
			displayName(r.Name), ttl, string(t), rdata.Render(t, r.Entry),
		})
	}
	sort.SliceStable(rendered, func(i, j int) bool {
		if rendered[i][0] != rendered[j][0] {
			return nameLess(rendered[i][0], rendered[j][0])
		}
		return rendered[i][3] < rendered[j][3]
	})

	nameWidth, ttlWidth := 0, 0
	for _, row := range rendered {
		nameWidth = max(nameWidth, len(row[0]))
		ttlWidth = max(ttlWidth, len(row[1]))
	}

	fmt.Fprintf(b, "\n; %s\n", t)
	for _, row := range rendered {
		b.WriteString(pad(row[0], nameWidth))
		b.WriteByte(' ')
		b.WriteString(pad(row[1], ttlWidth))
		b.WriteString(" IN ")
		b.WriteString(pad(row[2], len(string(t))))
		b.WriteByte(' ')
		b.WriteString(row[3])
		b.WriteByte('\n')
	}
}

// pad right-aligns nothing and left-aligns everything, which is what a zone
// file's columns do.
func pad(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

// nameLess sorts the apex first and then alphabetically, which is the order a
// reader scans a zone file in.
func nameLess(a, b string) bool {
	if a == "@" {
		return b != "@"
	}
	if b == "@" {
		return false
	}
	return a < b
}

// displayName renders an owner name for the file. An empty name means the apex,
// the same way the API reads it.
func displayName(n string) string {
	if n == "" {
		return "@"
	}
	return n
}

func sortedTypes(m map[dnsv1alpha1.RRType][]Record) []dnsv1alpha1.RRType {
	out := make([]dnsv1alpha1.RRType, 0, len(m))
	for t := range m {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }
