// SPDX-License-Identifier: AGPL-3.0-only

package rdata

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

func ptr[T any](v T) *T { return &v }

// flagEntry registers the flags for t, parses args, and returns the entry.
func flagEntry(tb testing.TB, t dnsv1alpha1.RRType, args []string) (dnsv1alpha1.RecordEntry, bool) {
	tb.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterFlags(fs, t)
	if err := fs.Parse(args); err != nil {
		tb.Fatalf("parsing %v for %s: %v", args, t, err)
	}
	e, anySet, err := FromFlags(fs, t)
	if err != nil {
		tb.Fatalf("FromFlags(%s, %v): %v", t, args, err)
	}
	return e, anySet
}

func TestParseRRType(t *testing.T) {
	for _, want := range SupportedTypes() {
		for _, spelling := range []string{string(want), strings.ToLower(string(want)), " " + string(want) + " "} {
			got, err := ParseRRType(spelling)
			if err != nil {
				t.Fatalf("ParseRRType(%q): %v", spelling, err)
			}
			if got != want {
				t.Fatalf("ParseRRType(%q) = %q, want %q", spelling, got, want)
			}
		}
	}
	if _, err := ParseRRType("DNSKEY"); err == nil {
		t.Fatal("ParseRRType(DNSKEY) succeeded, want error")
	} else if !strings.Contains(err.Error(), "AAAA") {
		t.Fatalf("error should name the supported set, got %q", err)
	}
}

func TestSupportedTypesIsComplete(t *testing.T) {
	if len(SupportedTypes()) != 14 {
		t.Fatalf("SupportedTypes() has %d entries, want 14", len(SupportedTypes()))
	}
	for _, rt := range SupportedTypes() {
		if jsonFieldFor(rt) == "" {
			t.Fatalf("%s has no typed field mapping", rt)
		}
	}
}

func TestIsStructured(t *testing.T) {
	want := map[dnsv1alpha1.RRType]bool{
		dnsv1alpha1.RRTypeMX: true, dnsv1alpha1.RRTypeSRV: true, dnsv1alpha1.RRTypeCAA: true,
		dnsv1alpha1.RRTypeTLSA: true, dnsv1alpha1.RRTypeHTTPS: true, dnsv1alpha1.RRTypeSVCB: true,
		dnsv1alpha1.RRTypeSOA: true,
	}
	for _, rt := range SupportedTypes() {
		if IsStructured(rt) != want[rt] {
			t.Errorf("IsStructured(%s) = %v, want %v", rt, IsStructured(rt), want[rt])
		}
	}
}

// presentationCase covers one type end to end: the presentation form, the
// entry it must produce, the canonical rendering of that entry, and the flags
// that must produce the identical entry.
type presentationCase struct {
	name    string
	typ     dnsv1alpha1.RRType
	input   string
	want    dnsv1alpha1.RecordEntry
	render  string
	flags   []string // nil for flat types, which are positional only
	noFlags bool
}

