// SPDX-License-Identifier: AGPL-3.0-only

package rdata

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// Regression tests for the findings of the adversarial review. Each one fails
// against the code as it was before the corresponding fix.

// quoteIfNeededSim reproduces pdns.quoteIfNeeded exactly: an already-quoted
// value is passed through, anything else is wrapped and has its semicolons
// escaped. Several tests below assert against the real backend behaviour rather
// than against an assumption about it.
func quoteIfNeededSim(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if s[i] == ';' && (i == 0 || s[i-1] != '\\') {
			b.WriteString(`\;`)
		} else {
			b.WriteByte(s[i])
		}
	}
	b.WriteByte('"')
	return b.String()
}

// txtWire returns an entry holding the API's stored form, which is what a read
// path that has not decoded hands to Key, Render, Fields and Validate.
func txtWire(logical string) dnsv1alpha1.RecordEntry {
	return dnsv1alpha1.RecordEntry{
		TXT: &dnsv1alpha1.TXTRecordSpec{Content: TXTContentForAPI(logical)},
	}
}

// ---------------------------------------------------------------------------
// HIGH 1: the package must agree with itself about what txt.content holds.
// ---------------------------------------------------------------------------

// TestTXTDeleteByValueMatches is the user-visible symptom: someone types the
// value exactly as they created it and the CLI deletes nothing, because the
// stored form and the typed form are different strings.
func TestTXTDeleteByValueMatches(t *testing.T) {
	for _, logical := range []string{
		"hello world",
		"v=spf1 include:_spf.example.com ~all",
		"v=DMARC1; p=none",
		strings.Repeat("a", 300),
	} {
		typed, err := ParseValue(dnsv1alpha1.RRTypeTXT, `"`+logical+`"`)
		if err != nil {
			// A value containing a quote-relevant character is covered below.
			typed, err = ParseValue(dnsv1alpha1.RRTypeTXT, logical)
			if err != nil {
				t.Fatalf("parsing %q: %v", logical, err)
			}
		}
		stored := txtWire(logical)
		if !Equal(dnsv1alpha1.RRTypeTXT, typed, stored) {
			t.Fatalf("delete-by-value would match nothing for %q:\n typed key %q\nstored key %q",
				logical, Key(dnsv1alpha1.RRTypeTXT, typed), Key(dnsv1alpha1.RRTypeTXT, stored))
		}
	}
}

// TestTXTRenderDoesNotAccumulateQuoting covers the read-back display bug: a
// set/re-set cycle must not add a layer of escaping every time.
func TestTXTRenderDoesNotAccumulateQuoting(t *testing.T) {
	const logical = `v=DMARC1; p=none`
	first := Render(dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RecordEntry{
		TXT: &dnsv1alpha1.TXTRecordSpec{Content: logical},
	})
	// Round the value through the API form and render again, as a read-back
	// would.
	second := Render(dnsv1alpha1.RRTypeTXT, txtWire(logical))
	if first != second {
		t.Fatalf("rendering the stored form differs from rendering the logical form\n first %q\nsecond %q",
			first, second)
	}
	// And a third pass, in case one layer is added per cycle.
	third := Render(dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RecordEntry{
		TXT: &dnsv1alpha1.TXTRecordSpec{Content: TXTContentForAPI(second)},
	})
	if third != first {
		t.Fatalf("quoting accumulates across cycles: %q then %q", first, third)
	}
}

// TestTXTLengthCapAppliesToLogicalForm: a record the CLI created must remain
// editable by the CLI. Escaping and chunking inflate the byte count, so
// measuring the stored form rejects values that were legal when written.
func TestTXTLengthCapAppliesToLogicalForm(t *testing.T) {
	// 2040 logical bytes, whose stored form exceeds the 2048 cap.
	logical := strings.Repeat("a", 2040)
	stored := TXTContentForAPI(logical)
	if len(stored) <= maxTXTLength {
		t.Fatalf("test is not exercising the case: stored form is only %d bytes", len(stored))
	}

	e := dnsv1alpha1.RecordEntry{Name: "@", TXT: &dnsv1alpha1.TXTRecordSpec{Content: logical}}
	if err := Validate(dnsv1alpha1.RRTypeTXT, e); err != nil {
		t.Fatalf("a 2040-byte logical value must validate: %v", err)
	}
	back := txtWire(logical)
	back.Name = "@"
	if err := Validate(dnsv1alpha1.RRTypeTXT, back); err != nil {
		t.Fatalf("the same value read back from the API must still validate: %v", err)
	}

	// The cap still bites on a genuinely oversized logical value.
	over := dnsv1alpha1.RecordEntry{Name: "@", TXT: &dnsv1alpha1.TXTRecordSpec{
		Content: strings.Repeat("a", maxTXTLength+1),
	}}
	if err := Validate(dnsv1alpha1.RRTypeTXT, over); err == nil {
		t.Fatal("an oversized logical value must still be rejected")
	}
}

