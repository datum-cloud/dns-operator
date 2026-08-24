// SPDX-License-Identifier: AGPL-3.0-only

package rdata

import (
	"errors"
	"os"
	"strings"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// entry parses value and attaches an owner name, the shape Validate sees.
func entry(tb testing.TB, t dnsv1alpha1.RRType, name, value string) dnsv1alpha1.RecordEntry {
	tb.Helper()
	e, err := ParseValue(t, value)
	if err != nil {
		tb.Fatalf("ParseValue(%s, %q): %v", t, value, err)
	}
	e.Name = name
	return e
}

// TestValidateAcceptsEveryType walks all 14 types with a value that must pass.
func TestValidateAcceptsEveryType(t *testing.T) {
	cases := []struct {
		typ   dnsv1alpha1.RRType
		name  string
		value string
	}{
		{dnsv1alpha1.RRTypeA, "www", "203.0.113.10"},
		{dnsv1alpha1.RRTypeAAAA, "www", "2001:db8::1"},
		{dnsv1alpha1.RRTypeALIAS, "@", "lb.example.net."},
		{dnsv1alpha1.RRTypeCNAME, "api", "lb.example.net."},
		{dnsv1alpha1.RRTypeTXT, "@", `"v=spf1 ~all"`},
		{dnsv1alpha1.RRTypeMX, "@", "10 mail.example.com."},
		{dnsv1alpha1.RRTypeSRV, "_sip._tcp", "10 5 5060 sip.example.com."},
		{dnsv1alpha1.RRTypeCAA, "@", `0 issue "letsencrypt.org"`},
		{dnsv1alpha1.RRTypeNS, "sub", "ns1.datum.net."},
		{dnsv1alpha1.RRTypeSOA, "@", "ns1.datum.net. hostmaster.example.com."},
		{dnsv1alpha1.RRTypePTR, "10", "host.example.com."},
		{dnsv1alpha1.RRTypeTLSA, "_443._tcp", "3 1 1 " + strings.Repeat("ab", 32)},
		{dnsv1alpha1.RRTypeHTTPS, "@", "1 . alpn=h3,h2"},
		{dnsv1alpha1.RRTypeSVCB, "_dns", "1 dns.example.net. port=853"},
		// Shapes worth pinning individually.
		{dnsv1alpha1.RRTypeA, "*", "203.0.113.10"},
		{dnsv1alpha1.RRTypeMX, "@", "0 ."},              // RFC 7505 null MX
		{dnsv1alpha1.RRTypeSRV, "_sip._tcp", "0 0 0 ."}, // RFC 2782 unavailable
		{dnsv1alpha1.RRTypeCNAME, "_domainconnect", "x.gd.domaincontrol.com."},
		{dnsv1alpha1.RRTypeHTTPS, "api", "0 svc.example.net."}, // alias mode
	}
	for _, tc := range cases {
		t.Run(string(tc.typ)+" "+tc.name+" "+tc.value, func(t *testing.T) {
			e := entry(t, tc.typ, tc.name, tc.value)
			if err := Validate(tc.typ, e); err != nil {
				t.Fatalf("Validate rejected a valid record: %v", err)
			}
			if err := ValidateInZone(tc.typ, e, "example.com"); err != nil {
				t.Fatalf("ValidateInZone rejected a valid record: %v", err)
			}
		})
	}
}

// TestValidateRejectsTypeFieldMismatch is the bug this package exists to
// prevent: the API admits an entry whose typed field belongs to another type,
// and internal/pdns then skips it, leaving a record that does not exist and no
// condition explaining why.
func TestValidateRejectsTypeFieldMismatch(t *testing.T) {
	cname := &dnsv1alpha1.CNAMERecordSpec{Content: "lb.example.net."}

	t.Run("A carrying CNAME data", func(t *testing.T) {
		e := dnsv1alpha1.RecordEntry{Name: "www", CNAME: cname}
		err := Validate(dnsv1alpha1.RRTypeA, e)
		if err == nil {
			t.Fatal("Validate accepted an A record whose only value is a CNAME")
		}
		for _, want := range []string{"type A", "CNAME data", "silently discards"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error %q should mention %q", err, want)
			}
		}
		if FixFor(err) == "" {
			t.Fatal("mismatch error should carry a fix")
		}
	})

	t.Run("no typed field at all", func(t *testing.T) {
		err := Validate(dnsv1alpha1.RRTypeA, dnsv1alpha1.RecordEntry{Name: "www"})
		if err == nil || !strings.Contains(err.Error(), "has no A value") {
			t.Fatalf("want a missing-value error, got %v", err)
		}
	})

	t.Run("two typed fields", func(t *testing.T) {
		e := dnsv1alpha1.RecordEntry{
			Name:  "www",
			A:     &dnsv1alpha1.ARecordSpec{Content: "203.0.113.10"},
			CNAME: cname,
		}
		err := Validate(dnsv1alpha1.RRTypeA, e)
		if err == nil || !strings.Contains(err.Error(), "2 type-specific fields") {
			t.Fatalf("want a multiple-fields error, got %v", err)
		}
	})

	// Every type must reject every other type's field.
	for _, want := range SupportedTypes() {
		for _, other := range SupportedTypes() {
			if other == want {
				continue
			}
			e, err := ParseValue(other, sampleValue(other))
			if err != nil {
				t.Fatalf("sample for %s: %v", other, err)
			}
			e.Name = "www"
			if Validate(want, e) == nil {
				t.Fatalf("Validate(%s) accepted an entry carrying %s data", want, other)
			}
		}
	}
}

