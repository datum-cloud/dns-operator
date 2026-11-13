// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// TestCreateAppendsToTheRRset — create keeps what is already at the name. This
// and TestSetReplacesEveryValue are the distinction the two verbs exist for.
func TestCreateAppendsToTheRRset(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))
	requireNoError(t, h.run("record", "create", testDomain, "www", "A", "203.0.113.11"))

	got := h.getSet(t, "example-com-a").Spec.Records
	if len(got) != 2 {
		t.Fatalf("records = %d, want 2 (create appends): %+v", len(got), got)
	}
	if got[0].A.Content != "203.0.113.10" || got[1].A.Content != "203.0.113.11" {
		t.Errorf("records = %v, %v", got[0].A.Content, got[1].A.Content)
	}
	mustContain(t, h.stdout(), "  record/example.com A www created")
	mustContain(t, collapse(h.stdout()), "www 5m IN A 203.0.113.11")
}

func TestSetReplacesEveryValueAtTheName(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA,
		aEntry("www", "203.0.113.10", ttl(300)),
		aEntry("www", "203.0.113.11", ttl(300)),
		aEntry("api", "203.0.113.12", ttl(60)),
	))
	requireNoError(t, h.run("record", "set", testDomain, "www", "A", "203.0.113.20"))

	got := h.getSet(t, "example-com-a").Spec.Records
	if len(got) != 2 {
		t.Fatalf("records = %d, want 2 (api survives, www collapses to one): %+v", len(got), got)
	}
	if got[0].Name != "api" || got[0].A.Content != "203.0.113.12" {
		t.Errorf("other owner names must be untouched, got %+v", got[0])
	}
	if got[1].Name != "www" || got[1].A.Content != "203.0.113.20" {
		t.Errorf("www = %+v, want the single replacement value", got[1])
	}
	mustContain(t, h.stdout(), "  record/example.com A www updated")
}

func TestSetOnAnUnusedNameReportsCreated(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("api", "203.0.113.12", nil)))
	requireNoError(t, h.run("record", "set", testDomain, "www", "A", "203.0.113.20"))
	mustContain(t, h.stdout(), "  record/example.com A www created")
}

// TestCreateRejectsAnExactDuplicate — PowerDNS rejects a whole record set when
// a value repeats, so an append that would duplicate is refused here.
func TestCreateRejectsAnExactDuplicate(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))
	ce := requireExit(t, h.run("record", "create", testDomain, "www", "A", "203.0.113.10"), util.ExitConflict)

	mustContain(t, ce.Error(), `www.example.com already has the A value "203.0.113.10"`)
	mustContain(t, ce.Fix(), "dns record set")
	if got := len(h.getSet(t, "example-com-a").Spec.Records); got != 1 {
		t.Errorf("records = %d, want 1 — nothing should have been written", got)
	}
}

// TestCreateSingleValuedTypeRefusesASecondValue — a name may have exactly one
// CNAME, and the backend would silently keep the first.
func TestCreateSingleValuedTypeRefusesASecondValue(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeCNAME, dnsv1alpha1.RecordEntry{
		Name: "cdn", CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "lb.example.net."},
	}))
	ce := requireExit(t, h.run("record", "create", testDomain, "cdn", "CNAME", "other.example.net."), util.ExitUsage)
	mustContain(t, ce.Error(), "single-valued")
	mustContain(t, ce.Fix(), "exactly one CNAME")
}

// TestCreateBuildsTheBucketWhenNoneExists — the (zone, type) object is created
// under the name the operator and the portal both use.
func TestCreateBuildsTheBucketWhenNoneExists(t *testing.T) {
	h := newHarness(t, testZone())
	requireNoError(t, h.run("record", "create", testDomain, "www", "A", "203.0.113.10", "--ttl", "5m"))

	rs := h.getSet(t, "example-com-a")
	if rs.Spec.DNSZoneRef.Name != testZoneObject {
		t.Errorf("dnsZoneRef = %q, want %q", rs.Spec.DNSZoneRef.Name, testZoneObject)
	}
	if rs.Spec.RecordType != dnsv1alpha1.RRTypeA {
		t.Errorf("recordType = %q, want A", rs.Spec.RecordType)
	}
	if len(rs.Spec.Records) != 1 || rs.Spec.Records[0].Name != "www" {
		t.Fatalf("records = %+v", rs.Spec.Records)
	}
	if rs.Spec.Records[0].TTL == nil || *rs.Spec.Records[0].TTL != 300 {
		t.Errorf("ttl = %v, want 300 (--ttl 5m)", rs.Spec.Records[0].TTL)
	}
}

