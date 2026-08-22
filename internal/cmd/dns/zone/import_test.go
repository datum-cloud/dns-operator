// SPDX-License-Identifier: AGPL-3.0-only

package zone

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/bind"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

const (
	importZoneObj = "example-com-abc123"
	importDomain  = "example.com"
)

// zoneFile writes content to a temporary file and returns its path.
func zoneFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "zone")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return path
}

// bulkZone is the zone every bulk test imports into.
func bulkZone() *dnsv1alpha1.DNSZone {
	return newZone(importZoneObj, importDomain)
}

// bulkSet builds a record set under the operator's own naming convention.
func bulkSet(t dnsv1alpha1.RRType, entries ...dnsv1alpha1.RecordEntry) *dnsv1alpha1.DNSRecordSet {
	return &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            importZoneObj + "-" + strings.ToLower(string(t)),
			Namespace:       util.ResourceNamespace,
			ResourceVersion: "1",
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: importZoneObj},
			RecordType: t,
			Records:    entries,
		},
	}
}

func aRecord(name, ip string, ttl *int64) dnsv1alpha1.RecordEntry {
	return dnsv1alpha1.RecordEntry{Name: name, TTL: ttl, A: &dnsv1alpha1.ARecordSpec{Content: ip}}
}

func ttlOf(v int64) *int64 { return &v }

// setEntries reads a record set back out of the fake API, or nil when the
// object does not exist.
func setEntries(t *testing.T, c client.Client, name string) *dnsv1alpha1.DNSRecordSet {
	t.Helper()
	var rs dnsv1alpha1.DNSRecordSet
	err := c.Get(context.Background(),
		client.ObjectKey{Namespace: util.ResourceNamespace, Name: name}, &rs)
	if err != nil {
		return nil
	}
	return &rs
}

func TestImportRequiresASource(t *testing.T) {
	c := newFakeClient(t, bulkZone())
	h := newHarness(t, c)

	err := h.run("zone", "import", importDomain)
	assertExitCode(t, err, util.ExitUsage)
	if !strings.Contains(err.Error(), "--file or --discover") {
		t.Errorf("error = %v, want it to name the two sources", err)
	}
}

func TestImportRejectsBothSources(t *testing.T) {
	c := newFakeClient(t, bulkZone())
	h := newHarness(t, c)

	err := h.run("zone", "import", importDomain, "--file", "x", "--discover")
	assertExitCode(t, err, util.ExitUsage)
}

func TestImportFromFile(t *testing.T) {
	c := newFakeClient(t, bulkZone())
	h := newHarness(t, c)

	path := zoneFile(t, `$ORIGIN example.com.
$TTL 3600
@    300  IN A     203.0.113.10
www  300  IN A     203.0.113.11
www  300  IN A     203.0.113.12
@         IN MX    10 mail
_dmarc    IN TXT   "v=DMARC1; p=none"
`)

	if err := h.run("zone", "import", importDomain, "--file", path); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}

	// One write per type, not one per record.
	aSet := setEntries(t, c, importZoneObj+"-a")
	if aSet == nil {
		t.Fatal("the A record set was not created")
	}
	if len(aSet.Spec.Records) != 3 {
		t.Errorf("A set holds %d entries, want 3", len(aSet.Spec.Records))
	}
	if setEntries(t, c, importZoneObj+"-mx") == nil {
		t.Error("the MX record set was not created")
	}
	txt := setEntries(t, c, importZoneObj+"-txt")
	if txt == nil {
		t.Fatal("the TXT record set was not created")
	}
	// TXT is stored in presentation form so internal/pdns does not re-quote it.
	if got := txt.Spec.Records[0].TXT.Content; !strings.HasPrefix(got, `"`) {
		t.Errorf("TXT content = %q, want it stored already quoted", got)
	}

	out := h.out.String()
	for _, want := range []string{"203.0.113.10", "created", "5 records"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary does not mention %q:\n%s", want, out)
		}
	}
}

// A file TTL that is not on the portal's preset ladder must survive. The portal
// rewrites 240 to 300, which silently changes what the user imported.
func TestImportPreservesArbitraryTTLs(t *testing.T) {
	c := newFakeClient(t, bulkZone())
	h := newHarness(t, c)

	path := zoneFile(t, "$ORIGIN example.com.\nwww 240 IN A 203.0.113.10\napi 7 IN A 203.0.113.11\n")
	if err := h.run("zone", "import", importDomain, "--file", path); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}

	set := setEntries(t, c, importZoneObj+"-a")
	got := map[string]int64{}
	for _, e := range set.Spec.Records {
		if e.TTL == nil {
			t.Fatalf("entry %q lost its TTL", e.Name)
		}
		got[e.Name] = *e.TTL
	}
	if got["www"] != 240 || got["api"] != 7 {
		t.Errorf("TTLs = %v, want www=240 api=7 — nothing may be snapped onto a ladder", got)
	}
}

func TestImportRewritesApexCNAME(t *testing.T) {
	c := newFakeClient(t, bulkZone())
	h := newHarness(t, c)

	path := zoneFile(t, "$ORIGIN example.com.\n@ 300 IN CNAME lb.example.net.\n")
	if err := h.run("zone", "import", importDomain, "--file", path); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}

	if setEntries(t, c, importZoneObj+"-cname") != nil {
		t.Error("an apex CNAME was written as a CNAME; it must become an ALIAS")
	}
	alias := setEntries(t, c, importZoneObj+"-alias")
	if alias == nil {
		t.Fatal("the apex CNAME was not rewritten to an ALIAS")
	}
	if got := alias.Spec.Records[0].ALIAS.Content; got != "lb.example.net." {
		t.Errorf("ALIAS content = %q, want %q", got, "lb.example.net.")
	}
	if !strings.Contains(h.err.String(), "apex CNAME") {
		t.Errorf("the rewrite was not reported:\n%s", h.err.String())
	}
}

