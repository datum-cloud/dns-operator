// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// zoneFilePath writes a zone file into a temporary directory and returns it.
func zoneFilePath(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "example.com.zone")
	if err := writeFile(path, content); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return path
}

func TestApplyRequiresAFile(t *testing.T) {
	h := newHarness(t, testZone())
	ce := requireExit(t, h.run("record", "apply", testDomain), util.ExitUsage)
	mustContain(t, ce.Error(), "--file is required")
}

func TestApplyNoChanges(t *testing.T) {
	h := newHarness(t, testZone(),
		recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))

	path := zoneFilePath(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.10\n")
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--yes"))
	mustContain(t, h.stdout(), "No changes.")
}

func TestApplyDiffVocabulary(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA,
		aEntry("www", "203.0.113.10", ttl(300)),
		aEntry("api", "203.0.113.20", ttl(300)),
	))

	// www gains a value, api's TTL changes, old is only live.
	path := zoneFilePath(t, `$ORIGIN example.com.
www 300  IN A 203.0.113.10
www 300  IN A 203.0.113.11
api 3600 IN A 203.0.113.20
`)
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--yes"))

	lines := collapsedLines(h.stdout())
	wantLines := []string{
		"+ www A 300 203.0.113.11",
		"→ api A 300 → 3600 203.0.113.20",
	}
	for _, want := range wantLines {
		found := false
		for _, l := range lines {
			if l == want {
				found = true
			}
		}
		if !found {
			t.Errorf("diff is missing %q\n--- got ---\n%s", want, h.stdout())
		}
	}
	mustContain(t, h.stdout(), "1 to add, 1 to change")
	mustContain(t, h.stdout(), "Applied 2 changes")

	set := h.getSet(t, testZoneObject+"-a")
	if len(set.Spec.Records) != 3 {
		t.Errorf("the A set holds %d entries, want 3", len(set.Spec.Records))
	}
}

// Without --prune a live record the file omits survives untouched.
func TestApplyWithoutPruneKeepsExtras(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA,
		aEntry("www", "203.0.113.10", ttl(300)),
		aEntry("old", "198.51.100.1", ttl(300)),
	))

	path := zoneFilePath(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.10\n")
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--yes"))
	mustContain(t, h.stdout(), "No changes.")

	set := h.getSet(t, testZoneObject+"-a")
	if len(set.Spec.Records) != 2 {
		t.Errorf("apply without --prune removed a record: %+v", set.Spec.Records)
	}
}

func TestApplyPruneDeletes(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA,
		aEntry("www", "203.0.113.10", ttl(300)),
		aEntry("old", "198.51.100.1", ttl(300)),
	))

	path := zoneFilePath(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.10\n")
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--prune", "--yes"))

	mustContain(t, collapse(h.stdout()), "- old A 300 198.51.100.1")
	set := h.getSet(t, testZoneObject+"-a")
	if len(set.Spec.Records) != 1 || set.Spec.Records[0].Name != "www" {
		t.Errorf("--prune left %+v, want only www", set.Spec.Records)
	}
}

// Pruning the last entry of a type must remove the object, because
// spec.records has MinItems=1 and an empty set is not a legal object.
func TestApplyPruneRemovesTheWholeSet(t *testing.T) {
	h := newHarness(t, testZone(),
		recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))),
		recordSet(dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RecordEntry{
			Name: "@", TTL: ttl(300), TXT: &dnsv1alpha1.TXTRecordSpec{Content: `"gone"`},
		}),
	)

	path := zoneFilePath(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.10\n")
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--prune", "--yes"))

	if !h.setMissing(t, testZoneObject+"-txt") {
		t.Error("pruning the last TXT entry left an empty record set behind")
	}
}

// --- platform-managed records ----------------------------------------------

func TestApplyPruneKeepsPlatformRecords(t *testing.T) {
	h := newHarness(t, testZone(), soaSet(), apexNSSet(),
		recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))

	// The file has neither the SOA nor the NS records, which is what an export
	// the user trimmed looks like.
	path := zoneFilePath(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.10\n")
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--prune", "--yes"))

	if h.setMissing(t, testZoneObject+"-soa") {
		t.Error("--prune deleted the zone's SOA record")
	}
	ns := h.getSet(t, testZoneObject+"-ns")
	if len(ns.Spec.Records) != 2 {
		t.Errorf("--prune modified the zone's apex NS records: %+v", ns.Spec.Records)
	}
	mustContain(t, h.stdout(), "No changes.")
}