// TestCreateInheritsTheNamesExistingTTL — TTL is per-RRset in DNS but per-entry
// in the API, and the backend applies the first entry's to the whole set. A new
// value written with a nil TTL beside a 3600 would report Auto and resolve 3600.
func TestCreateInheritsTheNamesExistingTTL(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(3600))))
	requireNoError(t, h.run("record", "create", testDomain, "www", "A", "203.0.113.11"))

	got := h.getSet(t, "example-com-a").Spec.Records
	if got[1].TTL == nil || *got[1].TTL != 3600 {
		t.Errorf("appended TTL = %v, want the name's existing 3600", got[1].TTL)
	}
	mustContain(t, collapse(h.stdout()), "www 1h IN A 203.0.113.11")
}

// TestCreateTTLReachesEveryValueAtTheName — an explicit --ttl that only landed
// on the new entry would appear to have been ignored, because the backend reads
// the first entry's.
func TestCreateTTLReachesEveryValueAtTheName(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA,
		aEntry("www", "203.0.113.10", ttl(3600)),
		aEntry("api", "203.0.113.12", ttl(3600)),
	))
	requireNoError(t, h.run("record", "create", testDomain, "www", "A", "203.0.113.11", "--ttl", "60"))

	for _, e := range h.getSet(t, "example-com-a").Spec.Records {
		want := int64(3600)
		if e.Name == "www" {
			want = 60
		}
		if e.TTL == nil || *e.TTL != want {
			t.Errorf("%s %s ttl = %v, want %d", e.Name, e.A.Content, e.TTL, want)
		}
	}
}

// TestCreateStructuredTypeByFlags echoes in presentation format, which is the
// notation the input was not in.
func TestCreateStructuredTypeByFlags(t *testing.T) {
	h := newHarness(t, testZone())
	requireNoError(t, h.run("record", "create", testDomain, "@", "MX",
		"--preference", "10", "--exchange", "mail.example.com.", "--ttl", "300"))

	rs := h.getSet(t, "example-com-mx")
	if rs.Spec.Records[0].MX.Preference != 10 || rs.Spec.Records[0].MX.Exchange != "mail.example.com." {
		t.Fatalf("mx = %+v", rs.Spec.Records[0].MX)
	}
	out := h.stdout()
	mustContain(t, out, "  record/example.com MX @ created")
	mustContain(t, collapse(out), "@ 5m IN MX 10 mail.example.com.")
	mustNotContain(t, out, "Preference:")
}

// TestCreateStructuredTypePositionallyTeachesTheFields is the same rule in the
// other direction: a value pasted in presentation format is echoed back with
// the named fields the flags use.
func TestCreateStructuredTypePositionallyTeachesTheFields(t *testing.T) {
	h := newHarness(t, testZone())
	requireNoError(t, h.run("record", "create", testDomain, "_sip._tcp", "SRV", "10 5 5060 sip.example.com."))

	out := h.stdout()
	mustContain(t, collapse(out), "_sip._tcp Auto IN SRV 10 5 5060 sip.example.com.")
	mustContain(t, out, "Priority:")
	mustContain(t, out, "Port:")
}

// TestMixingNotationsIsAUsageError — a merge would have to guess which value
// the user meant.
func TestMixingNotationsIsAUsageError(t *testing.T) {
	h := newHarness(t, testZone())
	ce := requireExit(t, h.run("record", "create", testDomain, "@", "MX",
		"10 mail.example.com.", "--preference", "20"), util.ExitUsage)
	mustContain(t, ce.Error(), "both positionally and as named flags")
}

// TestFlagsFromTheWrongTypeAreRejected — the union flag set is what makes the
// dynamic registration work, so it has to be policed after parsing.
func TestFlagsFromTheWrongTypeAreRejected(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantMsg string
		wantFix string
	}{
		{
			name:    "structured flag on a flat type",
			args:    []string{"record", "create", testDomain, "www", "A", "--preference", "10"},
			wantMsg: "--preference is not a flag for A records",
			wantFix: "A records take their value positionally",
		},
		{
			name:    "several at once",
			args:    []string{"record", "create", testDomain, "@", "MX", "--priority", "1", "--weight", "2"},
			wantMsg: "--priority, --weight are not flags for MX records",
			wantFix: "MX records take --preference, --exchange.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, testZone())
			ce := requireExit(t, h.run(tc.args...), util.ExitUsage)
			mustContain(t, ce.Error(), tc.wantMsg)
			mustContain(t, ce.Fix(), tc.wantFix)
		})
	}
}

