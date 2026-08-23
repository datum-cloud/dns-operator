// SPDX-License-Identifier: AGPL-3.0-only

package bind

import (
	"strconv"
	"strings"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
)

// got is a Record flattened to the three things a test cares about, so a table
// row reads as the record it describes rather than as a struct literal.
type got struct {
	name  string
	ttl   string
	rtype string
	value string
}

func flatten(t *testing.T, res ParseResult) []got {
	t.Helper()
	out := make([]got, 0, len(res.Records))
	for _, r := range res.Records {
		if r.Entry.Name != r.Name {
			t.Errorf("record on line %d: Entry.Name = %q, Record.Name = %q", r.Line, r.Entry.Name, r.Name)
		}
		out = append(out, got{
			name: r.Name,
			// Raw seconds, not rdata.FormatTTL: these cases assert what the
			// parser produced, and a humanized "5m" would hide whether the
			// parse or the formatter is under test.
			ttl:   rawTTL(r.TTL),
			rtype: string(r.Type),
			value: rdata.Render(r.Type, r.Entry),
		})
	}
	return out
}

func parseOK(t *testing.T, in, zone string, defaultTTL *int64) ParseResult {
	t.Helper()
	res, err := Parse(strings.NewReader(in), zone, defaultTTL)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return res
}