func TestApplyReportsPlatformRecordsItSkipped(t *testing.T) {
	h := newHarness(t, testZone(), apexNSSet())

	// The file asks to replace the delegation, which apply must decline and say
	// so rather than silently ignoring.
	path := zoneFilePath(t, "$ORIGIN example.com.\n@ 3600 IN NS ns9.evil.example.\n")
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--prune", "--yes"))

	ns := h.getSet(t, testZoneObject+"-ns")
	for _, e := range ns.Spec.Records {
		if strings.Contains(e.NS.Content, "evil") {
			t.Fatal("apply rewrote the zone's apex NS records")
		}
	}
	mustContain(t, h.stderr(), "belong to the platform")
	mustContain(t, h.stderr(), "apex NS")
}

func TestApplyNeverTouchesGatewayRecords(t *testing.T) {
	h := newHarness(t, testZone(),
		gatewaySet(dnsv1alpha1.RRTypeA, aEntry("edge", "203.0.113.1", ttl(300))))

	path := zoneFilePath(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.10\n")
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--prune", "--yes"))

	set := h.getSet(t, testZoneObject+"-a")
	if len(set.Spec.Records) != 1 || set.Spec.Records[0].Name != "edge" {
		t.Errorf("a Gateway-owned set was modified: %+v", set.Spec.Records)
	}
	mustContain(t, h.stderr(), "AI Edge")
	mustContain(t, h.stderr(), "edge-gw")
}

// --- confirmation -----------------------------------------------------------

// A prune that would delete records must refuse to run unattended: nothing
// recovers a deleted RRset.
func TestApplyPruneRefusesNonInteractively(t *testing.T) {
	t.Setenv("CI", "1")
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA,
		aEntry("www", "203.0.113.10", ttl(300)),
		aEntry("old", "198.51.100.1", ttl(300)),
	))

	path := zoneFilePath(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.10\n")
	err := h.run("record", "apply", testDomain, "-f", path, "--prune")
	ce := requireExit(t, err, util.ExitAborted)
	mustContain(t, ce.Error(), "refusing to delete")
	mustContain(t, ce.Fix(), "--yes")

	set := h.getSet(t, testZoneObject+"-a")
	if len(set.Spec.Records) != 2 {
		t.Error("the refused prune still modified the zone")
	}
}

// A non-destructive apply proceeds unattended, matching the record-delete tier.
func TestApplyAdditiveProceedsNonInteractively(t *testing.T) {
	t.Setenv("CI", "1")
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))

	path := zoneFilePath(t, `$ORIGIN example.com.
www 300 IN A 203.0.113.10
api 300 IN A 203.0.113.20
`)
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path))

	set := h.getSet(t, testZoneObject+"-a")
	if len(set.Spec.Records) != 2 {
		t.Errorf("the additive apply did not run: %+v", set.Spec.Records)
	}
}

func TestApplyDeclinedAtThePrompt(t *testing.T) {
	interactive(t)
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))
	h.answer("n\n")

	path := zoneFilePath(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.11\n")
	_ = requireExit(t, h.run("record", "apply", testDomain, "-f", path), util.ExitAborted)

	set := h.getSet(t, testZoneObject+"-a")
	if len(set.Spec.Records) != 1 {
		t.Error("a declined apply still modified the zone")
	}
}

// --- validation and reporting ------------------------------------------------

func TestApplyDryRunWritesNothing(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))

	path := zoneFilePath(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.11\n")
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--dry-run"))

	mustContain(t, h.stdout(), "Dry run")
	set := h.getSet(t, testZoneObject+"-a")
	if len(set.Spec.Records) != 1 {
		t.Errorf("--dry-run wrote to the API: %+v", set.Spec.Records)
	}
}

