// SPDX-License-Identifier: AGPL-3.0-only

package zone

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// populatedZones is the fixture behind the list goldens: one healthy zone, one
// still coming up, and one the backend rejected.
func populatedZones() []*dnsv1alpha1.DNSZone {
	return []*dnsv1alpha1.DNSZone{
		newZone("example-com-abc123", "example.com",
			withRecordCount(12),
			delegated("ns1.datum.net.", "ns2.datum.net.")),
		newZone("staging-acme-io-def456", "staging.acme.io",
			pending(), withRecordCount(2), withAge(3*time.Minute)),
		newZone("old-acme-io-ghi789", "old.acme.io",
			broken(),
			withRecordCount(8), withAge(21*24*time.Hour),
			delegated("ns-cloud-a1.googledomains.com.")),
	}
}

func populatedClient(t *testing.T) *harness {
	t.Helper()
	zones := populatedZones()
	return newHarness(t, newFakeClient(t, zones[0], zones[1], zones[2]))
}

func TestListTable(t *testing.T) {
	h := populatedClient(t)

	if err := h.run("zone", "list"); err != nil {
		t.Fatalf("zone list: %v", err)
	}

	want := strings.Join([]string{
		"NAME              STATUS    RECORDS   NAMESERVERS                      DELEGATED   AGE",
		"example.com       OK        12        ns1.datum.net., ns2.datum.net.   yes         14d",
		"old.acme.io       Error     8         ns1.datum.net., ns2.datum.net.   no          21d",
		"staging.acme.io   Pending   2         —                                unknown     3m",
		"",
		"3 zones — 1 OK, 1 Pending, 0 Rejected, 1 Error",
		"",
	}, "\n")

	if got := h.out.String(); got != want {
		t.Errorf("output =\n%s\nwant\n%s", got, want)
	}
}

func TestListBareGroupListsToo(t *testing.T) {
	h := populatedClient(t)

	if err := h.run("zone"); err != nil {
		t.Fatalf("zone: %v", err)
	}
	if !strings.Contains(h.out.String(), "3 zones — 1 OK, 1 Pending, 0 Rejected, 1 Error") {
		t.Errorf("bare group did not list:\n%s", h.out.String())
	}
}

func TestListWideAddsClassAndDomain(t *testing.T) {
	h := populatedClient(t)

	if err := h.run("zone", "list", "-o", "wide"); err != nil {
		t.Fatalf("zone list -o wide: %v", err)
	}

	out := h.out.String()
	header := strings.SplitN(out, "\n", 2)[0]
	for _, col := range []string{"CLASS", "DOMAIN"} {
		if !strings.Contains(header, col) {
			t.Errorf("wide header %q is missing %s", header, col)
		}
	}
	if !strings.Contains(out, DefaultZoneClass) {
		t.Errorf("wide output does not show the class:\n%s", out)
	}
	// The linked Domain object, not the domain name.
	if !strings.Contains(out, "example-com") {
		t.Errorf("wide output does not show the linked domain:\n%s", out)
	}
}

func TestListNoHeaders(t *testing.T) {
	h := populatedClient(t)

	if err := h.run("zone", "list", "--no-headers"); err != nil {
		t.Fatalf("zone list --no-headers: %v", err)
	}
	if strings.Contains(h.out.String(), "NAME") {
		t.Errorf("--no-headers still printed a header row:\n%s", h.out.String())
	}
}