func presentationCases() []presentationCase {
	return []presentationCase{
		{
			name: "A", typ: dnsv1alpha1.RRTypeA, input: "203.0.113.10",
			want:   dnsv1alpha1.RecordEntry{A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.10"}},
			render: "203.0.113.10", noFlags: true,
		},
		{
			name: "AAAA", typ: dnsv1alpha1.RRTypeAAAA, input: "2001:db8::1",
			want:   dnsv1alpha1.RecordEntry{AAAA: &dnsv1alpha1.AAAARecordSpec{Content: "2001:db8::1"}},
			render: "2001:db8::1", noFlags: true,
		},
		{
			name: "CNAME", typ: dnsv1alpha1.RRTypeCNAME, input: "LB.example.net.",
			want:   dnsv1alpha1.RecordEntry{CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "lb.example.net."}},
			render: "lb.example.net.", noFlags: true,
		},
		{
			name: "ALIAS", typ: dnsv1alpha1.RRTypeALIAS, input: "lb.example.net.",
			want:   dnsv1alpha1.RecordEntry{ALIAS: &dnsv1alpha1.ALIASRecordSpec{Content: "lb.example.net."}},
			render: "lb.example.net.", noFlags: true,
		},
		{
			name: "NS", typ: dnsv1alpha1.RRTypeNS, input: "ns1.datum.net.",
			want:   dnsv1alpha1.RecordEntry{NS: &dnsv1alpha1.NSRecordSpec{Content: "ns1.datum.net."}},
			render: "ns1.datum.net.", noFlags: true,
		},
		{
			name: "PTR", typ: dnsv1alpha1.RRTypePTR, input: "host.example.com.",
			want:   dnsv1alpha1.RecordEntry{PTR: &dnsv1alpha1.PTRRecordSpec{Content: "host.example.com."}},
			render: "host.example.com.", noFlags: true,
		},
		{
			name: "TXT quoted", typ: dnsv1alpha1.RRTypeTXT, input: `"v=spf1 include:_spf.example.com ~all"`,
			want:   dnsv1alpha1.RecordEntry{TXT: &dnsv1alpha1.TXTRecordSpec{Content: "v=spf1 include:_spf.example.com ~all"}},
			render: `"v=spf1 include:_spf.example.com ~all"`,
			flags:  []string{"--data", "v=spf1 include:_spf.example.com ~all"},
		},
		{
			name: "TXT unquoted with semicolon", typ: dnsv1alpha1.RRTypeTXT, input: "v=DMARC1; p=none",
			want:   dnsv1alpha1.RecordEntry{TXT: &dnsv1alpha1.TXTRecordSpec{Content: "v=DMARC1; p=none"}},
			render: `"v=DMARC1\; p=none"`,
			flags:  []string{"--data", "v=DMARC1; p=none"},
		},
		{
			name: "MX", typ: dnsv1alpha1.RRTypeMX, input: "10 mail.example.com.",
			want:   dnsv1alpha1.RecordEntry{MX: &dnsv1alpha1.MXRecordSpec{Preference: 10, Exchange: "mail.example.com."}},
			render: "10 mail.example.com.",
			flags:  []string{"--preference", "10", "--exchange", "mail.example.com."},
		},
		{
			name: "SRV", typ: dnsv1alpha1.RRTypeSRV, input: "10 5 5060 sipserver.example.com.",
			want: dnsv1alpha1.RecordEntry{SRV: &dnsv1alpha1.SRVRecordSpec{
				Priority: 10, Weight: 5, Port: 5060, Target: "sipserver.example.com.",
			}},
			render: "10 5 5060 sipserver.example.com.",
			flags: []string{"--priority", "10", "--weight", "5", "--port", "5060",
				"--target", "sipserver.example.com."},
		},
		{
			name: "CAA", typ: dnsv1alpha1.RRTypeCAA, input: `0 issue "letsencrypt.org"`,
			want:   dnsv1alpha1.RecordEntry{CAA: &dnsv1alpha1.CAARecordSpec{Flag: 0, Tag: "issue", Value: "letsencrypt.org"}},
			render: `0 issue "letsencrypt.org"`,
			flags:  []string{"--flag", "0", "--tag", "issue", "--value", "letsencrypt.org"},
		},
		{
			name: "TLSA", typ: dnsv1alpha1.RRTypeTLSA,
			input: "3 1 1 " + strings.Repeat("ab", 32),
			want: dnsv1alpha1.RecordEntry{TLSA: &dnsv1alpha1.TLSARecordSpec{
				Usage: 3, Selector: 1, MatchingType: 1, CertData: strings.Repeat("ab", 32),
			}},
			render: "3 1 1 " + strings.Repeat("ab", 32),
			flags: []string{"--usage", "3", "--selector", "1", "--matching-type", "1",
				"--cert-data", strings.Repeat("ab", 32)},
		},
		{
			name: "HTTPS service mode", typ: dnsv1alpha1.RRTypeHTTPS, input: "1 . alpn=h3,h2 port=443",
			want: dnsv1alpha1.RecordEntry{HTTPS: &dnsv1alpha1.HTTPSRecordSpec{
				Priority: 1, Target: ".", Params: map[string]string{"alpn": "h3,h2", "port": "443"},
			}},
			render: "1 . alpn=h3,h2 port=443",
			flags:  []string{"--priority", "1", "--target", ".", "--param", "alpn=h3,h2", "--param", "port=443"},
		},
		{
			name: "HTTPS alias mode", typ: dnsv1alpha1.RRTypeHTTPS, input: "0 svc.example.net.",
			want: dnsv1alpha1.RecordEntry{HTTPS: &dnsv1alpha1.HTTPSRecordSpec{
				Priority: 0, Target: "svc.example.net.",
			}},
			render: "0 svc.example.net.",
			flags:  []string{"--priority", "0", "--target", "svc.example.net."},
		},
		{
			name: "SVCB", typ: dnsv1alpha1.RRTypeSVCB, input: `1 svc.example.net. no-default-alpn ech="AEj+DQ"`,
			want: dnsv1alpha1.RecordEntry{SVCB: &dnsv1alpha1.HTTPSRecordSpec{
				Priority: 1, Target: "svc.example.net.",
				Params: map[string]string{"no-default-alpn": "", "ech": "AEj+DQ"},
			}},
			render: `1 svc.example.net. no-default-alpn ech="AEj+DQ"`,
			flags:  []string{"--priority", "1", "--target", "svc.example.net.", "--param", "no-default-alpn", "--param", "ech=AEj+DQ"},
		},
		{
			name: "SOA full", typ: dnsv1alpha1.RRTypeSOA,
			input: "ns1.datum.net. hostmaster.example.com. 2026010101 10800 3600 604800 3600",
			want: dnsv1alpha1.RecordEntry{SOA: &dnsv1alpha1.SOARecordSpec{
				MName: "ns1.datum.net.", RName: "hostmaster.example.com.",
				Serial: 2026010101, Refresh: 10800, Retry: 3600, Expire: 604800, TTL: 3600,
			}},
			render: "ns1.datum.net. hostmaster.example.com. 2026010101 10800 3600 604800 3600",
			flags: []string{"--mname", "ns1.datum.net.", "--rname", "hostmaster.example.com.",
				"--serial", "2026010101", "--refresh", "10800", "--retry", "3600",
				"--expire", "604800", "--minimum", "3600"},
		},
	}
}