func TestImportMergesAndSkipsDuplicates(t *testing.T) {
	existing := bulkSet(dnsv1alpha1.RRTypeA,
		aRecord("www", "203.0.113.10", ttlOf(300)),
		aRecord("api", "203.0.113.20", ttlOf(300)),
	)
	c := newFakeClient(t, bulkZone(), existing)
	h := newHarness(t, c)

	path := zoneFile(t, `$ORIGIN example.com.
www 300 IN A 203.0.113.10
www 300 IN A 203.0.113.11
`)
	if err := h.run("zone", "import", importDomain, "--file", path); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}

	set := setEntries(t, c, importZoneObj+"-a")
	if len(set.Spec.Records) != 3 {
		t.Fatalf("A set holds %d entries, want 3 (the two live ones plus one new)", len(set.Spec.Records))
	}
	if !strings.Contains(h.out.String(), "skipped") {
		t.Errorf("the duplicate was not reported as skipped:\n%s", h.out.String())
	}
	if !strings.Contains(h.out.String(), "created") {
		t.Errorf("the new record was not reported as created:\n%s", h.out.String())
	}
	// api is not in the file and must survive a merge.
	if !hasValue(set, "203.0.113.20") {
		t.Error("merging dropped a live record the file did not mention")
	}
}

func TestImportReplaceDiscardsTheLiveEntries(t *testing.T) {
	existing := bulkSet(dnsv1alpha1.RRTypeA,
		aRecord("www", "203.0.113.10", ttlOf(300)),
		aRecord("api", "203.0.113.20", ttlOf(300)),
	)
	c := newFakeClient(t, bulkZone(), existing)
	h := newHarness(t, c)

	path := zoneFile(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.99\n")
	if err := h.run("zone", "import", importDomain, "--file", path, "--replace", "--yes"); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}

	set := setEntries(t, c, importZoneObj+"-a")
	if len(set.Spec.Records) != 1 || !hasValue(set, "203.0.113.99") {
		t.Errorf("--replace left %d entries, want exactly the file's one: %+v",
			len(set.Spec.Records), set.Spec.Records)
	}
}

func TestImportReplaceIsConfirmed(t *testing.T) {
	interactiveZone(t)
	c := newFakeClient(t, bulkZone(), bulkSet(dnsv1alpha1.RRTypeA, aRecord("www", "203.0.113.10", ttlOf(300))))
	h := newHarness(t, c)
	h.in.WriteString("n\n")

	path := zoneFile(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.99\n")
	err := h.run("zone", "import", importDomain, "--file", path, "--replace")
	assertExitCode(t, err, util.ExitAborted)

	set := setEntries(t, c, importZoneObj+"-a")
	if !hasValue(set, "203.0.113.10") {
		t.Error("a declined --replace still modified the zone")
	}
}

// Only the types present in the input are replaced; a type the file never
// mentions is not touched at all.
func TestImportReplaceLeavesOtherTypesAlone(t *testing.T) {
	c := newFakeClient(t, bulkZone(),
		bulkSet(dnsv1alpha1.RRTypeA, aRecord("www", "203.0.113.10", ttlOf(300))),
		bulkSet(dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RecordEntry{
			Name: "@", TTL: ttlOf(300), TXT: &dnsv1alpha1.TXTRecordSpec{Content: `"keep me"`},
		}),
	)
	h := newHarness(t, c)

	path := zoneFile(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.99\n")
	if err := h.run("zone", "import", importDomain, "--file", path, "--replace", "--yes"); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}
	txt := setEntries(t, c, importZoneObj+"-txt")
	if txt == nil || len(txt.Spec.Records) != 1 {
		t.Fatal("--replace touched a type the file never mentioned")
	}
}

func TestImportReportsUnsupportedTypes(t *testing.T) {
	c := newFakeClient(t, bulkZone())
	h := newHarness(t, c)

	path := zoneFile(t, `$ORIGIN example.com.
www 300 IN A  203.0.113.10
@   300 IN DS 12345 8 2 abcdef
`)
	if err := h.run("zone", "import", importDomain, "--file", path); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}
	stderr := h.err.String()
	if !strings.Contains(stderr, "DS") || !strings.Contains(stderr, "line 3") {
		t.Errorf("the unsupported record was not reported with its line:\n%s", stderr)
	}
}

func TestImportRefusesAMalformedFile(t *testing.T) {
	c := newFakeClient(t, bulkZone())
	h := newHarness(t, c)

	path := zoneFile(t, "www 300 IN A not-an-ip\n")
	err := h.run("zone", "import", importDomain, "--file", path)
	assertExitCode(t, err, util.ExitUsage)
	if !strings.Contains(err.Error(), "line 1") {
		t.Errorf("error = %v, want it to name the line", err)
	}
}

// A client-side validation failure writes NOTHING.
//
// Half an imported zone file is worse than none of it: the user cannot tell
// which half landed, and the records that did land are live. Everything
// knowable without the API is therefore decided across the whole input before
// the first write is issued.
func TestImportWritesNothingWhenAnyRecordIsInvalid(t *testing.T) {
	c := newFakeClient(t, bulkZone())
	h := newHarness(t, c)

	// A non-apex NS is a subdomain delegation the user owns, so it is validated
	// rather than skipped as a platform record.
	path := zoneFile(t, `$ORIGIN example.com.
www 300 IN A  203.0.113.10
dev 300 IN NS ns1.datum.net.
dev 300 IN NS _bad_.datum.net.
`)
	err := h.run("zone", "import", importDomain, "--file", path)
	assertExitCode(t, err, util.ExitError)

	if setEntries(t, c, importZoneObj+"-a") != nil {
		t.Error("a valid record was written even though another record in the file was invalid")
	}
	if setEntries(t, c, importZoneObj+"-ns") != nil {
		t.Error("the invalid NS type was written")
	}

	out := h.out.String()
	if !strings.Contains(out, "failed") {
		t.Errorf("the failing record was not reported:\n%s", out)
	}
	// The records that were fine are named too, so the user knows they still
	// have to be imported once the file is fixed.
	if !strings.Contains(out, "not attempted") {
		t.Errorf("the untouched records were not accounted for:\n%s", out)
	}
}

// An API failure is a different story: by the time it happens some types are
// already committed, so it is reported per type rather than pretending the
// whole command was atomic.
func TestImportReportsApiFailurePerType(t *testing.T) {
	c := newFakeClientWith(t, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			rs, ok := obj.(*dnsv1alpha1.DNSRecordSet)
			if ok && rs.Spec.RecordType == dnsv1alpha1.RRTypeMX {
				return apierrors.NewInternalError(errStub{})
			}
			return cl.Create(ctx, obj, opts...)
		},
	}, bulkZone())
	h := newHarness(t, c)

	path := zoneFile(t, `$ORIGIN example.com.
www 300 IN A  203.0.113.10
@   300 IN MX 10 mail.example.com.
`)
	err := h.run("zone", "import", importDomain, "--file", path)
	assertExitCode(t, err, util.ExitError)

	if setEntries(t, c, importZoneObj+"-a") == nil {
		t.Error("a server-side failure in the MX type also lost the A type")
	}
	if setEntries(t, c, importZoneObj+"-mx") != nil {
		t.Error("the MX type was written despite the server rejecting it")
	}
}

