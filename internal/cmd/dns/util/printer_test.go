// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestParseOutputFormat(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		allowed  []OutputFormat
		want     OutputFormat
		wantErr  bool
		wantText string
	}{
		{name: "default set accepts table", in: "table", want: OutputTable},
		{name: "default set accepts name", in: "name", want: OutputName},
		{name: "default set accepts wide", in: "wide", want: OutputWide},
		{
			name:     "default set rejects unknown",
			in:       "csv",
			wantErr:  true,
			wantText: `invalid output format "csv" — must be one of: table, wide, json, yaml, name`,
		},
		{
			name:    "restricted set accepts a member",
			in:      "json",
			allowed: []OutputFormat{OutputJSON, OutputYAML},
			want:    OutputJSON,
		},
		{
			name:     "restricted set rejects a non-member",
			in:       "table",
			allowed:  []OutputFormat{OutputJSON, OutputYAML},
			wantErr:  true,
			wantText: `invalid output format "table" — must be one of: json, yaml`,
		},
		{
			name:     "empty string is a usage error",
			in:       "",
			wantErr:  true,
			wantText: `invalid output format "" — must be one of: table, wide, json, yaml, name`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseOutputFormat(tc.in, tc.allowed...)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseOutputFormat(%q) = %q, want an error", tc.in, got)
				}
				if err.Error() != tc.wantText {
					t.Errorf("error = %q, want %q", err.Error(), tc.wantText)
				}
				var ce *CLIError
				if !errors.As(err, &ce) || ce.Code() != ExitUsage {
					t.Errorf("error must be a CLIError with ExitUsage, got %#v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseOutputFormat(%q) returned %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseOutputFormat(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestOutputFormatIsMachine(t *testing.T) {
	tests := []struct {
		in   OutputFormat
		want bool
	}{
		{OutputTable, false},
		{OutputWide, false},
		{OutputJSON, true},
		{OutputYAML, true},
		{OutputName, true},
	}
	for _, tc := range tests {
		if got := tc.in.IsMachine(); got != tc.want {
			t.Errorf("%q.IsMachine() = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestOrDash(t *testing.T) {
	if got := OrDash(""); got != "—" {
		t.Errorf("OrDash(\"\") = %q, want an em dash", got)
	}
	if got := OrDash("ns1.datum.net."); got != "ns1.datum.net." {
		t.Errorf("OrDash() rewrote a non-empty value to %q", got)
	}
}

func TestPrintJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintJSON(&buf, map[string]string{"domainName": "example.com"}); err != nil {
		t.Fatalf("PrintJSON returned %v", err)
	}
	want := "{\n  \"domainName\": \"example.com\"\n}\n"
	if buf.String() != want {
		t.Errorf("PrintJSON wrote %q, want %q", buf.String(), want)
	}
}

func TestPrintYAML(t *testing.T) {
	var buf bytes.Buffer
	if err := PrintYAML(&buf, map[string]string{"domainName": "example.com"}); err != nil {
		t.Fatalf("PrintYAML returned %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "domainName: example.com" {
		t.Errorf("PrintYAML wrote %q", got)
	}
}

func TestNewTabWriter(t *testing.T) {
	var buf bytes.Buffer
	w := NewTabWriter(&buf)
	if _, err := w.Write([]byte("NAME\tSTATUS\nexample.com\tOK\n")); err != nil {
		t.Fatalf("writing rows: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush returned %v", err)
	}
	// Three spaces of padding after the widest cell in the column.
	if want := "NAME          STATUS\nexample.com   OK\n"; buf.String() != want {
		t.Errorf("tab writer produced %q, want %q", buf.String(), want)
	}
}
