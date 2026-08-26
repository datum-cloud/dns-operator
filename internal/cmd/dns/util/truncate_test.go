package util

import (
	"testing"
	"unicode/utf8"
)

func TestTruncateCell(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		max     int
		want    string
		wantCut bool
	}{
		{"short values are untouched", "203.0.113.10", 20, "203.0.113.10", false},
		{"a value exactly at the limit is untouched", "abcde", 5, "abcde", false},
		{"a long value is cut and marked", "abcdefghij", 5, "abcd…", true},
		{"the result never exceeds the budget", "abcdefghij", 5, "abcd…", true},
		{"a non-positive budget disables truncation", "abcdefghij", 0, "abcdefghij", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, cut := TruncateCell(tc.in, tc.max)
			if got != tc.want || cut != tc.wantCut {
				t.Errorf("TruncateCell(%q, %d) = (%q, %v), want (%q, %v)", tc.in, tc.max, got, cut, tc.want, tc.wantCut)
			}
			if n := len([]rune(got)); tc.max > 0 && n > tc.max {
				t.Errorf("result is %d runes, wider than the %d-rune budget", n, tc.max)
			}
		})
	}
}

// A multi-byte value must never be cut mid-character.
func TestTruncateCellIsRuneAware(t *testing.T) {
	got, cut := TruncateCell("héllo wörld ünicode", 8)
	if !cut {
		t.Fatal("expected truncation")
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncation produced invalid UTF-8: %q", got)
	}
	if n := len([]rune(got)); n != 8 {
		t.Errorf("got %d runes, want 8", n)
	}
}