func TestParseValue(t *testing.T) {
	for _, tc := range presentationCases() {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseValue(tc.typ, tc.input)
			if err != nil {
				t.Fatalf("ParseValue(%s, %q): %v", tc.typ, tc.input, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseValue(%s, %q)\n got %+v\nwant %+v", tc.typ, tc.input, got, tc.want)
			}
			if got.Name != "" || got.TTL != nil {
				t.Fatalf("ParseValue must not set Name or TTL, got name=%q ttl=%v", got.Name, got.TTL)
			}
		})
	}
}

func TestRender(t *testing.T) {
	for _, tc := range presentationCases() {
		t.Run(tc.name, func(t *testing.T) {
			if got := Render(tc.typ, tc.want); got != tc.render {
				t.Fatalf("Render(%s)\n got %q\nwant %q", tc.typ, got, tc.render)
			}
		})
	}
}

// TestRoundTrip is the stability property the whole grammar rests on: parsing a
// rendered value must give back the value that was rendered.
func TestRoundTrip(t *testing.T) {
	for _, tc := range presentationCases() {
		t.Run(tc.name, func(t *testing.T) {
			first, err := ParseValue(tc.typ, tc.input)
			if err != nil {
				t.Fatalf("first parse: %v", err)
			}
			rendered := Render(tc.typ, first)
			second, err := ParseValue(tc.typ, rendered)
			if err != nil {
				t.Fatalf("reparsing %q: %v", rendered, err)
			}
			if !reflect.DeepEqual(first, second) {
				t.Fatalf("round trip changed the value\n first %+v\nsecond %+v (via %q)", first, second, rendered)
			}
			if again := Render(tc.typ, second); again != rendered {
				t.Fatalf("render is not idempotent: %q then %q", rendered, again)
			}
		})
	}
}