// TestTXTFlagsAndPresentationAgree: --data must not bypass the decoder that a
// positional value goes through, or the two notations produce different records
// for the same text.
func TestTXTFlagsAndPresentationAgree(t *testing.T) {
	for _, in := range []string{
		`"v=spf1 ~all"`,
		`v=spf1 ~all`,
		`"v=DMARC1; p=none"`,
		`say "hi"`,
		`"chunk one" "chunk two"`,
	} {
		want, err := ParseValue(dnsv1alpha1.RRTypeTXT, in)
		if err != nil {
			t.Fatalf("ParseValue(%q): %v", in, err)
		}
		got, anySet := flagEntry(t, dnsv1alpha1.RRTypeTXT, []string{"--data", in})
		if !anySet {
			t.Fatalf("--data %q did not register as set", in)
		}
		if got.TXT.Content != want.TXT.Content {
			t.Fatalf("notations disagree for %q:\n--data %q\n  pos  %q",
				in, got.TXT.Content, want.TXT.Content)
		}
	}
}

// TestTXTFlagRejectsMixedQuoting: the malformed value must be refused through
// the flag exactly as it is through the positional, rather than reaching the
// backend as broken presentation data.
func TestTXTFlagRejectsMixedQuoting(t *testing.T) {
	const bad = `"a" b "c"`
	if _, err := ParseValue(dnsv1alpha1.RRTypeTXT, bad); err == nil {
		t.Fatal("the positional form must reject mixed quoting")
	}
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterFlags(fs, dnsv1alpha1.RRTypeTXT)
	if err := fs.Parse([]string{"--data", bad}); err != nil {
		t.Fatal(err)
	}
	_, _, err := FromFlags(fs, dnsv1alpha1.RRTypeTXT)
	if err == nil {
		t.Fatal("--data must reject mixed quoting too")
	}
	if !strings.Contains(err.Error(), "mixes quoted and unquoted") {
		t.Fatalf("unexpected error %q", err)
	}
}

// TestTXTCorruptionModes walks the values that pdns.quoteIfNeeded mangles when
// it is handed a logical string. Each must survive encode, backend
// pass-through, and decode, byte for byte.
//
// Note the semicolon row: it behaves identically with and without
// TXTContentForAPI, which is exactly why a caller who forgets the helper passes
// every obvious manual test and ships the other four.
func TestTXTCorruptionModes(t *testing.T) {
	cases := []struct {
		name    string
		logical string
	}{
		{"embedded quote", `say "hi"`},
		{"trailing backslash", `path\`},
		{"embedded backslash", `a\b`},
		{"semicolon", `v=DMARC1; p=none`},
		{"over 255 bytes", strings.Repeat("x", 300)},
		{"quote and semicolon", `a "b"; c`},
		{"only a quote", `"`},
		{"embedded newline", "line1\nline2"},
		{"trailing carriage return", "value\r"},
		{"literal backslash-digits", `a\010b`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire := TXTContentForAPI(tc.logical)

			// The backend must pass the encoded form through untouched.
			if got := quoteIfNeededSim(wire); got != wire {
				t.Fatalf("quoteIfNeeded would rewrite the stored form:\n stored %q\n after  %q", wire, got)
			}

			// No character-string may exceed 255 bytes of content.
			for _, part := range strings.Split(wire, `" "`) {
				if len(part) > 257 {
					t.Fatalf("a character-string of %d bytes exceeds the RFC 1035 limit", len(part))
				}
			}

			// And it must decode back to exactly what was written.
			if got := TXTContentFromAPI(wire); got != tc.logical {
				t.Fatalf("round trip changed the value:\n  in %q\n out %q\nvia %q", tc.logical, got, wire)
			}

			// Every read path agrees with the decode.
			stored := txtWire(tc.logical)
			logical := dnsv1alpha1.RecordEntry{TXT: &dnsv1alpha1.TXTRecordSpec{Content: tc.logical}}
			if !Equal(dnsv1alpha1.RRTypeTXT, stored, logical) {
				t.Fatalf("Key disagrees between the stored and logical forms of %q", tc.logical)
			}
			if Render(dnsv1alpha1.RRTypeTXT, stored) != Render(dnsv1alpha1.RRTypeTXT, logical) {
				t.Fatalf("Render disagrees between the stored and logical forms of %q", tc.logical)
			}
			sf := Fields(dnsv1alpha1.RRTypeTXT, stored)
			lf := Fields(dnsv1alpha1.RRTypeTXT, logical)
			if len(sf) == 0 || len(lf) == 0 || sf[0][1] != lf[0][1] {
				t.Fatalf("Fields disagrees between the stored and logical forms of %q", tc.logical)
			}
			if sf[0][1] != tc.logical {
				t.Fatalf("describe shows %q, want the logical value %q", sf[0][1], tc.logical)
			}
		})
	}
}