func sampleValue(t dnsv1alpha1.RRType) string {
	switch t {
	case dnsv1alpha1.RRTypeA:
		return "203.0.113.10"
	case dnsv1alpha1.RRTypeAAAA:
		return "2001:db8::1"
	case dnsv1alpha1.RRTypeALIAS, dnsv1alpha1.RRTypeCNAME:
		return "lb.example.net."
	case dnsv1alpha1.RRTypeNS:
		return "ns1.datum.net."
	case dnsv1alpha1.RRTypePTR:
		return "host.example.com."
	case dnsv1alpha1.RRTypeTXT:
		return `"hello"`
	case dnsv1alpha1.RRTypeMX:
		return "10 mail.example.com."
	case dnsv1alpha1.RRTypeSRV:
		return "10 5 5060 sip.example.com."
	case dnsv1alpha1.RRTypeCAA:
		return `0 issue "letsencrypt.org"`
	case dnsv1alpha1.RRTypeTLSA:
		return "3 1 1 " + strings.Repeat("ab", 32)
	case dnsv1alpha1.RRTypeHTTPS, dnsv1alpha1.RRTypeSVCB:
		return "1 . alpn=h2"
	case dnsv1alpha1.RRTypeSOA:
		return "ns1.datum.net. hostmaster.example.com."
	}
	return ""
}

