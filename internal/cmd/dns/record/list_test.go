// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// zoneFixture is the full spread: several types, several owner names in one
// type bucket, the operator's own SOA and NS, and a Gateway-owned set.
func zoneFixture() []client.Object {
	a := recordSet(dnsv1alpha1.RRTypeA,
		aEntry("www", "203.0.113.10", ttl(300)),
		aEntry("www", "203.0.113.11", ttl(300)),
		aEntry("api", "203.0.113.12", nil),
	)
	withOwnerStatus(a, "www", metav1.ConditionTrue, "Programmed", "")
	withOwnerStatus(a, "api", metav1.ConditionFalse, "Conflict",
		"The record name is outside the zone. Check that the name belongs to this DNS zone.")

	mx := recordSet(dnsv1alpha1.RRTypeMX, dnsv1alpha1.RecordEntry{
		Name: "@", TTL: ttl(300),
		MX: &dnsv1alpha1.MXRecordSpec{Preference: 10, Exchange: "mail.example.com."},
	})
	withOwnerStatus(mx, "@", metav1.ConditionTrue, "Programmed", "")

	soa := recordSet(dnsv1alpha1.RRTypeSOA, dnsv1alpha1.RecordEntry{
		Name: "@", TTL: ttl(3600),
		SOA: &dnsv1alpha1.SOARecordSpec{MName: "ns1.datum.net.", RName: "hostmaster.example.com.", Serial: 1},
	})
	ns := recordSet(dnsv1alpha1.RRTypeNS,
		dnsv1alpha1.RecordEntry{Name: "@", TTL: ttl(3600), NS: &dnsv1alpha1.NSRecordSpec{Content: "ns1.datum.net."}},
		dnsv1alpha1.RecordEntry{Name: "@", TTL: ttl(3600), NS: &dnsv1alpha1.NSRecordSpec{Content: "ns2.datum.net."}},
	)
	txt := withLabels(recordSet(dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RecordEntry{
		Name: "_acme-challenge", TTL: ttl(60),
		TXT: &dnsv1alpha1.TXTRecordSpec{Content: `"gateway-token"`},
	}), map[string]string{util.LabelSourceKind: "Gateway", util.LabelSourceName: "edge-gw"})

	return []client.Object{testZone(), a, mx, soa, ns, txt}
}

func TestListFlattensBucketsIntoRecords(t *testing.T) {
	h := newHarness(t, zoneFixture()...)
	requireNoError(t, h.run("record", "list", testDomain))

	got := collapsedLines(h.stdout())
	want := []string{
		"NAME TYPE TTL VALUE STATUS",
		"@ MX 300 10 mail.example.com. Programmed",
		"@ NS 3600 ns1.datum.net. Pending (platform)",
		"@ NS 3600 ns2.datum.net. Pending (platform)",
		"@ SOA 3600 ns1.datum.net. hostmaster.example.com. 1 10800 3600 604800 3600 Pending (platform)",
		"_acme-challenge TXT 60 \"gateway-token\" Pending (managed by AI Edge)",
		"api A Auto 203.0.113.12 Conflict",
		"www A 300 203.0.113.10 Programmed",
		"www A 300 203.0.113.11 Programmed",
		"8 records — 3 Programmed, 4 Pending, 1 Conflict",
	}
	if len(got) != len(want) {
		t.Fatalf("row count = %d, want %d\n--- got ---\n%s", len(got), len(want), h.stdout())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d =\n  %q\nwant\n  %q", i, got[i], want[i])
		}
	}
}