func TestListStatusFilter(t *testing.T) {
	tests := []struct {
		name    string
		filter  string
		want    []string
		notWant []string
		footer  string
	}{
		{
			name:    "ok",
			filter:  "ok",
			want:    []string{"example.com"},
			notWant: []string{"staging.acme.io", "old.acme.io"},
			footer:  "1 zone — 1 OK, 0 Pending, 0 Rejected, 0 Error",
		},
		{
			name:    "pending",
			filter:  "pending",
			want:    []string{"staging.acme.io"},
			notWant: []string{"example.com"},
			footer:  "1 zone — 0 OK, 1 Pending, 0 Rejected, 0 Error",
		},
		{
			name:    "error",
			filter:  "error",
			want:    []string{"old.acme.io"},
			notWant: []string{"example.com"},
			footer:  "1 zone — 0 OK, 0 Pending, 0 Rejected, 1 Error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := populatedClient(t)
			if err := h.run("zone", "list", "--status", tc.filter); err != nil {
				t.Fatalf("zone list --status %s: %v", tc.filter, err)
			}
			out := h.out.String()
			for _, s := range tc.want {
				if !strings.Contains(out, s) {
					t.Errorf("output is missing %q:\n%s", s, out)
				}
			}
			for _, s := range tc.notWant {
				if strings.Contains(out, s) {
					t.Errorf("output should not contain %q:\n%s", s, out)
				}
			}
			if !strings.Contains(out, tc.footer) {
				t.Errorf("footer is not %q:\n%s", tc.footer, out)
			}
		})
	}
}

// TestListRejectedIsItsOwnBucket pins the distinction the footer used to lose.
// A row that reads "Rejected" counted under "Error" contradicted the table
// directly above it, and the two states need different things from the user:
// a rejected zone's object has to be deleted, while an error may yet recover.
func TestListRejectedIsItsOwnBucket(t *testing.T) {
	h := newHarness(t, newFakeClient(t,
		newZone("example-com-abc123", "example.com", withRecordCount(12),
			delegated("ns1.datum.net.", "ns2.datum.net.")),
		newZone("claimed-com-def456", "claimed.com",
			rejected(), withAge(2*time.Hour)),
		newZone("old-acme-io-ghi789", "old.acme.io",
			broken(), withAge(21*24*time.Hour)),
	))

	if err := h.run("zone", "list"); err != nil {
		t.Fatalf("zone list: %v", err)
	}
	out := h.out.String()

	if !strings.Contains(out, "3 zones — 1 OK, 0 Pending, 1 Rejected, 1 Error") {
		t.Errorf("footer does not count Rejected separately:\n%s", out)
	}
	// The row and the footer have to agree.
	if !strings.Contains(out, "claimed.com") || !strings.Contains(out, "Rejected") {
		t.Errorf("the rejected zone is not shown as Rejected:\n%s", out)
	}
}

// TestListStatusErrorExcludesRejected is the other half of the ruling: the
// filter token stays exact, so `--status error` never sweeps up a zone the
// table calls Rejected.
func TestListStatusErrorExcludesRejected(t *testing.T) {
	h := newHarness(t, newFakeClient(t,
		newZone("claimed-com-def456", "claimed.com", rejected()),
		newZone("old-acme-io-ghi789", "old.acme.io", broken()),
	))

	if err := h.run("zone", "list", "--status", "error"); err != nil {
		t.Fatalf("zone list --status error: %v", err)
	}
	out := h.out.String()

	if strings.Contains(out, "claimed.com") {
		t.Errorf("--status error matched a Rejected zone:\n%s", out)
	}
	if !strings.Contains(out, "old.acme.io") {
		t.Errorf("--status error did not match the Error zone:\n%s", out)
	}
	if !strings.Contains(out, "1 zone — 0 OK, 0 Pending, 0 Rejected, 1 Error") {
		t.Errorf("footer =\n%s", out)
	}
}

func TestListStatusRejectedMatchesOnlyRejected(t *testing.T) {
	h := newHarness(t, newFakeClient(t,
		newZone("claimed-com-def456", "claimed.com", rejected()),
		newZone("old-acme-io-ghi789", "old.acme.io", broken()),
	))

	if err := h.run("zone", "list", "--status", "rejected"); err != nil {
		t.Fatalf("zone list --status rejected: %v", err)
	}
	out := h.out.String()

	if !strings.Contains(out, "claimed.com") || strings.Contains(out, "old.acme.io") {
		t.Errorf("--status rejected did not select exactly the Rejected zone:\n%s", out)
	}
	if !strings.Contains(out, "1 zone — 0 OK, 0 Pending, 1 Rejected, 0 Error") {
		t.Errorf("footer =\n%s", out)
	}
}