// TestClientSideValidationGate is the plugin's main reason to exist: the API
// server admits these and the backend then skips them without a condition.
func TestClientSideValidationGate(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantMsg string
		wantFix string
	}{
		{
			name:    "an address that is not one",
			args:    []string{"www", "A", "not-an-ip"},
			wantMsg: `"not-an-ip" is not a valid IPv4 address`,
		},
		{
			name:    "an IPv6 address under A",
			args:    []string{"www", "A", "2001:db8::1"},
			wantMsg: "is not a valid IPv4 address",
		},
		{
			name:    "a target without its trailing dot",
			args:    []string{"@", "MX", "--preference", "10", "--exchange", "mail"},
			wantMsg: "not a fully qualified domain name",
			wantFix: "mail.example.com.",
		},
		{
			name:    "a name that already spells out the zone",
			args:    []string{"www.example.com", "A", "203.0.113.10"},
			wantMsg: "already includes the zone domain",
			wantFix: `"www"`,
		},
		{
			name:    "the wrong arity in presentation format",
			args:    []string{"@", "MX", "mail.example.com."},
			wantMsg: "MX",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, testZone())
			ce := requireExit(t, h.run(append([]string{"record", "create", testDomain}, tc.args...)...), util.ExitUsage)
			mustContain(t, ce.Error(), tc.wantMsg)
			if tc.wantFix != "" {
				mustContain(t, ce.Fix(), tc.wantFix)
			}
			if !h.setMissing(t, "example-com-a") || !h.setMissing(t, "example-com-mx") {
				t.Errorf("a rejected record must not reach the API server")
			}
		})
	}
}

func TestCreateFromAWholeLine(t *testing.T) {
	h := newHarness(t, testZone())
	requireNoError(t, h.run("record", "create", testDomain, "--line", "www 300 IN A 203.0.113.10"))

	rs := h.getSet(t, "example-com-a")
	e := rs.Spec.Records[0]
	if e.Name != "www" || e.A.Content != "203.0.113.10" || e.TTL == nil || *e.TTL != 300 {
		t.Fatalf("entry = %+v (ttl %v)", e, e.TTL)
	}
	mustContain(t, collapse(h.stdout()), "www 5m IN A 203.0.113.10")
}

func TestLineIsExclusiveWithThePositionalForm(t *testing.T) {
	h := newHarness(t, testZone())
	ce := requireExit(t, h.run("record", "create", testDomain, "www", "A", "203.0.113.10",
		"--line", "www 300 IN A 203.0.113.10"), util.ExitUsage)
	mustContain(t, ce.Error(), "--line carries the whole record")
}

func TestLineIsExclusiveWithNamedFlags(t *testing.T) {
	h := newHarness(t, testZone())
	ce := requireExit(t, h.run("record", "create", testDomain,
		"--line", "@ 300 IN MX 10 mail.example.com.", "--preference", "20"), util.ExitUsage)
	mustContain(t, ce.Error(), "both with --line and as named flags")
}

// TestTXTDataFromStdin — SPF and DKIM values are where shell quoting bites, so
// --data reads a file or a pipe.
func TestTXTDataFromStdin(t *testing.T) {
	h := newHarness(t, testZone())
	h.answer("v=spf1 include:_spf.example.com ~all\n")
	requireNoError(t, h.run("record", "create", testDomain, "@", "TXT", "--data", "-"))

	rs := h.getSet(t, "example-com-txt")
	want := `"v=spf1 include:_spf.example.com ~all"`
	if got := rs.Spec.Records[0].TXT.Content; got != want {
		t.Errorf("txt.content = %q, want %q (quoted for the API)", got, want)
	}
	mustContain(t, h.stdout(), want)
}

func TestTXTDataFromFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/dkim.txt"
	if err := writeFile(path, "v=DKIM1; k=rsa; p=MIGf\n"); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	h := newHarness(t, testZone())
	requireNoError(t, h.run("record", "create", testDomain, "sel._domainkey", "TXT", "--data", "@"+path))
	mustContain(t, h.getSet(t, "example-com-txt").Spec.Records[0].TXT.Content, "v=DKIM1")
}