// TestListStatusPerOwnerName is the reason the table reads status.recordSets[]
// rather than the rolled-up condition: the rollup flattens every one of these
// into a generic Pending.
func TestListStatusPerOwnerName(t *testing.T) {
	tests := []struct {
		name       string
		status     metav1.ConditionStatus
		reason     string
		message    string
		accepted   string
		wantStatus string
	}{
		{name: "programmed", status: metav1.ConditionTrue, reason: "Programmed", wantStatus: util.StatusProgrammed},
		{name: "not owner", status: metav1.ConditionFalse, reason: "NotOwner", message: "another set owns it", wantStatus: util.StatusNotOwner},
		{name: "conflict", status: metav1.ConditionFalse, reason: "Conflict", message: "the backend reported a conflict", wantStatus: util.StatusConflict},
		{name: "pdns error", status: metav1.ConditionFalse, reason: "PDNSError", message: "the backend said no", wantStatus: util.StatusError},
		{name: "pending", status: metav1.ConditionFalse, reason: "Pending", wantStatus: util.StatusPending},
		{name: "unknown reason passes through", status: metav1.ConditionFalse, reason: "Throttled", message: "slow down", wantStatus: "Throttled"},
		{name: "rejected outranks", status: metav1.ConditionTrue, reason: "Programmed", accepted: "spec.records[0] is invalid", wantStatus: util.StatusRejected},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rs := recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300)))
			withOwnerStatus(rs, "www", tc.status, tc.reason, tc.message)
			if tc.accepted != "" {
				withAcceptedFalse(rs, tc.accepted)
			}

			h := newHarness(t, testZone(), rs)
			requireNoError(t, h.run("record", "list", testDomain))
			mustContain(t, collapse(h.stdout()), "www A 300 203.0.113.10 "+tc.wantStatus)
		})
	}
}

// TestListNoPerNameStatusIsPending covers the freshly created set, whose
// CRD-defaulted conditions are stamped at the Unix epoch and must never be
// rendered as an age.
func TestListNoPerNameStatusIsPending(t *testing.T) {
	rs := recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", nil))
	rs.CreationTimestamp = metav1.Unix(0, 0)

	h := newHarness(t, testZone(), rs)
	requireNoError(t, h.run("record", "list", testDomain, "-o", "wide"))

	mustContain(t, collapse(h.stdout()), "www A Auto 203.0.113.10 Pending example-com-a —")
	mustNotContain(t, h.stdout(), "56y")
}

func TestListFilters(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    []string
		notWant []string
	}{
		{
			name:    "by type",
			args:    []string{"--type", "MX"},
			want:    []string{"@ MX 300 10 mail.example.com. Programmed", "1 record — 1 Programmed"},
			notWant: []string{"www"},
		},
		{
			name: "by several types",
			args: []string{"--type", "A,MX"},
			want: []string{"@ MX", "www A", "api A"},
		},
		{
			name:    "by owner name",
			args:    []string{"--name", "www"},
			want:    []string{"www A 300 203.0.113.10", "2 records — 2 Programmed"},
			notWant: []string{"api A"},
		},
		{
			name:    "by apex",
			args:    []string{"--name", "@", "--type", "MX"},
			want:    []string{"@ MX 300"},
			notWant: []string{"www"},
		},
		{
			name:    "by status token",
			args:    []string{"--status", "conflict"},
			want:    []string{"api A Auto 203.0.113.12 Conflict", "1 record — 1 Conflict"},
			notWant: []string{"www A"},
		},
		{
			name:    "status token is the first word",
			args:    []string{"--status", "not"},
			want:    []string{"No records in zone example.com match the given filters."},
			notWant: []string{"NAME TYPE"},
		},
		{
			name:    "managed only",
			args:    []string{"--managed"},
			want:    []string{"(platform)", "(managed by AI Edge)"},
			notWant: []string{"www A", "@ MX"},
		},
		{
			name:    "no headers",
			args:    []string{"--type", "MX", "--no-headers"},
			want:    []string{"@ MX 300"},
			notWant: []string{"NAME TYPE TTL"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, zoneFixture()...)
			requireNoError(t, h.run(append([]string{"record", "list", testDomain}, tc.args...)...))
			out := strings.Join(collapsedLines(h.stdout()), "\n")
			for _, w := range tc.want {
				mustContain(t, out, w)
			}
			for _, w := range tc.notWant {
				mustNotContain(t, out, w)
			}
		})
	}
}

func TestListUnknownTypeIsUsageError(t *testing.T) {
	h := newHarness(t, zoneFixture()...)
	err := h.run("record", "list", testDomain, "--type", "NOPE")
	requireExit(t, err, util.ExitUsage)
}

func TestListUnknownZoneIsNotFound(t *testing.T) {
	h := newHarness(t, testZone())
	ce := requireExit(t, h.run("record", "list", "other.example"), util.ExitNotFound)
	mustContain(t, ce.Error(), `zone "other.example" not found`)
	mustContain(t, ce.Fix(), "datumctl dns zone list")
}