func TestApplyRefusesAMalformedFile(t *testing.T) {
	h := newHarness(t, testZone())

	path := zoneFilePath(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.10\napi 300 IN A nonsense\n")
	ce := requireExit(t, h.run("record", "apply", testDomain, "-f", path, "--yes"), util.ExitUsage)
	mustContain(t, ce.Error(), "line 3")
}

// The API admits an entry whose typed field does not match its record type, so
// nothing at all may be written until the whole file has been checked.
func TestApplyValidatesBeforeWriting(t *testing.T) {
	h := newHarness(t, testZone())

	// The A record is valid and the NS target is not: underscores are legal in
	// a CNAME or SRV target but not in a host name, so this is a file the API
	// would admit and the backend would then drop.
	path := zoneFilePath(t, `$ORIGIN example.com.
www 300 IN A  203.0.113.10
dev 300 IN NS _under_score_.datum.net.
`)
	err := h.run("record", "apply", testDomain, "-f", path, "--yes")
	_ = requireExit(t, err, util.ExitUsage)

	if !h.setMissing(t, testZoneObject+"-a") {
		t.Error("a file with one invalid record still wrote its valid ones")
	}
}

func TestApplyReportsUnsupportedTypes(t *testing.T) {
	h := newHarness(t, testZone())

	path := zoneFilePath(t, `$ORIGIN example.com.
www 300 IN A  203.0.113.10
@   300 IN DS 12345 8 2 abcdef
`)
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--yes"))
	mustContain(t, h.stderr(), "DS")
	mustContain(t, h.stderr(), "line 3")
}

func TestApplyCreatesAMissingType(t *testing.T) {
	h := newHarness(t, testZone())

	path := zoneFilePath(t, "$ORIGIN example.com.\n@ 3600 IN MX 10 mail.example.com.\n")
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--yes"))

	set := h.getSet(t, testZoneObject+"-mx")
	if len(set.Spec.Records) != 1 || set.Spec.Records[0].MX.Preference != 10 {
		t.Errorf("the MX set was not created correctly: %+v", set.Spec.Records)
	}
}

// TXT is stored in presentation form, so an unchanged TXT record must not read
// back as a change on the next apply.
func TestApplyTXTRoundTrips(t *testing.T) {
	h := newHarness(t, testZone())

	path := zoneFilePath(t,
		"$ORIGIN example.com.\n@ 300 IN TXT \"v=spf1 include:_spf.example.com ~all\"\n")
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--yes"))

	stored := h.getSet(t, testZoneObject+"-txt").Spec.Records[0].TXT.Content
	if !strings.HasPrefix(stored, `"`) {
		t.Errorf("TXT content = %q, want it stored already quoted", stored)
	}

	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--yes"))
	mustContain(t, h.stdout(), "No changes.")
}

func TestApplyAppliesTheZonesOwnExport(t *testing.T) {
	// The closed loop the design promises: export, apply, no diff. The SOA and
	// NS sets are present so the platform-managed path is exercised too.
	h := newHarness(t, testZone(), soaSet(), apexNSSet(),
		recordSet(dnsv1alpha1.RRTypeA,
			aEntry("@", "203.0.113.10", ttl(300)),
			aEntry("www", "203.0.113.11", ttl(300)),
		),
		recordSet(dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RecordEntry{
			Name: "_dmarc", TTL: ttl(300),
			TXT: &dnsv1alpha1.TXTRecordSpec{Content: `"v=DMARC1\; p=none"`},
		}),
	)

	// Hand-written to match what zone export emits for this fixture.
	path := zoneFilePath(t, `$ORIGIN example.com.
$TTL 300

@      3600 IN SOA ns1.datum.net. hostmaster.example.com. 1 10800 3600 604800 3600
@      3600 IN NS  ns1.datum.net.
@      3600 IN NS  ns2.datum.net.
@      300  IN A   203.0.113.10
www    300  IN A   203.0.113.11
_dmarc 300  IN TXT "v=DMARC1; p=none"
`)
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--prune", "--yes"))
	mustContain(t, h.stdout(), "No changes.")
}

func TestApplyUnknownZone(t *testing.T) {
	h := newHarness(t, testZone())
	path := zoneFilePath(t, "www 300 IN A 203.0.113.10\n")
	err := h.run("record", "apply", "nope.example", "-f", path, "--yes")
	_ = requireExit(t, err, util.ExitNotFound)
}

// A never-transitioned condition must not confuse the apply path; the fixture
// exists so the epoch default does not creep into a diff.
func TestApplyIgnoresConditionNoise(t *testing.T) {
	set := recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300)))
	set.Status.Conditions = []metav1.Condition{{
		Type: "Programmed", Status: metav1.ConditionUnknown, Reason: "Pending",
		LastTransitionTime: metav1.Unix(0, 0),
	}}
	h := newHarness(t, testZone(), set)

	path := zoneFilePath(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.10\n")
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--yes"))
	mustContain(t, h.stdout(), "No changes.")
}

