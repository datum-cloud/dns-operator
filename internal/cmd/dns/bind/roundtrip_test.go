// SPDX-License-Identifier: AGPL-3.0-only

package bind

import (
	"sort"
	"strings"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
)

// The round trip is the contract behind `zone export | edit | record apply`: a
// file this package writes must read back as the same records, or a user who
// exports and re-applies without touching anything sees a diff.

const roundTripZone = "example.com"

// roundTripRecords covers all fourteen supported types, both TTL spellings, the
// apex and named owners, and the values most likely to be mangled by quoting:
// a TXT string over a character-string's length, one carrying a semicolon, a
// CAA value, and an HTTPS record with service parameters.
func roundTripRecords(t *testing.T) []Record {
	t.Helper()
	return []Record{
		rec(t, "@", ttlPtr(3600), dnsv1alpha1.RRTypeSOA,
			"ns1.datum.net. hostmaster.example.com. 2024010101 10800 3600 604800 3600"),
		rec(t, "@", ttlPtr(3600), dnsv1alpha1.RRTypeNS, "ns1.datum.net."),
		rec(t, "@", ttlPtr(3600), dnsv1alpha1.RRTypeNS, "ns2.datum.net."),
		rec(t, "@", ttlPtr(300), dnsv1alpha1.RRTypeA, "203.0.113.10"),
		rec(t, "www", ttlPtr(300), dnsv1alpha1.RRTypeA, "203.0.113.11"),
		rec(t, "www", ttlPtr(300), dnsv1alpha1.RRTypeA, "203.0.113.12"),
		rec(t, "@", nil, dnsv1alpha1.RRTypeAAAA, "2001:db8::1"),
		rec(t, "*", ttlPtr(60), dnsv1alpha1.RRTypeA, "203.0.113.99"),
		rec(t, "shop", ttlPtr(300), dnsv1alpha1.RRTypeALIAS, "lb.example.net."),
		rec(t, "cdn", ttlPtr(300), dnsv1alpha1.RRTypeCNAME, "lb.example.net."),
		rec(t, "@", ttlPtr(3600), dnsv1alpha1.RRTypeMX, "10 mail.example.com."),
		rec(t, "@", ttlPtr(3600), dnsv1alpha1.RRTypeMX, "20 backup.example.net."),
		rec(t, "@", ttlPtr(300), dnsv1alpha1.RRTypeTXT, "v=spf1 include:_spf.example.com ~all"),
		rec(t, "_dmarc", ttlPtr(300), dnsv1alpha1.RRTypeTXT, "v=DMARC1; p=none; rua=mailto:d@example.com"),
		rec(t, "dkim", ttlPtr(300), dnsv1alpha1.RRTypeTXT, "p="+strings.Repeat("A", 400)),
		rec(t, "quoted", ttlPtr(300), dnsv1alpha1.RRTypeTXT, `he said "hi" \ then left`),
		rec(t, "_sip._tcp", ttlPtr(300), dnsv1alpha1.RRTypeSRV, "10 5 5060 sipserver.example.com."),
		rec(t, "@", ttlPtr(3600), dnsv1alpha1.RRTypeCAA, "0 issue letsencrypt.org"),
		rec(t, "@", ttlPtr(3600), dnsv1alpha1.RRTypeCAA, `0 iodef "mailto:security@example.com"`),
		rec(t, "1.0", ttlPtr(300), dnsv1alpha1.RRTypePTR, "host.example.com."),
		rec(t, "_443._tcp", ttlPtr(300), dnsv1alpha1.RRTypeTLSA, "3 1 1 "+strings.Repeat("ab", 32)),
		rec(t, "api", ttlPtr(300), dnsv1alpha1.RRTypeHTTPS, "1 . alpn=h3,h2 port=443"),
		rec(t, "svc", ttlPtr(300), dnsv1alpha1.RRTypeSVCB, "1 svc.example.net. alpn=h2 ipv4hint=203.0.113.1"),
	}
}

// key is the identity a round trip must preserve: owner, type, effective TTL,
// and the rendered value. Order is not part of it — Emit groups by type on
// purpose — so both sides are sorted before comparison.
type key struct {
	name  string
	rtype string
	ttl   int64
	value string
}

func keys(records []Record, defaultTTL int64) []key {
	out := make([]key, 0, len(records))
	for _, r := range records {
		ttl := defaultTTL
		if r.TTL != nil {
			ttl = *r.TTL
		}
		out = append(out, key{
			name:  r.Name,
			rtype: string(r.Type),
			ttl:   ttl,
			value: rdata.Render(r.Type, r.Entry),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].rtype != out[j].rtype {
			return out[i].rtype < out[j].rtype
		}
		if out[i].name != out[j].name {
			return out[i].name < out[j].name
		}
		return out[i].value < out[j].value
	})
	return out
}

func TestRoundTrip(t *testing.T) {
	const defaultTTL = 3600
	original := roundTripRecords(t)

	file := emitString(t, roundTripZone, defaultTTL, original)
	t.Logf("emitted zone file:\n%s", file)

	res, err := Parse(strings.NewReader(file), roundTripZone, nil)
	if err != nil {
		t.Fatalf("re-parsing the emitted file: %v", err)
	}
	if len(res.Unsupported) != 0 {
		t.Errorf("re-parse reported unsupported records: %+v", res.Unsupported)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("re-parse reported warnings: %v", res.Warnings)
	}
	if res.Origin != roundTripZone+"." {
		t.Errorf("Origin = %q, want %q", res.Origin, roundTripZone+".")
	}

	want := keys(original, defaultTTL)
	have := keys(res.Records, defaultTTL)
	if len(have) != len(want) {
		t.Fatalf("round trip produced %d records, want %d", len(have), len(want))
	}
	for i := range want {
		if have[i] != want[i] {
			t.Errorf("record %d:\n got %+v\nwant %+v", i, have[i], want[i])
		}
	}
}