func TestParseRecords(t *testing.T) {
	tests := []struct {
		name       string
		zone       string
		defaultTTL *int64
		in         string
		want       []got
	}{
		{
			name: "flat types, absolute and relative names",
			zone: "example.com",
			in: "$ORIGIN example.com.\n" +
				"@       IN A     203.0.113.10\n" +
				"www     IN A     203.0.113.11\n" +
				"www.example.com. IN AAAA 2001:db8::1\n" +
				"*       IN A     203.0.113.12\n",
			want: []got{
				{"@", "Auto", "A", "203.0.113.10"},
				{"www", "Auto", "A", "203.0.113.11"},
				{"www", "Auto", "AAAA", "2001:db8::1"},
				{"*", "Auto", "A", "203.0.113.12"},
			},
		},
		{
			name: "owner-name inheritance from leading whitespace",
			zone: "example.com",
			in: "www 300 IN A 203.0.113.10\n" +
				"    300 IN A 203.0.113.11\n" +
				"\tIN A 203.0.113.12\n" +
				"api IN A 203.0.113.20\n" +
				"  IN A 203.0.113.21\n",
			want: []got{
				{"www", "300", "A", "203.0.113.10"},
				{"www", "300", "A", "203.0.113.11"},
				{"www", "Auto", "A", "203.0.113.12"},
				{"api", "Auto", "A", "203.0.113.20"},
				{"api", "Auto", "A", "203.0.113.21"},
			},
		},
		{
			name: "inheritance survives a blank line and a comment",
			zone: "example.com",
			in: "www IN A 203.0.113.10\n" +
				"\n" +
				"; a comment\n" +
				"    IN A 203.0.113.11\n",
			want: []got{
				{"www", "Auto", "A", "203.0.113.10"},
				{"www", "Auto", "A", "203.0.113.11"},
			},
		},
		{
			name: "$TTL applies to records that give none",
			zone: "example.com",
			in: "$TTL 3600\n" +
				"www IN A 203.0.113.10\n" +
				"api 60 IN A 203.0.113.11\n" +
				"$TTL 1h30m\n" +
				"cdn IN A 203.0.113.12\n",
			want: []got{
				{"www", "3600", "A", "203.0.113.10"},
				{"api", "60", "A", "203.0.113.11"},
				{"cdn", "5400", "A", "203.0.113.12"},
			},
		},
		{
			name:       "the caller's default TTL fills in when the file declares none",
			zone:       "example.com",
			defaultTTL: ttlPtr(900),
			in:         "www IN A 203.0.113.10\n",
			want:       []got{{"www", "900", "A", "203.0.113.10"}},
		},
		{
			name: "BIND duration TTLs",
			zone: "example.com",
			in: "a 1s IN A 203.0.113.1\n" +
				"b 5m IN A 203.0.113.2\n" +
				"c 2h IN A 203.0.113.3\n" +
				"d 1D IN A 203.0.113.4\n" +
				"e 1W IN A 203.0.113.5\n" +
				"f 1w2d3h IN A 203.0.113.6\n",
			want: []got{
				{"a", "1", "A", "203.0.113.1"},
				{"b", "300", "A", "203.0.113.2"},
				{"c", "7200", "A", "203.0.113.3"},
				{"d", "86400", "A", "203.0.113.4"},
				{"e", "604800", "A", "203.0.113.5"},
				{"f", "788400", "A", "203.0.113.6"},
			},
		},
		{
			name: "TTL and class in either order, class optional",
			zone: "example.com",
			in: "a 300 IN A 203.0.113.1\n" +
				"b IN 300 A 203.0.113.2\n" +
				"c 300 A 203.0.113.3\n" +
				"d A 203.0.113.4\n" +
				"e in a 203.0.113.5\n",
			want: []got{
				{"a", "300", "A", "203.0.113.1"},
				{"b", "300", "A", "203.0.113.2"},
				{"c", "300", "A", "203.0.113.3"},
				{"d", "Auto", "A", "203.0.113.4"},
				{"e", "Auto", "A", "203.0.113.5"},
			},
		},
		{
			name: "parenthesised SOA with per-field comments",
			zone: "example.com",
			in: "$ORIGIN example.com.\n" +
				"@ IN SOA ns1.example.com. hostmaster.example.com. (\n" +
				"        2024010101 ; serial\n" +
				"        10800      ; refresh\n" +
				"        3600       ; retry\n" +
				"        604800     ; expire\n" +
				"        3600 )     ; minimum\n",
			want: []got{{
				"@", "Auto", "SOA",
				"ns1.example.com. hostmaster.example.com. 2024010101 10800 3600 604800 3600",
			}},
		},
		{
			name: "parentheses opened and closed on one line",
			zone: "example.com",
			in:   "@ IN SOA ns1. hostmaster. ( 1 2 3 4 5 )\n",
			want: []got{{"@", "Auto", "SOA", "ns1. hostmaster. 1 2 3 4 5"}},
		},
		{
			name: "relative targets expand against $ORIGIN",
			zone: "example.com",
			in: "$ORIGIN example.com.\n" +
				"www  IN CNAME lb\n" +
				"apex IN CNAME @\n" +
				"@    IN MX 10 mail\n" +
				"@    IN NS ns1\n" +
				"_sip._tcp IN SRV 10 5 5060 sip\n" +
				"cdn  IN CNAME lb.example.net.\n",
			want: []got{
				{"www", "Auto", "CNAME", "lb.example.com."},
				{"apex", "Auto", "CNAME", "example.com."},
				{"@", "Auto", "MX", "10 mail.example.com."},
				{"@", "Auto", "NS", "ns1.example.com."},
				{"_sip._tcp", "Auto", "SRV", "10 5 5060 sip.example.com."},
				{"cdn", "Auto", "CNAME", "lb.example.net."},
			},
		},
		{
			name: "a $ORIGIN below the zone still yields zone-relative names",
			zone: "example.com",
			in: "$ORIGIN sub.example.com.\n" +
				"www IN A 203.0.113.10\n" +
				"@   IN A 203.0.113.11\n",
			want: []got{
				{"www.sub", "Auto", "A", "203.0.113.10"},
				{"sub", "Auto", "A", "203.0.113.11"},
			},
		},
		{
			name: "a relative $ORIGIN expands against the prevailing one",
			zone: "example.com",
			in: "$ORIGIN example.com.\n" +
				"$ORIGIN sub\n" +
				"www IN A 203.0.113.10\n",
			want: []got{{"www.sub", "Auto", "A", "203.0.113.10"}},
		},
		{
			name: "quoted TXT, concatenation, and semicolons that are data",
			zone: "example.com",
			in: "@      IN TXT \"v=spf1 include:_spf.example.com ~all\"\n" +
				"_dmarc IN TXT \"v=DMARC1; p=none\"\n" +
				"split  IN TXT \"part one \" \"part two\"\n" +
				"bare   IN TXT unquoted-value\n" +
				"esc    IN TXT \"a \\\"quoted\\\" word\"\n",
			want: []got{
				{"@", "Auto", "TXT", `"v=spf1 include:_spf.example.com ~all"`},
				{"_dmarc", "Auto", "TXT", `"v=DMARC1\; p=none"`},
				{"split", "Auto", "TXT", `"part one part two"`},
				{"bare", "Auto", "TXT", `"unquoted-value"`},
				{"esc", "Auto", "TXT", `"a \"quoted\" word"`},
			},
		},
		{
			name: "a TXT string over 255 characters is chunked on the way out",
			zone: "example.com",
			in:   "long IN TXT \"" + strings.Repeat("a", 300) + "\"\n",
			want: []got{{
				"long", "Auto", "TXT",
				`"` + strings.Repeat("a", 255) + `" "` + strings.Repeat("a", 45) + `"`,
			}},
		},
		{
			name: "structured types in presentation format",
			zone: "example.com",
			in: "@ IN MX 10 mail.example.com.\n" +
				"_sip._tcp IN SRV 10 5 5060 sipserver.example.com.\n" +
				"@ IN CAA 0 issue \"letsencrypt.org\"\n" +
				"_443._tcp IN TLSA 3 1 1 " + sha256Hex + "\n" +
				"api IN HTTPS 1 . alpn=h3,h2 port=443\n" +
				"svc IN SVCB 1 svc.example.net. alpn=h2\n" +
				"@ IN ALIAS lb.example.net.\n" +
				"10 IN PTR host.example.com.\n",
			want: []got{
				{"@", "Auto", "MX", "10 mail.example.com."},
				{"_sip._tcp", "Auto", "SRV", "10 5 5060 sipserver.example.com."},
				{"@", "Auto", "CAA", `0 issue "letsencrypt.org"`},
				{"_443._tcp", "Auto", "TLSA", "3 1 1 " + sha256Hex},
				{"api", "Auto", "HTTPS", "1 . alpn=h3,h2 port=443"},
				{"svc", "Auto", "SVCB", "1 svc.example.net. alpn=h2"},
				{"@", "Auto", "ALIAS", "lb.example.net."},
				{"10", "Auto", "PTR", "host.example.com."},
			},
		},
		{
			name: "comments, blank lines and CRLF are all noise",
			zone: "example.com",
			in: "; leading comment\r\n" +
				"\r\n" +
				"   ; indented comment\r\n" +
				"www IN A 203.0.113.10 ; trailing comment\r\n" +
				"\r\n",
			want: []got{{"www", "Auto", "A", "203.0.113.10"}},
		},
		{
			name: "the file's own $ORIGIN wins over the zone argument for expansion",
			zone: "example.com",
			in: "$ORIGIN example.com.\n" +
				"www IN A 203.0.113.10\n",
			want: []got{{"www", "Auto", "A", "203.0.113.10"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := parseOK(t, tc.in, tc.zone, tc.defaultTTL)
			assertRecords(t, flatten(t, res), tc.want)
		})
	}
}

func assertRecords(t *testing.T, have, want []got) {
	t.Helper()
	if len(have) != len(want) {
		t.Fatalf("got %d records, want %d\n got: %+v\nwant: %+v", len(have), len(want), have, want)
	}
	for i := range want {
		if have[i] != want[i] {
			t.Errorf("record %d:\n got %+v\nwant %+v", i, have[i], want[i])
		}
	}
}

func TestParseOrigin(t *testing.T) {
	tests := []struct {
		name string
		zone string
		in   string
		want string
	}{
		{"from the zone argument", "example.com", "www IN A 203.0.113.10\n", "example.com."},
		{"from the directive", "example.com", "$ORIGIN other.test.\n", "other.test."},
		{"the last directive wins", "example.com", "$ORIGIN a.test.\n$ORIGIN b.test.\n", "b.test."},
		{"no zone and no directive", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if res := parseOK(t, tc.in, tc.zone, nil); res.Origin != tc.want {
				t.Errorf("Origin = %q, want %q", res.Origin, tc.want)
			}
		})
	}
}