func TestImportDryRunWritesNothing(t *testing.T) {
	c := newFakeClient(t, bulkZone())
	h := newHarness(t, c)

	path := zoneFile(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.10\n")
	if err := h.run("zone", "import", importDomain, "--file", path, "--dry-run"); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}
	if setEntries(t, c, importZoneObj+"-a") != nil {
		t.Error("--dry-run created a record set")
	}
	if !strings.Contains(h.out.String(), "Dry run") {
		t.Errorf("the dry run was not announced:\n%s", h.out.String())
	}
}

func TestImportRefusesGatewayOwnedTypes(t *testing.T) {
	owned := bulkSet(dnsv1alpha1.RRTypeA, aRecord("edge", "203.0.113.1", ttlOf(300)))
	owned.Labels = map[string]string{
		util.LabelSourceKind: util.ValueSourceKindGateway,
		util.LabelSourceName: "public",
	}
	c := newFakeClient(t, bulkZone(), owned)
	h := newHarness(t, c)

	path := zoneFile(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.10\n")
	err := h.run("zone", "import", importDomain, "--file", path)
	assertExitCode(t, err, util.ExitError)

	set := setEntries(t, c, importZoneObj+"-a")
	if len(set.Spec.Records) != 1 {
		t.Error("a Gateway-owned record set was modified")
	}
	if !strings.Contains(h.out.String(), "AI Edge") {
		t.Errorf("the refusal was not explained:\n%s", h.out.String())
	}
}

func TestImportConflictIsReportedInTheUsersVocabulary(t *testing.T) {
	c := newFakeClientWith(t, interceptor.Funcs{
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			return apierrors.NewConflict(
				schema.GroupResource{Group: dnsv1alpha1.GroupVersion.Group, Resource: "dnsrecordsets"},
				importZoneObj+"-a", errStub{})
		},
	}, bulkZone(), bulkSet(dnsv1alpha1.RRTypeA, aRecord("www", "203.0.113.10", ttlOf(300))))
	h := newHarness(t, c)

	path := zoneFile(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.11\n")
	err := h.run("zone", "import", importDomain, "--file", path)
	assertExitCode(t, err, util.ExitError)
	if !strings.Contains(h.out.String(), "changed while this command was running") {
		t.Errorf("a lost precondition was not explained:\n%s", h.out.String())
	}
}

// --- discovery --------------------------------------------------------------

func TestImportFromDiscovery(t *testing.T) {
	disc := &dnsv1alpha1.DNSZoneDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: importZoneObj + "-discovery", Namespace: util.ResourceNamespace},
		Spec:       dnsv1alpha1.DNSZoneDiscoverySpec{DNSZoneRef: corev1.LocalObjectReference{Name: importZoneObj}},
		Status: dnsv1alpha1.DNSZoneDiscoveryStatus{
			Conditions: []metav1.Condition{{
				Type: "Discovered", Status: metav1.ConditionTrue, Reason: "Discovered",
				LastTransitionTime: metav1.Now(),
			}},
			RecordSets: []dnsv1alpha1.DiscoveredRecordSet{
				{
					RecordType: dnsv1alpha1.RRTypeA,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "@", TTL: ttlOf(300), A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.10"}},
						{Name: "www", TTL: ttlOf(300), A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.11"}},
					},
				},
				{
					RecordType: dnsv1alpha1.RRTypeMX,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "@", TTL: ttlOf(3600), MX: &dnsv1alpha1.MXRecordSpec{
							Preference: 10, Exchange: "mail.example.com.",
						}},
					},
				},
			},
		},
	}
	c := newFakeClient(t, bulkZone(), disc)
	h := newHarness(t, c)

	if err := h.run("zone", "import", importDomain, "--discover"); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}

	if set := setEntries(t, c, importZoneObj+"-a"); set == nil || len(set.Spec.Records) != 2 {
		t.Error("the discovered A records were not imported")
	}
	if set := setEntries(t, c, importZoneObj+"-mx"); set == nil {
		t.Error("the discovered MX record was not imported")
	}

	// The gap in what discovery returns has to be stated, or a migration looks
	// complete when it is not.
	stderr := h.err.String()
	for _, want := range []string{"NS, SOA, PTR and ALIAS", "Discovered"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("discovery output does not mention %q:\n%s", want, stderr)
		}
	}
}

func TestImportDiscoveryCreatesTheRequest(t *testing.T) {
	// With no discovery object and a controller that never answers, the command
	// must create the request and then give up on its own timeout rather than
	// hanging.
	c := newFakeClient(t, bulkZone())
	h := newHarness(t, c)

	err := h.run("zone", "import", importDomain, "--discover", "--timeout", "1s")
	assertExitCode(t, err, util.ExitUnavailable)

	var list dnsv1alpha1.DNSZoneDiscoveryList
	if lerr := c.List(context.Background(), &list, client.InNamespace(util.ResourceNamespace)); lerr != nil {
		t.Fatalf("listing discoveries: %v", lerr)
	}
	if len(list.Items) != 1 {
		t.Fatalf("got %d discovery objects, want 1", len(list.Items))
	}
	if list.Items[0].Spec.DNSZoneRef.Name != importZoneObj {
		t.Errorf("discovery targets %q, want %q", list.Items[0].Spec.DNSZoneRef.Name, importZoneObj)
	}
}

// --- helpers ----------------------------------------------------------------

func hasValue(set *dnsv1alpha1.DNSRecordSet, want string) bool {
	if set == nil {
		return false
	}
	for _, e := range set.Spec.Records {
		if strings.Contains(rdata.Render(set.Spec.RecordType, e), want) {
			return true
		}
	}
	return false
}

// interactiveZone makes the confirmation prompts reachable: util.NonInteractive
// treats a set CI variable as "nobody can answer".
func interactiveZone(t *testing.T) {
	t.Helper()
	t.Setenv("CI", "")
	if err := os.Unsetenv("CI"); err != nil {
		t.Fatalf("unsetting CI: %v", err)
	}
}

// --- hazards rdata exists to catch -------------------------------------------

