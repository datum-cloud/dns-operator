// SPDX-License-Identifier: AGPL-3.0-only

package zone

import (
	"encoding/json"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// describedZone is a fully delegated zone with a representative record mix.
func describedZone(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, newFakeClient(t,
		newZone("example-com-abc123", "example.com",
			withRecordCount(12),
			delegated("ns1.datum.net.", "ns2.datum.net.")),
		newRecordSet("example-com-a", "example-com-abc123", dnsv1alpha1.RRTypeA, 3),
		newRecordSet("example-com-ns", "example-com-abc123", dnsv1alpha1.RRTypeNS, 2),
		newRecordSet("example-com-mx", "example-com-abc123", dnsv1alpha1.RRTypeMX, 1),
		newRecordSet("example-com-txt", "example-com-abc123", dnsv1alpha1.RRTypeTXT, 4),
		newRecordSet("example-com-cname", "example-com-abc123", dnsv1alpha1.RRTypeCNAME, 1),
		newRecordSet("example-com-soa", "example-com-abc123", dnsv1alpha1.RRTypeSOA, 1),
	))
}

func TestDescribeDelegated(t *testing.T) {
	h := describedZone(t)

	if err := h.run("zone", "describe", "example.com"); err != nil {
		t.Fatalf("zone describe: %v", err)
	}

	want := strings.Join([]string{
		"Zone         example.com                      project: acme-prod",
		"Class        datum-external-global-dns",
		"Created      14d ago",
		"",
		"Status       OK — zone programmed, 12 records live",
		"Delegation   Complete — all 2 nameservers set at the registrar",
		"",
		"Nameservers",
		"  ns1.datum.net.   set at registrar",
		"  ns2.datum.net.   set at registrar",
		"",
		"Records      12 across 6 types",
		"  SOA 1    NS 2    A 3    CNAME 1    MX 1    TXT 4",
		"",
		"Next steps:",
		"  List records:            datumctl dns record list example.com",
		"  Add a record:            datumctl dns record create example.com www A 203.0.113.10",
		"  Export as a zone file:   datumctl dns zone export example.com",
		"",
	}, "\n")

	if got := h.out.String(); got != want {
		t.Errorf("output =\n%s\nwant\n%s", got, want)
	}

	// A fully delegated zone has nothing to instruct.
	if strings.Contains(h.out.String(), "Set these nameservers") {
		t.Error("a complete delegation should not print registrar instructions")
	}
}

func TestDescribeUndelegatedPrintsTheInstruction(t *testing.T) {
	h := newHarness(t, newFakeClient(t,
		newZone("example-com-abc123", "example.com",
			withRecordCount(0),
			delegated("ns-cloud-a1.googledomains.com.", "ns-cloud-a2.googledomains.com.")),
	))

	if err := h.run("zone", "describe", "example.com"); err != nil {
		t.Fatalf("zone describe: %v", err)
	}
	out := h.out.String()

	wantBlock := strings.Join([]string{
		"Delegation   Incomplete — 0 of 2 nameservers set at the registrar",
		"",
		"Nameservers",
		"  ns1.datum.net.   not set at registrar",
		"  ns2.datum.net.   not set at registrar",
		"",
		"Records      none yet",
		"",
		"Set these nameservers at your domain registrar:",
		"  ns1.datum.net.",
		"  ns2.datum.net.",
		"",
		"Currently delegated to:",
		"  ns-cloud-a1.googledomains.com.",
		"  ns-cloud-a2.googledomains.com.",
		"",
		"Re-check with: datumctl dns zone nameservers example.com --check",
	}, "\n")

	if !strings.Contains(out, wantBlock) {
		t.Errorf("output =\n%s\nwant it to contain\n%s", out, wantBlock)
	}
}