// TestTXTQuoteWrappedContentCollapses pins the one case the two
// representations genuinely cannot distinguish: a logical value whose first and
// last characters are both quotes is indistinguishable from the wire form of
// its own contents, so it is read as the latter.
//
// This is not a bug that can be fixed here — pdns.quoteIfNeeded makes exactly
// the same guess, so submitting the value any other way corrupts it at the
// backend instead. The alternative, not decoding on read, costs far more: it
// breaks delete-by-value for every ordinary TXT record and accumulates a layer
// of quoting on every set/re-set cycle.
func TestTXTQuoteWrappedContentCollapses(t *testing.T) {
	const ambiguous = `"quoted"`
	if got := TXTContentFromAPI(TXTContentForAPI(ambiguous)); got != "quoted" {
		t.Fatalf("expected the documented collapse to %q, got %q", "quoted", got)
	}
	// It is still self-consistent: every read path agrees on the same answer,
	// so nothing downstream sees two different values for one record.
	stored := txtWire(ambiguous)
	logical := dnsv1alpha1.RecordEntry{TXT: &dnsv1alpha1.TXTRecordSpec{Content: ambiguous}}
	if !Equal(dnsv1alpha1.RRTypeTXT, stored, logical) {
		t.Fatal("the two forms must at least agree with each other")
	}
	// And a value that merely contains quotes, rather than being wrapped in
	// them, survives untouched.
	for _, safe := range []string{`say "hi"`, `"a" and "b" plus`, `x"y"`} {
		if got := TXTContentFromAPI(TXTContentForAPI(safe)); got != safe {
			t.Fatalf("interior quotes must survive: %q became %q", safe, got)
		}
	}
}