// The backend applies the first entry's TTL to a whole owner name and drops the
// rest without a word. Merging a file into live records is exactly how an owner
// ends up with two, so the import has to say so.
func TestImportWarnsOnConflictingTTLs(t *testing.T) {
	c := newFakeClient(t, bulkZone(),
		bulkSet(dnsv1alpha1.RRTypeA, aRecord("www", "203.0.113.10", ttlOf(300))))
	h := newHarness(t, c)

	path := zoneFile(t, "$ORIGIN example.com.\nwww 900 IN A 203.0.113.11\n")
	if err := h.run("zone", "import", importDomain, "--file", path); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}
	if !strings.Contains(h.err.String(), "TTL") {
		t.Errorf("a mixed-TTL owner was not reported:\n%s", h.err.String())
	}
}

// The same owner spelled relatively and absolutely is one RRset to the backend,
// so the TTL disagreement between the two spellings must still be caught.
func TestImportWarnsOnConflictingTTLsAcrossSpellings(t *testing.T) {
	c := newFakeClient(t, bulkZone())
	h := newHarness(t, c)

	path := zoneFile(t, `$ORIGIN example.com.
www              300 IN A 203.0.113.10
www.example.com. 900 IN A 203.0.113.11
`)
	if err := h.run("zone", "import", importDomain, "--file", path); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}
	if !strings.Contains(h.err.String(), "TTL") {
		t.Errorf("a mixed-TTL owner spelled two ways was not reported:\n%s", h.err.String())
	}
}

// An out-of-zone owner name fails the whole PATCH at the backend, and a
// DNSRecordSet PATCH carries every owner name in the bucket — so one bad line
// would otherwise take down every other record of its type. It has to be caught
// client-side, named, and attributed to its line.
func TestImportRejectsAnOutOfZoneName(t *testing.T) {
	c := newFakeClient(t, bulkZone())
	h := newHarness(t, c)

	path := zoneFile(t, `$ORIGIN example.com.
www              300 IN A 203.0.113.10
other.example.net. 300 IN A 203.0.113.99
`)
	err := h.run("zone", "import", importDomain, "--file", path)
	assertExitCode(t, err, util.ExitError)

	out := h.out.String()
	if !strings.Contains(out, "line 3") {
		t.Errorf("the out-of-zone record was not attributed to its line:\n%s", out)
	}
	// A DNSRecordSet PATCH carries every owner name in the bucket, so the whole
	// type would fail at the backend anyway — but the point is that it is caught
	// here, named, and nothing is written.
	if setEntries(t, c, importZoneObj+"-a") != nil {
		t.Error("an out-of-zone name in the file still wrote the rest of the file")
	}
}

// Validate-in-a-loop passes a two-value CNAME set; only the whole-slice check
// catches it, and the backend keeps the first and drops the rest silently.
func TestImportRejectsAMultiValueCNAME(t *testing.T) {
	c := newFakeClient(t, bulkZone())
	h := newHarness(t, c)

	path := zoneFile(t, `$ORIGIN example.com.
www 300 IN CNAME one.example.net.
www 300 IN CNAME two.example.net.
`)
	err := h.run("zone", "import", importDomain, "--file", path)
	assertExitCode(t, err, util.ExitError)
	if setEntries(t, c, importZoneObj+"-cname") != nil {
		t.Error("a two-value CNAME set was written; the backend would drop one silently")
	}
}

// A DKIM key is routinely over 255 bytes and is exactly what people migrate.
// It must reach the API pre-chunked, or PowerDNS rejects the character-string.
func TestImportChunksALongTXTRecord(t *testing.T) {
	c := newFakeClient(t, bulkZone())
	h := newHarness(t, c)

	key := "v=DKIM1; k=rsa; p=" + strings.Repeat("M", 400)
	path := zoneFile(t, "$ORIGIN example.com.\nsel._domainkey 300 IN TXT \""+key+"\"\n")
	if err := h.run("zone", "import", importDomain, "--file", path); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}

	stored := setEntries(t, c, importZoneObj+"-txt").Spec.Records[0].TXT.Content
	if !strings.HasPrefix(stored, `"`) {
		t.Fatalf("TXT content = %.40q…, want it stored already quoted", stored)
	}
	if strings.Count(stored, `" "`) != 1 {
		t.Errorf("a %d-byte TXT value was not split into two character-strings: %.80q…",
			len(key), stored)
	}
	// And the stored form decodes back to the one logical value it started as,
	// which is what keeps a re-export or a re-apply from showing a diff.
	if got := rdata.TXTContentFromAPI(stored); got != key {
		t.Errorf("the stored TXT value does not decode back to what was imported:\n got %.60q…\nwant %.60q…", got, key)
	}
}

// An entry whose typed field does not match its record type makes buildRRSets
// emit an rrset with no records, which the client turns into a DELETE of
// whatever lives at that owner. Nothing may reach the API unvalidated.
func TestImportNeverWritesAnUnvalidatedEntry(t *testing.T) {
	live := bulkSet(dnsv1alpha1.RRTypeA, aRecord("www", "203.0.113.10", ttlOf(300)))
	c := newFakeClient(t, bulkZone(), live)
	h := newHarness(t, c)

	// A NS value the strict host rule rejects, at the same owner name as a live
	// A record, under --replace: the most dangerous shape there is.
	path := zoneFile(t, "$ORIGIN example.com.\nwww 300 IN NS _bad_.datum.net.\n")
	err := h.run("zone", "import", importDomain, "--file", path, "--replace", "--yes")
	assertExitCode(t, err, util.ExitError)

	set := setEntries(t, c, importZoneObj+"-a")
	if !hasValue(set, "203.0.113.10") {
		t.Error("an invalid entry reached the API and took a live record with it")
	}
	if len(set.Spec.Records) != 1 {
		t.Errorf("the live A set was modified: %+v", set.Spec.Records)
	}
	if setEntries(t, c, importZoneObj+"-ns") != nil {
		t.Error("an invalid NS entry was written")
	}
}

// The whole file is parsed before the first API call, so a broken file fails as
// a line-numbered usage error rather than as whatever the API says first.
// Proved by pointing at a zone that does not exist: the file's error must win.
func TestImportValidatesBeforeTouchingTheAPI(t *testing.T) {
	c := newFakeClient(t)
	h := newHarness(t, c)

	path := zoneFile(t, `$ORIGIN nope.example.
www 300 IN A 203.0.113.10
api 300 IN A nonsense
`)
	err := h.run("zone", "import", "nope.example", "--file", path)
	assertExitCode(t, err, util.ExitUsage)
	if !strings.Contains(err.Error(), "line 3") {
		t.Errorf("error = %v, want it to name the line", err)
	}
	if strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want the file's error to win over the zone lookup", err)
	}
}