// TestValidateRejections covers every distinct invalid case, entry by entry.
func TestValidateRejections(t *testing.T) {
	cases := []struct {
		name string
		typ  dnsv1alpha1.RRType
		e    dnsv1alpha1.RecordEntry
		want string
		fix  string
	}{
		{
			name: "empty owner name", typ: dnsv1alpha1.RRTypeA,
			e:    dnsv1alpha1.RecordEntry{A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.10"}},
			want: "record name is empty", fix: "@",
		},
		{
			name: "invalid owner name", typ: dnsv1alpha1.RRTypeA,
			e:    dnsv1alpha1.RecordEntry{Name: "www!", A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.10"}},
			want: "not a valid owner name",
		},
		{
			name: "CNAME at the apex", typ: dnsv1alpha1.RRTypeCNAME,
			e:    dnsv1alpha1.RecordEntry{Name: "@", CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "lb.example.net."}},
			want: "may not exist at the zone apex", fix: "ALIAS",
		},
		{
			name: "SOA away from the apex", typ: dnsv1alpha1.RRTypeSOA,
			e: dnsv1alpha1.RecordEntry{Name: "www", SOA: &dnsv1alpha1.SOARecordSpec{
				MName: "ns1.datum.net.", RName: "hostmaster.example.com.",
			}},
			want: "may not exist at", fix: "@",
		},
		{
			name: "negative TTL", typ: dnsv1alpha1.RRTypeA,
			e: dnsv1alpha1.RecordEntry{
				Name: "www", TTL: ptr(int64(-1)),
				A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.10"},
			},
			want: "is negative",
		},
		{
			name: "TTL beyond 31 bits", typ: dnsv1alpha1.RRTypeA,
			e: dnsv1alpha1.RecordEntry{
				Name: "www", TTL: ptr(int64(2147483648)),
				A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.10"},
			},
			want: "exceeds the maximum",
		},
		{
			name: "A with a bad address", typ: dnsv1alpha1.RRTypeA,
			e:    dnsv1alpha1.RecordEntry{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "999.1.1.1"}},
			want: "not a valid IPv4 address",
		},
		{
			name: "CNAME target without a trailing dot", typ: dnsv1alpha1.RRTypeCNAME,
			e:    dnsv1alpha1.RecordEntry{Name: "api", CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "lb.example.net"}},
			want: "not a fully qualified domain name", fix: "absolute",
		},
		{
			name: "CNAME target of @", typ: dnsv1alpha1.RRTypeCNAME,
			e:    dnsv1alpha1.RecordEntry{Name: "api", CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "@"}},
			want: "may not be",
		},
		{
			name: "empty CNAME target", typ: dnsv1alpha1.RRTypeCNAME,
			e:    dnsv1alpha1.RecordEntry{Name: "api", CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "  "}},
			want: "must not be empty",
		},
		{
			name: "MX exchange without a trailing dot", typ: dnsv1alpha1.RRTypeMX,
			e: dnsv1alpha1.RecordEntry{Name: "@", MX: &dnsv1alpha1.MXRecordSpec{
				Preference: 10, Exchange: "mail",
			}},
			want: "MX exchange \"mail\" is not a fully qualified domain name",
		},
		{
			name: "MX exchange with an underscore", typ: dnsv1alpha1.RRTypeMX,
			e: dnsv1alpha1.RecordEntry{Name: "@", MX: &dnsv1alpha1.MXRecordSpec{
				Preference: 10, Exchange: "mail_1.example.com.",
			}},
			want: "contains an underscore",
		},
		{
			name: "null MX with a non-zero preference", typ: dnsv1alpha1.RRTypeMX,
			e: dnsv1alpha1.RecordEntry{Name: "@", MX: &dnsv1alpha1.MXRecordSpec{
				Preference: 10, Exchange: ".",
			}},
			want: "accepts no mail", fix: "0 .",
		},
		{
			name: "NS with an underscore", typ: dnsv1alpha1.RRTypeNS,
			e:    dnsv1alpha1.RecordEntry{Name: "sub", NS: &dnsv1alpha1.NSRecordSpec{Content: "ns_1.datum.net."}},
			want: "contains an underscore",
		},
		{
			name: "NS pointing at the root", typ: dnsv1alpha1.RRTypeNS,
			e:    dnsv1alpha1.RecordEntry{Name: "sub", NS: &dnsv1alpha1.NSRecordSpec{Content: "."}},
			want: "may not be the DNS root",
		},
		{
			name: "PTR without a trailing dot", typ: dnsv1alpha1.RRTypePTR,
			e:    dnsv1alpha1.RecordEntry{Name: "10", PTR: &dnsv1alpha1.PTRRecordSpec{Content: "host"}},
			want: "not a fully qualified domain name",
		},
		{
			name: "empty TXT", typ: dnsv1alpha1.RRTypeTXT,
			e:    dnsv1alpha1.RecordEntry{Name: "@", TXT: &dnsv1alpha1.TXTRecordSpec{Content: ""}},
			want: "TXT data must not be empty",
		},
		{
			name: "oversized TXT", typ: dnsv1alpha1.RRTypeTXT,
			e: dnsv1alpha1.RecordEntry{Name: "@", TXT: &dnsv1alpha1.TXTRecordSpec{
				Content: strings.Repeat("a", 2049),
			}},
			want: "the maximum is 2048",
		},
		{
			name: "SRV target without a trailing dot", typ: dnsv1alpha1.RRTypeSRV,
			e: dnsv1alpha1.RecordEntry{Name: "_sip._tcp", SRV: &dnsv1alpha1.SRVRecordSpec{
				Priority: 10, Weight: 5, Port: 5060, Target: "sip",
			}},
			want: "SRV target \"sip\" is not a fully qualified domain name",
		},
		{
			name: "CAA tag out of the API pattern", typ: dnsv1alpha1.RRTypeCAA,
			e: dnsv1alpha1.RecordEntry{Name: "@", CAA: &dnsv1alpha1.CAARecordSpec{
				Flag: 0, Tag: "Issue-Wild", Value: "letsencrypt.org",
			}},
			want: "must match [a-z0-9]+",
		},
		{
			name: "empty CAA value", typ: dnsv1alpha1.RRTypeCAA,
			e: dnsv1alpha1.RecordEntry{Name: "@", CAA: &dnsv1alpha1.CAARecordSpec{
				Flag: 0, Tag: "issue", Value: "",
			}},
			want: "CAA value must not be empty",
		},
		{
			name: "CAA value with a quote", typ: dnsv1alpha1.RRTypeCAA,
			e: dnsv1alpha1.RecordEntry{Name: "@", CAA: &dnsv1alpha1.CAARecordSpec{
				Flag: 0, Tag: "issue", Value: `a"b`,
			}},
			want: "cannot encode",
		},
		{
			name: "TLSA usage out of range", typ: dnsv1alpha1.RRTypeTLSA,
			e: dnsv1alpha1.RecordEntry{Name: "_443._tcp", TLSA: &dnsv1alpha1.TLSARecordSpec{
				Usage: 9, Selector: 1, MatchingType: 1, CertData: strings.Repeat("ab", 32),
			}},
			want: "usage 9 is out of range",
		},
		{
			name: "TLSA selector out of range", typ: dnsv1alpha1.RRTypeTLSA,
			e: dnsv1alpha1.RecordEntry{Name: "_443._tcp", TLSA: &dnsv1alpha1.TLSARecordSpec{
				Usage: 3, Selector: 5, MatchingType: 1, CertData: strings.Repeat("ab", 32),
			}},
			want: "selector 5 is out of range",
		},
		{
			name: "TLSA matching type out of range", typ: dnsv1alpha1.RRTypeTLSA,
			e: dnsv1alpha1.RecordEntry{Name: "_443._tcp", TLSA: &dnsv1alpha1.TLSARecordSpec{
				Usage: 3, Selector: 1, MatchingType: 7, CertData: strings.Repeat("ab", 32),
			}},
			want: "matching type 7 is out of range",
		},
		{
			name: "TLSA data is not hex", typ: dnsv1alpha1.RRTypeTLSA,
			e: dnsv1alpha1.RecordEntry{Name: "_443._tcp", TLSA: &dnsv1alpha1.TLSARecordSpec{
				Usage: 3, Selector: 1, MatchingType: 0, CertData: "zzzz",
			}},
			want: "is not hexadecimal",
		},
		{
			name: "TLSA digest length wrong for SHA-256", typ: dnsv1alpha1.RRTypeTLSA,
			e: dnsv1alpha1.RecordEntry{Name: "_443._tcp", TLSA: &dnsv1alpha1.TLSARecordSpec{
				Usage: 3, Selector: 1, MatchingType: 1, CertData: "abcd",
			}},
			want: "needs a SHA-256 digest, got 4 hex digits",
		},
		{
			name: "TLSA digest length wrong for SHA-512", typ: dnsv1alpha1.RRTypeTLSA,
			e: dnsv1alpha1.RecordEntry{Name: "_443._tcp", TLSA: &dnsv1alpha1.TLSARecordSpec{
				Usage: 3, Selector: 1, MatchingType: 2, CertData: strings.Repeat("ab", 32),
			}},
			want: "needs a SHA-512 digest",
		},
		{
			name: "HTTPS empty target", typ: dnsv1alpha1.RRTypeHTTPS,
			e: dnsv1alpha1.RecordEntry{Name: "@", HTTPS: &dnsv1alpha1.HTTPSRecordSpec{
				Priority: 1, Target: "",
			}},
			want: "target must not be empty",
		},
		{
			name: "HTTPS target without a trailing dot", typ: dnsv1alpha1.RRTypeHTTPS,
			e: dnsv1alpha1.RecordEntry{Name: "@", HTTPS: &dnsv1alpha1.HTTPSRecordSpec{
				Priority: 1, Target: "svc.example.net",
			}},
			want: "not a fully qualified domain name",
		},
		{
			name: "HTTPS alias mode with parameters", typ: dnsv1alpha1.RRTypeHTTPS,
			e: dnsv1alpha1.RecordEntry{Name: "@", HTTPS: &dnsv1alpha1.HTTPSRecordSpec{
				Priority: 0, Target: "svc.example.net.", Params: map[string]string{"alpn": "h2"},
			}},
			want: "would be discarded",
		},
		{
			name: "HTTPS alias mode pointing at itself", typ: dnsv1alpha1.RRTypeHTTPS,
			e: dnsv1alpha1.RecordEntry{Name: "@", HTTPS: &dnsv1alpha1.HTTPSRecordSpec{
				Priority: 0, Target: ".",
			}},
			want: "alias mode, which needs a real target",
		},
		{
			name: "SVCB parameter with no value", typ: dnsv1alpha1.RRTypeSVCB,
			e: dnsv1alpha1.RecordEntry{Name: "@", SVCB: &dnsv1alpha1.HTTPSRecordSpec{
				Priority: 1, Target: ".", Params: map[string]string{"alpn": ""},
			}},
			want: "has no value",
		},
		{
			name: "SVCB parameter value with a space", typ: dnsv1alpha1.RRTypeSVCB,
			e: dnsv1alpha1.RecordEntry{Name: "@", SVCB: &dnsv1alpha1.HTTPSRecordSpec{
				Priority: 1, Target: ".", Params: map[string]string{"alpn": "h2 h3"},
			}},
			want: "containing whitespace",
		},
		{
			name: "SOA mname without a trailing dot", typ: dnsv1alpha1.RRTypeSOA,
			e: dnsv1alpha1.RecordEntry{Name: "@", SOA: &dnsv1alpha1.SOARecordSpec{
				MName: "ns1", RName: "hostmaster.example.com.",
			}},
			want: "SOA mname \"ns1\" is not a fully qualified domain name",
		},
		{
			name: "SOA rname written as an email address", typ: dnsv1alpha1.RRTypeSOA,
			e: dnsv1alpha1.RecordEntry{Name: "@", SOA: &dnsv1alpha1.SOARecordSpec{
				MName: "ns1.datum.net.", RName: "admin@example.com",
			}},
			want: "contains \"@\"", fix: "dot notation",
		},
		{
			name: "SOA rname is not a mailbox", typ: dnsv1alpha1.RRTypeSOA,
			e: dnsv1alpha1.RecordEntry{Name: "@", SOA: &dnsv1alpha1.SOARecordSpec{
				MName: "ns1.datum.net.", RName: "example.com.",
			}},
			want: "is not a mailbox address",
		},
		{
			name: "SOA rname without a trailing dot", typ: dnsv1alpha1.RRTypeSOA,
			e: dnsv1alpha1.RecordEntry{Name: "@", SOA: &dnsv1alpha1.SOARecordSpec{
				MName: "ns1.datum.net.", RName: "hostmaster.example.com",
			}},
			want: "SOA rname \"hostmaster.example.com\" is not a fully qualified domain name",
		},
		{
			name: "label longer than 63 characters", typ: dnsv1alpha1.RRTypeA,
			e: dnsv1alpha1.RecordEntry{
				Name: strings.Repeat("a", 64),
				A:    &dnsv1alpha1.ARecordSpec{Content: "203.0.113.10"},
			},
			want: "label longer than 63 characters",
		},
		{
			name: "partial wildcard label", typ: dnsv1alpha1.RRTypeA,
			e: dnsv1alpha1.RecordEntry{
				Name: "*www", A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.10"},
			},
			want: "partial wildcard label",
		},
		{
			name: "wildcard that is not leftmost", typ: dnsv1alpha1.RRTypeA,
			e: dnsv1alpha1.RecordEntry{
				Name: "dev.*", A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.10"},
			},
			want: "not the leftmost label",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.typ, tc.e)
			if err == nil {
				t.Fatalf("Validate(%s) accepted %+v", tc.typ, tc.e)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
			assertLowercaseStart(t, err)
			if tc.fix != "" && !strings.Contains(FixFor(err), tc.fix) {
				t.Fatalf("fix %q does not contain %q", FixFor(err), tc.fix)
			}
		})
	}
}