// TestTXTDuplicateSeesThroughTheStorageEncoding — TXT is stored already quoted,
// so a comparison against freshly parsed input has to canonicalise first.
func TestTXTDuplicateSeesThroughTheStorageEncoding(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RecordEntry{
		Name: "@", TXT: &dnsv1alpha1.TXTRecordSpec{Content: `"v=spf1 -all"`},
	}))
	ce := requireExit(t, h.run("record", "create", testDomain, "@", "TXT", "v=spf1 -all"), util.ExitConflict)
	mustContain(t, ce.Error(), "already has the TXT value")
}

// TestWriteCarriesTheResourceVersionPrecondition is the whole point of the
// read-modify-write: without it, two people editing different names in the same
// type bucket silently overwrite each other.
func TestWriteCarriesTheResourceVersionPrecondition(t *testing.T) {
	var sawPrecondition bool
	ic := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			data, err := patch.Data(obj)
			if err != nil {
				return err
			}
			sawPrecondition = strings.Contains(string(data), `"resourceVersion"`)
			return c.Patch(ctx, obj, patch, opts...)
		},
	}

	h := newHarnessWithInterceptor(t, &ic, testZone(),
		recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))
	requireNoError(t, h.run("record", "create", testDomain, "www", "A", "203.0.113.11"))

	if !sawPrecondition {
		t.Error("the patch did not carry a resourceVersion precondition")
	}
}

// TestConflictRetriesOnceThenSucceeds — the edit is re-applied against fresh
// state rather than the stale object being re-sent.
func TestConflictRetriesOnceThenSucceeds(t *testing.T) {
	var patches int
	ic := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patches++
			if patches == 1 {
				return apierrors.NewConflict(
					schema.GroupResource{Group: "dns.networking.miloapis.com", Resource: "dnsrecordsets"},
					obj.GetName(), errOptimisticLock)
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	}

	h := newHarnessWithInterceptor(t, &ic, testZone(),
		recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))
	requireNoError(t, h.run("record", "create", testDomain, "www", "A", "203.0.113.11"))

	if patches != 2 {
		t.Errorf("patch attempts = %d, want 2 (one conflict, one retry)", patches)
	}
	if got := len(h.getSet(t, "example-com-a").Spec.Records); got != 2 {
		t.Errorf("records = %d, want 2 — the retry must re-apply the edit", got)
	}
}

// TestPersistentConflictReportsItInTheUsersVocabulary — two conflicts mean
// something else is writing continuously, and the user needs to know.
func TestPersistentConflictReportsItInTheUsersVocabulary(t *testing.T) {
	var patches int
	ic := interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patches++
			return apierrors.NewConflict(
				schema.GroupResource{Group: "dns.networking.miloapis.com", Resource: "dnsrecordsets"},
				obj.GetName(), errOptimisticLock)
		},
	}

	h := newHarnessWithInterceptor(t, &ic, testZone(),
		recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))
	ce := requireExit(t, h.run("record", "create", testDomain, "www", "A", "203.0.113.11"), util.ExitConflict)

	if patches != 2 {
		t.Errorf("patch attempts = %d, want 2 — one retry, not a loop", patches)
	}
	if got, want := ce.Error(), "the A records for example.com changed while this command was running"; got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
	mustContain(t, ce.Fix(), "re-run the command — someone else modified the same record type.")
}

// TestDryRunShowsTheDiffAndWritesNothing.
func TestDryRunShowsTheDiffAndWritesNothing(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))
	requireNoError(t, h.run("record", "set", testDomain, "www", "A", "203.0.113.20", "--dry-run"))

	out := collapse(h.stdout())
	mustContain(t, out, "Dry run — no changes were made.")
	mustContain(t, out, "record/example.com A www would be updated")
	mustContain(t, out, "- www 5m IN A 203.0.113.10")
	mustContain(t, out, "+ www 5m IN A 203.0.113.20")

	if got := h.getSet(t, "example-com-a").Spec.Records[0].A.Content; got != "203.0.113.10" {
		t.Errorf("a dry run wrote %q", got)
	}
}

func TestWarningsGoToStderr(t *testing.T) {
	h := newHarness(t, testZone())
	requireNoError(t, h.run("record", "create", testDomain, "@", "CAA",
		"--flag", "0", "--tag", "weird", "--value", "letsencrypt.org"))

	mustContain(t, h.stderr(), "Warning:")
	mustContain(t, h.stderr(), "RFC 8659")
	mustNotContain(t, h.stdout(), "Warning:")
}