// An undotted positional is a DNSZone object name, not a domain, so the pre-API
// pass has no zone to check names against — but the import must still work.
func TestImportAcceptsAnObjectNamePositional(t *testing.T) {
	c := newFakeClient(t, bulkZone())
	h := newHarness(t, c)

	path := zoneFile(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.10\n")
	if err := h.run("zone", "import", importZoneObj, "--file", path); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}
	if !hasValue(setEntries(t, c, importZoneObj+"-a"), "203.0.113.10") {
		t.Error("import by object name did not write the record")
	}
}

// --- platform-managed records -------------------------------------------------

// providerExport is what a real zone file looks like coming out of another DNS
// provider: an apex SOA and apex NS records naming the OLD provider, alongside
// the ordinary records the user actually wants moved. This is the flagship
// input for `zone import --file`, and the shape that makes the platform guard
// load-bearing rather than theoretical.
const providerExport = `$ORIGIN example.com.
$TTL 3600
@   IN SOA ns1.oldprovider.net. hostmaster.oldprovider.net. (
        2019010101 10800 3600 604800 3600 )
@   IN NS  ns1.oldprovider.net.
@   IN NS  ns2.oldprovider.net.
dev IN NS  ns1.delegated.example.net.
@   300 IN A   203.0.113.10
www 300 IN A   203.0.113.11
@   IN MX  10 mail.example.com.
@   300 IN TXT "v=spf1 -all"
`

// platformZone is a zone the operator has already provisioned: it has its own
// SOA and its own apex NS records pointing at Datum.
func platformZone(t *testing.T) (client.Client, *harness) {
	t.Helper()
	soa := bulkSet(dnsv1alpha1.RRTypeSOA, dnsv1alpha1.RecordEntry{
		Name: "@", TTL: ttlOf(3600),
		SOA: &dnsv1alpha1.SOARecordSpec{
			MName: "ns1.datum.net.", RName: "hostmaster.example.com.",
			Serial: 2024010101, Refresh: 10800, Retry: 3600, Expire: 604800, TTL: 3600,
		},
	})
	ns := bulkSet(dnsv1alpha1.RRTypeNS,
		dnsv1alpha1.RecordEntry{Name: "@", TTL: ttlOf(3600), NS: &dnsv1alpha1.NSRecordSpec{Content: "ns1.datum.net."}},
		dnsv1alpha1.RecordEntry{Name: "@", TTL: ttlOf(3600), NS: &dnsv1alpha1.NSRecordSpec{Content: "ns2.datum.net."}},
	)
	c := newFakeClient(t, bulkZone(), soa, ns)
	return c, newHarness(t, c)
}

// assertDelegationIntact reads the platform's two sets back and checks the old
// provider never got into either of them.
func assertDelegationIntact(t *testing.T, c client.Client) {
	t.Helper()

	soa := setEntries(t, c, importZoneObj+"-soa")
	if soa == nil {
		t.Fatal("the zone's SOA record set was deleted")
	}
	if got := soa.Spec.Records[0].SOA.MName; got != "ns1.datum.net." {
		t.Errorf("the SOA MNAME now points at %q — the import replaced the zone's SOA", got)
	}

	ns := setEntries(t, c, importZoneObj+"-ns")
	if ns == nil {
		t.Fatal("the zone's NS record set was deleted")
	}
	var apex []string
	for _, e := range ns.Spec.Records {
		if e.Name == "@" {
			apex = append(apex, e.NS.Content)
		}
	}
	sort.Strings(apex)
	want := []string{"ns1.datum.net.", "ns2.datum.net."}
	if !equalStrings(apex, want) {
		t.Errorf("the zone's apex NS records are now %v, want %v — delegation was modified", apex, want)
	}
}

// The merge path's own delegation case, with no SOA in the file.
//
// It is separated from the full provider export deliberately: a two-value SOA
// set is rejected by the single-valued check, so with the guard removed the
// full export fails on the SOA before the NS damage can be asserted. On its own
// the NS merge succeeds and does the harm quietly — the zone advertises both
// Datum's nameservers and the old provider's, and resolvers get different
// answers depending on which they ask. That is the failure this test sees and
// the full-export test cannot.
func TestImportMergeDoesNotAppendForeignNameservers(t *testing.T) {
	c, h := platformZone(t)

	path := zoneFile(t, `$ORIGIN example.com.
$TTL 3600
@   IN NS ns1.oldprovider.net.
@   IN NS ns2.oldprovider.net.
www 300 IN A 203.0.113.11
`)
	if err := h.run("zone", "import", importDomain, "--file", path); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}

	assertDelegationIntact(t, c)
	if !hasValue(setEntries(t, c, importZoneObj+"-a"), "203.0.113.11") {
		t.Error("the ordinary record was not imported")
	}
}

// The default merge path must not append the old provider's nameservers beside
// Datum's: the zone would advertise both and resolve inconsistently depending
// on which nameserver a resolver happened to ask.
func TestImportSkipsPlatformRecords(t *testing.T) {
	c, h := platformZone(t)

	path := zoneFile(t, providerExport)
	if err := h.run("zone", "import", importDomain, "--file", path); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}

	assertDelegationIntact(t, c)

	// The ordinary records still land.
	if !hasValue(setEntries(t, c, importZoneObj+"-a"), "203.0.113.10") {
		t.Error("the apex A record was not imported")
	}
	if !hasValue(setEntries(t, c, importZoneObj+"-a"), "203.0.113.11") {
		t.Error("the www A record was not imported")
	}
	if setEntries(t, c, importZoneObj+"-mx") == nil {
		t.Error("the MX record was not imported")
	}
	if setEntries(t, c, importZoneObj+"-txt") == nil {
		t.Error("the TXT record was not imported")
	}

	// A subdomain delegation is the user's, not the platform's, and belongs in
	// the same object beside the apex records.
	if !hasValue(setEntries(t, c, importZoneObj+"-ns"), "ns1.delegated.example.net.") {
		t.Error("a non-apex NS delegation was skipped; only apex NS is the platform's")
	}

	// Silently dropping records from an import is its own bug — the user has to
	// be told their file's SOA and NS were not applied, and why.
	out := h.out.String()
	if !strings.Contains(out, "skipped") {
		t.Errorf("the platform records were dropped without a word:\n%s", out)
	}
	if !strings.Contains(out, "SOA record is managed by the platform") {
		t.Errorf("the skipped SOA was not explained:\n%s", out)
	}
	if !strings.Contains(out, "break delegation") {
		t.Errorf("the skipped apex NS records were not explained:\n%s", out)
	}
}