func TestParseUnsupportedTypes(t *testing.T) {
	in := "$ORIGIN example.com.\n" +
		"www   IN A 203.0.113.10\n" +
		"@     IN DS 12345 8 2 abcdef\n" +
		"@     IN DNSKEY 256 3 8 AwEAAa==\n" +
		"@     IN SPF \"v=spf1 ~all\"\n" +
		"old   IN DNAME target.example.net.\n" +
		"gen   IN TYPE65534 \\# 5 0123456789\n" +
		"api   IN A 203.0.113.11\n"

	res := parseOK(t, in, "example.com", nil)

	if len(res.Records) != 2 {
		t.Fatalf("got %d supported records, want 2: %+v", len(res.Records), flatten(t, res))
	}
	wantTypes := []string{"DS", "DNSKEY", "SPF", "DNAME", "TYPE65534"}
	wantLines := []int{3, 4, 5, 6, 7}
	if len(res.Unsupported) != len(wantTypes) {
		t.Fatalf("got %d unsupported, want %d: %+v", len(res.Unsupported), len(wantTypes), res.Unsupported)
	}
	for i, u := range res.Unsupported {
		if u.Type != wantTypes[i] {
			t.Errorf("unsupported %d: Type = %q, want %q", i, u.Type, wantTypes[i])
		}
		if u.Line != wantLines[i] {
			t.Errorf("unsupported %d (%s): Line = %d, want %d", i, u.Type, u.Line, wantLines[i])
		}
		if u.Raw == "" {
			t.Errorf("unsupported %d (%s): Raw is empty, the user must be shown the line", i, u.Type)
		}
	}
}