// TestTXTUnencodedContentIsStillMangled documents why the helper exists: the
// same values handed to the backend raw do not survive.
func TestTXTUnencodedContentIsStillMangled(t *testing.T) {
	for _, logical := range []string{`say "hi"`, `path\`, `a\b`} {
		raw := quoteIfNeededSim(logical)
		if decoded, err := decodeTXTStrings(raw); err == nil && decoded == logical {
			t.Fatalf("expected %q to be mangled when submitted unencoded, but it survived as %q",
				logical, raw)
		}
	}
}

// ---------------------------------------------------------------------------
// HIGH 2: Validate must read owner names the way the backend will.
// ---------------------------------------------------------------------------

func TestValidateInZoneRejectsZoneSuffixedName(t *testing.T) {
	e := dnsv1alpha1.RecordEntry{
		Name: "www.example.com",
		A:    &dnsv1alpha1.ARecordSpec{Content: "203.0.113.10"},
	}
	err := ValidateInZone(dnsv1alpha1.RRTypeA, e, "example.com")
	if err == nil {
		t.Fatal("a name that already spells out the zone must be rejected; the backend writes www.example.com.example.com.")
	}
	if !strings.Contains(err.Error(), "already includes the zone domain") {
		t.Fatalf("unexpected error %q", err)
	}
	if !strings.Contains(FixFor(err), `"www"`) {
		t.Fatalf("fix %q should offer the bare label", FixFor(err))
	}
}

func TestValidateInZoneApexIsQualified(t *testing.T) {
	const zone = "example.com"

	// Every spelling of the apex must be treated as the apex.
	for _, apexName := range []string{"@", "example.com."} {
		cname := dnsv1alpha1.RecordEntry{
			Name:  apexName,
			CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "lb.example.net."},
		}
		if err := ValidateInZone(dnsv1alpha1.RRTypeCNAME, cname, zone); err == nil {
			t.Fatalf("a CNAME at %q is a CNAME at the apex and must be rejected", apexName)
		} else if !strings.Contains(err.Error(), "zone apex") {
			t.Fatalf("unexpected error for %q: %v", apexName, err)
		}

		soa := dnsv1alpha1.RecordEntry{Name: apexName, SOA: &dnsv1alpha1.SOARecordSpec{
			MName: "ns1.datum.net.", RName: "hostmaster.example.com.",
		}}
		if err := ValidateInZone(dnsv1alpha1.RRTypeSOA, soa, zone); err != nil {
			t.Fatalf("an SOA at %q IS at the apex and must be accepted: %v", apexName, err)
		}
	}

	// And a non-apex name still is not the apex.
	cname := dnsv1alpha1.RecordEntry{
		Name:  "www.example.com.",
		CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "lb.example.net."},
	}
	if err := ValidateInZone(dnsv1alpha1.RRTypeCNAME, cname, zone); err != nil {
		t.Fatalf("a CNAME at an absolute non-apex name must be accepted: %v", err)
	}
	soa := dnsv1alpha1.RecordEntry{Name: "www.example.com.", SOA: &dnsv1alpha1.SOARecordSpec{
		MName: "ns1.datum.net.", RName: "hostmaster.example.com.",
	}}
	if err := ValidateInZone(dnsv1alpha1.RRTypeSOA, soa, zone); err == nil {
		t.Fatal("an SOA away from the apex must still be rejected")
	}
}

// TestValidateWithoutZoneKeepsLiteralBehaviour pins what the zone-less path can
// and cannot do, so the difference is deliberate rather than accidental.
func TestValidateWithoutZoneKeepsLiteralBehaviour(t *testing.T) {
	cname := dnsv1alpha1.RecordEntry{
		Name:  "@",
		CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "lb.example.net."},
	}
	if err := Validate(dnsv1alpha1.RRTypeCNAME, cname); err == nil {
		t.Fatal("the literal apex must still be caught without a zone")
	}
	soa := dnsv1alpha1.RecordEntry{Name: "@", SOA: &dnsv1alpha1.SOARecordSpec{
		MName: "ns1.datum.net.", RName: "hostmaster.example.com.",
	}}
	if err := Validate(dnsv1alpha1.RRTypeSOA, soa); err != nil {
		t.Fatalf("the literal apex must still be accepted for SOA without a zone: %v", err)
	}
}

// ---------------------------------------------------------------------------
// MEDIUM 1: an out-of-zone name fails the whole record set, not just itself.
// ---------------------------------------------------------------------------

func TestValidateInZoneRejectsOutOfZoneName(t *testing.T) {
	e := dnsv1alpha1.RecordEntry{
		Name: "www.other.net.",
		A:    &dnsv1alpha1.ARecordSpec{Content: "203.0.113.10"},
	}
	err := ValidateInZone(dnsv1alpha1.RRTypeA, e, "example.com")
	if err == nil {
		t.Fatal("an out-of-zone name must be an error: PowerDNS rejects the whole PATCH, taking every other record in the same call with it")
	}
	if !strings.Contains(err.Error(), "outside zone") {
		t.Fatalf("unexpected error %q", err)
	}
	if !strings.Contains(FixFor(err), "entire record set") {
		t.Fatalf("fix %q should say the blast radius is the whole record set", FixFor(err))
	}

	// NormalizeName keeps it as a warning: normalizing is not submitting, and
	// the caller may be listing rather than writing.
	if _, warns, nerr := NormalizeNameWithWarnings("www.other.net.", "example.com"); nerr != nil {
		t.Fatalf("NormalizeName must still succeed: %v", nerr)
	} else if len(warns) != 1 {
		t.Fatalf("NormalizeName should still warn, got %v", warns)
	}
}

// ---------------------------------------------------------------------------
// MEDIUM 2: TTL grouping must match how the backend merges owners.
// ---------------------------------------------------------------------------

func TestWarningsInZoneGroupsByQualifiedName(t *testing.T) {
	a := dnsv1alpha1.RecordEntry{
		Name: "www", TTL: ptr(int64(300)),
		A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.10"},
	}
	b := dnsv1alpha1.RecordEntry{
		Name: "www.example.com.", TTL: ptr(int64(900)),
		A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.11"},
	}

	got := WarningsInZone(dnsv1alpha1.RRTypeA, "example.com", a, b)
	if len(got) != 1 || !strings.Contains(got[0], "disagree on TTL") {
		t.Fatalf("two spellings of one owner with different TTLs must warn, got %v", got)
	}
	if !strings.Contains(got[0], "applies the first one, 300") {
		t.Fatalf("the warning should name the TTL that wins, got %q", got[0])
	}

	// Genuinely different owners still do not warn.
	c := dnsv1alpha1.RecordEntry{
		Name: "api", TTL: ptr(int64(900)),
		A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.12"},
	}
	if got := WarningsInZone(dnsv1alpha1.RRTypeA, "example.com", a, c); len(got) != 0 {
		t.Fatalf("different owners must not warn, got %v", got)
	}

	// The zone-less form still works for names spelled the same way.
	d := dnsv1alpha1.RecordEntry{
		Name: "WWW", TTL: ptr(int64(900)),
		A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.13"},
	}
	if got := Warnings(dnsv1alpha1.RRTypeA, a, d); len(got) != 1 {
		t.Fatalf("case-distinct spellings of one owner must warn, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// LOW: escapes.
// ---------------------------------------------------------------------------

func TestCAAValueRejectsBackslash(t *testing.T) {
	e := dnsv1alpha1.RecordEntry{Name: "@", CAA: &dnsv1alpha1.CAARecordSpec{
		Flag: 0, Tag: "issue", Value: `letsencrypt\.org`,
	}}
	err := Validate(dnsv1alpha1.RRTypeCAA, e)
	if err == nil {
		t.Fatal("a backslash in a CAA value must be rejected: quoteIfNeeded escapes only semicolons, so what is rendered and what is written would differ")
	}
	if !strings.Contains(err.Error(), "backslash") {
		t.Fatalf("unexpected error %q", err)
	}
}

// TestRenderedCAAMatchesTheBackend closes the loop on the rule above: for every
// value the CLI accepts, Render must produce exactly what pdns writes.
func TestRenderedCAAMatchesTheBackend(t *testing.T) {
	for _, value := range []string{"letsencrypt.org", "mailto:security@example.com", "a;b"} {
		e := dnsv1alpha1.RecordEntry{Name: "@", CAA: &dnsv1alpha1.CAARecordSpec{
			Flag: 0, Tag: "issue", Value: value,
		}}
		if err := Validate(dnsv1alpha1.RRTypeCAA, e); err != nil {
			t.Fatalf("%q should be accepted: %v", value, err)
		}
		rendered := Render(dnsv1alpha1.RRTypeCAA, e)
		backend := "0 issue " + quoteIfNeededSim(value)
		if rendered != backend {
			t.Fatalf("Render and the backend disagree for %q:\n render %q\nbackend %q",
				value, rendered, backend)
		}
	}
}

func TestSOARNameKeepsEscapedDot(t *testing.T) {
	const in = `ns1.datum.net. first\.last.example.com.`
	e, err := ParseValue(dnsv1alpha1.RRTypeSOA, in)
	if err != nil {
		t.Fatalf("ParseValue: %v", err)
	}
	if e.SOA.RName != `first\.last.example.com.` {
		t.Fatalf("the escaped dot was resolved away: got %q — that is a different mailbox", e.SOA.RName)
	}
	e.Name = "@"
	if err := Validate(dnsv1alpha1.RRTypeSOA, e); err != nil {
		t.Fatalf("an escaped local part is a valid mailbox: %v", err)
	}
	if got := Render(dnsv1alpha1.RRTypeSOA, e); !strings.Contains(got, `first\.last.example.com.`) {
		t.Fatalf("Render dropped the escape: %q", got)
	}

	// The label count must still be computed on unescaped dots, so an escaped
	// name with too few real labels is still not a mailbox.
	short, err := ParseValue(dnsv1alpha1.RRTypeSOA, `ns1.datum.net. first\.last.com.`)
	if err != nil {
		t.Fatalf("ParseValue: %v", err)
	}
	short.Name = "@"
	if err := Validate(dnsv1alpha1.RRTypeSOA, short); err == nil {
		t.Fatal("two real labels is not a mailbox, escaped or not")
	}
}

// ---------------------------------------------------------------------------
// Deliberate strictness, confirmed by review. Do not relax these.
// ---------------------------------------------------------------------------

// TestCaseDistinctOwnersAreOneOwner: "www" and "WWW" are the same name in DNS,
// so a second CNAME between them is a violation. buildRRSets would otherwise
// key two case-distinct rrsets off those spellings and hand PowerDNS a name
// that already has a CNAME.
func TestCaseDistinctOwnersAreOneOwner(t *testing.T) {
	a := entry(t, dnsv1alpha1.RRTypeCNAME, "www", "a.example.net.")
	b := entry(t, dnsv1alpha1.RRTypeCNAME, "WWW", "b.example.net.")
	err := ValidateEntriesInZone(dnsv1alpha1.RRTypeCNAME, []dnsv1alpha1.RecordEntry{a, b}, "example.com")
	if err == nil {
		t.Fatal("case-distinct spellings of one owner must count as one owner")
	}
	if !strings.Contains(err.Error(), "single-valued") {
		t.Fatalf("unexpected error %q", err)
	}
}

// TestMismatchGuardStillHolds re-pins the check the whole package exists for,
// after the validateName rework moved code around it.
func TestMismatchGuardStillHolds(t *testing.T) {
	e := dnsv1alpha1.RecordEntry{
		Name:  "www",
		CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "lb.example.net."},
	}
	for _, zone := range []string{"", "example.com"} {
		if err := ValidateInZone(dnsv1alpha1.RRTypeA, e, zone); err == nil {
			t.Fatalf("a recordType:A entry carrying cname data must be rejected (zone %q)", zone)
		}
	}
}

// TestEntryForAPIRoundTrip: the boundary pair must be idempotent and must not
// mutate the entry it was handed, since a caller may still be holding it.
func TestEntryForAPIRoundTrip(t *testing.T) {
	const logical = `v=DKIM1; p=` + `AAAA"BBBB`
	e := dnsv1alpha1.RecordEntry{Name: "@", TXT: &dnsv1alpha1.TXTRecordSpec{Content: logical}}

	wire := EntryForAPI(dnsv1alpha1.RRTypeTXT, e)
	if e.TXT.Content != logical {
		t.Fatal("EntryForAPI mutated the entry it was given")
	}
	if wire.TXT.Content == logical {
		t.Fatal("EntryForAPI did not encode")
	}
	if again := EntryForAPI(dnsv1alpha1.RRTypeTXT, wire); again.TXT.Content != wire.TXT.Content {
		t.Fatalf("EntryForAPI is not idempotent: %q then %q", wire.TXT.Content, again.TXT.Content)
	}
	if back := EntryFromAPI(dnsv1alpha1.RRTypeTXT, wire); back.TXT.Content != logical {
		t.Fatalf("EntryFromAPI did not restore the logical value: %q", back.TXT.Content)
	}

	// Every other type passes through untouched.
	a := entry(t, dnsv1alpha1.RRTypeA, "www", "203.0.113.10")
	if EntryForAPI(dnsv1alpha1.RRTypeA, a).A.Content != "203.0.113.10" {
		t.Fatal("EntryForAPI must not touch non-TXT types")
	}
}

// TestRequireFQDNSuggestionDoesNotCauseTheDoublingBug: the fix line for a
// relative target must not propose a name that walks the user into the trap the
// owner-name rule exists to prevent. A bare label is a name inside the zone; a
// value that already has a dot in it just needs terminating.
func TestRequireFQDNSuggestionDoesNotCauseTheDoublingBug(t *testing.T) {
	cases := []struct {
		name    string
		typ     dnsv1alpha1.RRType
		e       dnsv1alpha1.RecordEntry
		zone    string
		wantFix string
	}{
		{
			name: "single label with a zone is qualified",
			typ:  dnsv1alpha1.RRTypeMX,
			e: dnsv1alpha1.RecordEntry{Name: "@", MX: &dnsv1alpha1.MXRecordSpec{
				Preference: 10, Exchange: "mail",
			}},
			zone:    "example.com",
			wantFix: `"mail.example.com."`,
		},
		{
			name: "multi-label is only terminated",
			typ:  dnsv1alpha1.RRTypeCNAME,
			e: dnsv1alpha1.RecordEntry{Name: "cdn", CNAME: &dnsv1alpha1.CNAMERecordSpec{
				Content: "lb.example.net",
			}},
			zone:    "example.com",
			wantFix: `"lb.example.net."`,
		},
		{
			name: "multi-label without a zone is still terminated",
			typ:  dnsv1alpha1.RRTypeCNAME,
			e: dnsv1alpha1.RecordEntry{Name: "cdn", CNAME: &dnsv1alpha1.CNAMERecordSpec{
				Content: "lb.example.net",
			}},
			wantFix: `"lb.example.net."`,
		},
		{
			name: "an escaped dot does not make a label multi-label",
			typ:  dnsv1alpha1.RRTypeSOA,
			e: dnsv1alpha1.RecordEntry{Name: "@", SOA: &dnsv1alpha1.SOARecordSpec{
				MName: "ns1.datum.net.", RName: `first\.last.example.com`,
			}},
			zone:    "example.com",
			wantFix: `"first\.last.example.com."`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateInZone(tc.typ, tc.e, tc.zone)
			if err == nil {
				t.Fatal("a relative target must be rejected")
			}
			fix := FixFor(err)
			if !strings.Contains(fix, tc.wantFix) {
				t.Fatalf("fix %q should suggest %s", fix, tc.wantFix)
			}
			// The suggestion must never be the doubled form.
			if tc.zone != "" && strings.Contains(fix, "."+tc.zone+"."+tc.zone+".") {
				t.Fatalf("fix %q proposes a doubled name", fix)
			}
		})
	}
}