// --replace means "replace the records I am giving you", not "dismantle the
// zone". Without the guard this is the path that destroys delegation outright.
func TestImportReplaceStillSkipsPlatformRecords(t *testing.T) {
	c, h := platformZone(t)

	path := zoneFile(t, providerExport)
	if err := h.run("zone", "import", importDomain, "--file", path, "--replace", "--yes"); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}

	assertDelegationIntact(t, c)

	if !hasValue(setEntries(t, c, importZoneObj+"-a"), "203.0.113.10") {
		t.Error("--replace did not import the ordinary records")
	}
	if !hasValue(setEntries(t, c, importZoneObj+"-ns"), "ns1.delegated.example.net.") {
		t.Error("--replace dropped the user's subdomain delegation")
	}
}

// The guard is shape-based, not set-based, because the operator creates
// <zone>-soa and <zone>-ns only once the zone's nameservers are assigned. On a
// zone that has not reached that point there is no set to classify, and an
// import would create one under exactly the name the operator later looks
// for — making the old provider's SOA the zone's SOA permanently.
func TestImportSkipsPlatformRecordsBeforeTheOperatorCreatesThem(t *testing.T) {
	c := newFakeClient(t, bulkZone())
	h := newHarness(t, c)

	path := zoneFile(t, providerExport)
	if err := h.run("zone", "import", importDomain, "--file", path); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}

	if setEntries(t, c, importZoneObj+"-soa") != nil {
		t.Error("the import created the zone's SOA set from the old provider's record")
	}
	ns := setEntries(t, c, importZoneObj+"-ns")
	if ns == nil {
		t.Fatal("the subdomain delegation was not imported")
	}
	for _, e := range ns.Spec.Records {
		if e.Name == "@" {
			t.Errorf("the import created an apex NS record: %s", e.NS.Content)
		}
	}
}

// A dry run must reach the same conclusions, so --dry-run is an honest preview
// of what --no-dry-run would do.
func TestImportDryRunSkipsPlatformRecordsToo(t *testing.T) {
	c, h := platformZone(t)

	path := zoneFile(t, providerExport)
	if err := h.run("zone", "import", importDomain, "--file", path, "--dry-run"); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}
	assertDelegationIntact(t, c)
	if !strings.Contains(h.out.String(), "managed by the platform") {
		t.Errorf("the dry run did not report the platform skips:\n%s", h.out.String())
	}
}

// --- owner-name identity ------------------------------------------------------

// The backend keys an RRset on the QUALIFIED owner name: pdns.QualifyOwner
// treats "@", "" and "example.com." as one owner, and "www" and
// "www.example.com." as one owner. The CRD's name pattern admits every one of
// those spellings, so anything in import.go that compares owner names literally
// is wrong in a way that only shows up on records it did not write itself.
//
// No first-party writer produces the alternative spellings today — the operator
// writes "@", the portal writes `name || '@'`, the parser routes through
// NormalizeNameWithWarnings — but `zone import --discover` takes its records
// from the OLD PROVIDER'S zone data by way of DNSZoneDiscovery status, and the
// only fixup applied is `if name == "" { name = "@" }`. That input is external,
// it is the flagship migration path, and it is the one most likely to be re-run
// with --replace.

func nsEntry(name, content string) dnsv1alpha1.RecordEntry {
	return dnsv1alpha1.RecordEntry{
		Name: name, TTL: ttlOf(3600),
		NS: &dnsv1alpha1.NSRecordSpec{Content: content},
	}
}

// The delegation-loss case, by a different route than the dead guard: the
// platform's apex NS records are stored as "example.com." rather than "@", so a
// guard that tests the literal name does not recognise them, the keep list
// comes back empty, and --replace drops them. The report said "1 created" with
// no skip and no warning — the same catastrophic outcome as an absent guard,
// defeated by a spelling.
func TestImportReplaceKeepsPlatformNSSpelledAbsolutely(t *testing.T) {
	live := bulkSet(dnsv1alpha1.RRTypeNS,
		nsEntry("example.com.", "ns1.datum.net."),
		nsEntry("example.com.", "ns2.datum.net."),
		nsEntry("dev", "ns1.old-provider.example."),
	)
	c := newFakeClient(t, bulkZone(), live)
	h := newHarness(t, c)

	path := zoneFile(t, "$ORIGIN example.com.\ndev 3600 IN NS ns1.new.example.\n")
	if err := h.run("zone", "import", importDomain, "--file", path, "--replace", "--yes"); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}

	var apex, dev []string
	for _, e := range setEntries(t, c, importZoneObj+"-ns").Spec.Records {
		if rdata.FQDN(e.Name, importDomain) == rdata.FQDN("@", importDomain) {
			apex = append(apex, e.NS.Content)
			continue
		}
		dev = append(dev, e.NS.Content)
	}
	sort.Strings(apex)

	if !equalStrings(apex, []string{"ns1.datum.net.", "ns2.datum.net."}) {
		t.Errorf("the platform's apex NS records are now %v, want both Datum nameservers — "+
			"--replace dropped them because they were spelled %q rather than \"@\"",
			apex, "example.com.")
	}
	if !equalStrings(dev, []string{"ns1.new.example."}) {
		t.Errorf("the subdomain delegation is %v, want it replaced with the file's value", dev)
	}
}

// A record stored under its absolute spelling is the SAME record as the file's
// relative one. Comparing literally appends a second copy, and
// ValidateEntriesInZone — which does group by FQDN — then reports a duplicate
// value for a file containing exactly one such record: the user is told their
// input is wrong when it is not.
func TestImportMatchesAnAbsolutelySpelledOwner(t *testing.T) {
	c := newFakeClient(t, bulkZone(),
		bulkSet(dnsv1alpha1.RRTypeA, aRecord("www.example.com.", "203.0.113.10", ttlOf(300))))
	h := newHarness(t, c)

	path := zoneFile(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.10\n")
	if err := h.run("zone", "import", importDomain, "--file", path); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}
	if !strings.Contains(h.out.String(), "skipped") {
		t.Errorf("the record was not recognised as one already present:\n%s", h.out.String())
	}

	set := setEntries(t, c, importZoneObj+"-a")
	if len(set.Spec.Records) != 1 {
		t.Errorf("the A set holds %d entries, want 1 — the same record was stored twice", len(set.Spec.Records))
	}
}