func TestParseWarnings(t *testing.T) {
	tests := []struct {
		name string
		zone string
		in   string
		want string
	}{
		{
			name: "an absolute name outside the zone",
			zone: "example.com",
			in:   "www.example.net. IN A 203.0.113.10\n",
			want: "outside zone",
		},
		{
			name: "a duplicate value within one name and type",
			zone: "example.com",
			in:   "www IN A 203.0.113.10\nwww IN A 203.0.113.10\n",
			want: "duplicate of the A record",
		},
		{
			name: "a duplicate spelled differently still counts",
			zone: "example.com",
			in:   "www IN CNAME lb.example.net.\nwww IN CNAME LB.example.net.\n",
			want: "duplicate of the CNAME record",
		},
		{
			name: "a relative owner that spells out the zone",
			zone: "example.com",
			in:   "www.example.com IN A 203.0.113.10\n",
			want: "add a trailing dot if you meant the name literally",
		},
		{
			name: "an unrecognised directive is ignored, not fatal",
			zone: "example.com",
			in:   "$WHATEVER 1\nwww IN A 203.0.113.10\n",
			want: "ignored unrecognised directive",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := parseOK(t, tc.in, tc.zone, nil)
			if !containsSubstring(res.Warnings, tc.want) {
				t.Errorf("warnings = %v, want one containing %q", res.Warnings, tc.want)
			}
		})
	}
}

// sha256Hex is a syntactically valid SHA-256 digest, the only length TLSA
// matching type 1 accepts.
var sha256Hex = strings.Repeat("ab", 32)