// --- hazards rdata exists to catch -------------------------------------------

// A TXT record stored in chunked presentation form must read back as its one
// logical value, or apply shows a spurious diff on every run.
func TestApplyChunkedTXTShowsNoSpuriousDiff(t *testing.T) {
	key := "v=DKIM1; k=rsa; p=" + strings.Repeat("M", 400)
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RecordEntry{
		Name: "sel._domainkey", TTL: ttl(300),
		TXT: rdata.EntryForAPI(dnsv1alpha1.RRTypeTXT,
			dnsv1alpha1.RecordEntry{TXT: &dnsv1alpha1.TXTRecordSpec{Content: key}}).TXT,
	}))

	path := zoneFilePath(t, "$ORIGIN example.com.\nsel._domainkey 300 IN TXT \""+key+"\"\n")
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--prune", "--yes"))
	mustContain(t, h.stdout(), "No changes.")
}

// An out-of-zone owner name fails the whole PATCH at the backend, and the PATCH
// carries every owner name in the bucket. Nothing may be written until the file
// has been checked.
func TestApplyRejectsAnOutOfZoneName(t *testing.T) {
	h := newHarness(t, testZone())

	path := zoneFilePath(t, `$ORIGIN example.com.
www                300 IN A 203.0.113.10
other.example.net. 300 IN A 203.0.113.99
`)
	err := h.run("record", "apply", testDomain, "-f", path, "--yes")
	_ = requireExit(t, err, util.ExitUsage)
	mustContain(t, err.Error(), "line 3")

	if !h.setMissing(t, testZoneObject+"-a") {
		t.Error("an out-of-zone name in the file still wrote the rest of its type")
	}
}

// Only the whole-slice check catches a two-value CNAME; the backend keeps the
// first and drops the rest without a condition.
func TestApplyRejectsAMultiValueCNAME(t *testing.T) {
	h := newHarness(t, testZone())

	path := zoneFilePath(t, `$ORIGIN example.com.
www 300 IN CNAME one.example.net.
www 300 IN CNAME two.example.net.
`)
	// Bad input is a usage error, and it is caught before the file reaches the
	// API rather than after a write has already been attempted.
	err := h.run("record", "apply", testDomain, "-f", path, "--yes")
	ce := requireExit(t, err, util.ExitUsage)
	mustContain(t, ce.Error(), "single-valued")
	if !h.setMissing(t, testZoneObject+"-cname") {
		t.Error("a two-value CNAME set was written")
	}
}

// The whole file is parsed and validated before the first API call, so a broken
// file fails as a line-numbered usage error rather than as whatever the API
// happens to say first. Proved by pointing at a zone that does not exist: the
// file's error must win over the not-found.
func TestApplyValidatesBeforeTouchingTheAPI(t *testing.T) {
	h := newHarness(t, testZone())

	path := zoneFilePath(t, `$ORIGIN nope.example.
www 300 IN A 203.0.113.10
api 300 IN A nonsense
`)
	err := h.run("record", "apply", "nope.example", "-f", path, "--yes")
	ce := requireExit(t, err, util.ExitUsage)
	mustContain(t, ce.Error(), "line 3")
	mustNotContain(t, ce.Error(), "not found")
}

// An undotted positional is a DNSZone object name, not a domain, so
// zone-relative rules cannot run against it before the zone is resolved — but
// syntax and rdata errors still must.
func TestApplyPreflightHandlesAnObjectNamePositional(t *testing.T) {
	h := newHarness(t, testZone())

	path := zoneFilePath(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.10\n")
	requireNoError(t, h.run("record", "apply", testZoneObject, "-f", path, "--yes"))

	set := h.getSet(t, testZoneObject+"-a")
	if len(set.Spec.Records) != 1 || set.Spec.Records[0].Name != "www" {
		t.Errorf("apply by object name did not write the record: %+v", set.Spec.Records)
	}
}

// The backend applies the first entry's TTL to a whole owner name and drops the
// rest, so an apply that merges a second TTL onto one owner has to say so.
func TestApplyWarnsOnConflictingTTLs(t *testing.T) {
	h := newHarness(t, testZone(),
		recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))

	path := zoneFilePath(t, "$ORIGIN example.com.\nwww 900 IN A 203.0.113.11\n")
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--yes"))
	mustContain(t, h.stderr(), "TTL")
}

