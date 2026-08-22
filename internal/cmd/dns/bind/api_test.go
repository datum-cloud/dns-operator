// SPDX-License-Identifier: AGPL-3.0-only

package bind

import (
	"strings"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
)

// The wire/logical conversion is rdata's, but the property it guarantees is one
// this package's Emit and diff paths depend on: if the two are not inverses,
// every TXT record in a zone shows as a change on every apply and exports as
// escaped garbage. The test lives here because that is where the consequence
// lands.
func TestEntryAPIRoundTrip(t *testing.T) {
	values := []string{
		"v=spf1 include:_spf.example.com ~all",
		"v=DMARC1; p=none; rua=mailto:d@example.com",
		`he said "hi" \ then left`,
		"v=DKIM1; k=rsa; p=" + strings.Repeat("M", 400),
		strings.Repeat("x", 255),
		strings.Repeat("x", 256),
		"",
	}
	for _, v := range values {
		logical := dnsv1alpha1.RecordEntry{Name: "t", TXT: &dnsv1alpha1.TXTRecordSpec{Content: v}}
		stored := rdata.EntryForAPI(dnsv1alpha1.RRTypeTXT, logical)
		back := rdata.EntryFromAPI(dnsv1alpha1.RRTypeTXT, stored)

		if got := back.TXT.Content; got != v {
			t.Errorf("round trip of a %d-byte value:\n got %q\nwant %q", len(v), got, v)
		}
		if !rdata.Equal(dnsv1alpha1.RRTypeTXT, back, logical) {
			t.Errorf("a %d-byte value does not compare equal after a round trip", len(v))
		}
		// The stored form is what internal/pdns must pass through untouched.
		if v != "" && !strings.HasPrefix(stored.TXT.Content, `"`) {
			t.Errorf("stored form of a %d-byte value is not quoted: %.40q", len(v), stored.TXT.Content)
		}
	}
}

// Every other type is carried through unchanged; only TXT has a wire form that
// differs from its value today, and the bulk paths must not care which.
func TestEntryAPILeavesOtherTypesAlone(t *testing.T) {
	for _, rt := range rdata.SupportedTypes() {
		if rt == dnsv1alpha1.RRTypeTXT {
			continue
		}
		e := rec(t, "www", ttlPtr(300), rt, sampleValue(rt)).Entry
		if got := rdata.Render(rt, rdata.EntryForAPI(rt, e)); got != rdata.Render(rt, e) {
			t.Errorf("%s was altered on the way to the API: %q", rt, got)
		}
		if got := rdata.Render(rt, rdata.EntryFromAPI(rt, e)); got != rdata.Render(rt, e) {
			t.Errorf("%s was altered on the way back: %q", rt, got)
		}
	}
}

// RecordsFromSet is the read side of the same boundary: it must produce records
// the emitter and the diff can use directly.
func TestRecordsFromSet(t *testing.T) {
	set := &dnsv1alpha1.DNSRecordSet{
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeTXT,
			Records: []dnsv1alpha1.RecordEntry{
				{Name: "", TTL: ttlPtr(300), TXT: &dnsv1alpha1.TXTRecordSpec{Content: `"one" "two"`}},
			},
		},
	}
	got := RecordsFromSet(set)
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	// The API's empty name is the apex, which the CLI always shows as "@".
	if got[0].Name != "@" || got[0].Entry.Name != "@" {
		t.Errorf("owner name = %q / %q, want \"@\"", got[0].Name, got[0].Entry.Name)
	}
	if got[0].Entry.TXT.Content != "onetwo" {
		t.Errorf("TXT content = %q, want the concatenated value", got[0].Entry.TXT.Content)
	}
}

// sampleValue is one valid presentation-format value per type.
func sampleValue(t dnsv1alpha1.RRType) string {
	switch t {
	case dnsv1alpha1.RRTypeA:
		return "203.0.113.10"
	case dnsv1alpha1.RRTypeAAAA:
		return "2001:db8::1"
	case dnsv1alpha1.RRTypeALIAS, dnsv1alpha1.RRTypeCNAME, dnsv1alpha1.RRTypePTR:
		return "lb.example.net."
	case dnsv1alpha1.RRTypeNS:
		return "ns1.datum.net."
	case dnsv1alpha1.RRTypeMX:
		return "10 mail.example.com."
	case dnsv1alpha1.RRTypeSRV:
		return "10 5 5060 sip.example.com."
	case dnsv1alpha1.RRTypeCAA:
		return "0 issue letsencrypt.org"
	case dnsv1alpha1.RRTypeSOA:
		return "ns1.datum.net. hostmaster.example.com. 1 2 3 4 5"
	case dnsv1alpha1.RRTypeTLSA:
		return "3 1 1 " + strings.Repeat("ab", 32)
	case dnsv1alpha1.RRTypeHTTPS, dnsv1alpha1.RRTypeSVCB:
		return "1 . alpn=h2"
	}
	return ""
}

// An SOA written with only its two names renders with the backend's defaults
// substituted, so an imported zone that leaves the timers out must still show
// no diff when it is re-applied.
func TestSOADefaultsAreStableAcrossARoundTrip(t *testing.T) {
	res := parseOK(t, "$ORIGIN example.com.\n@ 3600 IN SOA ns1.datum.net. hostmaster.example.com.\n",
		"example.com", nil)
	if len(res.Records) != 1 {
		t.Fatalf("got %d records, want 1", len(res.Records))
	}

	file := emitString(t, "example.com", 3600, res.Records)
	again := parseOK(t, file, "example.com", nil)

	before, after := res.Records[0], again.Records[0]
	if !rdata.Equal(dnsv1alpha1.RRTypeSOA, before.Entry, after.Entry) {
		t.Errorf("a two-field SOA is not stable across a round trip:\n got %q\nwant %q",
			rdata.Render(dnsv1alpha1.RRTypeSOA, after.Entry),
			rdata.Render(dnsv1alpha1.RRTypeSOA, before.Entry))
	}
}