func TestCreateWithoutAValueExplainsTheGrammar(t *testing.T) {
	h := newHarness(t, testZone())
	ce := requireExit(t, h.run("record", "create", testDomain, "www", "A"), util.ExitUsage)
	mustContain(t, ce.Error(), "a value is required for a A record")
	mustContain(t, ce.Fix(), "203.0.113.10")
}

func TestCreateMissingArgumentsExplainsTheGrammar(t *testing.T) {
	h := newHarness(t, testZone())
	ce := requireExit(t, h.run("record", "create", testDomain, "www"), util.ExitUsage)
	mustContain(t, ce.Error(), "a name, a type and at least one value are required")
}

// TestArgumentsAreValidatedBeforeTheZoneIsFetched.
//
// A missing zone must not mask a bad argument. The exit code for identical
// malformed input has to be the same whether or not the zone happens to exist,
// or the contract is useless to the scripts it exists for — and a user who
// typo'd both should learn both, not one per round trip.
func TestArgumentsAreValidatedBeforeTheZoneIsFetched(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantMsg string
	}{
		{
			name:    "create with an unknown type",
			args:    []string{"record", "create", testDomain, "www", "NOTATYPE", "1.2.3.4"},
			wantMsg: "NOTATYPE",
		},
		{
			name:    "create with a malformed value",
			args:    []string{"record", "create", testDomain, "www", "A", "not-an-ip"},
			wantMsg: "not a valid IPv4 address",
		},
		{
			name:    "create with a flag from another type",
			args:    []string{"record", "create", testDomain, "www", "A", "--preference", "10"},
			wantMsg: "not a flag for A records",
		},
		{
			name:    "create mixing the two notations",
			args:    []string{"record", "create", testDomain, "@", "MX", "10 mail.example.com.", "--preference", "20"},
			wantMsg: "both positionally and as named flags",
		},
		{
			name:    "create with a name that spells out the zone",
			args:    []string{"record", "create", testDomain, "www.example.com", "A", "203.0.113.10"},
			wantMsg: "already includes the zone domain",
		},
		{
			name:    "create with an unparseable ttl",
			args:    []string{"record", "create", testDomain, "www", "A", "203.0.113.10", "--ttl", "soon"},
			wantMsg: "soon",
		},
		{
			name:    "set with an unknown type",
			args:    []string{"record", "set", testDomain, "www", "NOTATYPE", "1.2.3.4"},
			wantMsg: "NOTATYPE",
		},
		{
			name:    "set with a malformed value",
			args:    []string{"record", "set", testDomain, "www", "A", "not-an-ip"},
			wantMsg: "not a valid IPv4 address",
		},
		{
			name:    "delete with an unknown type",
			args:    []string{"record", "delete", testDomain, "www", "NOTATYPE", "--yes"},
			wantMsg: "NOTATYPE",
		},
		{
			name:    "delete with a malformed value",
			args:    []string{"record", "delete", testDomain, "www", "A", "not-an-ip", "--yes"},
			wantMsg: "not a valid IPv4 address",
		},
		{
			name:    "delete with a name that spells out the zone",
			args:    []string{"record", "delete", testDomain, "www.example.com", "A", "--yes"},
			wantMsg: "already includes the zone domain",
		},
		{
			name:    "describe with an unknown type",
			args:    []string{"record", "describe", testDomain, "www", "NOTATYPE"},
			wantMsg: "NOTATYPE",
		},
		{
			name:    "list with an unknown type",
			args:    []string{"record", "list", testDomain, "--type", "NOTATYPE"},
			wantMsg: "NOTATYPE",
		},
		{
			name:    "list with a name that spells out the zone",
			args:    []string{"record", "list", testDomain, "--name", "www.example.com"},
			wantMsg: "already includes the zone domain",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			interactive(t)
			// No zone, no records: the API has nothing to answer with, so the
			// only thing that can produce exit 2 here is client-side parsing.
			h := newHarness(t)
			ce := requireExit(t, h.run(tc.args...), util.ExitUsage)
			mustContain(t, ce.Error(), tc.wantMsg)
			mustNotContain(t, ce.Error(), "not found")
		})
	}
}