// --- owner-name identity ------------------------------------------------------

// pdns.QualifyOwner keys an RRset on the QUALIFIED owner name, so "www" and
// "www.example.com." are one owner and "@", "" and "example.com." are one
// owner. The CRD's name pattern admits all of them. Comparing the literal
// strings sees two owners where the backend sees one, and the diff then lies:
// an add for a record that already exists, and under --prune a delete for the
// record it is about to re-add.
//
// The zone package had the same defect and it was found by review there; these
// pin the apply half so it cannot come back on either side.
func TestApplyMatchesAnAbsolutelySpelledOwner(t *testing.T) {
	h := newHarness(t, testZone(),
		recordSet(dnsv1alpha1.RRTypeA, aEntry("www.example.com.", "203.0.113.10", ttl(300))))

	path := zoneFilePath(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.10\n")
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--prune", "--yes"))
	mustContain(t, h.stdout(), "No changes.")

	set := h.getSet(t, testZoneObject+"-a")
	if len(set.Spec.Records) != 1 {
		t.Errorf("the A set holds %d entries, want 1 — the same record was counted twice", len(set.Spec.Records))
	}
}

// The apex spelled as the bare domain is still the apex.
func TestApplyMatchesAnAbsolutelySpelledApex(t *testing.T) {
	h := newHarness(t, testZone(),
		recordSet(dnsv1alpha1.RRTypeA, aEntry("example.com.", "203.0.113.10", ttl(300))))

	path := zoneFilePath(t, "$ORIGIN example.com.\n@ 300 IN A 203.0.113.10\n")
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--prune", "--yes"))
	mustContain(t, h.stdout(), "No changes.")
}

// The dangerous shape: --prune against a zone whose platform apex NS records
// are stored absolutely. A literal comparison does not recognise them as the
// platform's, so they fall out of the keep set and get pruned.
func TestApplyPruneKeepsPlatformNSSpelledAbsolutely(t *testing.T) {
	ns := recordSet(dnsv1alpha1.RRTypeNS,
		dnsv1alpha1.RecordEntry{
			Name: "example.com.", TTL: ttl(3600),
			NS: &dnsv1alpha1.NSRecordSpec{Content: "ns1.datum.net."},
		},
		dnsv1alpha1.RecordEntry{
			Name: "example.com.", TTL: ttl(3600),
			NS: &dnsv1alpha1.NSRecordSpec{Content: "ns2.datum.net."},
		},
	)
	h := newHarness(t, testZone(), ns)

	// A file that mentions no NS records at all, which is what --prune acts on.
	path := zoneFilePath(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.10\n")
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--prune", "--yes"))

	live := h.getSet(t, testZoneObject+"-ns")
	if len(live.Spec.Records) != 2 {
		t.Errorf("the zone's apex NS records are now %+v, want both Datum nameservers — "+
			"--prune dropped them because they were spelled %q rather than \"@\"",
			live.Spec.Records, "example.com.")
	}
}

// apexNSSetSpelledOut is the zone's delegation with its owner stored fully
// qualified rather than as "@". The backend qualifies both onto one RRset, so
// this is the same delegation — but a literal IsApex test does not see it.
func apexNSSetSpelledOut() *dnsv1alpha1.DNSRecordSet {
	return recordSet(dnsv1alpha1.RRTypeNS,
		dnsv1alpha1.RecordEntry{Name: testDomain + ".", TTL: ttl(3600), NS: &dnsv1alpha1.NSRecordSpec{Content: "ns1.datum.net."}},
		dnsv1alpha1.RecordEntry{Name: testDomain + ".", TTL: ttl(3600), NS: &dnsv1alpha1.NSRecordSpec{Content: "ns2.datum.net."}},
	)
}

// TestPruneProtectsPlatformRecordsWhateverTheirSpellingOrObjectName.
//
// Every case here is a zone that create, set and delete all refuse to touch.
// apply is the only verb that turns the same question into a DELETE, so it is
// the only one where getting it wrong costs a zone its delegation rather than a
// label — and each of these was doing exactly that, at exit 0, with no warning.
//
// To confirm these are mutation-verified rather than merely passing: revert
// isPlatformShape to the old `rs.Name == zoneObjName+"-soa"` /
// `rdata.IsApex(entry.Name)` form and each subtest fails with the platform
// records gone from the fake API server.
func TestPruneProtectsPlatformRecordsWhateverTheirSpellingOrObjectName(t *testing.T) {
	tests := []struct {
		name    string
		seed    *dnsv1alpha1.DNSRecordSet
		setName string
		// records is how many entries must survive; 0 means the object itself
		// must survive with its contents untouched.
		wantEntries int
	}{
		{
			name:        "apex NS stored as the fully qualified name",
			seed:        apexNSSetSpelledOut(),
			setName:     testZoneObject + "-ns",
			wantEntries: 2,
		},
		{
			name: "apex NS in an object the user named",
			seed: func() *dnsv1alpha1.DNSRecordSet {
				rs := apexNSSet()
				rs.Name = "my-ns-records"
				return rs
			}(),
			setName:     "my-ns-records",
			wantEntries: 2,
		},
		{
			name: "SOA in an object the user named",
			seed: func() *dnsv1alpha1.DNSRecordSet {
				rs := soaSet()
				rs.Name = "my-soa"
				return rs
			}(),
			setName:     "my-soa",
			wantEntries: 1,
		},
		{
			name: "apex NS spelled out AND in an object the user named",
			seed: func() *dnsv1alpha1.DNSRecordSet {
				rs := apexNSSetSpelledOut()
				rs.Name = "delegation"
				return rs
			}(),
			setName:     "delegation",
			wantEntries: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, testZone(), tc.seed,
				recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))

			// A trimmed export: the file carries the A record and nothing else.
			path := zoneFilePath(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.10\n")
			requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--prune", "--yes"))

			if h.setMissing(t, tc.setName) {
				t.Fatalf("--prune deleted %s — the zone's platform records are gone", tc.setName)
			}
			if got := len(h.getSet(t, tc.setName).Spec.Records); got != tc.wantEntries {
				t.Errorf("%s has %d entries, want %d untouched", tc.setName, got, tc.wantEntries)
			}
			mustNotContain(t, h.stdout(), "- example.com. NS")
			mustNotContain(t, h.stdout(), "- @ NS")
		})
	}
}

