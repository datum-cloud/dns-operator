// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

func TestDeleteOneValueLeavesTheRest(t *testing.T) {
	interactive(t)
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA,
		aEntry("www", "203.0.113.10", ttl(300)),
		aEntry("www", "203.0.113.11", ttl(300)),
	))
	h.answer("y\n")
	requireNoError(t, h.run("record", "delete", testDomain, "www", "A", "203.0.113.11"))

	got := h.getSet(t, "example-com-a").Spec.Records
	if len(got) != 1 || got[0].A.Content != "203.0.113.10" {
		t.Fatalf("records = %+v, want only 203.0.113.10", got)
	}
	mustContain(t, h.stderr(), "Delete the A record 203.0.113.11 for www.example.com? [y/N]")
	mustContain(t, h.stdout(), "  record/example.com A www deleted")
	mustContain(t, collapse(h.stdout()), "- www 300 IN A 203.0.113.11")
}

// TestDeleteAllAtANameStatesTheCount — deleting three records is a different
// decision from deleting one, so the prompt says which it is.
func TestDeleteAllAtANameStatesTheCount(t *testing.T) {
	interactive(t)
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA,
		aEntry("www", "203.0.113.10", ttl(300)),
		aEntry("www", "203.0.113.11", ttl(300)),
		aEntry("www", "203.0.113.12", ttl(300)),
		aEntry("api", "203.0.113.20", ttl(300)),
	))
	h.answer("y\n")
	requireNoError(t, h.run("record", "delete", testDomain, "www", "A"))

	mustContain(t, h.stderr(), "Delete all 3 A records for www.example.com? [y/N]")

	got := h.getSet(t, "example-com-a").Spec.Records
	if len(got) != 1 || got[0].Name != "api" {
		t.Fatalf("records = %+v, want only the api entry", got)
	}
}

func TestDeleteDeclinedChangesNothing(t *testing.T) {
	interactive(t)
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))
	h.answer("n\n")
	ce := requireExit(t, h.run("record", "delete", testDomain, "www", "A"), util.ExitAborted)

	mustContain(t, ce.Error(), "aborted; nothing was deleted")
	if got := len(h.getSet(t, "example-com-a").Spec.Records); got != 1 {
		t.Errorf("records = %d, want 1", got)
	}
}

func TestDeleteYesSkipsThePrompt(t *testing.T) {
	interactive(t)
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))
	requireNoError(t, h.run("record", "delete", testDomain, "www", "A", "--yes"))
	mustNotContain(t, h.stderr(), "[y/N]")
}

// TestDeleteLastEntryRemovesTheObject — spec.records has MinItems=1, so an
// emptied bucket cannot be written back; the object itself goes.
func TestDeleteLastEntryRemovesTheObject(t *testing.T) {
	interactive(t)
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))
	requireNoError(t, h.run("record", "delete", testDomain, "www", "A", "--yes"))

	if !h.setMissing(t, "example-com-a") {
		t.Error("the emptied DNSRecordSet was left behind; MinItems=1 makes that an invalid object")
	}
	mustContain(t, h.stdout(), "record set example-com-a removed — no A records remain in the zone")
}

func TestDeleteUnknownValueIsNotFound(t *testing.T) {
	interactive(t)
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))
	ce := requireExit(t, h.run("record", "delete", testDomain, "www", "A", "203.0.113.99", "--yes"), util.ExitNotFound)
	mustContain(t, ce.Error(), `www.example.com has no A value "203.0.113.99"`)
	mustContain(t, ce.Fix(), "record list example.com --name www --type A")
}

func TestDeleteUnknownNameIsNotFound(t *testing.T) {
	interactive(t)
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))
	ce := requireExit(t, h.run("record", "delete", testDomain, "nope", "A", "--yes"), util.ExitNotFound)
	mustContain(t, ce.Error(), "no A records for nope.example.com")
}

func TestDeleteMissingBucketIsNotFound(t *testing.T) {
	interactive(t)
	h := newHarness(t, testZone())
	requireExit(t, h.run("record", "delete", testDomain, "www", "A", "--yes"), util.ExitNotFound)
}

// TestDeleteMatchesTheValueThroughItsSpelling — Key/Equal fold the differences
// that do not change what the record resolves to.
func TestDeleteMatchesTheValueThroughItsSpelling(t *testing.T) {
	interactive(t)
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeMX, dnsv1alpha1.RecordEntry{
		Name: "@", TTL: ttl(300),
		MX: &dnsv1alpha1.MXRecordSpec{Preference: 10, Exchange: "MAIL.example.com."},
	}))
	requireNoError(t, h.run("record", "delete", testDomain, "@", "MX", "10 mail.example.com.", "--yes"))
	if !h.setMissing(t, "example-com-mx") {
		t.Error("the record was not matched through its spelling")
	}
}

func TestDeleteDryRunWritesNothing(t *testing.T) {
	interactive(t)
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))
	requireNoError(t, h.run("record", "delete", testDomain, "www", "A", "--dry-run"))

	out := collapse(h.stdout())
	mustContain(t, out, "Dry run — no changes were made.")
	mustContain(t, out, "- www 300 IN A 203.0.113.10")
	mustContain(t, out, "record set example-com-a would be removed")
	mustNotContain(t, h.stderr(), "[y/N]")

	if h.setMissing(t, "example-com-a") {
		t.Error("a dry run deleted the object")
	}
}

// TestDeleteReportsAMissingValueBeforeTheManagedGuard — being told a
// platform-managed record is protected, when the value named does not exist at
// all, sends the user off to argue with the wrong problem.
func TestDeleteReportsAMissingValueBeforeTheManagedGuard(t *testing.T) {
	interactive(t)
	h := newHarness(t, testZone(), apexNSSet())
	ce := requireExit(t, h.run("record", "delete", testDomain, "@", "NS", "ns9.datum.net.", "--yes"), util.ExitNotFound)

	mustContain(t, ce.Error(), `has no NS value "ns9.datum.net."`)
	mustNotContain(t, ce.Error(), "platform-managed")
}

// TestDeleteStillGuardsAValueThatDoesExist.
func TestDeleteStillGuardsAValueThatDoesExist(t *testing.T) {
	interactive(t)
	h := newHarness(t, testZone(), apexNSSet())
	ce := requireExit(t, h.run("record", "delete", testDomain, "@", "NS", "ns1.datum.net.", "--yes"), util.ExitUsage)

	mustContain(t, ce.Error(), "is a platform-managed record")
	mustContain(t, ce.Fix(), "editing apex NS records can break delegation")
	if got := len(h.getSet(t, "example-com-ns").Spec.Records); got != 2 {
		t.Errorf("records = %d, want 2 — nothing should have been written", got)
	}
}