// TestZoneNotFoundStillReportsNotFound — the reordering must not swallow the
// case the exit code is actually for.
func TestZoneNotFoundStillReportsNotFound(t *testing.T) {
	interactive(t)
	for _, args := range [][]string{
		{"record", "create", testDomain, "www", "A", "203.0.113.10"},
		{"record", "set", testDomain, "www", "A", "203.0.113.10"},
		{"record", "delete", testDomain, "www", "A", "--yes"},
		{"record", "describe", testDomain, "www"},
		{"record", "list", testDomain},
	} {
		t.Run(args[1], func(t *testing.T) {
			h := newHarness(t)
			ce := requireExit(t, h.run(args...), util.ExitNotFound)
			mustContain(t, ce.Error(), `zone "example.com" not found`)
		})
	}
}

// TestZoneMayBeNamedByItsObject — the positional accepts the DNSZone object's
// name, in which case the owner-name rules have to be re-derived against the
// domain it actually serves.
func TestZoneMayBeNamedByItsObject(t *testing.T) {
	h := newHarness(t, testZone())
	requireNoError(t, h.run("record", "create", testZoneObject, "www.example.com.", "A", "203.0.113.10"))

	got := h.getSet(t, "example-com-a").Spec.Records
	if len(got) != 1 || got[0].Name != "www" {
		t.Fatalf("entry = %+v, want the name reduced to \"www\" against example.com", got)
	}
}

// TestMultiValueCNAMEInOneCommandIsRejected — single-valuedness is a property
// of the set, so validation is a whole-slice call. Entry by entry, both values
// pass and the backend keeps one without a word.
func TestMultiValueCNAMEInOneCommandIsRejected(t *testing.T) {
	h := newHarness(t, testZone())
	ce := requireExit(t, h.run("record", "create", testDomain, "cdn", "CNAME",
		"one.example.net.", "two.example.net."), util.ExitUsage)
	mustContain(t, ce.Error(), "single-valued")
	if !h.setMissing(t, "example-com-cname") {
		t.Error("a rejected set reached the API server")
	}
}

// TestTXTIsEncodedForTheWireAndDecodedForDisplay.
//
// internal/pdns wraps whatever it receives in one quoted character-string, so a
// value over 255 bytes has to arrive already chunked or PowerDNS rejects it —
// which is most real DKIM keys. The encode belongs to rdata.EntryForAPI; this
// asserts the write path calls it, and that nothing a person reads shows the
// wire form.
func TestTXTIsEncodedForTheWireAndDecodedForDisplay(t *testing.T) {
	long := strings.Repeat("k", 400)

	h := newHarness(t, testZone())
	requireNoError(t, h.run("record", "create", testDomain, "sel._domainkey", "TXT", "--data", long))

	stored := h.getSet(t, "example-com-txt").Spec.Records[0].TXT.Content
	if !strings.HasPrefix(stored, `"`) || !strings.Contains(stored, `" "`) {
		t.Fatalf("stored content is not chunked into quoted character-strings: %.60q...", stored)
	}
	for _, chunk := range strings.Split(stored, `" "`) {
		if len(strings.Trim(chunk, `"`)) > 255 {
			t.Errorf("a character-string exceeds 255 bytes: %d", len(chunk))
		}
	}

	// Reading it back must show the logical value, not the wire form.
	requireNoError(t, h.run("record", "describe", testDomain, "sel._domainkey"))
	mustContain(t, h.stdout(), long)
	mustNotContain(t, h.stdout(), `\"`)
}

// TestTXTDeleteByValueMatchesTheStoredWireForm — the encode must not make a
// record undeletable by the value the user typed.
func TestTXTDeleteByValueMatchesTheStoredWireForm(t *testing.T) {
	interactive(t)
	long := strings.Repeat("k", 400)

	h := newHarness(t, testZone())
	requireNoError(t, h.run("record", "create", testDomain, "sel._domainkey", "TXT", "--data", long))
	requireNoError(t, h.run("record", "delete", testDomain, "sel._domainkey", "TXT", long, "--yes"))

	if !h.setMissing(t, "example-com-txt") {
		t.Error("delete-by-value did not match the stored wire form")
	}
}

// TestSetReportsUnchangedWhenNothingChanged — `set` run twice must not claim to
// have updated something the second time.
func TestSetReportsUnchangedWhenNothingChanged(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))
	requireNoError(t, h.run("record", "set", testDomain, "www", "A", "203.0.113.10", "--ttl", "300"))
	mustContain(t, h.stdout(), "  record/example.com A www unchanged")
	mustNotContain(t, h.stdout(), "updated")
}