// TestFourVerbsAgreeOnOnePlatformRecord — the review's framing was that apply
// was the only verb whose protection was set-based while the other three were
// shape-based, so one zone state produced four different answers. This asserts
// they now agree.
func TestFourVerbsAgreeOnOnePlatformRecord(t *testing.T) {
	seed := func() *dnsv1alpha1.DNSRecordSet {
		rs := apexNSSetSpelledOut()
		rs.Name = "delegation"
		return rs
	}

	t.Run("delete refuses", func(t *testing.T) {
		interactive(t)
		h := newHarness(t, testZone(), seed())
		_ = requireExit(t, h.run("record", "delete", testDomain, "@", "NS", "--yes"), util.ExitUsage)
	})
	t.Run("set refuses", func(t *testing.T) {
		h := newHarness(t, testZone(), seed())
		_ = requireExit(t, h.run("record", "set", testDomain, "@", "NS", "ns9.datum.net."), util.ExitUsage)
	})
	t.Run("create refuses", func(t *testing.T) {
		h := newHarness(t, testZone(), seed())
		_ = requireExit(t, h.run("record", "create", testDomain, "@", "NS", "ns9.datum.net."), util.ExitUsage)
	})
	t.Run("apply --prune preserves", func(t *testing.T) {
		h := newHarness(t, testZone(), seed())
		path := zoneFilePath(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.10\n")
		requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--prune", "--yes"))
		if h.setMissing(t, "delegation") {
			t.Error("apply --prune deleted the delegation the other three verbs protect")
		}
	})
	t.Run("list marks it", func(t *testing.T) {
		h := newHarness(t, testZone(), seed())
		requireNoError(t, h.run("record", "list", testDomain))
		mustContain(t, h.stdout(), markerPlatform)
	})
}

// TestConvergeRevalidatesOnRetry.
//
// The retry recomputes the result from fresh state, so validating the
// prefetched computation checks a slice that is then discarded. Here the file
// touches one owner in the CNAME bucket while a concurrent writer adds a second
// CNAME at a DIFFERENT owner in the same bucket — invalid, RFC 1034 forbids it
// and PowerDNS 422s the whole set, and entirely invisible to the prefetched
// computation because that entry did not exist when the plan was made.
//
// Mutation check: move the ValidateEntriesInZone call back out of the closure
// and onto tp.next, and this writes the two-value set at exit 0.
func TestConvergeRevalidatesOnRetry(t *testing.T) {
	var patches int
	racer := dnsv1alpha1.RecordEntry{Name: "cdn", TTL: ttl(300), CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "racer.example.net."}}

	ic := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patches++
			if patches > 1 {
				return c.Patch(ctx, obj, patch, opts...)
			}
			var live dnsv1alpha1.DNSRecordSet
			if err := c.Get(ctx, client.ObjectKeyFromObject(obj), &live); err != nil {
				return err
			}
			live.Spec.Records = append(live.Spec.Records, racer)
			if err := c.Update(ctx, &live); err != nil {
				return err
			}
			return apierrors.NewConflict(
				schema.GroupResource{Group: "dns.networking.miloapis.com", Resource: "dnsrecordsets"},
				obj.GetName(), errOptimisticLock)
		},
	}

	h := newHarnessWithInterceptor(t, &ic, testZone(),
		recordSet(dnsv1alpha1.RRTypeCNAME, dnsv1alpha1.RecordEntry{
			Name: "cdn", TTL: ttl(300), CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "old.example.net."},
		}))

	// The file names a different owner in the same bucket, so cdn's entries are
	// carried through as editable neighbours rather than replaced.
	path := zoneFilePath(t, "$ORIGIN example.com.\nshop 300 IN CNAME shops.example.net.\n")
	err := h.run("record", "apply", testDomain, "-f", path, "--yes")

	// The racer's own Update is in the store; what matters is that OUR write was
	// refused rather than layered on top of it, so the file's entry never lands.
	stored := h.getSet(t, testZoneObject+"-cname").Spec.Records
	for _, e := range stored {
		if sameOwner(e.Name, "shop", testDomain) {
			t.Fatalf("the invalid recomputation was written: %+v", stored)
		}
	}
	if err == nil {
		t.Fatal("an invalid recomputation was not reported; a refused write must not exit 0")
	}
	mustContain(t, util.ClassifyError(err).Error(), "single-valued")
}