func containsSubstring(haystack []string, want string) bool {
	for _, h := range haystack {
		if strings.Contains(h, want) {
			return true
		}
	}
	return false
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name     string
		zone     string
		in       string
		wantLine int
		wantMsg  string
		wantFix  string
	}{
		{
			name:     "no owner name to inherit",
			zone:     "example.com",
			in:       "    IN A 203.0.113.10\n",
			wantLine: 1,
			wantMsg:  "no previous record",
			wantFix:  "\"@\" for the zone apex",
		},
		{
			name:     "a line with no type",
			zone:     "example.com",
			in:       "www IN A 203.0.113.10\nbroken 300\n",
			wantLine: 2,
			wantMsg:  "has no type",
		},
		{
			name:     "a type with no value",
			zone:     "example.com",
			in:       "www IN A\n",
			wantLine: 1,
			wantMsg:  "has no value",
		},
		{
			name:     "a misspelled type is an error, not an unsupported record",
			zone:     "example.com",
			in:       "www IN AAA 203.0.113.10\n",
			wantLine: 1,
			wantMsg:  "is not a DNS record type",
		},
		{
			name:     "invalid rdata reports rdata's own message and fix",
			zone:     "example.com",
			in:       "\n\nwww IN A not-an-ip\n",
			wantLine: 3,
			wantMsg:  "is not a valid IPv4 address",
			wantFix:  "single IPv4 address",
		},
		{
			name:     "MX with a non-numeric preference",
			zone:     "example.com",
			in:       "@ IN MX abc mail.example.com.\n",
			wantLine: 1,
			wantMsg:  "MX preference",
		},
		{
			name:     "an unterminated quoted string",
			zone:     "example.com",
			in:       "@ IN TXT \"never closed\n",
			wantLine: 1,
			wantMsg:  "unterminated quoted string",
		},
		{
			name:     "an unclosed parenthesis",
			zone:     "example.com",
			in:       "@ IN SOA ns1. host. (\n 1 2 3 4 5\n",
			wantLine: 1,
			wantMsg:  "unbalanced \"(\"",
		},
		{
			name:     "a stray closing parenthesis",
			zone:     "example.com",
			in:       "www IN A 203.0.113.10 )\n",
			wantLine: 1,
			wantMsg:  "unbalanced \")\"",
		},
		{
			name:     "$TTL with no value",
			zone:     "example.com",
			in:       "$TTL\n",
			wantLine: 1,
			wantMsg:  "$TTL needs a value",
		},
		{
			name:     "$TTL that is not a duration",
			zone:     "example.com",
			in:       "$TTL 5x\n",
			wantLine: 1,
			wantMsg:  "is not a number of seconds or a duration",
			wantFix:  "built from s, m, h, d and w",
		},
		{
			name:     "$ORIGIN with no name",
			zone:     "example.com",
			in:       "$ORIGIN\n",
			wantLine: 1,
			wantMsg:  "$ORIGIN needs a domain name",
		},
		{
			name:     "$INCLUDE is refused rather than silently skipped",
			zone:     "example.com",
			in:       "$INCLUDE sub.zone\n",
			wantLine: 1,
			wantMsg:  "$INCLUDE is not supported",
			wantFix:  "inline the included file",
		},
		{
			name:     "$GENERATE is refused",
			zone:     "example.com",
			in:       "$GENERATE 1-10 host$ A 203.0.113.$\n",
			wantLine: 1,
			wantMsg:  "$GENERATE is not supported",
		},
		{
			name:     "a TTL outside the 32-bit range",
			zone:     "example.com",
			in:       "www 4294967296 IN A 203.0.113.10\n",
			wantLine: 1,
			wantMsg:  "outside the range",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tc.in), tc.zone, nil)
			if err == nil {
				t.Fatal("Parse succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantMsg)
			}
			prefix := "line " + itoa(tc.wantLine) + ":"
			if !strings.HasPrefix(err.Error(), prefix) {
				t.Errorf("error = %q, want it to start with %q", err, prefix)
			}
			if tc.wantFix != "" && !strings.Contains(FixFor(err), tc.wantFix) {
				t.Errorf("FixFor = %q, want it to contain %q", FixFor(err), tc.wantFix)
			}
		})
	}
}

// A relative $ORIGIN with nothing to expand against is the one directive error
// that depends on the zone argument being empty.
func TestParseRelativeOriginWithoutZone(t *testing.T) {
	_, err := Parse(strings.NewReader("$ORIGIN sub\n"), "", nil)
	if err == nil || !strings.Contains(err.Error(), "is relative") {
		t.Fatalf("err = %v, want a relative-origin error", err)
	}
}