// TestListEmptyZoneTeachesTheNextStep — an empty zone is the normal first
// state, so it is never an error.
func TestListEmptyZoneTeachesTheNextStep(t *testing.T) {
	h := newHarness(t, testZone())
	requireNoError(t, h.run("record", "list", testDomain))

	out := h.stdout()
	mustContain(t, out, "No records found in zone example.com.")
	mustContain(t, out, "Get started:")
	mustContain(t, out, "datumctl dns record create example.com www A 203.0.113.10")
}

// TestListEmptyFilteredSuggestsTheFilteredType keeps the example useful when a
// filter is what emptied the listing.
func TestListEmptyFilteredSuggestsTheFilteredType(t *testing.T) {
	h := newHarness(t, testZone())
	requireNoError(t, h.run("record", "list", testDomain, "--type", "MX"))

	out := h.stdout()
	mustContain(t, out, "No records in zone example.com match the given filters.")
	mustContain(t, out, "record create example.com @ MX --preference 10 --exchange mail.example.com.")
}

// TestListJSONEmitsRawObjects — the flat view is a presentation; -o json is the
// object contract and is dispatched before flattening.
func TestListJSONEmitsRawObjects(t *testing.T) {
	h := newHarness(t, zoneFixture()...)
	requireNoError(t, h.run("record", "list", testDomain, "--type", "A", "-o", "json"))

	var list dnsv1alpha1.DNSRecordSetList
	if err := json.Unmarshal(h.out.Bytes(), &list); err != nil {
		t.Fatalf("output is not a DNSRecordSetList: %v\n%s", err, h.stdout())
	}
	if len(list.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(list.Items))
	}
	if got := len(list.Items[0].Spec.Records); got != 3 {
		t.Errorf("records = %d, want 3 — the objects must not be flattened", got)
	}
	mustNotContain(t, h.stdout(), "NAME")
}

func TestListYAMLEmitsRawObjects(t *testing.T) {
	h := newHarness(t, zoneFixture()...)
	requireNoError(t, h.run("record", "list", testDomain, "--type", "A", "-o", "yaml"))
	mustContain(t, h.stdout(), "kind: DNSRecordSetList")
	mustContain(t, h.stdout(), "recordType: A")
}

// TestListNameOutputAddressesRecords emits the (name, type) pairs the other
// verbs take, deduplicated across the values at a name.
func TestListNameOutputAddressesRecords(t *testing.T) {
	h := newHarness(t, zoneFixture()...)
	requireNoError(t, h.run("record", "list", testDomain, "--type", "A", "-o", "name"))

	got := collapsedLines(h.stdout())
	want := []string{"api/A", "www/A"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("-o name = %v, want %v", got, want)
	}
}

func TestListQuietDropsTheFooter(t *testing.T) {
	h := newHarness(t, zoneFixture()...)
	requireNoError(t, h.run("record", "list", testDomain, "--type", "MX", "--quiet"))
	mustNotContain(t, h.stdout(), "1 record —")
	mustContain(t, h.stdout(), "@ ")
}

// TestStatusFilterAcceptsTheTwoWordStatus — the first word of "Not owner" is
// the useless token `not`, so the whole status is accepted too, spacing and
// punctuation folded.
func TestStatusFilterAcceptsTheTwoWordStatus(t *testing.T) {
	rs := recordSet(dnsv1alpha1.RRTypeA,
		aEntry("www", "203.0.113.10", ttl(300)),
		aEntry("api", "203.0.113.12", ttl(300)),
	)
	withOwnerStatus(rs, "www", metav1.ConditionFalse, "NotOwner", "another record set owns this name")
	withOwnerStatus(rs, "api", metav1.ConditionTrue, "Programmed", "")

	for _, token := range []string{"not", "not-owner", "notowner", "Not Owner", "NOT-OWNER"} {
		t.Run(token, func(t *testing.T) {
			h := newHarness(t, testZone(), rs.DeepCopy())
			requireNoError(t, h.run("record", "list", testDomain, "--status", token))

			out := collapse(h.stdout())
			mustContain(t, out, "www A 300 203.0.113.10 Not owner")
			mustNotContain(t, out, "api")
			mustContain(t, out, "1 record — 1 Not owner")
		})
	}
}