// TestRoundTripIsIdempotent is the stronger property: once a set of records has
// been through the parser, emitting and re-parsing it is a fixed point, byte
// for byte. That is what makes `export | edit | apply` show "No changes." when
// nothing was edited.
//
// The first emit is excluded from the comparison because a record whose TTL is
// nil is written without one and reads back carrying $TTL — the single
// documented lossy step, and one that happens at most once.
func TestRoundTripIsIdempotent(t *testing.T) {
	const defaultTTL = 3600

	parseEmit := func(file string) string {
		t.Helper()
		res, err := Parse(strings.NewReader(file), roundTripZone, nil)
		if err != nil {
			t.Fatalf("re-parse: %v", err)
		}
		return emitString(t, roundTripZone, defaultTTL, res.Records)
	}

	first := parseEmit(emitString(t, roundTripZone, defaultTTL, roundTripRecords(t)))
	second := parseEmit(first)
	if first != second {
		t.Errorf("emit is not a fixed point\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestRoundTripThroughEveryValidatedRecord checks that what survives the round
// trip is also what the mutation path will accept, so `zone export` can never
// produce a file `record apply` refuses.
func TestRoundTripValidates(t *testing.T) {
	file := emitString(t, roundTripZone, 3600, roundTripRecords(t))
	res, err := Parse(strings.NewReader(file), roundTripZone, nil)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, r := range res.Records {
		if err := rdata.ValidateInZone(r.Type, r.Entry, roundTripZone); err != nil {
			t.Errorf("%s %s: %v", r.Name, r.Type, err)
		}
	}
}

// TestRoundTripControlCharacters is the guard for a class the emitter used to
// lose outright.
//
// A tab in a TXT value was eaten by the tabwriter the value column ran through,
// and a newline was written literally, producing a file this package's own
// scanner rejects with "unterminated quoted string". rdata now escapes control
// characters as \DDD per RFC 1035 §5.1 and the value column no longer passes
// through any padding, but the property belongs here as well as one layer down:
// the failure was only reachable through Emit, so a test that only exercises
// rdata cannot see it come back.
func TestRoundTripControlCharacters(t *testing.T) {
	values := map[string]string{
		"tab":                  "before\tafter",
		"newline":              "line1\nline2",
		"carriage return":      "line1\rline2",
		"trailing tab":         "value\t",
		"trailing newline":     "value\n",
		"trailing CR":          "value\r",
		"trailing CRLF":        "value\r\n",
		"leading CR":           "\rvalue",
		"CR alone":             "\r",
		"NUL and DEL":          "a\x00b\x7fc",
		"every control byte":   allControlBytes(),
		"control past a chunk": strings.Repeat("x", 254) + "\t" + strings.Repeat("y", 254),
	}

	for name, v := range values {
		t.Run(name, func(t *testing.T) {
			// The entry is built directly rather than through rdata.ParseValue,
			// which trims an unquoted value — correct for a value typed on the
			// command line, and not the shape this test is about. What matters
			// here is a value already sitting in a RecordEntry, as one read off
			// the API is.
			original := []Record{txtRecord("t", 300, v)}
			file := emitString(t, roundTripZone, 300, original)

			// The emitted file must not carry a raw control character: a literal
			// newline ends the character-string, and a literal tab or CR is
			// whitespace the scanner would fold away.
			for i := 0; i < len(file); i++ {
				if c := file[i]; c != '\n' && (c < 0x20 || c == 0x7f) {
					t.Fatalf("emitted file carries a raw control byte %#02x at offset %d:\n%q", c, i, file)
				}
			}

			res, err := Parse(strings.NewReader(file), roundTripZone, nil)
			if err != nil {
				t.Fatalf("re-parsing:\n%q\n%v", file, err)
			}
			if len(res.Records) != 1 {
				t.Fatalf("got %d records, want 1 from:\n%q", len(res.Records), file)
			}
			if got := res.Records[0].Entry.TXT.Content; got != v {
				t.Errorf("value did not survive the round trip:\n got %q\nwant %q\nfile: %q", got, v, file)
			}
		})
	}
}

// A physical line that genuinely ends in CR is indistinguishable from a CRLF
// line ending, so the scanner trims it. This pins that the trim cannot reach
// inside a value: an escaped CR is three ASCII digits by the time it is
// written, and a raw one would have to survive quoting to get here at all.
func TestScanTrailingCRDoesNotReachIntoValues(t *testing.T) {
	// The escaped form, exactly as Emit writes it.
	res := parseOK(t, "$ORIGIN example.com.\nt 300 IN TXT \"value\\013\"\r\n", "example.com", nil)
	if len(res.Records) != 1 {
		t.Fatalf("got %d records, want 1", len(res.Records))
	}
	if got := res.Records[0].Entry.TXT.Content; got != "value\r" {
		t.Errorf("TXT content = %q, want %q — the CRLF trim ate an escaped CR", got, "value\r")
	}
}

// txtRecord builds a TXT record around a literal content string, bypassing the
// presentation-format parser.
func txtRecord(name string, ttl int64, content string) Record {
	entry := dnsv1alpha1.RecordEntry{
		Name: name,
		TTL:  ttlPtr(ttl),
		TXT:  &dnsv1alpha1.TXTRecordSpec{Content: content},
	}
	return Record{Name: name, TTL: entry.TTL, Type: dnsv1alpha1.RRTypeTXT, Entry: entry}
}

func allControlBytes() string {
	var b strings.Builder
	for c := 1; c < 0x20; c++ {
		b.WriteByte(byte(c))
	}
	b.WriteByte(0x7f)
	return b.String()
}