// TestValidateFixNamesTheZone checks the inline suggestion the design calls for
// on a missing trailing dot.
func TestValidateFixNamesTheZone(t *testing.T) {
	e := dnsv1alpha1.RecordEntry{Name: "@", MX: &dnsv1alpha1.MXRecordSpec{
		Preference: 10, Exchange: "mail",
	}}
	err := ValidateInZone(dnsv1alpha1.RRTypeMX, e, "example.com")
	if err == nil {
		t.Fatal("want an error")
	}
	if want := `"mail.example.com."`; !strings.Contains(FixFor(err), want) {
		t.Fatalf("fix %q should suggest %s", FixFor(err), want)
	}
}

func TestValidateEntries(t *testing.T) {
	a1 := entry(t, dnsv1alpha1.RRTypeA, "www", "203.0.113.10")
	a2 := entry(t, dnsv1alpha1.RRTypeA, "www", "203.0.113.11")

	if err := ValidateEntries(dnsv1alpha1.RRTypeA, []dnsv1alpha1.RecordEntry{a1, a2}); err != nil {
		t.Fatalf("a multi-value A set must be accepted: %v", err)
	}
	if err := ValidateEntries(dnsv1alpha1.RRTypeA, nil); err == nil {
		t.Fatal("an empty set must be rejected")
	}

	t.Run("duplicate value", func(t *testing.T) {
		dup := entry(t, dnsv1alpha1.RRTypeA, "www", "203.0.113.10")
		err := ValidateEntries(dnsv1alpha1.RRTypeA, []dnsv1alpha1.RecordEntry{a1, dup})
		if err == nil || !strings.Contains(err.Error(), "duplicate value") {
			t.Fatalf("want a duplicate error, got %v", err)
		}
	})

	t.Run("duplicate differing only by trailing dot", func(t *testing.T) {
		x := entry(t, dnsv1alpha1.RRTypeNS, "sub", "ns1.datum.net.")
		y := entry(t, dnsv1alpha1.RRTypeNS, "sub", "ns1.datum.net.")
		err := ValidateEntries(dnsv1alpha1.RRTypeNS, []dnsv1alpha1.RecordEntry{x, y})
		if err == nil || !strings.Contains(err.Error(), "duplicate value") {
			t.Fatalf("want a duplicate error, got %v", err)
		}
	})

	// internal/pdns keeps the first CNAME/ALIAS and drops the rest, and
	// overwrites the SOA rrset, so extra values would vanish without a word.
	for _, tc := range []struct {
		typ         dnsv1alpha1.RRType
		name        string
		v1, v2      string
		wantFixPart string
	}{
		{dnsv1alpha1.RRTypeCNAME, "api", "a.example.net.", "b.example.net.", "exactly one CNAME"},
		{dnsv1alpha1.RRTypeALIAS, "@", "a.example.net.", "b.example.net.", "writes the first"},
		{dnsv1alpha1.RRTypeSOA, "@",
			"ns1.datum.net. h.example.com.", "ns2.datum.net. h.example.com.", "writes the last"},
	} {
		t.Run("multi-value "+string(tc.typ), func(t *testing.T) {
			e1 := entry(t, tc.typ, tc.name, tc.v1)
			e2 := entry(t, tc.typ, tc.name, tc.v2)
			err := ValidateEntries(tc.typ, []dnsv1alpha1.RecordEntry{e1, e2})
			if err == nil {
				t.Fatalf("Validate accepted a multi-value %s set", tc.typ)
			}
			if !strings.Contains(err.Error(), "is single-valued") {
				t.Fatalf("unexpected error %q", err)
			}
			if !strings.Contains(FixFor(err), tc.wantFixPart) {
				t.Fatalf("fix %q should contain %q", FixFor(err), tc.wantFixPart)
			}
		})
	}

	t.Run("single-valued types are fine at different names", func(t *testing.T) {
		e1 := entry(t, dnsv1alpha1.RRTypeCNAME, "a", "x.example.net.")
		e2 := entry(t, dnsv1alpha1.RRTypeCNAME, "b", "x.example.net.")
		if err := ValidateEntriesInZone(dnsv1alpha1.RRTypeCNAME,
			[]dnsv1alpha1.RecordEntry{e1, e2}, "example.com"); err != nil {
			t.Fatalf("two CNAMEs at different owners must be accepted: %v", err)
		}
	})
}