// The same aliasing on a single-valued type failed the whole import with
// "has 2 values but is single-valued" for a file holding one CNAME.
func TestImportMatchesAnAbsolutelySpelledSingleValuedOwner(t *testing.T) {
	c := newFakeClient(t, bulkZone(),
		bulkSet(dnsv1alpha1.RRTypeCNAME, dnsv1alpha1.RecordEntry{
			Name: "api.example.com.", TTL: ttlOf(300),
			CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "lb.example.net."},
		}))
	h := newHarness(t, c)

	path := zoneFile(t, "$ORIGIN example.com.\napi 300 IN CNAME lb.example.net.\n")
	if err := h.run("zone", "import", importDomain, "--file", path); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}
	if len(setEntries(t, c, importZoneObj+"-cname").Spec.Records) != 1 {
		t.Error("a second CNAME was appended beside the one already stored")
	}
}

// The discovery path is where a foreign apex spelling actually arrives, so it
// gets its own case rather than relying on the file path standing in for it.
func TestImportDiscoveryApexSpellingIsRecognised(t *testing.T) {
	disc := &dnsv1alpha1.DNSZoneDiscovery{
		ObjectMeta: metav1.ObjectMeta{
			Name: importZoneObj + "-discovery", Namespace: util.ResourceNamespace,
		},
		Spec: dnsv1alpha1.DNSZoneDiscoverySpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: importZoneObj},
		},
		Status: dnsv1alpha1.DNSZoneDiscoveryStatus{
			Conditions: []metav1.Condition{{
				Type: "Discovered", Status: metav1.ConditionTrue, Reason: "Discovered",
				LastTransitionTime: metav1.Now(),
			}},
			RecordSets: []dnsv1alpha1.DiscoveredRecordSet{{
				RecordType: dnsv1alpha1.RRTypeNS,
				// The old provider's spelling, which the CLI cannot control.
				Records: []dnsv1alpha1.RecordEntry{nsEntry("example.com.", "ns1.oldprovider.net.")},
			}},
		},
	}
	c := newFakeClient(t, bulkZone(), disc)
	h := newHarness(t, c)

	if err := h.run("zone", "import", importDomain, "--discover"); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}
	if setEntries(t, c, importZoneObj+"-ns") != nil {
		t.Error("a discovered apex NS record spelled absolutely was imported; " +
			"it must be recognised as the platform's and skipped")
	}
	if !strings.Contains(h.out.String(), "break delegation") {
		t.Errorf("the skip was not reported:\n%s", h.out.String())
	}
}

// rewriteApexCNAME's own contract, tested directly.
//
// The end-to-end case below cannot reach it: both input paths canonicalise
// owner names before the rewrite runs — the parser via
// NormalizeNameWithWarnings, and discovery likewise — so a non-canonical apex
// spelling does not survive to this function today. The zone-aware apex test is
// kept anyway, because the function's contract is "an apex CNAME becomes an
// ALIAS" and nothing in its signature says the caller must pre-normalize. This
// test pins that contract at the only level where it is observable.
func TestRewriteApexCNAMEAcceptsEverySpellingOfTheApex(t *testing.T) {
	for _, name := range []string{"@", "", "example.com.", "EXAMPLE.COM."} {
		t.Run("owner "+strconv.Quote(name), func(t *testing.T) {
			in := []bind.Record{{
				Name: name, TTL: ttlOf(300), Type: dnsv1alpha1.RRTypeCNAME,
				Entry: dnsv1alpha1.RecordEntry{
					Name: name, TTL: ttlOf(300),
					CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "lb.example.net."},
				},
			}}
			out := rewriteApexCNAME(in, importDomain, io.Discard)

			if out[0].Type != dnsv1alpha1.RRTypeALIAS {
				t.Fatalf("owner %q: type = %s, want ALIAS — the apex CNAME was not rewritten",
					name, out[0].Type)
			}
			if out[0].Entry.ALIAS == nil || out[0].Entry.ALIAS.Content != "lb.example.net." {
				t.Errorf("owner %q: ALIAS = %+v, want lb.example.net.", name, out[0].Entry.ALIAS)
			}
			if out[0].Entry.CNAME != nil {
				t.Errorf("owner %q: the CNAME field was left set", name)
			}
		})
	}
}

// A non-apex CNAME is left exactly as it is.
func TestRewriteApexCNAMELeavesNamedRecordsAlone(t *testing.T) {
	in := []bind.Record{{
		Name: "www", Type: dnsv1alpha1.RRTypeCNAME,
		Entry: dnsv1alpha1.RecordEntry{
			Name: "www", CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "lb.example.net."},
		},
	}}
	if out := rewriteApexCNAME(in, importDomain, io.Discard); out[0].Type != dnsv1alpha1.RRTypeCNAME {
		t.Errorf("a www CNAME was rewritten to %s", out[0].Type)
	}
}

// End to end: an apex CNAME written as the bare domain imports as an ALIAS.
// This passes through the parser's canonicalisation rather than the rewrite's
// own apex test, so it is a behaviour assertion, not a guard on that test.
func TestImportRewritesApexCNAMESpelledAbsolutely(t *testing.T) {
	c := newFakeClient(t, bulkZone())
	h := newHarness(t, c)

	path := zoneFile(t, "$ORIGIN example.com.\nexample.com. 300 IN CNAME lb.example.net.\n")
	if err := h.run("zone", "import", importDomain, "--file", path); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}

	alias := setEntries(t, c, importZoneObj+"-alias")
	if alias == nil {
		t.Fatal("an apex CNAME spelled \"example.com.\" was not rewritten to an ALIAS")
	}
	if got := alias.Spec.Records[0].ALIAS.Content; got != "lb.example.net." {
		t.Errorf("ALIAS content = %q, want %q", got, "lb.example.net.")
	}
	if setEntries(t, c, importZoneObj+"-cname") != nil {
		t.Error("the CNAME was written as a CNAME at the apex")
	}
}