func TestDescribePartialDelegation(t *testing.T) {
	h := newHarness(t, newFakeClient(t,
		newZone("example-com-abc123", "example.com",
			delegated("ns1.datum.net.", "ns-cloud-a2.googledomains.com.")),
	))

	if err := h.run("zone", "describe", "example.com"); err != nil {
		t.Fatalf("zone describe: %v", err)
	}
	out := h.out.String()

	if !strings.Contains(out, "Delegation   Partial — 1 of 2 nameservers set at the registrar") {
		t.Errorf("partial delegation is not reported:\n%s", out)
	}
	if !strings.Contains(out, "  ns1.datum.net.   set at registrar") {
		t.Errorf("the nameserver that is set is not annotated:\n%s", out)
	}
	if !strings.Contains(out, "  ns2.datum.net.   not set at registrar") {
		t.Errorf("the nameserver that is missing is not annotated:\n%s", out)
	}
}

func TestDescribeShowsDescription(t *testing.T) {
	z := newZone("example-com-abc123", "example.com")
	z.Annotations = map[string]string{descriptionAnnotation: "production apex"}
	h := newHarness(t, newFakeClient(t, z))

	if err := h.run("zone", "describe", "example.com"); err != nil {
		t.Fatalf("zone describe: %v", err)
	}
	if !strings.Contains(h.out.String(), "Description  production apex") {
		t.Errorf("the description annotation is not rendered:\n%s", h.out.String())
	}
}

func TestDescribeJSONEmitsRawZone(t *testing.T) {
	h := describedZone(t)

	if err := h.run("zone", "describe", "example.com", "-o", "json"); err != nil {
		t.Fatalf("zone describe -o json: %v", err)
	}

	var z dnsv1alpha1.DNSZone
	if err := json.Unmarshal(h.out.Bytes(), &z); err != nil {
		t.Fatalf("output is not a DNSZone: %v\n%s", err, h.out.String())
	}
	if z.Spec.DomainName != "example.com" {
		t.Errorf("domainName = %q, want example.com", z.Spec.DomainName)
	}
	if z.Kind != "DNSZone" {
		t.Errorf("kind = %q, want DNSZone", z.Kind)
	}
	if strings.Contains(h.out.String(), "Next steps") {
		t.Error("-o json must not carry the rendered view")
	}
}

func TestDescribeNotFound(t *testing.T) {
	h := newHarness(t, newFakeClient(t, newZone("example-com-abc123", "example.com")))

	err := h.run("zone", "describe", "missing.com")
	if err == nil {
		t.Fatal("expected an error for a zone that does not exist")
	}
	assertExitCode(t, err, util.ExitNotFound)
}

func TestDescribeRejectsTableFormat(t *testing.T) {
	h := describedZone(t)

	// describe has no table form: wide, json, and yaml are the whole set.
	err := h.run("zone", "describe", "example.com", "-o", "table")
	if err == nil {
		t.Fatal("expected an error for -o table on describe")
	}
	assertExitCode(t, err, util.ExitUsage)
}

// recordsBlock returns the "Records" block of a describe view: the headline
// line, the breakdown, and any note under it. Asserting the rendered block
// rather than the counts is deliberate — the previous regression here was a
// missing breakdown that no count assertion could have caught.
func recordsBlock(t *testing.T, out string) string {
	t.Helper()

	lines := strings.Split(out, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "Records") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("no Records block in:\n%s", out)
	}
	end := start + 1
	for end < len(lines) && strings.HasPrefix(lines[end], "  ") {
		end++
	}
	return strings.Join(lines[start:end], "\n")
}

// describeWith renders a zone carrying the given record sets and returns its
// Records block.
func describeWith(t *testing.T, z *dnsv1alpha1.DNSZone, sets ...*dnsv1alpha1.DNSRecordSet) string {
	t.Helper()

	objs := make([]client.Object, 0, 1+len(sets))
	objs = append(objs, z)
	for _, rs := range sets {
		objs = append(objs, rs)
	}
	h := newHarness(t, newFakeClient(t, objs...))
	if err := h.run("zone", "describe", "example.com"); err != nil {
		t.Fatalf("zone describe: %v", err)
	}
	return recordsBlock(t, h.out.String())
}