// TestFlagsMatchPresentation asserts the two notations are one grammar: the
// flags and the presentation string must build the same entry.
func TestFlagsMatchPresentation(t *testing.T) {
	for _, tc := range presentationCases() {
		t.Run(tc.name, func(t *testing.T) {
			if tc.noFlags {
				fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
				RegisterFlags(fs, tc.typ)
				if fs.HasFlags() {
					t.Fatalf("%s is a flat type and must register no rdata flags", tc.typ)
				}
				return
			}
			got, anySet := flagEntry(t, tc.typ, tc.flags)
			if !anySet {
				t.Fatal("anySet = false with flags given")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("flags %v\n got %+v\nwant %+v", tc.flags, got, tc.want)
			}
		})
	}
}

func TestFromFlagsNoneSet(t *testing.T) {
	for _, rt := range SupportedTypes() {
		fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
		RegisterFlags(fs, rt)
		if err := fs.Parse(nil); err != nil {
			t.Fatal(err)
		}
		_, anySet, err := FromFlags(fs, rt)
		if err != nil {
			t.Fatalf("FromFlags(%s) with no flags: %v", rt, err)
		}
		if anySet {
			t.Fatalf("FromFlags(%s) reported anySet with no flags given", rt)
		}
	}
}

func TestRegisterFlagsOnlyRelevant(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterFlags(fs, dnsv1alpha1.RRTypeMX)
	if fs.Lookup("preference") == nil || fs.Lookup("exchange") == nil {
		t.Fatal("MX must register --preference and --exchange")
	}
	for _, unwanted := range []string{"priority", "weight", "port", "target", "tag", "param", "mname"} {
		if fs.Lookup(unwanted) != nil {
			t.Fatalf("MX must not register --%s", unwanted)
		}
	}
}

func TestFromFlagsSOAZeroIsRejected(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterFlags(fs, dnsv1alpha1.RRTypeSOA)
	if err := fs.Parse([]string{"--mname", "ns1.datum.net.", "--rname", "h.example.com.", "--refresh", "0"}); err != nil {
		t.Fatal(err)
	}
	_, _, err := FromFlags(fs, dnsv1alpha1.RRTypeSOA)
	if err == nil {
		t.Fatal("--refresh 0 accepted, want error")
	}
	if !strings.Contains(err.Error(), "may not be 0") {
		t.Fatalf("unexpected error %q", err)
	}
	if fix := FixFor(err); !strings.Contains(fix, "10800") {
		t.Fatalf("fix should name the substituted default, got %q", fix)
	}
}