func TestStatusFilterCompletionOffersTheUsableToken(t *testing.T) {
	f := listCommand().Flags().Lookup("status")
	if f == nil {
		t.Fatal("--status is not registered")
	}
	mustContain(t, f.Usage, "not-owner")
	mustNotContain(t, f.Usage, "|not|")
}

// TestFooterPluralisesTheNounButNotTheStatus.
func TestFooterPluralisesTheNounButNotTheStatus(t *testing.T) {
	tests := []struct {
		name    string
		entries []dnsv1alpha1.RecordEntry
		want    string
	}{
		{
			name:    "one",
			entries: []dnsv1alpha1.RecordEntry{aEntry("www", "203.0.113.10", nil)},
			want:    "1 record — 1 Pending",
		},
		{
			name: "several",
			entries: []dnsv1alpha1.RecordEntry{
				aEntry("www", "203.0.113.10", nil),
				aEntry("www", "203.0.113.11", nil),
			},
			want: "2 records — 2 Pending",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, tc.entries...))
			requireNoError(t, h.run("record", "list", testDomain))
			mustContain(t, collapse(h.stdout()), tc.want)
		})
	}
}

// TestMachineOutputSaysWhichFiltersItIgnored — a silently dropped flag is worse
// than a refused one. --type narrows the objects server-side; the row filters
// select out of a view that -o json never builds.
func TestMachineOutputSaysWhichFiltersItIgnored(t *testing.T) {
	h := newHarness(t, zoneFixture()...)
	requireNoError(t, h.run("record", "list", testDomain, "-o", "json", "--name", "www", "--managed"))

	mustContain(t, h.stderr(), "Warning:")
	mustContain(t, h.stderr(), "--name, --managed are row filters and do not apply")
	mustContain(t, h.stderr(), "only --type narrows those")
	mustNotContain(t, h.stdout(), "Warning:")

	// The objects themselves are untouched by the ignored filters.
	var list dnsv1alpha1.DNSRecordSetList
	if err := json.Unmarshal(h.out.Bytes(), &list); err != nil {
		t.Fatalf("output is not a DNSRecordSetList: %v", err)
	}
	if len(list.Items) != 5 {
		t.Errorf("items = %d, want all 5 buckets", len(list.Items))
	}
}

func TestMachineOutputIsQuietWhenOnlyTypeIsGiven(t *testing.T) {
	h := newHarness(t, zoneFixture()...)
	requireNoError(t, h.run("record", "list", testDomain, "-o", "json", "--type", "A"))
	mustNotContain(t, h.stderr(), "Warning:")
}