// The discovery path canonicalises owner names the way the parser does for a
// file, so the summary table and every downstream comparison see one spelling
// regardless of how the records arrived.
func TestImportDiscoveryNormalizesOwnerNames(t *testing.T) {
	disc := &dnsv1alpha1.DNSZoneDiscovery{
		ObjectMeta: metav1.ObjectMeta{
			Name: importZoneObj + "-discovery", Namespace: util.ResourceNamespace,
		},
		Spec: dnsv1alpha1.DNSZoneDiscoverySpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: importZoneObj},
		},
		Status: dnsv1alpha1.DNSZoneDiscoveryStatus{
			Conditions: []metav1.Condition{{
				Type: "Discovered", Status: metav1.ConditionTrue, Reason: "Discovered",
				LastTransitionTime: metav1.Now(),
			}},
			RecordSets: []dnsv1alpha1.DiscoveredRecordSet{{
				RecordType: dnsv1alpha1.RRTypeA,
				Records: []dnsv1alpha1.RecordEntry{
					// The old provider's spellings, which the CLI cannot control.
					{Name: "example.com.", TTL: ttlOf(300), A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.10"}},
					{Name: "www.example.com.", TTL: ttlOf(300), A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.11"}},
					{Name: "", TTL: ttlOf(300), A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.12"}},
				},
			}},
		},
	}
	c := newFakeClient(t, bulkZone(), disc)
	h := newHarness(t, c)

	if err := h.run("zone", "import", importDomain, "--discover"); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}

	stored := setEntries(t, c, importZoneObj+"-a").Spec.Records
	names := make([]string, 0, len(stored))
	for _, e := range stored {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	if !equalStrings(names, []string{"@", "@", "www"}) {
		t.Errorf("stored owner names are %v, want [@ @ www] — discovered names were not canonicalised", names)
	}
	if strings.Contains(h.out.String(), "example.com.") {
		t.Errorf("the summary table shows an absolute owner name:\n%s", h.out.String())
	}
}

// The fix for the keep list was invisible: `--replace` printed a byte-identical
// "1 record — 1 created" whether it preserved the delegation or destroyed it.
// The single observable that would have caught two separate delegation bugs was
// the one thing missing, so the preserved records are now named.
//
// Note the asymmetry this closes. A platform record arriving in the FILE was
// already reported as skipped; a platform record already LIVE and carried
// through --replace was reported as nothing at all. Same decision, same
// records, and only one of them was visible.
func TestImportReplaceReportsWhatItKept(t *testing.T) {
	live := bulkSet(dnsv1alpha1.RRTypeNS,
		nsEntry("example.com.", "ns1.datum.net."),
		nsEntry("example.com.", "ns2.datum.net."),
		nsEntry("dev", "ns1.old-provider.example."),
	)
	c := newFakeClient(t, bulkZone(), live)
	h := newHarness(t, c)

	path := zoneFile(t, "$ORIGIN example.com.\nsub 3600 IN NS ns1.other.example.\n")
	if err := h.run("zone", "import", importDomain, "--file", path, "--replace", "--yes"); err != nil {
		t.Fatalf("import: %v\n%s", err, h.err.String())
	}
	out := h.out.String()

	// One kept line per preserved record, each carrying its reason — the same
	// register the skip lines use, which is what makes those legible.
	for _, ns := range []string{"ns1.datum.net.", "ns2.datum.net."} {
		line := ""
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, ns) {
				line = l
			}
		}
		if line == "" {
			t.Errorf("no line for the preserved record %s:\n%s", ns, out)
			continue
		}
		if !strings.Contains(line, outcomeKept) {
			t.Errorf("the preserved record %s is not reported as kept: %q", ns, collapse(line))
		}
		if !strings.Contains(line, "managed by the platform") {
			t.Errorf("the kept line for %s carries no reason: %q", ns, collapse(line))
		}
		// The stored spelling, not a normalised one: seeing "example.com." here
		// is the signal that the guard recognised a record it once walked past.
		if !strings.Contains(line, "example.com.") {
			t.Errorf("the kept line for %s does not show the stored owner name: %q", ns, collapse(line))
		}
	}

	// A kept record was never in the input, so it must not inflate the input
	// total — the file held one record.
	if !strings.Contains(out, "1 record — 1 created") {
		t.Errorf("the input tally does not read as one file record:\n%s", out)
	}
	if !strings.Contains(out, "2 records already in the zone kept") {
		t.Errorf("the preservation tally is missing:\n%s", out)
	}
}

// collapse squeezes the tabwriter padding so a line can be asserted on by its
// content rather than its column widths.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// A Gateway-owned set that does NOT carry source-kind must still be refused.
//
// This guard used to test source-kind alone, which was the weaker of the two
// copies of the same rule and the one protecting the bulk path. The producer's
// own garbage collector selects on managed, managed-by, source-name and
// source-namespace and pointedly not on source-kind, so a rule resting on that
// one label fails open the day it stops being written — and failing open here
// means the import writes, the controller reverts it, and the user is handed a
// success report for a change that silently disappeared.
func TestImportRefusesGatewayOwnedWithoutSourceKind(t *testing.T) {
	owned := bulkSet(dnsv1alpha1.RRTypeA, aRecord("edge", "203.0.113.1", ttlOf(300)))
	owned.Labels = map[string]string{
		util.LabelDNSManaged:      util.ValueDNSManaged,
		util.LabelManagedBy:       util.ValueManagedByNetworking,
		util.LabelSourceName:      "public",
		util.LabelSourceNamespace: "default",
		// source-kind deliberately absent.
	}
	c := newFakeClient(t, bulkZone(), owned)
	h := newHarness(t, c)

	path := zoneFile(t, "$ORIGIN example.com.\nwww 300 IN A 203.0.113.10\n")
	err := h.run("zone", "import", importDomain, "--file", path)
	assertExitCode(t, err, util.ExitError)

	set := setEntries(t, c, importZoneObj+"-a")
	if len(set.Spec.Records) != 1 || set.Spec.Records[0].Name != "edge" {
		t.Errorf("a Gateway-owned set was modified: %+v", set.Spec.Records)
	}
	if !strings.Contains(h.out.String(), "AI Edge") {
		t.Errorf("the refusal was not explained:\n%s", h.out.String())
	}
	// The owning Gateway is named from the source labels, which survive too.
	if !strings.Contains(h.out.String(), "public") {
		t.Errorf("the owning Gateway was not named:\n%s", h.out.String())
	}
}