// TestApplyRepointsASingleValuedRecord.
//
// Without --prune, a file entry for a single-valued type replaces whatever is at
// that owner rather than joining it. Matching by value appended instead, and
// `record apply -f` on a repointed CNAME failed with "2 values but is
// single-valued" — no concurrency needed, and the most ordinary thing anyone
// would do with a zone file.
func TestApplyRepointsASingleValuedRecord(t *testing.T) {
	h := newHarness(t, testZone(),
		recordSet(dnsv1alpha1.RRTypeCNAME, dnsv1alpha1.RecordEntry{
			Name: "cdn", TTL: ttl(300), CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "old.example.net."},
		}))

	path := zoneFilePath(t, "$ORIGIN example.com.\ncdn 300 IN CNAME new.example.net.\n")
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--yes"))

	stored := h.getSet(t, testZoneObject+"-cname").Spec.Records
	if len(stored) != 1 || stored[0].CNAME.Content != "new.example.net." {
		t.Errorf("records = %+v, want the single new value", stored)
	}
}

// TestApplyWithholdsThePlatformRecordsInAProviderExport.
//
// The migration path: a zone file exported from the previous provider carries
// that provider's SOA and apex NS records, applied to a Datum zone the operator
// has not provisioned yet. With nothing live to compare against, the owner-name
// test has no protected entry to match, so the shape test is the only thing
// standing between the user and a zone permanently delegated to the nameservers
// they were migrating away from — ensureNSRecordSet and ensureSOARecordSet both
// skip once any set of the type exists, so the operator never corrects it.
//
// Grown from a probe left on the package by the review. Mutation check: drop the
// protectedEntry clause from resolve's file filter and both sets are created.
func TestApplyWithholdsThePlatformRecordsInAProviderExport(t *testing.T) {
	h := newHarness(t, testZone())
	path := zoneFilePath(t, `$ORIGIN example.com.
$TTL 3600
@ IN SOA ns1.oldprovider.net. hostmaster.oldprovider.net. 1 10800 3600 604800 3600
@ IN NS  ns1.oldprovider.net.
@ IN NS  ns2.oldprovider.net.
www 300 IN A 203.0.113.10
`)
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--yes"))

	for _, name := range []string{testZoneObject + "-soa", testZoneObject + "-ns"} {
		if !h.setMissing(t, name) {
			t.Errorf("%s was created from the file: %+v", name, h.getSet(t, name).Spec.Records)
		}
	}

	// The record that is genuinely the user's still lands.
	if got := h.getSet(t, testZoneObject+"-a").Spec.Records; len(got) != 1 {
		t.Errorf("the A record did not apply: %+v", got)
	}

	// And the user is told, with a reason rather than an empty "()".
	errOut := h.stderr()
	mustContain(t, errOut, "3 changes in the file were not applied")
	mustContain(t, errOut, "ns1.oldprovider.net.")
	mustContain(t, errOut, "the zone's SOA record")
	mustContain(t, errOut, "the zone's apex NS records")
	mustNotContain(t, errOut, "()")
}