func TestWarnings(t *testing.T) {
	t.Run("unknown CAA tag warns rather than failing", func(t *testing.T) {
		e := entry(t, dnsv1alpha1.RRTypeCAA, "@", `0 weirdtag "x"`)
		if err := Validate(dnsv1alpha1.RRTypeCAA, e); err != nil {
			t.Fatalf("an unknown but well-formed CAA tag must be accepted: %v", err)
		}
		w := Warnings(dnsv1alpha1.RRTypeCAA, e)
		if len(w) != 1 || !strings.Contains(w[0], "weirdtag") {
			t.Fatalf("want a warning naming the tag, got %v", w)
		}
	})

	t.Run("unusual CAA flag", func(t *testing.T) {
		e := entry(t, dnsv1alpha1.RRTypeCAA, "@", `7 issue "x.org"`)
		if err := Validate(dnsv1alpha1.RRTypeCAA, e); err != nil {
			t.Fatal(err)
		}
		if w := Warnings(dnsv1alpha1.RRTypeCAA, e); len(w) != 1 || !strings.Contains(w[0], "flag 7") {
			t.Fatalf("want a flag warning, got %v", w)
		}
	})

	t.Run("known tags are quiet", func(t *testing.T) {
		for _, tag := range []string{"issue", "issuewild", "iodef", "contactemail"} {
			e := entry(t, dnsv1alpha1.RRTypeCAA, "@", `0 `+tag+` "x.org"`)
			if w := Warnings(dnsv1alpha1.RRTypeCAA, e); len(w) != 0 {
				t.Fatalf("tag %q should not warn, got %v", tag, w)
			}
		}
	})

	t.Run("unregistered SVCB parameter", func(t *testing.T) {
		e := entry(t, dnsv1alpha1.RRTypeHTTPS, "@", "1 . weird=1")
		if w := Warnings(dnsv1alpha1.RRTypeHTTPS, e); len(w) != 1 {
			t.Fatalf("want one warning, got %v", w)
		}
		e = entry(t, dnsv1alpha1.RRTypeHTTPS, "@", "1 . key65000=1")
		if w := Warnings(dnsv1alpha1.RRTypeHTTPS, e); len(w) != 0 {
			t.Fatalf("keyNNNNN is a valid generic key, got %v", w)
		}
	})

	t.Run("SOA timers below the recommended minimums", func(t *testing.T) {
		e := entry(t, dnsv1alpha1.RRTypeSOA, "@", "ns1.datum.net. h.example.com. 1 600 60 60 60")
		w := Warnings(dnsv1alpha1.RRTypeSOA, e)
		if len(w) != 3 {
			t.Fatalf("want refresh, retry and expire warnings, got %v", w)
		}
	})

	t.Run("disagreeing TTLs across one owner", func(t *testing.T) {
		a := entry(t, dnsv1alpha1.RRTypeA, "www", "203.0.113.10")
		a.TTL = ptr(int64(300))
		b := entry(t, dnsv1alpha1.RRTypeA, "www", "203.0.113.11")
		b.TTL = ptr(int64(60))
		w := Warnings(dnsv1alpha1.RRTypeA, a, b)
		if len(w) != 1 || !strings.Contains(w[0], "applies the first one, 5m") {
			t.Fatalf("want one TTL warning, got %v", w)
		}
		// Agreeing TTLs are silent.
		b.TTL = ptr(int64(300))
		if w := Warnings(dnsv1alpha1.RRTypeA, a, b); len(w) != 0 {
			t.Fatalf("want no warning, got %v", w)
		}
	})

	t.Run("SRV port 0", func(t *testing.T) {
		e := entry(t, dnsv1alpha1.RRTypeSRV, "_sip._tcp", "10 5 0 sip.example.com.")
		if w := Warnings(dnsv1alpha1.RRTypeSRV, e); len(w) != 1 {
			t.Fatalf("want a port warning, got %v", w)
		}
	})

	t.Run("TTL 0", func(t *testing.T) {
		e := entry(t, dnsv1alpha1.RRTypeA, "www", "203.0.113.10")
		e.TTL = ptr(int64(0))
		if err := Validate(dnsv1alpha1.RRTypeA, e); err != nil {
			t.Fatalf("TTL 0 is legal: %v", err)
		}
		if w := Warnings(dnsv1alpha1.RRTypeA, e); len(w) != 1 {
			t.Fatalf("want a TTL warning, got %v", w)
		}
	})
}

