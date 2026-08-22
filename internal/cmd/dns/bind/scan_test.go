// SPDX-License-Identifier: AGPL-3.0-only

package bind

import (
	"strings"
	"testing"
)

// The scanner is where the zone-file grammar's three traps live: a ";" that is
// data rather than a comment, a "(" that is data rather than a continuation,
// and leading whitespace that carries meaning. Each gets a case here so a
// regression shows up as a scanner failure rather than as a puzzling record.
func TestScan(t *testing.T) {
	type want struct {
		text     string
		line     int
		inherits bool
	}
	tests := []struct {
		name string
		in   string
		want []want
	}{
		{
			name: "blank lines and comments produce no statements",
			in:   "\n; a comment\n\t; an indented comment\n   \n",
		},
		{
			name: "a trailing comment is stripped, the record is not",
			in:   "www IN A 203.0.113.10 ; the web server\n",
			want: []want{{"www IN A 203.0.113.10", 1, false}},
		},
		{
			name: "a semicolon inside a quoted string is data",
			in:   "@ IN TXT \"v=DMARC1; p=none\"\n",
			want: []want{{"@ IN TXT \"v=DMARC1; p=none\"", 1, false}},
		},
		{
			name: "leading whitespace marks an inherited owner name",
			in:   "www IN A 203.0.113.10\n\tIN A 203.0.113.11\n  IN A 203.0.113.12\n",
			want: []want{
				{"www IN A 203.0.113.10", 1, false},
				{"IN A 203.0.113.11", 2, true},
				{"IN A 203.0.113.12", 3, true},
			},
		},
		{
			name: "a parenthesised record folds onto one line and keeps its first line number",
			in:   "@ IN SOA ns1. host. (\n  1 ; serial\n  2\n  3 4 5 )\n@ IN A 203.0.113.10\n",
			want: []want{
				{"@ IN SOA ns1. host. 1 2 3 4 5", 1, false},
				{"@ IN A 203.0.113.10", 5, false},
			},
		},
		{
			name: "a parenthesised record whose owner is inherited stays inherited",
			in:   "@ IN NS ns1.\n  IN SOA ns1. host. (\n  1 2 3 4 5 )\n",
			want: []want{
				{"@ IN NS ns1.", 1, false},
				{"IN SOA ns1. host. 1 2 3 4 5", 2, true},
			},
		},
		{
			name: "parentheses inside a quoted string are data",
			in:   "@ IN TXT \"a (parenthesised) note\"\n@ IN A 203.0.113.10\n",
			want: []want{
				{"@ IN TXT \"a (parenthesised) note\"", 1, false},
				{"@ IN A 203.0.113.10", 2, false},
			},
		},
		{
			name: "runs of whitespace collapse outside quotes and survive inside them",
			in:   "www\t\t300   IN\tA\t203.0.113.10\n@ IN TXT \"two  spaces\"\n",
			want: []want{
				{"www 300 IN A 203.0.113.10", 1, false},
				{"@ IN TXT \"two  spaces\"", 2, false},
			},
		},
		{
			name: "CRLF line endings",
			in:   "www IN A 203.0.113.10\r\n@ IN A 203.0.113.11\r\n",
			want: []want{
				{"www IN A 203.0.113.10", 1, false},
				{"@ IN A 203.0.113.11", 2, false},
			},
		},
		{
			name: "an escaped quote does not open a string",
			in:   `@ IN TXT "he said \"hi\"; then left"` + "\n",
			want: []want{{`@ IN TXT "he said \"hi\"; then left"`, 1, false}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scan(strings.NewReader(tc.in))
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d statements, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				if got[i].text != w.text {
					t.Errorf("statement %d text = %q, want %q", i, got[i].text, w.text)
				}
				if got[i].line != w.line {
					t.Errorf("statement %d line = %d, want %d", i, got[i].line, w.line)
				}
				if got[i].inherits != w.inherits {
					t.Errorf("statement %d inherits = %v, want %v", i, got[i].inherits, w.inherits)
				}
			}
		})
	}
}

func TestScanErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"unterminated quote", "@ IN TXT \"open\n", "unterminated quoted string"},
		{"stray close paren", "@ IN A 203.0.113.10 )\n", `unbalanced ")"`},
		{"unclosed paren at EOF", "@ IN SOA ns1. host. (\n 1 2 3\n", `unbalanced "("`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := scan(strings.NewReader(tc.in))
			if err == nil {
				t.Fatal("scan succeeded, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
			if !strings.HasPrefix(err.Error(), "line ") {
				t.Errorf("error = %q, want it to name a line", err)
			}
		})
	}
}

func TestSplitFields(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		texts []string
		// rdataFrom is the offset of the field at index rdataAt, used to slice
		// the value out of the line with its quoting intact.
		rdataAt int
		rdata   string
	}{
		{
			name:    "plain fields",
			in:      "www 300 IN A 203.0.113.10",
			texts:   []string{"www", "300", "IN", "A", "203.0.113.10"},
			rdataAt: 4,
			rdata:   "203.0.113.10",
		},
		{
			name:    "a quoted field keeps its quotes and its spaces",
			in:      `@ IN TXT "one two" "three"`,
			texts:   []string{"@", "IN", "TXT", `"one two"`, `"three"`},
			rdataAt: 3,
			rdata:   `"one two" "three"`,
		},
		{
			name:    "tabs separate fields",
			in:      "www\tIN\tA\t203.0.113.10",
			texts:   []string{"www", "IN", "A", "203.0.113.10"},
			rdataAt: 3,
			rdata:   "203.0.113.10",
		},
		{
			name:    "an escaped space stays inside its field",
			in:      `@ IN TXT a\ b`,
			texts:   []string{"@", "IN", "TXT", `a\ b`},
			rdataAt: 3,
			rdata:   `a\ b`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			toks := splitFields(tc.in)
			if len(toks) != len(tc.texts) {
				t.Fatalf("got %d fields, want %d: %+v", len(toks), len(tc.texts), toks)
			}
			for i, want := range tc.texts {
				if toks[i].text != want {
					t.Errorf("field %d = %q, want %q", i, toks[i].text, want)
				}
			}
			if got := tc.in[toks[tc.rdataAt].start:]; got != tc.rdata {
				t.Errorf("rdata slice = %q, want %q", got, tc.rdata)
			}
		})
	}
}

func TestParseTTLTable(t *testing.T) {
	ok := map[string]int64{
		"0":          0,
		"300":        300,
		"3600":       3600,
		"1s":         1,
		"1m":         60,
		"1h":         3600,
		"1d":         86400,
		"1w":         604800,
		"1W":         604800,
		"1h30m":      5400,
		"1w2d3h4m5s": 604800 + 2*86400 + 3*3600 + 4*60 + 5,
		"2147483647": maxTTL,
	}
	for in, want := range ok {
		got, err := parseTTL(in)
		if err != nil {
			t.Errorf("parseTTL(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseTTL(%q) = %d, want %d", in, got, want)
		}
	}

	bad := []string{"", "abc", "5x", "1h30", "-1", "2147483648", "1h2147483647d", "h1"}
	for _, in := range bad {
		if got, err := parseTTL(in); err == nil {
			t.Errorf("parseTTL(%q) = %d, want an error", in, got)
		}
	}
}