// TestSetReportsUnchangedAcrossTheAutoBoundary — a nil TTL and an explicit 300
// resolve to the same record, so re-setting one as the other is not a change.
func TestSetReportsUnchangedAcrossTheAutoBoundary(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", nil)))
	requireNoError(t, h.run("record", "set", testDomain, "www", "A", "203.0.113.10", "--ttl", "300"))
	mustContain(t, h.stdout(), "unchanged")
}

func TestSetReportsUpdatedWhenSomethingChanged(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300))))
	requireNoError(t, h.run("record", "set", testDomain, "www", "A", "203.0.113.20"))
	mustContain(t, h.stdout(), "  record/example.com A www updated")
}

// TestEncodeHappensOnceAtTheWriteBoundary — no editFunc encodes; applyEdit does,
// for every entry in the bucket, so a future edit function cannot forget.
func TestEncodeHappensOnceAtTheWriteBoundary(t *testing.T) {
	long := strings.Repeat("z", 300)

	// A neighbour stored in its logical form by some other client: internal/pdns
	// would wrap it in one over-long character-string, so the write corrects it.
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeTXT,
		dnsv1alpha1.RecordEntry{Name: "legacy", TXT: &dnsv1alpha1.TXTRecordSpec{Content: long}},
	))
	requireNoError(t, h.run("record", "create", testDomain, "fresh", "TXT", "hello"))

	for _, e := range h.getSet(t, "example-com-txt").Spec.Records {
		if !strings.HasPrefix(e.TXT.Content, `"`) {
			t.Errorf("%s was written unencoded: %.40q", e.Name, e.TXT.Content)
		}
	}
}

// TestEncodeIsIdempotentAcrossRepeatedWrites — the boundary encode runs over
// untouched neighbours too, so it must not re-quote what it already quoted.
func TestEncodeIsIdempotentAcrossRepeatedWrites(t *testing.T) {
	h := newHarness(t, testZone())
	requireNoError(t, h.run("record", "create", testDomain, "@", "TXT", "v=spf1 -all"))
	first := h.getSet(t, "example-com-txt").Spec.Records[0].TXT.Content

	requireNoError(t, h.run("record", "create", testDomain, "other", "TXT", "second"))
	for _, e := range h.getSet(t, "example-com-txt").Spec.Records {
		if e.Name == "@" && e.TXT.Content != first {
			t.Errorf("the untouched neighbour was re-encoded: %q -> %q", first, e.TXT.Content)
		}
	}
}

// TestRetimedNeighboursAreNotReportedAsCreated — --ttl retimes every value at
// the name, so a neighbour differing only by TTL must not be echoed back under
// "created".
func TestRetimedNeighboursAreNotReportedAsCreated(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA,
		aEntry("www", "203.0.113.10", ttl(3600)),
		aEntry("www", "203.0.113.11", ttl(3600)),
	))
	requireNoError(t, h.run("record", "create", testDomain, "www", "A", "203.0.113.12", "--ttl", "60"))

	out := collapse(h.stdout())
	mustContain(t, out, "www 1m IN A 203.0.113.12")
	mustNotContain(t, out, "203.0.113.10")
	mustNotContain(t, out, "203.0.113.11")

	// The retiming still happened; it is just not news.
	for _, e := range h.getSet(t, "example-com-a").Spec.Records {
		if e.TTL == nil || *e.TTL != 60 {
			t.Errorf("%s ttl = %v, want 60", e.A.Content, e.TTL)
		}
	}
}

// --- which record set a write lands in ---------------------------------------
//
// A (zone, type) pair can be spread over several DNSRecordSet objects, and a
// live zone routinely is: the CLI's own bucket beside one per Gateway. The
// owner name is the key the backend collides on, so these fix which object a
// write chooses — getting it wrong either fragments a name across two sets,
// which surfaces as the Conflict and Not owner statuses `record list` reports,
// or writes into a set a controller reverts.