func TestParseValueErrors(t *testing.T) {
	cases := []struct {
		name  string
		typ   dnsv1alpha1.RRType
		input string
		want  string
	}{
		{"empty", dnsv1alpha1.RRTypeA, "   ", "must not be empty"},
		{"A is not an IP", dnsv1alpha1.RRTypeA, "example.com", "not a valid IPv4 address"},
		{"A given an IPv6", dnsv1alpha1.RRTypeA, "2001:db8::1", "not a valid IPv4 address"},
		{"AAAA given an IPv4", dnsv1alpha1.RRTypeAAAA, "203.0.113.10", "not a valid IPv6 address"},
		{"AAAA with a zone", dnsv1alpha1.RRTypeAAAA, "fe80::1%eth0", "not a valid IPv6 address"},
		{"MX arity short", dnsv1alpha1.RRTypeMX, "mail.example.com.", "has 1 fields, expected 2"},
		{"MX arity long", dnsv1alpha1.RRTypeMX, "10 a. b.", "has 3 fields, expected 2"},
		{"MX preference not a number", dnsv1alpha1.RRTypeMX, "high mail.example.com.", "not a number between 0 and 65535"},
		{"MX preference too big", dnsv1alpha1.RRTypeMX, "70000 mail.example.com.", "not a number between 0 and 65535"},
		{"SRV arity", dnsv1alpha1.RRTypeSRV, "10 5 sip.example.com.", "has 3 fields, expected 4"},
		{"CAA arity", dnsv1alpha1.RRTypeCAA, "0 issue", "has 2 fields, expected 3"},
		{"CAA flag too big", dnsv1alpha1.RRTypeCAA, "300 issue x", "not a number between 0 and 255"},
		{"TLSA arity", dnsv1alpha1.RRTypeTLSA, "3 1 1", "has 3 fields, expected at least 4"},
		{"HTTPS arity", dnsv1alpha1.RRTypeHTTPS, "1", "has 1 fields, expected at least 2"},
		{"HTTPS duplicate param", dnsv1alpha1.RRTypeHTTPS, "1 . alpn=h2 alpn=h3", "set more than once"},
		{"HTTPS empty param key", dnsv1alpha1.RRTypeHTTPS, "1 . =h2", "empty key"},
		{"SOA arity", dnsv1alpha1.RRTypeSOA, "ns1. h.example.com. 1", "has 3 fields, expected 2 or 7"},
		{"SOA literal zero refresh", dnsv1alpha1.RRTypeSOA,
			"ns1.datum.net. h.example.com. 1 0 3600 604800 3600", "refresh may not be 0"},
		{"TXT unterminated quote", dnsv1alpha1.RRTypeTXT, `"unterminated`, "unterminated quoted string"},
		{"TXT mixed quoting", dnsv1alpha1.RRTypeTXT, `"a" b`, "mixes quoted and unquoted"},
		{"unknown type", dnsv1alpha1.RRType("DNSKEY"), "x", "unsupported record type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseValue(tc.typ, tc.input)
			if err == nil {
				t.Fatalf("ParseValue(%s, %q) succeeded, want error", tc.typ, tc.input)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
			if got := err.Error(); got != "" && strings.HasSuffix(got, ".") {
				t.Fatalf("error %q ends with a period", got)
			}
			assertLowercaseStart(t, err)
		})
	}
}

func TestTXTChunking(t *testing.T) {
	long := strings.Repeat("a", 300)
	e, err := ParseValue(dnsv1alpha1.RRTypeTXT, long)
	if err != nil {
		t.Fatal(err)
	}
	rendered := Render(dnsv1alpha1.RRTypeTXT, e)
	want := `"` + strings.Repeat("a", 255) + `" "` + strings.Repeat("a", 45) + `"`
	if rendered != want {
		t.Fatalf("chunking\n got %q\nwant %q", rendered, want)
	}
	back, err := ParseValue(dnsv1alpha1.RRTypeTXT, rendered)
	if err != nil {
		t.Fatal(err)
	}
	if back.TXT.Content != long {
		t.Fatalf("chunked value did not round trip, got %d bytes", len(back.TXT.Content))
	}
	if TXTContentForAPI(long) != rendered {
		t.Fatal("TXTContentForAPI must return the chunked presentation form")
	}
}

// TestTXTContentForAPISurvivesQuoteIfNeeded reproduces internal/pdns's
// quoteIfNeeded: a value that is already quoted end to end is passed through
// untouched, which is exactly why chunked TXT has to be pre-quoted.
func TestTXTContentForAPISurvivesQuoteIfNeeded(t *testing.T) {
	quoteIfNeeded := func(s string) string {
		if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
			return s
		}
		return `"` + s + `"`
	}
	for _, in := range []string{"hello", "v=DMARC1; p=none", strings.Repeat("a", 300)} {
		api := TXTContentForAPI(in)
		if quoteIfNeeded(api) != api {
			t.Fatalf("quoteIfNeeded would re-wrap %q", api)
		}
		back, err := ParseValue(dnsv1alpha1.RRTypeTXT, api)
		if err != nil {
			t.Fatalf("reparsing %q: %v", api, err)
		}
		if back.TXT.Content != in {
			t.Fatalf("value changed: %q -> %q", in, back.TXT.Content)
		}
	}
}