// TestSuggestedFQDNIsItselfValid closes the loop: whatever the fix proposes must
// pass the validation that rejected the original.
func TestSuggestedFQDNIsItselfValid(t *testing.T) {
	const zone = "example.com"
	for _, exchange := range []string{"mail", "lb.example.net", "a.b.c.example.org"} {
		e := dnsv1alpha1.RecordEntry{Name: "@", MX: &dnsv1alpha1.MXRecordSpec{
			Preference: 10, Exchange: exchange,
		}}
		err := ValidateInZone(dnsv1alpha1.RRTypeMX, e, zone)
		if err == nil {
			t.Fatalf("%q should have been rejected", exchange)
		}
		fix := FixFor(err)
		open := strings.Index(fix, `"`)
		closeIdx := strings.LastIndex(fix, `"`)
		if open < 0 || closeIdx <= open {
			t.Fatalf("fix %q carries no suggestion", fix)
		}
		suggested := fix[open+1 : closeIdx]

		fixed := dnsv1alpha1.RecordEntry{Name: "@", MX: &dnsv1alpha1.MXRecordSpec{
			Preference: 10, Exchange: suggested,
		}}
		if err := ValidateInZone(dnsv1alpha1.RRTypeMX, fixed, zone); err != nil {
			t.Fatalf("taking the advice for %q produced %q, which is still invalid: %v",
				exchange, suggested, err)
		}
	}
}