func TestListRejectsUnknownStatusFilter(t *testing.T) {
	h := populatedClient(t)

	err := h.run("zone", "list", "--status", "broken")
	if err == nil {
		t.Fatal("expected an error for an unknown status filter")
	}
	assertExitCode(t, err, util.ExitUsage)
}

func TestListEmpty(t *testing.T) {
	h := newHarness(t, newFakeClient(t))

	if err := h.run("zone", "list"); err != nil {
		t.Fatalf("zone list on an empty project: %v", err)
	}

	want := strings.Join([]string{
		"No DNS zones found in project acme-prod.",
		"",
		"Get started:",
		"  datumctl dns zone create example.com",
		"",
	}, "\n")
	if got := h.out.String(); got != want {
		t.Errorf("output =\n%q\nwant\n%q", got, want)
	}
}

func TestListFilteredEmptyNamesTheFilter(t *testing.T) {
	h := newHarness(t, newFakeClient(t, newZone("example-com-abc123", "example.com")))

	if err := h.run("zone", "list", "--status", "error"); err != nil {
		t.Fatalf("zone list --status error: %v", err)
	}

	want := "No DNS zones in project acme-prod match status=error.\n"
	if got := h.out.String(); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if strings.Contains(h.out.String(), "Get started") {
		t.Error("a filtered-empty list should not print the getting-started block")
	}
}

func TestListJSONEmitsRawAPIObjects(t *testing.T) {
	h := populatedClient(t)

	if err := h.run("zone", "list", "-o", "json"); err != nil {
		t.Fatalf("zone list -o json: %v", err)
	}

	var list dnsv1alpha1.DNSZoneList
	if err := json.Unmarshal(h.out.Bytes(), &list); err != nil {
		t.Fatalf("output is not a DNSZoneList: %v\n%s", err, h.out.String())
	}
	if len(list.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(list.Items))
	}
	if list.Items[0].Spec.DNSZoneClassName == "" {
		t.Error("raw API fields are missing from -o json")
	}
	if strings.Contains(h.out.String(), "DELEGATED") {
		t.Error("-o json must not carry the rendered table")
	}
}

func TestListJSONIgnoresFiltering(t *testing.T) {
	h := populatedClient(t)

	// The machine contract is the raw list the API served, dispatched before
	// any client-side work — including the filter.
	if err := h.run("zone", "list", "-o", "json", "--status", "ok"); err != nil {
		t.Fatalf("zone list -o json --status ok: %v", err)
	}
	var list dnsv1alpha1.DNSZoneList
	if err := json.Unmarshal(h.out.Bytes(), &list); err != nil {
		t.Fatalf("output is not a DNSZoneList: %v", err)
	}
	if len(list.Items) != 3 {
		t.Errorf("items = %d, want all 3 unfiltered", len(list.Items))
	}
}

func TestListYAML(t *testing.T) {
	h := populatedClient(t)

	if err := h.run("zone", "list", "-o", "yaml"); err != nil {
		t.Fatalf("zone list -o yaml: %v", err)
	}
	out := h.out.String()
	for _, want := range []string{"kind: DNSZoneList", "domainName: example.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("yaml output is missing %q:\n%s", want, out)
		}
	}
}

func TestListName(t *testing.T) {
	h := populatedClient(t)

	if err := h.run("zone", "list", "-o", "name"); err != nil {
		t.Fatalf("zone list -o name: %v", err)
	}

	want := "example.com\nold.acme.io\nstaging.acme.io\n"
	if got := h.out.String(); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestListNameOnEmptyPrintsNothing(t *testing.T) {
	h := newHarness(t, newFakeClient(t))

	if err := h.run("zone", "list", "-o", "name"); err != nil {
		t.Fatalf("zone list -o name: %v", err)
	}
	if h.out.Len() != 0 {
		t.Errorf("-o name on an empty project printed %q, want nothing", h.out.String())
	}
}

func TestListRejectsUnknownOutputFormat(t *testing.T) {
	h := populatedClient(t)

	err := h.run("zone", "list", "-o", "toml")
	if err == nil {
		t.Fatal("expected an error for an unknown output format")
	}
	assertExitCode(t, err, util.ExitUsage)
}