func TestNormalizeName(t *testing.T) {
	const zone = "example.com"
	cases := []struct {
		in       string
		want     string
		wantErr  string
		wantFix  string
		wantWarn string
	}{
		{in: "@", want: "@"},
		{in: "", want: "@"},
		{in: ".", want: "@"},
		{in: "www", want: "www"},
		{in: "WWW", want: "www"},
		{in: "  www  ", want: "www"},
		{in: "*", want: "*"},
		{in: "*.dev", want: "*.dev"},
		{in: "_dmarc", want: "_dmarc"},
		{in: "_sip._tcp", want: "_sip._tcp"},
		{in: "a.b.c", want: "a.b.c"},
		{in: "example.com.", want: "@"},
		{in: "EXAMPLE.COM.", want: "@"},
		{in: "www.example.com.", want: "www"},
		{in: "_sip._tcp.example.com.", want: "_sip._tcp"},
		{
			in: "www.other.net.", want: "www.other.net.",
			wantWarn: "outside zone",
		},
		{
			in:      "www.example.com",
			wantErr: `record name "www.example.com" already includes the zone domain`,
			wantFix: `use "www", or "www.example.com." with a trailing dot`,
		},
		{
			in:      "example.com",
			wantErr: "is the zone itself",
			wantFix: `use "@"`,
		},
		{in: "www!", wantErr: "not a valid owner name"},
		{in: "a b", wantErr: "contains whitespace"},
		{in: "a..b", wantErr: "empty label"},
		{in: strings.Repeat("a", 64), wantErr: "label longer than 63 characters"},
		{in: "*www", wantErr: "partial wildcard label"},
		{in: "dev.*", wantErr: "not the leftmost label"},
	}

	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, warns, err := NormalizeNameWithWarnings(tc.in, zone)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("NormalizeName(%q) = %q, want error", tc.in, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tc.wantErr)
				}
				if tc.wantFix != "" && !strings.Contains(FixFor(err), tc.wantFix) {
					t.Fatalf("fix %q does not contain %q", FixFor(err), tc.wantFix)
				}
				assertLowercaseStart(t, err)
				return
			}
			if err != nil {
				t.Fatalf("NormalizeName(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("NormalizeName(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if tc.wantWarn == "" {
				if len(warns) != 0 {
					t.Fatalf("unexpected warnings %v", warns)
				}
			} else if len(warns) != 1 || !strings.Contains(warns[0], tc.wantWarn) {
				t.Fatalf("warnings %v do not contain %q", warns, tc.wantWarn)
			}
		})
	}
}

// TestNormalizeNameMatchesQualifyOwner is the point of the whole exercise: the
// normalized name, put through the backend's qualification rule, must land on
// the RRset the user meant.
func TestNormalizeNameMatchesQualifyOwner(t *testing.T) {
	const zone = "example.com"
	cases := map[string]string{
		"@":                "example.com.",
		"www":              "www.example.com.",
		"www.example.com.": "www.example.com.",
		"example.com.":     "example.com.",
		"*.dev":            "*.dev.example.com.",
		"_sip._tcp":        "_sip._tcp.example.com.",
		"www.other.net.":   "www.other.net.",
	}
	for in, want := range cases {
		norm, err := NormalizeName(in, zone)
		if err != nil {
			t.Fatalf("NormalizeName(%q): %v", in, err)
		}
		if got := FQDN(norm, zone); got != want {
			t.Fatalf("FQDN(NormalizeName(%q)) = %q, want %q", in, got, want)
		}
	}
}

// TestFQDNReproducesTheTrap documents the behaviour NormalizeName defends
// against: pdns.QualifyOwner appends the zone to anything without a trailing
// dot, so a name that already spells out the zone is doubled.
func TestFQDNReproducesTheTrap(t *testing.T) {
	if got := FQDN("www.example.com", "example.com"); got != "www.example.com.example.com." {
		t.Fatalf("FQDN should mirror QualifyOwner, got %q", got)
	}
	if _, err := NormalizeName("www.example.com", "example.com"); err == nil {
		t.Fatal("NormalizeName must reject the name that produces that")
	}
}