// TestTXTControlCharactersAreEscaped: RFC 1035 §5.1 requires a control
// character in a character-string to be written as a \DDD decimal escape. A
// literal newline ends the line, and a character-string may not span lines, so
// emitting one produces a zone file that no parser can read back — `zone
// export` would write a file that fails on re-import, long after the export
// that caused it.
func TestTXTControlCharactersAreEscaped(t *testing.T) {
	cases := []struct {
		name    string
		logical string
		escape  string
	}{
		{"newline", "line1\nline2", `\010`},
		{"carriage return", "line1\rline2", `\013`},
		{"trailing carriage return", "value\r", `\013`},
		{"tab", "a\tb", `\009`},
		{"NUL", "a\x00b", `\000`},
		{"DEL", "a\x7fb", `\127`},
		{"bell", "a\ab", `\007`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := dnsv1alpha1.RecordEntry{Name: "@", TXT: &dnsv1alpha1.TXTRecordSpec{Content: tc.logical}}
			rendered := Render(dnsv1alpha1.RRTypeTXT, e)

			if strings.ContainsAny(rendered, "\n\r\t\x00\x7f\a") {
				t.Fatalf("Render emitted a raw control character: %q", rendered)
			}
			if !strings.Contains(rendered, tc.escape) {
				t.Fatalf("Render produced %q, want it to contain %s", rendered, tc.escape)
			}
			// A character-string may not span lines.
			if strings.Contains(rendered, "\n") {
				t.Fatalf("the rendered character-string spans lines: %q", rendered)
			}

			// It must survive the backend and decode back exactly.
			wire := TXTContentForAPI(tc.logical)
			if got := quoteIfNeededSim(wire); got != wire {
				t.Fatalf("quoteIfNeeded would rewrite %q", wire)
			}
			if got := TXTContentFromAPI(wire); got != tc.logical {
				t.Fatalf("round trip changed the value: %q -> %q via %q", tc.logical, got, wire)
			}
			// And re-parsing the rendered form gives the same entry back.
			back, err := ParseValue(dnsv1alpha1.RRTypeTXT, rendered)
			if err != nil {
				t.Fatalf("reparsing %q: %v", rendered, err)
			}
			if back.TXT.Content != tc.logical {
				t.Fatalf("reparse changed the value: %q -> %q", tc.logical, back.TXT.Content)
			}

			// Accepted, but worth telling the user about.
			if err := Validate(dnsv1alpha1.RRTypeTXT, e); err != nil {
				t.Fatalf("a control character must not be rejected — the CLI has to be able to read back a record created elsewhere: %v", err)
			}
			w := Warnings(dnsv1alpha1.RRTypeTXT, e)
			if len(w) != 1 || !strings.Contains(w[0], "control character") {
				t.Fatalf("want one control-character warning, got %v", w)
			}
		})
	}
}