// TestJSONItemsCarryTheirOwnGVK — a typed client leaves apiVersion and kind
// blank, and without them the output cannot be piped back through
// `kubectl apply -f`, which is most of the reason to ask for it.
func TestJSONItemsCarryTheirOwnGVK(t *testing.T) {
	h := newHarness(t, zoneFixture()...)
	requireNoError(t, h.run("record", "list", testDomain, "--type", "A", "-o", "json"))

	var doc struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Items      []struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
		} `json:"items"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}
	if doc.Kind != "DNSRecordSetList" || doc.APIVersion == "" {
		t.Errorf("list GVK = %q %q", doc.APIVersion, doc.Kind)
	}
	if len(doc.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(doc.Items))
	}
	if doc.Items[0].Kind != "DNSRecordSet" || doc.Items[0].APIVersion == "" {
		t.Errorf("item GVK = %q %q, want it stamped", doc.Items[0].APIVersion, doc.Items[0].Kind)
	}
}

// TestListDoesNotStampGVKOnTheCachedObjects — recordSetList copies before
// stamping, so nothing else in the process sees a mutated object.
func TestListDoesNotStampGVKOnTheCachedObjects(t *testing.T) {
	sets := []dnsv1alpha1.DNSRecordSet{*recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", nil))}
	_ = recordSetList(sets)
	if sets[0].Kind != "" {
		t.Errorf("the caller's object was mutated: kind = %q", sets[0].Kind)
	}
}

// TestUnknownStatusIsAUsageError — a typo used to exit 0 with "No records
// match", indistinguishable from a genuinely empty result, so a monitor asking
// "is anything in Conflict" got a clean bill of health from a misspelling.
func TestUnknownStatusIsAUsageError(t *testing.T) {
	for _, token := range []string{"bogus", "programed", "conflicts", ""} {
		t.Run(token, func(t *testing.T) {
			h := newHarness(t, zoneFixture()...)
			args := []string{"record", "list", testDomain, "--status", token}
			if token == "" {
				// An empty --status is "no filter", not an unknown one.
				requireNoError(t, h.run(args...))
				return
			}
			ce := requireExit(t, h.run(args...), util.ExitUsage)
			mustContain(t, ce.Error(), "unknown status")
			mustContain(t, ce.Fix(), "not-owner")
			mustContain(t, ce.Fix(), "programmed")
		})
	}
}

// TestAServerInventedReasonIsAValidFilter.
//
// RecordStatus passes a reason the CLI does not recognise through raw as the
// status word, so a row can legitimately read "Throttled". Rejecting the token
// as unknown would refuse to filter on a value the table is displaying.
func TestAServerInventedReasonIsAValidFilter(t *testing.T) {
	rs := recordSet(dnsv1alpha1.RRTypeA,
		aEntry("www", "203.0.113.10", ttl(300)),
		aEntry("api", "203.0.113.12", ttl(300)),
	)
	withOwnerStatus(rs, "www", metav1.ConditionFalse, "Throttled", "slow down")
	withOwnerStatus(rs, "api", metav1.ConditionTrue, "Programmed", "")

	h := newHarness(t, testZone(), rs)
	requireNoError(t, h.run("record", "list", testDomain, "--status", "throttled"))

	out := collapse(h.stdout())
	mustContain(t, out, "www A 300 203.0.113.10 Throttled")
	mustNotContain(t, out, "api")
	mustContain(t, out, "1 record — 1 Throttled")
}

// TestAKnownStatusMatchingNothingIsACleanExit.
//
// This is a PROPERTY, not an incidental: a known status token that selects no
// rows must exit 0 with the empty state. "Nothing is in Conflict" is a real
// answer to a real question, and turning it into a non-zero exit would break
// every monitor that asks it. Asserted for every advertised token so the
// deferred unknown-token check cannot start swallowing them.
func TestAKnownStatusMatchingNothingIsACleanExit(t *testing.T) {
	// One record, Programmed: every other status selects nothing.
	rs := recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", ttl(300)))
	withOwnerStatus(rs, "www", metav1.ConditionTrue, "Programmed", "")

	for _, token := range statusFilterTokens() {
		if token == "programmed" {
			continue
		}
		t.Run(token, func(t *testing.T) {
			h := newHarness(t, testZone(), rs.DeepCopy())
			requireNoError(t, h.run("record", "list", testDomain, "--status", token))
			mustContain(t, h.stdout(), "No records in zone example.com match the given filters.")
		})
	}
}

// TestStatusHelpIsGeneratedFromTheStatusList — the usage string was hand-written
// and advertised six tokens where eight were accepted. It is now built from the
// same list the validator and the completion use, so the three cannot disagree.
func TestStatusHelpIsGeneratedFromTheStatusList(t *testing.T) {
	usage := listCommand().Flags().Lookup("status").Usage
	for _, token := range statusFilterTokens() {
		mustContain(t, usage, token)
	}
	mustContain(t, usage, "the first word alone also works")

	// pflag reads the first backquoted word in a usage string as the flag's
	// value placeholder, which rendered this flag as `--status not`.
	if strings.Contains(usage, "`") {
		t.Errorf("usage contains a backquote, which pflag will take as the value name: %q", usage)
	}
	if got := listCommand().Flags().Lookup("status").Value.Type(); got != "string" {
		t.Errorf("value type = %q, want string", got)
	}

	// Unreachable for a record: RecordStatus returns it only for a nil set.
	mustNotContain(t, usage, "unknown")
	for _, token := range statusFilterTokens() {
		if token == "unknown" {
			t.Error("unknown is advertised but can never match a record")
		}
	}
}

// TestEveryAdvertisedStatusTokenIsAccepted keeps the help text and the filter
// from drifting apart.
func TestEveryAdvertisedStatusTokenIsAccepted(t *testing.T) {
	for _, token := range statusFilterTokens() {
		t.Run(token, func(t *testing.T) {
			h := newHarness(t, zoneFixture()...)
			requireNoError(t, h.run("record", "list", testDomain, "--status", token))
		})
	}
}