func TestIsApex(t *testing.T) {
	for _, in := range []string{"@", ""} {
		if !IsApex(in) {
			t.Fatalf("IsApex(%q) = false", in)
		}
	}
	for _, in := range []string{"www", "*", "example.com."} {
		if IsApex(in) {
			t.Fatalf("IsApex(%q) = true", in)
		}
	}
}

func TestParseTTL(t *testing.T) {
	cases := []struct {
		in      string
		want    *int64
		wantErr string
	}{
		{in: "", want: nil},
		{in: "auto", want: nil},
		{in: "AUTO", want: nil},
		{in: "  ", want: nil},
		{in: "300", want: ptr(int64(300))},
		{in: "0", want: ptr(int64(0))},
		{in: "240", want: ptr(int64(240))}, // no snapping onto a preset ladder
		{in: "5m", want: ptr(int64(300))},
		{in: "1h", want: ptr(int64(3600))},
		{in: "24h", want: ptr(int64(86400))},
		{in: "1h30m", want: ptr(int64(5400))},
		{in: "1d", want: ptr(int64(86400))},
		{in: "1w", want: ptr(int64(604800))},
		{in: "2W", want: ptr(int64(1209600))},
		{in: "1d12h", want: ptr(int64(129600))},
		// A decimal point is fine as long as it lands on a whole second.
		{in: "1.5h", want: ptr(int64(5400))},
		{in: "-1", wantErr: "is negative"},
		{in: "-5m", wantErr: "is negative"},
		{in: "2147483648", wantErr: "exceeds the maximum"},
		{in: "1.5s", wantErr: "not a whole number of seconds"},
		{in: "later", wantErr: "invalid TTL"},
		{in: "300s extra", wantErr: "invalid TTL"},
		{in: "300ms", wantErr: "invalid TTL"},
		{in: "1.5d", wantErr: "invalid TTL"}, // time.ParseDuration has no "d"
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseTTL(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseTTL(%q) = %v, want error", tc.in, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tc.wantErr)
				}
				assertLowercaseStart(t, err)
				return
			}
			if err != nil {
				t.Fatalf("ParseTTL(%q): %v", tc.in, err)
			}
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("ParseTTL(%q) = %d, want nil", tc.in, *got)
			case tc.want != nil && got == nil:
				t.Fatalf("ParseTTL(%q) = nil, want %d", tc.in, *tc.want)
			case tc.want != nil && *got != *tc.want:
				t.Fatalf("ParseTTL(%q) = %d, want %d", tc.in, *got, *tc.want)
			}
		})
	}
}

func TestFormatTTL(t *testing.T) {
	if got := FormatTTL(nil); got != "Auto" {
		t.Fatalf("FormatTTL(nil) = %q, want Auto", got)
	}
	// Every rendered TTL carries its unit. A bare number in a TTL column
	// cannot be read without guessing at seconds versus minutes.
	cases := map[int64]string{
		0:       "0s",
		1:       "1s",
		5:       "5s",
		59:      "59s",
		60:      "1m",
		90:      "90s", // does not divide evenly, so it stays in seconds
		300:     "5m",
		3600:    "1h",
		5400:    "90m", // 90m, not 1h30m: one unit is easier to scan
		7200:    "2h",
		86400:   "1d",
		129600:  "36h", // 1d12h would need two units, so hours win
		604800:  "1w",
		1209600: "2w",
	}
	for secs, want := range cases {
		if got := FormatTTL(ptr(secs)); got != want {
			t.Errorf("FormatTTL(%d) = %q, want %q", secs, got, want)
		}
	}
}

// TestFormatTTLRoundTrips is the property that makes the humanized column
// safe: whatever the table shows can be pasted straight back into --ttl and
// mean the same thing.
// TestParseTTLSecondsSharesTheGrammar pins the invariant that made bind stop
// carrying its own TTL parser: whatever `--ttl` accepts, a zone file accepts,
// and the two agree on the number.
func TestParseTTLSecondsSharesTheGrammar(t *testing.T) {
	for _, in := range []string{"300", "5m", "1h", "1d", "1w", "1h30m", "2W", "1.5h"} {
		viaPtr, err := ParseTTL(in)
		if err != nil {
			t.Fatalf("ParseTTL(%q): %v", in, err)
		}
		viaSecs, err := ParseTTLSeconds(in)
		if err != nil {
			t.Fatalf("ParseTTLSeconds(%q): %v", in, err)
		}
		if viaPtr == nil || *viaPtr != viaSecs {
			t.Errorf("%q: ParseTTL = %v, ParseTTLSeconds = %d", in, viaPtr, viaSecs)
		}
	}
}

// TestErrTTLRangeIsDistinguishable is what lets a caller render its own wording
// for a too-large TTL without matching on the message text. bind does exactly
// that, because a zone file has a line number to point at.
func TestErrTTLRangeIsDistinguishable(t *testing.T) {
	for _, in := range []string{"-1", "-5m", "2147483648", "9999w"} {
		_, err := ParseTTL(in)
		if !errors.Is(err, ErrTTLRange) {
			t.Errorf("ParseTTL(%q) = %v, want an ErrTTLRange", in, err)
		}
	}
	// A value that is not a TTL at all is a different failure.
	if _, err := ParseTTL("later"); errors.Is(err, ErrTTLRange) {
		t.Errorf(`ParseTTL("later") reported a range error: %v`, err)
	}
	// The sentinel must not leak into what the user reads.
	_, err := ParseTTL("2147483648")
	if got := err.Error(); got != `TTL "2147483648" exceeds the maximum of 2147483647 seconds` {
		t.Errorf("message = %q", got)
	}
}