func TestDescribeRecordBreakdown(t *testing.T) {
	const obj = "example-com-abc123"

	tests := []struct {
		name  string
		count int
		sets  []*dnsv1alpha1.DNSRecordSet
		want  string
	}{
		{
			name:  "types read in zone-file order, not alphabetical",
			count: 12,
			sets: []*dnsv1alpha1.DNSRecordSet{
				newRecordSet("txt", obj, dnsv1alpha1.RRTypeTXT, 4),
				newRecordSet("a", obj, dnsv1alpha1.RRTypeA, 3),
				newRecordSet("ns", obj, dnsv1alpha1.RRTypeNS, 2),
				newRecordSet("soa", obj, dnsv1alpha1.RRTypeSOA, 1),
				newRecordSet("mx", obj, dnsv1alpha1.RRTypeMX, 1),
				newRecordSet("cname", obj, dnsv1alpha1.RRTypeCNAME, 1),
			},
			want: "Records      12 across 6 types\n" +
				"  SOA 1    NS 2    A 3    CNAME 1    MX 1    TXT 4",
		},
		{
			name:  "a single type is not pluralised",
			count: 3,
			sets: []*dnsv1alpha1.DNSRecordSet{
				newRecordSet("a", obj, dnsv1alpha1.RRTypeA, 3),
			},
			want: "Records      3 across 1 type\n  A 3",
		},
		{
			name:  "types with no records do not appear",
			count: 5,
			sets: []*dnsv1alpha1.DNSRecordSet{
				newRecordSet("a", obj, dnsv1alpha1.RRTypeA, 4),
				newRecordSet("aaaa", obj, dnsv1alpha1.RRTypeAAAA, 1),
				// An empty set contributes no entries and no column.
				newRecordSet("txt", obj, dnsv1alpha1.RRTypeTXT, 0),
			},
			want: "Records      5 across 2 types\n  A 4    AAAA 1",
		},
		{
			name:  "entries are counted, not record sets",
			count: 30,
			sets: []*dnsv1alpha1.DNSRecordSet{
				newRecordSet("a", obj, dnsv1alpha1.RRTypeA, 30),
			},
			want: "Records      30 across 1 type\n  A 30",
		},
		{
			name:  "every supported type at once",
			count: 14,
			sets: []*dnsv1alpha1.DNSRecordSet{
				newRecordSet("svcb", obj, dnsv1alpha1.RRTypeSVCB, 1),
				newRecordSet("https", obj, dnsv1alpha1.RRTypeHTTPS, 1),
				newRecordSet("tlsa", obj, dnsv1alpha1.RRTypeTLSA, 1),
				newRecordSet("ptr", obj, dnsv1alpha1.RRTypePTR, 1),
				newRecordSet("caa", obj, dnsv1alpha1.RRTypeCAA, 1),
				newRecordSet("srv", obj, dnsv1alpha1.RRTypeSRV, 1),
				newRecordSet("txt", obj, dnsv1alpha1.RRTypeTXT, 1),
				newRecordSet("mx", obj, dnsv1alpha1.RRTypeMX, 1),
				newRecordSet("alias", obj, dnsv1alpha1.RRTypeALIAS, 1),
				newRecordSet("cname", obj, dnsv1alpha1.RRTypeCNAME, 1),
				newRecordSet("aaaa", obj, dnsv1alpha1.RRTypeAAAA, 1),
				newRecordSet("a", obj, dnsv1alpha1.RRTypeA, 1),
				newRecordSet("ns", obj, dnsv1alpha1.RRTypeNS, 1),
				newRecordSet("soa", obj, dnsv1alpha1.RRTypeSOA, 1),
			},
			want: "Records      14 across 14 types\n" +
				"  SOA 1    NS 1    A 1    AAAA 1    CNAME 1    ALIAS 1    MX 1    " +
				"TXT 1    SRV 1    CAA 1    PTR 1    TLSA 1    HTTPS 1    SVCB 1",
		},
		{
			name:  "a type the ordering does not know is listed last",
			count: 4,
			sets: []*dnsv1alpha1.DNSRecordSet{
				newRecordSet("a", obj, dnsv1alpha1.RRTypeA, 3),
				newRecordSet("future", obj, dnsv1alpha1.RRType("FUTURE"), 1),
			},
			want: "Records      4 across 2 types\n  A 3    FUTURE 1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			z := newZone(obj, "example.com", withRecordCount(tc.count),
				delegated("ns1.datum.net.", "ns2.datum.net."))
			if got := describeWith(t, z, tc.sets...); got != tc.want {
				t.Errorf("Records block =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

func TestDescribeRecordCountDisagreement(t *testing.T) {
	const obj = "example-com-abc123"

	// status.recordCount trails the record sets while the operator reconciles.
	// Showing a headline that the columns under it do not add up to reads as a
	// counting bug, so the view names the gap instead.
	z := newZone(obj, "example.com", withRecordCount(12),
		delegated("ns1.datum.net.", "ns2.datum.net."))
	got := describeWith(t, z,
		newRecordSet("a", obj, dnsv1alpha1.RRTypeA, 3),
		newRecordSet("mx", obj, dnsv1alpha1.RRTypeMX, 1),
	)

	want := "Records      12 across 2 types\n" +
		"  A 3    MX 1\n" +
		"  the per-type counts add up to 4, not the 12 the zone reports — the operator is still catching up"
	if got != want {
		t.Errorf("Records block =\n%s\nwant\n%s", got, want)
	}
}

func TestDescribeRecordCountWithoutSets(t *testing.T) {
	// The zone reports records but none of its sets came back. A bare total
	// with no explanation is what the previous version printed, and it left the
	// reader wondering where the breakdown went.
	z := newZone("example-com-abc123", "example.com", withRecordCount(12),
		delegated("ns1.datum.net.", "ns2.datum.net."))

	got := describeWith(t, z)
	want := "Records      12\n  no record sets came back for this zone, so there is no per-type breakdown"
	if got != want {
		t.Errorf("Records block =\n%s\nwant\n%s", got, want)
	}
}

func TestDescribeRecordSummaryFallsBackToTheSets(t *testing.T) {
	const obj = "example-com-abc123"

	// A zone reconciled moments ago can still report 0 while its sets exist.
	// Claiming "none yet" there would be wrong.
	z := newZone(obj, "example.com", delegated("ns1.datum.net.", "ns2.datum.net."))
	got := describeWith(t, z, newRecordSet("a", obj, dnsv1alpha1.RRTypeA, 2))

	want := "Records      2 across 1 type\n  A 2"
	if got != want {
		t.Errorf("Records block =\n%s\nwant\n%s", got, want)
	}
}

func TestDescribeRecordSummaryEmptyZone(t *testing.T) {
	z := newZone("example-com-abc123", "example.com",
		delegated("ns1.datum.net.", "ns2.datum.net."))

	if got, want := describeWith(t, z), "Records      none yet"; got != want {
		t.Errorf("Records block = %q, want %q", got, want)
	}
}

// TestDescribeRecordsBlockWhenTheListingIsDenied covers the contradiction that
// omitting the block produced: the Status line above it still asserts a record
// count from status, so a reader saw "12 records live" and no Records block and
// could not tell "no records" from "not allowed to look".
func TestDescribeRecordsBlockWhenTheListingIsDenied(t *testing.T) {
	c := newFakeClientWith(t, denyRecordSetList(),
		newZone("example-com-abc123", "example.com", withRecordCount(12),
			delegated("ns1.datum.net.", "ns2.datum.net.")),
	)
	h := newHarness(t, c)

	if err := h.run("zone", "describe", "example.com"); err != nil {
		t.Fatalf("zone describe: %v", err)
	}
	out := h.out.String()

	if !strings.Contains(out, "Status       OK — zone programmed, 12 records live") {
		t.Fatalf("the Status line no longer asserts a count:\n%s", out)
	}
	want := "Records      12\n" +
		"  the per-type breakdown is unavailable — you are not authorized to list record sets in this project"
	if got := recordsBlock(t, out); got != want {
		t.Errorf("Records block =\n%s\nwant\n%s", got, want)
	}
}

func TestDescribeOutputName(t *testing.T) {
	// The root advertises -o name on every command and completes it; describe
	// rejecting it was a trap for a script setting the flag globally.
	h := describedZone(t)

	if err := h.run("zone", "describe", "example.com", "-o", "name"); err != nil {
		t.Fatalf("zone describe -o name: %v", err)
	}
	if got, want := h.out.String(), "example.com\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}