// A controller owns the names inside its set, not the type. A zone whose only A
// set belongs to one must still accept a record at an unrelated name.
func TestCreateAtAFreeNameIsNotBlockedByAManagedSet(t *testing.T) {
	gw := withLabels(recordSet(dnsv1alpha1.RRTypeA, aEntry("edge", "203.0.113.1", nil)),
		map[string]string{util.LabelSourceKind: "Gateway", util.LabelSourceName: "edge-gw"})

	h := newHarness(t, testZone(), gw)
	if err := h.run("record", "create", testDomain, "blog", "A", "203.0.113.9"); err != nil {
		t.Fatalf("creating blog A was refused: %v\nstderr: %s", err, h.stderr())
	}

	// The controller's set is left exactly as it was.
	got := h.getSet(t, gw.Name)
	if len(got.Spec.Records) != 1 || got.Spec.Records[0].Name != "edge" {
		t.Errorf("the managed set was modified: %+v", got.Spec.Records)
	}

	// ...and the record went somewhere, under a name that did not collide.
	var sets dnsv1alpha1.DNSRecordSetList
	if err := h.client.List(context.Background(), &sets); err != nil {
		t.Fatal(err)
	}
	var holder *dnsv1alpha1.DNSRecordSet
	for i := range sets.Items {
		if sets.Items[i].Name != gw.Name && sets.Items[i].Spec.RecordType == dnsv1alpha1.RRTypeA {
			holder = &sets.Items[i]
		}
	}
	if holder == nil {
		t.Fatalf("no new A set was created; sets: %d", len(sets.Items))
	}
	if len(holder.Spec.Records) != 1 || holder.Spec.Records[0].Name != "blog" {
		t.Errorf("new set holds %+v, want one entry for blog", holder.Spec.Records)
	}
}

// The set already holding the name wins over the one that merely sorts first,
// so a second value joins the values it belongs with rather than starting a
// rival entry for the same key in another object.
func TestCreateJoinsTheSetThatAlreadyHoldsTheName(t *testing.T) {
	first := recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.1", nil))
	second := recordSet(dnsv1alpha1.RRTypeA, aEntry("blog", "203.0.113.2", nil))
	second.Name = first.Name + "-extra" // sorts after, holds the name we write

	h := newHarness(t, testZone(), first, second)
	if err := h.run("record", "create", testDomain, "blog", "A", "203.0.113.3"); err != nil {
		t.Fatalf("create: %v\nstderr: %s", err, h.stderr())
	}

	if got := h.getSet(t, second.Name); len(got.Spec.Records) != 2 {
		t.Errorf("the set holding blog has %d entries, want 2: %+v", len(got.Spec.Records), got.Spec.Records)
	}
	if got := h.getSet(t, first.Name); len(got.Spec.Records) != 1 {
		t.Errorf("the set that merely sorted first was written to: %+v", got.Spec.Records)
	}
}

// A name absent from every set of its type joins an existing writable bucket
// rather than starting a second one, so a zone does not accumulate an object
// per record.
func TestCreateAtANewNameJoinsTheExistingBucket(t *testing.T) {
	existing := recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.1", nil))

	h := newHarness(t, testZone(), existing)
	if err := h.run("record", "create", testDomain, "blog", "A", "203.0.113.9"); err != nil {
		t.Fatalf("create: %v", err)
	}

	got := h.getSet(t, existing.Name)
	if len(got.Spec.Records) != 2 {
		t.Errorf("bucket has %d entries, want 2 — the record started a new set instead of joining", len(got.Spec.Records))
	}
	var sets dnsv1alpha1.DNSRecordSetList
	if err := h.client.List(context.Background(), &sets); err != nil {
		t.Fatal(err)
	}
	if len(sets.Items) != 1 {
		t.Errorf("zone has %d record sets, want 1", len(sets.Items))
	}
}

// Type is the other half of the key: a write must never land in a set of a
// different type, however that set is named or ordered.
func TestCreateNeverLandsInAnotherType(t *testing.T) {
	txt := recordSet(dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RecordEntry{
		Name: "blog", TXT: &dnsv1alpha1.TXTRecordSpec{Content: "hello"},
	})

	h := newHarness(t, testZone(), txt)
	if err := h.run("record", "create", testDomain, "blog", "A", "203.0.113.9"); err != nil {
		t.Fatalf("create: %v", err)
	}

	if got := h.getSet(t, txt.Name); len(got.Spec.Records) != 1 {
		t.Errorf("the TXT set was written to by an A create: %+v", got.Spec.Records)
	}
	if got := h.getSet(t, testZoneObject+"-a"); got.Spec.RecordType != dnsv1alpha1.RRTypeA {
		t.Errorf("new set has type %q, want A", got.Spec.RecordType)
	}
}