// TestApplyWithholdsThemUnderPruneToo — --prune is the case where getting this
// wrong also deletes, so it gets its own assertion rather than riding on the
// default path.
func TestApplyWithholdsThemUnderPruneToo(t *testing.T) {
	h := newHarness(t, testZone(), soaSet(), apexNSSetSpelledOut())
	path := zoneFilePath(t, `$ORIGIN example.com.
$TTL 3600
@ IN SOA ns1.oldprovider.net. hostmaster.oldprovider.net. 1 10800 3600 604800 3600
@ IN NS  ns1.oldprovider.net.
www 300 IN A 203.0.113.10
`)
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--prune", "--yes"))

	soa := h.getSet(t, testZoneObject+"-soa")
	if soa.Spec.Records[0].SOA.MName != "ns1.datum.net." {
		t.Errorf("the zone's SOA was overwritten from the file: %+v", soa.Spec.Records[0].SOA)
	}
	ns := h.getSet(t, testZoneObject+"-ns")
	if len(ns.Spec.Records) != 2 {
		t.Errorf("the delegation changed: %+v", ns.Spec.Records)
	}
	for _, e := range ns.Spec.Records {
		if e.NS.Content == "ns1.oldprovider.net." {
			t.Error("the old provider's nameserver joined the delegation")
		}
	}
}

// Applying a provider export to a zone the operator has not finished
// provisioning must not create its SOA or apex NS from the old provider's
// records.
//
// The keep list protects entries that are live and protected, which is nothing
// at all before the operator has created <zone>-soa and <zone>-ns — it does
// that only once the zone's nameservers are assigned. So the owner-name test
// had no protected entry to match, the file's records went through, and the
// imported SOA would land under exactly the name the operator later looks for,
// making it the zone's SOA permanently. The file side is now tested by shape as
// well, which is the same closure `zone import` uses.
func TestApplyDoesNotCreatePlatformSetsFromAFile(t *testing.T) {
	h := newHarness(t, testZone())

	path := zoneFilePath(t, `$ORIGIN example.com.
$TTL 3600
@ IN SOA ns1.oldprovider.net. hostmaster.oldprovider.net. 1 10800 3600 604800 3600
@ IN NS  ns1.oldprovider.net.
@ IN NS  ns2.oldprovider.net.
www 300 IN A 203.0.113.10
`)
	requireNoError(t, h.run("record", "apply", testDomain, "-f", path, "--yes"))

	if !h.setMissing(t, testZoneObject+"-soa") {
		t.Error("apply created the zone's SOA set from the old provider's record")
	}
	if !h.setMissing(t, testZoneObject+"-ns") {
		t.Error("apply created the zone's apex NS records from the old provider's")
	}
	if h.setMissing(t, testZoneObject+"-a") {
		t.Error("the ordinary record was not applied")
	}

	// And the user is told, with a reason — a nil set is not "no reason".
	mustContain(t, h.stderr(), "belong to the platform")
	mustContain(t, h.stderr(), "the zone's SOA record")
	mustContain(t, h.stderr(), "the zone's apex NS records")
}