func TestParseEntriesValidate(t *testing.T) {
	// Every record a real zone file yields must survive the same validation the
	// mutation commands run, or import would submit a record the backend drops.
	in := "$ORIGIN example.com.\n" +
		"$TTL 3600\n" +
		"@ IN SOA ns1.example.com. hostmaster.example.com. 2024010101 10800 3600 604800 3600\n" +
		"@ IN NS ns1\n" +
		"@ IN NS ns2\n" +
		"@ IN A 203.0.113.10\n" +
		"@ IN AAAA 2001:db8::1\n" +
		"@ IN MX 10 mail\n" +
		"@ IN CAA 0 issue \"letsencrypt.org\"\n" +
		"@ IN TXT \"v=spf1 -all\"\n" +
		"www IN CNAME @\n" +
		"_sip._tcp IN SRV 10 5 5060 sip\n" +
		"_443._tcp IN TLSA 3 1 1 " + sha256Hex + "\n" +
		"api IN HTTPS 1 . alpn=h2\n" +
		"svc IN SVCB 1 svc.example.net.\n" +
		"1.0 IN PTR host\n"

	res := parseOK(t, in, "example.com", nil)
	for _, r := range res.Records {
		if err := rdata.ValidateInZone(r.Type, r.Entry, "example.com"); err != nil {
			t.Errorf("line %d (%s %s): %v", r.Line, r.Name, r.Type, err)
		}
	}
}

func TestParseAllSupportedTypesCovered(t *testing.T) {
	// ALIAS cannot sit at the apex alongside the CNAME test above, so it gets
	// its own file; the point of the test is that no supported type is missing
	// from the parser's reach.
	in := "$ORIGIN example.com.\n" +
		"@ IN SOA ns1.example.com. hostmaster.example.com. 1 2 3 4 5\n" +
		"@ IN NS ns1.example.com.\n" +
		"@ IN A 203.0.113.10\n" +
		"@ IN AAAA 2001:db8::1\n" +
		"@ IN ALIAS lb.example.net.\n" +
		"www IN CNAME lb.example.net.\n" +
		"@ IN TXT \"hello\"\n" +
		"@ IN MX 10 mail.example.com.\n" +
		"_sip._tcp IN SRV 10 5 5060 sip.example.com.\n" +
		"@ IN CAA 0 issue \"letsencrypt.org\"\n" +
		"1.0 IN PTR host.example.com.\n" +
		"_443._tcp IN TLSA 3 1 1 abcdef\n" +
		"api IN HTTPS 1 .\n" +
		"svc IN SVCB 1 svc.example.net.\n"

	res := parseOK(t, in, "example.com", nil)
	seen := map[dnsv1alpha1.RRType]bool{}
	for _, r := range res.Records {
		seen[r.Type] = true
	}
	for _, want := range rdata.SupportedTypes() {
		if !seen[want] {
			t.Errorf("type %s never parsed", want)
		}
	}
}

func TestParseLongInputDoesNotTruncate(t *testing.T) {
	// The scanner's buffer is raised above bufio's default for exactly this.
	long := strings.Repeat("x", 100_000)
	res := parseOK(t, "big IN TXT \""+long+"\"\n", "example.com", nil)
	if len(res.Records) != 1 {
		t.Fatalf("got %d records, want 1", len(res.Records))
	}
	if got := res.Records[0].Entry.TXT.Content; len(got) != len(long) {
		t.Errorf("TXT content is %d bytes, want %d", len(got), len(long))
	}
}

