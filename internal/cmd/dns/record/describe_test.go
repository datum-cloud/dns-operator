// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// TestDescribeShowsTheNamedFields — a record entered as presentation format is
// shown by its named fields, so each notation teaches the other.
func TestDescribeShowsTheNamedFields(t *testing.T) {
	srv := recordSet(dnsv1alpha1.RRTypeSRV, dnsv1alpha1.RecordEntry{
		Name: "_sip._tcp", TTL: ttl(300),
		SRV: &dnsv1alpha1.SRVRecordSpec{Priority: 10, Weight: 5, Port: 5060, Target: "sip.example.com."},
	})
	withOwnerStatus(srv, "_sip._tcp", metav1.ConditionTrue, "Programmed", "")

	h := newHarness(t, testZone(), srv)
	requireNoError(t, h.run("record", "describe", testDomain, "_sip._tcp"))

	out := h.stdout()
	mustContain(t, collapse(out), "Record _sip._tcp.example.com")
	mustContain(t, collapse(out), "Type SRV")
	mustContain(t, collapse(out), "TTL 300")
	mustContain(t, out, "10 5 5060 sip.example.com.")
	mustContain(t, collapse(out), "Priority: 10")
	mustContain(t, collapse(out), "Weight: 5")
	mustContain(t, collapse(out), "Port: 5060")
	mustContain(t, collapse(out), "Target: sip.example.com.")
	mustContain(t, collapse(out), "Status Programmed")
	mustContain(t, out, "Next steps:")
	mustContain(t, out, "datumctl dns record set example.com _sip._tcp SRV <value>")
}

// TestDescribeShowsTheBackendsSentenceVerbatim — those messages are written for
// people already, and rewording them would only add a layer to be wrong.
func TestDescribeShowsTheBackendsSentenceVerbatim(t *testing.T) {
	const message = "The record name is outside the zone. Check that the name belongs to this DNS zone."

	rs := recordSet(dnsv1alpha1.RRTypeA, aEntry("api", "203.0.113.12", nil))
	withOwnerStatus(rs, "api", metav1.ConditionFalse, "Conflict", message)

	h := newHarness(t, testZone(), rs)
	requireNoError(t, h.run("record", "describe", testDomain, "api", "A"))

	mustContain(t, collapse(h.stdout()), "Status "+util.StatusConflict)
	mustContain(t, h.stdout(), message)
}

// TestDescribeSpellsOutWhatAutoMeans — the number is never a mystery.
func TestDescribeSpellsOutWhatAutoMeans(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", nil)))
	requireNoError(t, h.run("record", "describe", testDomain, "www"))
	mustContain(t, collapse(h.stdout()), "TTL Auto (300)")
}

// TestDescribeReportsDisagreeingTTLs — the API stores TTL per entry, the
// backend applies the first one to the whole RRset.
func TestDescribeReportsDisagreeingTTLs(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA,
		aEntry("www", "203.0.113.10", ttl(300)),
		aEntry("www", "203.0.113.11", ttl(60)),
	))
	requireNoError(t, h.run("record", "describe", testDomain, "www"))
	mustContain(t, collapse(h.stdout()), "TTL 300 (values disagree; the backend applies the first)")
}

// TestDescribeWithoutATypeShowsEveryTypeAtTheName.
func TestDescribeWithoutATypeShowsEveryTypeAtTheName(t *testing.T) {
	a := recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300)))
	txt := recordSet(dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RecordEntry{
		Name: "www", TTL: ttl(300), TXT: &dnsv1alpha1.TXTRecordSpec{Content: `"hello"`},
	})

	h := newHarness(t, testZone(), a, txt)
	requireNoError(t, h.run("record", "describe", testDomain, "www"))

	out := h.stdout()
	mustContain(t, collapse(out), "Type A")
	mustContain(t, collapse(out), "Type TXT")
	if strings.Count(out, "Next steps:") != 1 {
		t.Errorf("Next steps should appear once, got %d", strings.Count(out, "Next steps:"))
	}
}

func TestDescribeNarrowsToOneType(t *testing.T) {
	a := recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300)))
	txt := recordSet(dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RecordEntry{
		Name: "www", TTL: ttl(300), TXT: &dnsv1alpha1.TXTRecordSpec{Content: `"hello"`},
	})

	h := newHarness(t, testZone(), a, txt)
	requireNoError(t, h.run("record", "describe", testDomain, "www", "A"))

	mustContain(t, collapse(h.stdout()), "Type A")
	mustNotContain(t, h.stdout(), "TXT")
}

func TestDescribeMarksManagedRecords(t *testing.T) {
	h := newHarness(t, testZone(), gatewaySet(dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RecordEntry{
		Name: "_acme", TXT: &dnsv1alpha1.TXTRecordSpec{Content: `"token"`},
	}))
	requireNoError(t, h.run("record", "describe", testDomain, "_acme"))
	mustContain(t, collapse(h.stdout()), `Managed by AI Edge — Gateway "edge-gw"; this record is read-only`)
}

func TestDescribeApexUsesTheDomain(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeMX, dnsv1alpha1.RecordEntry{
		Name: "@", TTL: ttl(300),
		MX: &dnsv1alpha1.MXRecordSpec{Preference: 10, Exchange: "mail.example.com."},
	}))
	requireNoError(t, h.run("record", "describe", testDomain, "@"))

	mustContain(t, collapse(h.stdout()), "Record example.com")
	mustContain(t, collapse(h.stdout()), "Preference: 10")
}

func TestDescribeUnknownNameIsNotFound(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", nil)))
	ce := requireExit(t, h.run("record", "describe", testDomain, "nope"), util.ExitNotFound)
	mustContain(t, ce.Error(), "no records for nope.example.com")
}

func TestDescribeJSONEmitsTheOwningSets(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", nil)))
	requireNoError(t, h.run("record", "describe", testDomain, "www", "-o", "json"))
	mustContain(t, h.stdout(), `"kind": "DNSRecordSetList"`)
	mustContain(t, h.stdout(), `"recordType": "A"`)
}

// TestDescribeNeverRendersTheEpochAsAnAge — a fresh DNSRecordSet's defaulted
// conditions are stamped 1970-01-01.
func TestDescribeNeverRendersTheEpochAsAnAge(t *testing.T) {
	rs := recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", nil))
	rs.CreationTimestamp = metav1.Unix(0, 0)

	h := newHarness(t, testZone(), rs)
	requireNoError(t, h.run("record", "describe", testDomain, "www"))
	mustContain(t, collapse(h.stdout()), "Created —")
	mustNotContain(t, h.stdout(), "56y")
}

// TestDescribeAutoNamesTheRealDefault — the number comes from util.DefaultTTL,
// which is pinned to internal/pdns, not from a literal copied into this package.
func TestDescribeAutoNamesTheRealDefault(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", nil)))
	requireNoError(t, h.run("record", "describe", testDomain, "www"))
	mustContain(t, collapse(h.stdout()), fmt.Sprintf("TTL Auto (%d)", util.DefaultTTL))
}

// TestDescribeDoesNotCallAutoAndTheDefaultADisagreement — they resolve to the
// same number, so the entries do not actually disagree.
func TestDescribeDoesNotCallAutoAndTheDefaultADisagreement(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA,
		aEntry("www", "203.0.113.10", nil),
		aEntry("www", "203.0.113.11", ttl(util.DefaultTTL)),
	))
	requireNoError(t, h.run("record", "describe", testDomain, "www"))
	mustNotContain(t, h.stdout(), "values disagree")
}