func TestFormatTTLRoundTrips(t *testing.T) {
	for secs := int64(0); secs <= 700000; secs += 7 {
		rendered := FormatTTL(ptr(secs))
		back, err := ParseTTL(rendered)
		if err != nil {
			t.Fatalf("ParseTTL(FormatTTL(%d) = %q): %v", secs, rendered, err)
		}
		if back == nil || *back != secs {
			t.Fatalf("FormatTTL(%d) = %q, which parses back to %v", secs, rendered, back)
		}
	}
}

func TestParseLine(t *testing.T) {
	cases := []struct {
		in      string
		name    string
		ttl     *int64
		typ     dnsv1alpha1.RRType
		rdata   string
		wantErr string
	}{
		{in: "www 300 IN A 203.0.113.10", name: "www", ttl: ptr(int64(300)),
			typ: dnsv1alpha1.RRTypeA, rdata: "203.0.113.10"},
		{in: "www IN A 203.0.113.10", name: "www", typ: dnsv1alpha1.RRTypeA, rdata: "203.0.113.10"},
		{in: "www 300 A 203.0.113.10", name: "www", ttl: ptr(int64(300)),
			typ: dnsv1alpha1.RRTypeA, rdata: "203.0.113.10"},
		{in: "www A 203.0.113.10", name: "www", typ: dnsv1alpha1.RRTypeA, rdata: "203.0.113.10"},
		{in: "www IN 300 A 203.0.113.10", name: "www", ttl: ptr(int64(300)),
			typ: dnsv1alpha1.RRTypeA, rdata: "203.0.113.10"},
		{in: "www 5m a 203.0.113.10", name: "www", ttl: ptr(int64(300)),
			typ: dnsv1alpha1.RRTypeA, rdata: "203.0.113.10"},
		{in: "@ 3600 IN MX 10 mail.example.com.", name: "@", ttl: ptr(int64(3600)),
			typ: dnsv1alpha1.RRTypeMX, rdata: "10 mail.example.com."},
		{in: "\t_dmarc\t3600\tIN\tTXT\t\"v=DMARC1; p=none\"", name: "_dmarc", ttl: ptr(int64(3600)),
			typ: dnsv1alpha1.RRTypeTXT, rdata: `"v=DMARC1; p=none"`},
		{in: `www 300 IN A 203.0.113.10 ; the old one`, name: "www", ttl: ptr(int64(300)),
			typ: dnsv1alpha1.RRTypeA, rdata: "203.0.113.10"},
		{in: "api CNAME lb.example.net.", name: "api",
			typ: dnsv1alpha1.RRTypeCNAME, rdata: "lb.example.net."},
		{in: "svc 60 IN HTTPS 1 . alpn=h3,h2 port=443", name: "svc", ttl: ptr(int64(60)),
			typ: dnsv1alpha1.RRTypeHTTPS, rdata: "1 . alpn=h3,h2 port=443"},
		{in: "", wantErr: "is empty"},
		{in: "www A", wantErr: "missing a type or value"},
		{in: "www 300 IN", wantErr: "no record type"},
		{in: "www 300 IN NOPE x", wantErr: "unsupported record type"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseLine(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseLine(%q) = %+v, want error", tc.in, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tc.wantErr)
				}
				assertLowercaseStart(t, err)
				return
			}
			if err != nil {
				t.Fatalf("ParseLine(%q): %v", tc.in, err)
			}
			if got.Name != tc.name {
				t.Errorf("name = %q, want %q", got.Name, tc.name)
			}
			if FormatTTL(got.TTL) != FormatTTL(tc.ttl) {
				t.Errorf("ttl = %s, want %s", FormatTTL(got.TTL), FormatTTL(tc.ttl))
			}
			if got.Type != tc.typ {
				t.Errorf("type = %q, want %q", got.Type, tc.typ)
			}
			if got.Rdata != tc.rdata {
				t.Errorf("rdata = %q, want %q", got.Rdata, tc.rdata)
			}
			if _, perr := ParseValue(got.Type, got.Rdata); perr != nil {
				t.Errorf("rdata %q does not parse as %s: %v", got.Rdata, got.Type, perr)
			}
		})
	}
}

// TestParseLineFeedsTheRestOfThePipeline walks a pasted line all the way to a
// validated entry, the path `record create --line` takes.
func TestParseLineFeedsTheRestOfThePipeline(t *testing.T) {
	line, err := ParseLine("www 300 IN A 203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	name, err := NormalizeName(line.Name, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	e, err := ParseValue(line.Type, line.Rdata)
	if err != nil {
		t.Fatal(err)
	}
	e.Name, e.TTL = name, line.TTL
	if err := ValidateInZone(line.Type, e, "example.com"); err != nil {
		t.Fatalf("validating the parsed line: %v", err)
	}
	if got := Render(line.Type, e); got != "203.0.113.10" {
		t.Fatalf("Render = %q", got)
	}
}

func TestResolveTXTData(t *testing.T) {
	if got, err := ResolveTXTData("v=spf1 ~all", nil); err != nil || got != "v=spf1 ~all" {
		t.Fatalf("literal: %q %v", got, err)
	}
	if got, err := ResolveTXTData("-", strings.NewReader("from stdin\n")); err != nil || got != "from stdin" {
		t.Fatalf("stdin: %q %v", got, err)
	}
	path := t.TempDir() + "/txt"
	if err := writeFile(path, "from file\n"); err != nil {
		t.Fatal(err)
	}
	if got, err := ResolveTXTData("@"+path, nil); err != nil || got != "from file" {
		t.Fatalf("file: %q %v", got, err)
	}
	if _, err := ResolveTXTData("@", nil); err == nil {
		t.Fatal("@ with no path must fail")
	}
	if _, err := ResolveTXTData("@/no/such/file", nil); err == nil {
		t.Fatal("a missing file must fail")
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