// TestParseInheritanceAcrossOrigin is the regression test for the one axis
// inheritance got wrong. RFC 1035 §5.1 inherits the previous record's *name*,
// not the token that spelled it, so a blank-owner record after a mid-file
// $ORIGIN still belongs to the owner above it. Re-expanding the written token
// against the new origin produced a valid-looking name at the wrong owner —
// silent, and on the import path it means the record the user asked for is
// simply missing.
func TestParseInheritanceAcrossOrigin(t *testing.T) {
	tests := []struct {
		name string
		zone string
		in   string
		want []got
	}{
		{
			name: "a mid-file $ORIGIN does not move an inherited owner",
			zone: "example.com",
			in: "$ORIGIN example.com.\n" +
				"$TTL 300\n" +
				"www IN A 203.0.113.10\n" +
				"$ORIGIN sub.example.com.\n" +
				"    IN A 203.0.113.11\n",
			want: []got{
				{"www", "300", "A", "203.0.113.10"},
				{"www", "300", "A", "203.0.113.11"},
			},
		},
		{
			name: "an inherited apex stays the old origin's apex",
			zone: "example.com",
			in: "$ORIGIN example.com.\n" +
				"@ IN A 203.0.113.10\n" +
				"$ORIGIN sub.example.com.\n" +
				"  IN A 203.0.113.11\n" +
				"@ IN A 203.0.113.12\n",
			want: []got{
				{"@", "Auto", "A", "203.0.113.10"},
				{"@", "Auto", "A", "203.0.113.11"},
				{"sub", "Auto", "A", "203.0.113.12"},
			},
		},
		{
			name: "inheritance from an absolute owner is unaffected by a new origin",
			zone: "example.com",
			in: "api.example.com. IN A 203.0.113.10\n" +
				"$ORIGIN other.example.com.\n" +
				"  IN A 203.0.113.11\n",
			want: []got{
				{"api", "Auto", "A", "203.0.113.10"},
				{"api", "Auto", "A", "203.0.113.11"},
			},
		},
		{
			name: "an unsupported record still sets the owner for the next line",
			zone: "example.com",
			in: "$ORIGIN example.com.\n" +
				"www IN DS 12345 8 2 abcdef\n" +
				"    IN A  203.0.113.10\n",
			want: []got{{"www", "Auto", "A", "203.0.113.10"}},
		},
		{
			name: "inheritance is unaffected by an intervening $TTL",
			zone: "example.com",
			in: "www 300 IN A 203.0.113.10\n" +
				"$TTL 900\n" +
				"    IN A 203.0.113.11\n",
			want: []got{
				{"www", "300", "A", "203.0.113.10"},
				{"www", "900", "A", "203.0.113.11"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertRecords(t, flatten(t, parseOK(t, tc.in, tc.zone, nil)), tc.want)
		})
	}
}

// $ORIGIN . reparents the file to the DNS root. The root trims to the empty
// string, which is also how "no origin" is spelled, so the two have to be kept
// apart or the directive is silently ignored.
func TestParseOriginRoot(t *testing.T) {
	res := parseOK(t, "$ORIGIN example.com.\n$ORIGIN .\nwww.example.com. IN A 203.0.113.10\n",
		"example.com", nil)
	if res.Origin != "" {
		t.Errorf("Origin = %q, want the root", res.Origin)
	}
	assertRecords(t, flatten(t, res), []got{{"www", "Auto", "A", "203.0.113.10"}})
}

// A class other than IN cannot be stored — the API has no class field — so the
// record is imported as IN and the reader is told.
func TestParseNonINClassWarns(t *testing.T) {
	res := parseOK(t, "$ORIGIN example.com.\nc CH A 203.0.113.10\n", "example.com", nil)
	if len(res.Records) != 1 {
		t.Fatalf("got %d records, want 1", len(res.Records))
	}
	if !containsSubstring(res.Warnings, `class "CH" is not supported`) {
		t.Errorf("warnings = %v, want one naming the discarded class", res.Warnings)
	}
}

// A negative number in the TTL slot is a broken TTL, not an unknown RR type.
func TestParseNegativeTTLSaysSo(t *testing.T) {
	_, err := Parse(strings.NewReader("www -5 IN A 203.0.113.10\n"), "example.com", nil)
	if err == nil {
		t.Fatal("Parse succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "not a number of seconds or a duration") {
		t.Errorf("error = %q, want it to name the TTL", err)
	}
	if strings.Contains(err.Error(), "record type") {
		t.Errorf("error = %q, want it not to blame the record type", err)
	}
}

// rawTTL renders a parsed TTL as the plain number of seconds, with nil — the
// "inherit the zone default" case — spelled as Auto.
func rawTTL(ttl *int64) string {
	if ttl == nil {
		return "Auto"
	}
	return strconv.FormatInt(*ttl, 10)
}