func TestSOADefaultsAreShown(t *testing.T) {
	old := nowFunc
	nowFunc = func() time.Time { return time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC) }
	defer func() { nowFunc = old }()

	e, err := ParseValue(dnsv1alpha1.RRTypeSOA, "ns1.datum.net. hostmaster.example.com.")
	if err != nil {
		t.Fatal(err)
	}
	got := Render(dnsv1alpha1.RRTypeSOA, e)
	want := "ns1.datum.net. hostmaster.example.com. 2026082201 10800 3600 604800 3600"
	if got != want {
		t.Fatalf("Render with defaults\n got %q\nwant %q", got, want)
	}
	fields := Fields(dnsv1alpha1.RRTypeSOA, e)
	if fields[3] != [2]string{"Refresh", "10800 (default)"} {
		t.Fatalf("Fields should mark substituted defaults, got %v", fields[3])
	}
}

func TestFieldsCoverEveryType(t *testing.T) {
	for _, tc := range presentationCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := Fields(tc.typ, tc.want)
			if len(f) == 0 {
				t.Fatalf("Fields(%s) returned nothing", tc.typ)
			}
			for _, kv := range f {
				if kv[0] == "" {
					t.Fatalf("Fields(%s) has an empty label", tc.typ)
				}
			}
		})
	}
	// An entry with no value for the type yields no fields rather than panicking.
	for _, rt := range SupportedTypes() {
		if f := Fields(rt, dnsv1alpha1.RecordEntry{}); f != nil {
			t.Fatalf("Fields(%s, empty) = %v, want nil", rt, f)
		}
		if r := Render(rt, dnsv1alpha1.RecordEntry{}); r != "" {
			t.Fatalf("Render(%s, empty) = %q, want empty", rt, r)
		}
	}
}

func TestKeyAndEqual(t *testing.T) {
	cases := []struct {
		name  string
		typ   dnsv1alpha1.RRType
		a, b  string
		equal bool
	}{
		{"A identical", dnsv1alpha1.RRTypeA, "203.0.113.10", "203.0.113.10", true},
		{"A different", dnsv1alpha1.RRTypeA, "203.0.113.10", "203.0.113.11", false},
		{"AAAA compressed vs expanded", dnsv1alpha1.RRTypeAAAA,
			"2001:db8::1", "2001:0db8:0000:0000:0000:0000:0000:0001", true},
		{"CNAME trailing dot insensitive", dnsv1alpha1.RRTypeCNAME, "lb.example.net.", "lb.example.net", true},
		{"CNAME case insensitive", dnsv1alpha1.RRTypeCNAME, "LB.Example.NET.", "lb.example.net.", true},
		{"NS trailing dot insensitive", dnsv1alpha1.RRTypeNS, "ns1.datum.net.", "ns1.datum.net", true},
		{"PTR different", dnsv1alpha1.RRTypePTR, "a.example.com.", "b.example.com.", false},
		{"ALIAS trailing dot insensitive", dnsv1alpha1.RRTypeALIAS, "lb.example.net.", "lb.example.net", true},
		{"TXT is case sensitive", dnsv1alpha1.RRTypeTXT, "Hello", "hello", false},
		{"TXT quoted equals unquoted", dnsv1alpha1.RRTypeTXT, `"hello"`, "hello", true},
		{"MX trailing dot insensitive", dnsv1alpha1.RRTypeMX, "10 mail.example.com.", "10 mail.example.com", true},
		{"MX preference matters", dnsv1alpha1.RRTypeMX, "10 mail.example.com.", "20 mail.example.com.", false},
		{"SRV trailing dot insensitive", dnsv1alpha1.RRTypeSRV, "10 5 5060 a.example.com.", "10 5 5060 a.example.com", true},
		{"SRV port matters", dnsv1alpha1.RRTypeSRV, "10 5 5060 a.example.com.", "10 5 5061 a.example.com.", false},
		{"CAA tag is lowercased at parse", dnsv1alpha1.RRTypeCAA, `0 issue "x.org"`, `0 ISSUE "x.org"`, true},
		{"CAA quoted equals unquoted", dnsv1alpha1.RRTypeCAA, `0 issue "x.org"`, "0 issue x.org", true},
		{"TLSA hex case insensitive", dnsv1alpha1.RRTypeTLSA,
			"3 1 1 " + strings.Repeat("ab", 32), "3 1 1 " + strings.Repeat("AB", 32), true},
		{"HTTPS param order irrelevant", dnsv1alpha1.RRTypeHTTPS,
			"1 . alpn=h3,h2 port=443", "1 . port=443 alpn=h3,h2", true},
		{"HTTPS target dot insensitive", dnsv1alpha1.RRTypeHTTPS, "1 a.example.net.", "1 a.example.net", true},
		{"SVCB differs by priority", dnsv1alpha1.RRTypeSVCB, "1 a.example.net.", "2 a.example.net.", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ParseValue(tc.typ, tc.a)
			if err != nil {
				t.Fatalf("parsing %q: %v", tc.a, err)
			}
			b, err := ParseValue(tc.typ, tc.b)
			if err != nil {
				t.Fatalf("parsing %q: %v", tc.b, err)
			}
			if got := Equal(tc.typ, a, b); got != tc.equal {
				t.Fatalf("Equal(%s, %q, %q) = %v, want %v (keys %q / %q)",
					tc.typ, tc.a, tc.b, got, tc.equal, Key(tc.typ, a), Key(tc.typ, b))
			}
			if (Key(tc.typ, a) == Key(tc.typ, b)) != tc.equal {
				t.Fatal("Key and Equal disagree")
			}
		})
	}
}