// TestTXTEscapedDecimalRoundTrip: a value that literally contains a backslash
// followed by digits must not be confused with a \DDD escape in either
// direction.
func TestTXTEscapedDecimalRoundTrip(t *testing.T) {
	for _, logical := range []string{`\010`, `a\123b`, `\\010`, `100\`, `\`} {
		wire := TXTContentForAPI(logical)
		if got := TXTContentFromAPI(wire); got != logical {
			t.Fatalf("literal backslash-digits changed: %q -> %q via %q", logical, got, wire)
		}
	}
	// And the escape really does decode when it is one.
	if got := TXTContentFromAPI(`"\010"`); got != "\n" {
		t.Fatalf(`decoding "\010" gave %q, want a newline`, got)
	}
	// A three-digit sequence beyond a byte is not an escape.
	if got := TXTContentFromAPI(`"\999"`); got != "999" {
		t.Fatalf(`decoding "\999" gave %q`, got)
	}
}

// TestExportedTXTLineIsReadableBack is the zone-export shape: a rendered record
// must be one physical line, and re-tokenizing it must recover the value.
func TestExportedTXTLineIsReadableBack(t *testing.T) {
	const logical = "line1\nline2\ttabbed\r"
	e := dnsv1alpha1.RecordEntry{Name: "multi", TXT: &dnsv1alpha1.TXTRecordSpec{Content: logical}}
	line := "multi\t300\tIN\tTXT\t" + Render(dnsv1alpha1.RRTypeTXT, e)

	if n := strings.Count(line, "\n"); n != 0 {
		t.Fatalf("the exported record spans %d extra lines:\n%s", n, line)
	}
	parsed, err := ParseLine(line)
	if err != nil {
		t.Fatalf("the exported line does not parse back: %v", err)
	}
	back, err := ParseValue(parsed.Type, parsed.Rdata)
	if err != nil {
		t.Fatalf("the exported rdata does not parse back: %v", err)
	}
	if back.TXT.Content != logical {
		t.Fatalf("export/import changed the value: %q -> %q", logical, back.TXT.Content)
	}

	// A scanner that trims a trailing CR at the line boundary must have
	// nothing to trim, because the CR is inside an escape.
	if strings.HasSuffix(line, "\r") {
		t.Fatal("the exported line ends in a raw CR, which a line scanner would eat silently")
	}
}

// TestIsApexIn covers every spelling that defeated a guard built on the literal
// IsApex test. Four separate guards failed open on the "example.com." row.
func TestIsApexIn(t *testing.T) {
	const zone = "example.com"
	cases := []struct {
		name string
		zone string
		want bool
		why  string
	}{
		{"@", zone, true, "the canonical spelling"},
		{"", zone, true, "the API and QualifyOwner both read empty as the apex"},
		{"example.com.", zone, true, "absolute and equal to the zone — the spelling that defeated four guards"},
		{"EXAMPLE.COM.", zone, true, "DNS names are case-insensitive"},
		{"  example.com.  ", zone, true, "surrounding whitespace is not part of the name"},
		{"example.com", zone, false,
			"relative, so it qualifies to example.com.example.com. — the doubling trap, not the apex"},
		{"www", zone, false, "an ordinary label"},
		{"www.example.com.", zone, false, "absolute, but not the zone"},
		{"*", zone, false, "a wildcard is a label, not the apex"},
		{"other.net.", zone, false, "the apex of a different zone"},
		{"example.com.", "other.net", false, "right spelling, wrong zone"},
		{"example.com.", "example.com.", true, "a zone written with its own trailing dot"},
		{"@", "", true, "no zone: the literal spellings still answer"},
		{"", "", true, "no zone: the literal spellings still answer"},
		{"example.com.", "", false, "no zone: nothing to compare an absolute name against"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/"+tc.zone, func(t *testing.T) {
			if got := IsApexIn(tc.name, tc.zone); got != tc.want {
				t.Fatalf("IsApexIn(%q, %q) = %v, want %v — %s", tc.name, tc.zone, got, tc.want, tc.why)
			}
		})
	}
}

// TestIsApexInAgreesWithQualifyOwner: the helper's whole value is that it
// answers the question the backend will ask, so it must agree with the
// qualification rule rather than with any spelling convention.
func TestIsApexInAgreesWithQualifyOwner(t *testing.T) {
	const zone = "example.com"
	for _, name := range []string{"@", "", "example.com.", "EXAMPLE.COM.", "www", "www.example.com.", "example.com", "*"} {
		wantApex := FQDN(name, zone) == zone+"."
		if got := IsApexIn(name, zone); got != wantApex {
			t.Fatalf("IsApexIn(%q) = %v but FQDN gives %q against zone %q",
				name, got, FQDN(name, zone), zone+".")
		}
	}
}

// TestIsApexInIsStrictlyWiderThanIsApex: every literal apex is an apex in any
// zone, and the helper additionally catches what the literal test misses.
func TestIsApexInIsStrictlyWiderThanIsApex(t *testing.T) {
	const zone = "example.com"
	for _, name := range []string{"@", ""} {
		if !IsApexIn(name, zone) {
			t.Fatalf("IsApexIn(%q) must agree with IsApex", name)
		}
	}
	if IsApex("example.com.") {
		t.Fatal("IsApex is documented as the literal test; widening it would change every existing caller")
	}
	if !IsApexIn("example.com.", zone) {
		t.Fatal("IsApexIn must catch what IsApex misses — that is the entire point")
	}
}
