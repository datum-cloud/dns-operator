// SPDX-License-Identifier: AGPL-3.0-only

package bind

import (
	"strings"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
)

// rec builds a Record from presentation format, which is how every fixture in
// this file is written: a test that spells out RecordEntry literals tests the
// literal, not the emitter.
func rec(t *testing.T, name string, ttl *int64, rrType dnsv1alpha1.RRType, value string) Record {
	t.Helper()
	entry, err := rdata.ParseValue(rrType, value)
	if err != nil {
		t.Fatalf("ParseValue(%s, %q): %v", rrType, value, err)
	}
	entry.Name = name
	entry.TTL = ttl
	return Record{Name: name, TTL: ttl, Type: rrType, Entry: entry}
}

func emitString(t *testing.T, origin string, defaultTTL int64, records []Record) string {
	t.Helper()
	var b strings.Builder
	if err := Emit(&b, origin, defaultTTL, records); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return b.String()
}

func TestEmitLayout(t *testing.T) {
	records := []Record{
		rec(t, "www", ttlPtr(300), dnsv1alpha1.RRTypeA, "203.0.113.11"),
		rec(t, "@", ttlPtr(300), dnsv1alpha1.RRTypeA, "203.0.113.10"),
		rec(t, "@", nil, dnsv1alpha1.RRTypeMX, "10 mail.example.com."),
		rec(t, "@", ttlPtr(3600), dnsv1alpha1.RRTypeNS, "ns1.datum.net."),
		rec(t, "@", ttlPtr(3600), dnsv1alpha1.RRTypeSOA,
			"ns1.datum.net. hostmaster.example.com. 1 2 3 4 5"),
	}

	out := emitString(t, "Example.COM.", 3600, records)

	if !strings.HasPrefix(out, "$ORIGIN example.com.\n$TTL 3600\n") {
		t.Errorf("output does not open with the directives:\n%s", out)
	}
	// SOA and NS lead, as in every zone file a reader has seen before.
	order := []string{"; SOA", "; NS", "; A", "; MX"}
	pos := -1
	for _, want := range order {
		at := strings.Index(out, want)
		if at < 0 {
			t.Fatalf("missing group header %q in:\n%s", want, out)
		}
		if at < pos {
			t.Errorf("group %q is out of order in:\n%s", want, out)
		}
		pos = at
	}
	// The apex sorts before named records within a group.
	if strings.Index(out, "203.0.113.10") > strings.Index(out, "203.0.113.11") {
		t.Errorf("the apex A record should sort first:\n%s", out)
	}
	// A record with no TTL of its own inherits $TTL and writes no number.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "IN MX") && strings.Contains(line, "3600") {
			t.Errorf("a nil-TTL record must not be written with an explicit TTL: %q", line)
		}
	}
}

func TestEmitTTL(t *testing.T) {
	tests := []struct {
		name       string
		defaultTTL int64
		want       string
	}{
		{"an explicit default", 900, "$TTL 900"},
		{"zero falls back to the backend's own default", 0, "$TTL 300"},
		{"negative falls back too", -5, "$TTL 300"},
		{"out of range falls back", rdata.MaxTTL + 1, "$TTL 300"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := emitString(t, "example.com", tc.defaultTTL, nil)
			if !strings.Contains(out, tc.want) {
				t.Errorf("output = %q, want it to contain %q", out, tc.want)
			}
		})
	}
}

func TestEmitTXTChunking(t *testing.T) {
	long := strings.Repeat("k", 300)
	out := emitString(t, "example.com", 300, []Record{
		rec(t, "dkim", nil, dnsv1alpha1.RRTypeTXT, long),
	})
	want := `"` + strings.Repeat("k", 255) + `" "` + strings.Repeat("k", 45) + `"`
	if !strings.Contains(out, want) {
		t.Errorf("a TXT string over 255 bytes must be chunked into character-strings:\n%s", out)
	}
}

func TestEmitRejectsAnEmptyValue(t *testing.T) {
	// A Record whose Entry carries no typed field would emit "www 300 IN A"
	// with nothing after it, which the parser would then reject on re-read.
	bad := Record{Name: "www", Type: dnsv1alpha1.RRTypeA, Entry: dnsv1alpha1.RecordEntry{Name: "www"}}
	if err := Emit(&strings.Builder{}, "example.com", 300, []Record{bad}); err == nil {
		t.Fatal("Emit succeeded on a record with no value, want an error")
	}
}

func TestEmitRequiresAnOrigin(t *testing.T) {
	if err := Emit(&strings.Builder{}, "  ", 300, nil); err == nil {
		t.Fatal("Emit succeeded with no origin, want an error")
	}
}

func TestEmitEmptyZone(t *testing.T) {
	// An empty zone still produces a valid file, so `zone export` on a fresh
	// zone gives something a user can edit rather than nothing at all.
	out := emitString(t, "example.com", 300, nil)
	res, err := Parse(strings.NewReader(out), "example.com", nil)
	if err != nil {
		t.Fatalf("re-parsing an empty export: %v", err)
	}
	if len(res.Records) != 0 {
		t.Errorf("got %d records from an empty export", len(res.Records))
	}
	if res.Origin != "example.com." {
		t.Errorf("Origin = %q, want %q", res.Origin, "example.com.")
	}
}

// TestEmitCoversEveryType guards against a new RR type being added to the API
// without a slot in emitOrder, which would drop it to the fallback block.
func TestEmitCoversEveryType(t *testing.T) {
	inOrder := map[dnsv1alpha1.RRType]bool{}
	for _, rt := range emitOrder {
		inOrder[rt] = true
	}
	for _, rt := range rdata.SupportedTypes() {
		if !inOrder[rt] {
			t.Errorf("type %s is missing from emitOrder", rt)
		}
	}
	if len(emitOrder) != len(rdata.SupportedTypes()) {
		t.Errorf("emitOrder has %d types, rdata supports %d", len(emitOrder), len(rdata.SupportedTypes()))
	}
}