// TestKeySOADefaults pins the rule that an SOA leaving a timer unset is the
// same record as one spelling out the value the backend would substitute.
func TestKeySOADefaults(t *testing.T) {
	unset, err := ParseValue(dnsv1alpha1.RRTypeSOA, "ns1.datum.net. h.example.com.")
	if err != nil {
		t.Fatal(err)
	}
	spelled, err := ParseValue(dnsv1alpha1.RRTypeSOA,
		"ns1.datum.net. h.example.com. 7 10800 3600 604800 3600")
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(dnsv1alpha1.RRTypeSOA, unset, spelled) {
		t.Fatalf("unset timers should equal the substituted defaults:\n%q\n%q",
			Key(dnsv1alpha1.RRTypeSOA, unset), Key(dnsv1alpha1.RRTypeSOA, spelled))
	}
}

func TestKeyIgnoresNameAndTTL(t *testing.T) {
	a, _ := ParseValue(dnsv1alpha1.RRTypeA, "203.0.113.10")
	b := a
	a.Name, a.TTL = "www", ptr(int64(300))
	b.Name, b.TTL = "api", ptr(int64(60))
	if !Equal(dnsv1alpha1.RRTypeA, a, b) {
		t.Fatal("Key must ignore Name and TTL")
	}
}

func TestKeyEmptyEntryIsNeverEqual(t *testing.T) {
	var empty dnsv1alpha1.RecordEntry
	if Key(dnsv1alpha1.RRTypeA, empty) != "" {
		t.Fatal("Key of an empty entry should be empty")
	}
	if Equal(dnsv1alpha1.RRTypeA, empty, empty) {
		t.Fatal("two valueless entries must not compare equal")
	}
}

// TestKeyIsTypeQualified keeps a CNAME value from matching an ALIAS value that
// happens to point at the same host.
func TestKeyIsTypeQualified(t *testing.T) {
	c, _ := ParseValue(dnsv1alpha1.RRTypeCNAME, "lb.example.net.")
	a, _ := ParseValue(dnsv1alpha1.RRTypeALIAS, "lb.example.net.")
	if Key(dnsv1alpha1.RRTypeCNAME, c) == Key(dnsv1alpha1.RRTypeALIAS, a) {
		t.Fatal("keys of different types must differ")
	}
}

// assertLowercaseStart enforces the house style for error strings. A leading
// capital is allowed only when it is an acronym Go's own convention exempts —
// here, an RR type name or "TTL".
func assertLowercaseStart(t *testing.T, err error) {
	t.Helper()
	msg := err.Error()
	if msg == "" {
		t.Fatal("empty error message")
	}
	first, _, _ := strings.Cut(msg, " ")
	if strings.ToLower(first) == first {
		return
	}
	if first == "TTL" {
		return
	}
	if _, perr := ParseRRType(first); perr == nil {
		return
	}
	t.Fatalf("error %q starts with an uppercase word that is not an RR type", msg)
}
