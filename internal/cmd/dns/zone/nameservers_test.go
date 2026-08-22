// SPDX-License-Identifier: AGPL-3.0-only

package zone

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// testDelegationSource stands in for the "as delegated by the com nameservers"
// provenance the real lookup reports.
const testDelegationSource = "as delegated by the com nameservers"

// stubProbes replaces the live DNS queries for the duration of a test.
func stubProbes(t *testing.T, probes map[string]nsProbe, public []string, publicErr error) {
	t.Helper()

	prevProbe, prevLookup := probeNameserver, lookupPublicNS
	probeNameserver = func(_ context.Context, server, _ string, _ time.Duration) nsProbe {
		if p, known := probes[server]; known {
			p.Server = server
			return p
		}
		return nsProbe{Server: server, State: probeUnreachable, Detail: "no stub"}
	}
	lookupPublicNS = func(context.Context, string, time.Duration) ([]string, string, error) {
		return public, testDelegationSource, publicErr
	}
	t.Cleanup(func() {
		probeNameserver, lookupPublicNS = prevProbe, prevLookup
	})
}

func TestNameservers(t *testing.T) {
	h := newHarness(t, newFakeClient(t,
		newZone("example-com-abc123", "example.com",
			delegated("ns1.datum.net.", "ns2.datum.net.")),
	))

	if err := h.run("zone", "nameservers", "example.com"); err != nil {
		t.Fatalf("zone nameservers: %v", err)
	}

	want := strings.Join([]string{
		"Nameservers for example.com",
		"  ns1.datum.net.   set at registrar",
		"  ns2.datum.net.   set at registrar",
		"",
		"Delegation   Complete — all 2 nameservers set at the registrar",
		"",
	}, "\n")
	if got := h.out.String(); got != want {
		t.Errorf("output =\n%s\nwant\n%s", got, want)
	}
}

func TestNameserversUndelegatedInstructs(t *testing.T) {
	h := newHarness(t, newFakeClient(t,
		newZone("example-com-abc123", "example.com",
			delegated("ns-cloud-a1.googledomains.com.")),
	))

	if err := h.run("zone", "ns", "example.com"); err != nil {
		t.Fatalf("zone ns: %v", err)
	}
	out := h.out.String()

	for _, want := range []string{
		"Delegation   Incomplete — 0 of 2 nameservers set at the registrar",
		"Set these nameservers at your domain registrar:",
		"Currently delegated to:\n  ns-cloud-a1.googledomains.com.",
		"Re-check with: datumctl dns zone nameservers example.com --check",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

func TestNameserversCheckReportsLiveResolution(t *testing.T) {
	stubProbes(t,
		map[string]nsProbe{
			"ns1.datum.net.": {State: probeAuthoritative, Detail: "2 NS records for example.com"},
			"ns2.datum.net.": {State: probeUnreachable, Detail: "i/o timeout"},
		},
		[]string{"ns1.datum.net.", "ns2.datum.net."}, nil)

	h := newHarness(t, newFakeClient(t,
		newZone("example-com-abc123", "example.com",
			delegated("ns1.datum.net.", "ns2.datum.net.")),
	))

	if err := h.run("zone", "nameservers", "example.com", "--check"); err != nil {
		t.Fatalf("zone nameservers --check: %v", err)
	}
	out := h.out.String()

	wantBlock := strings.Join([]string{
		"Live check",
		"  ns1.datum.net.   authoritative — 2 NS records for example.com",
		"  ns2.datum.net.   unreachable — i/o timeout",
		"",
		"Public delegation (" + testDelegationSource + ")",
		"  ns1.datum.net.",
		"  ns2.datum.net.",
		"",
		"  Public DNS delegates example.com to its assigned nameservers.",
	}, "\n")
	if !strings.Contains(out, wantBlock) {
		t.Errorf("output =\n%s\nwant it to contain\n%s", out, wantBlock)
	}
}

func TestNameserversCheckReportsAStaleDelegation(t *testing.T) {
	stubProbes(t,
		map[string]nsProbe{
			"ns1.datum.net.": {State: probeAuthoritative, Detail: "2 NS records for example.com"},
			"ns2.datum.net.": {State: probeAuthoritative, Detail: "2 NS records for example.com"},
		},
		[]string{"ns-cloud-a1.googledomains.com."}, nil)

	h := newHarness(t, newFakeClient(t,
		newZone("example-com-abc123", "example.com"),
	))

	if err := h.run("zone", "nameservers", "example.com", "--check"); err != nil {
		t.Fatalf("zone nameservers --check: %v", err)
	}
	out := h.out.String()

	// The control plane is happy and the zone is served; the registrar has
	// simply not been pointed at it yet. That is the distinction --check
	// exists to draw.
	if !strings.Contains(out, "  Public DNS does not yet delegate example.com to its assigned nameservers.") {
		t.Errorf("output does not report the stale delegation:\n%s", out)
	}
	if !strings.Contains(out, "Registrar changes can take up to 48 hours to propagate.") {
		t.Errorf("output does not explain propagation:\n%s", out)
	}
}

func TestNameserversCheckWithNoPublicRecords(t *testing.T) {
	stubProbes(t, map[string]nsProbe{
		"ns1.datum.net.": {State: probeAuthoritative, Detail: "2 NS records for example.com"},
		"ns2.datum.net.": {State: probeAuthoritative, Detail: "2 NS records for example.com"},
	}, nil, nil)

	h := newHarness(t, newFakeClient(t, newZone("example-com-abc123", "example.com")))

	if err := h.run("zone", "nameservers", "example.com", "--check"); err != nil {
		t.Fatalf("zone nameservers --check: %v", err)
	}
	if !strings.Contains(h.out.String(), "example.com has no NS records in public DNS yet") {
		t.Errorf("output does not report the missing delegation:\n%s", h.out.String())
	}
}

func TestNameserversCheckSurvivesAResolverFailure(t *testing.T) {
	// A broken local resolver must not fail the command: the control-plane
	// half of the answer is still worth printing.
	stubProbes(t, nil, nil, errors.New("no such host"))

	h := newHarness(t, newFakeClient(t, newZone("example-com-abc123", "example.com")))

	if err := h.run("zone", "nameservers", "example.com", "--check"); err != nil {
		t.Fatalf("zone nameservers --check: %v", err)
	}
	if !strings.Contains(h.out.String(), "could not resolve NS for example.com — no such host") {
		t.Errorf("output does not report the resolver failure:\n%s", h.out.String())
	}
}

func TestNameserversCheckWithNoAssignedNameservers(t *testing.T) {
	stubProbes(t, nil, nil, nil)

	h := newHarness(t, newFakeClient(t,
		newZone("example-com-abc123", "example.com", pending()),
	))

	if err := h.run("zone", "nameservers", "example.com", "--check"); err != nil {
		t.Fatalf("zone nameservers --check: %v", err)
	}
	if !strings.Contains(h.out.String(), "no nameservers assigned yet — nothing to query") {
		t.Errorf("output does not explain there is nothing to query:\n%s", h.out.String())
	}
}

func TestNameserversNotFound(t *testing.T) {
	h := newHarness(t, newFakeClient(t, newZone("example-com-abc123", "example.com")))

	err := h.run("zone", "nameservers", "missing.com")
	if err == nil {
		t.Fatal("expected an error for a zone that does not exist")
	}
	assertExitCode(t, err, util.ExitNotFound)
}

func TestPublicMatchesExpected(t *testing.T) {
	tests := []struct {
		name     string
		public   []string
		expected []string
		want     bool
	}{
		{
			name:     "exact match",
			public:   []string{"ns1.datum.net.", "ns2.datum.net."},
			expected: []string{"ns1.datum.net.", "ns2.datum.net."},
			want:     true,
		},
		{
			name:     "trailing dots and case are normalized",
			public:   []string{"NS1.Datum.net", "ns2.datum.net"},
			expected: []string{"ns1.datum.net.", "ns2.datum.net."},
			want:     true,
		},
		{
			name:     "extra nameservers at the registrar are fine",
			public:   []string{"ns1.datum.net.", "ns2.datum.net.", "ns1.old-provider.net."},
			expected: []string{"ns1.datum.net.", "ns2.datum.net."},
			want:     true,
		},
		{
			name:     "a missing nameserver is a mismatch",
			public:   []string{"ns1.datum.net."},
			expected: []string{"ns1.datum.net.", "ns2.datum.net."},
			want:     false,
		},
		{
			name:     "nothing assigned is never a match",
			public:   []string{"ns1.datum.net."},
			expected: nil,
			want:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := publicMatchesExpected(tc.public, tc.expected); got != tc.want {
				t.Errorf("publicMatchesExpected(%v, %v) = %v, want %v",
					tc.public, tc.expected, got, tc.want)
			}
		})
	}
}

// TestNameserversCheckOffersTheFixItJustEarned is the regression guard for a
// check that diagnosed the problem and withheld the remedy. The control plane
// knows nothing here — no observed Domain, so the state is correctly Unknown —
// and the live query is the only evidence there is. It has to be enough.
func TestNameserversCheckOffersTheFixItJustEarned(t *testing.T) {
	stubProbes(t,
		map[string]nsProbe{
			"ns1.datum.net.": {State: probeAuthoritative, Detail: "2 NS records for example.com"},
			"ns2.datum.net.": {State: probeAuthoritative, Detail: "2 NS records for example.com"},
		},
		[]string{"ns-cloud-a1.googledomains.com."}, nil)

	h := newHarness(t, newFakeClient(t, newZone("example-com-abc123", "example.com")))
	if err := h.run("zone", "nameservers", "example.com", "--check"); err != nil {
		t.Fatalf("zone nameservers --check: %v", err)
	}
	out := h.out.String()

	if !strings.Contains(out, "Public DNS does not yet delegate example.com") {
		t.Fatalf("the live check did not report the problem:\n%s", out)
	}

	wantFix := strings.Join([]string{
		"Set these nameservers at your domain registrar:",
		"  ns1.datum.net.",
		"  ns2.datum.net.",
		"",
		"Currently delegated to:",
		"  ns-cloud-a1.googledomains.com.",
	}, "\n")
	if !strings.Contains(out, wantFix) {
		t.Errorf("the check found the problem and withheld the remedy:\n%s\nwant it to contain\n%s", out, wantFix)
	}
	// The live answer is real evidence; the block must not fall back to
	// "unknown" when the query just said what the registrar publishes.
	if strings.Contains(out, "no registrar nameservers observed") {
		t.Errorf("the instruction block ignored the live observation:\n%s", out)
	}
}

func TestNameserversCheckWithNoPublicDelegationStillInstructs(t *testing.T) {
	// No NS at all in public DNS is also something to act on.
	stubProbes(t, map[string]nsProbe{
		"ns1.datum.net.": {State: probeAuthoritative, Detail: "2 NS records for example.com"},
		"ns2.datum.net.": {State: probeAuthoritative, Detail: "2 NS records for example.com"},
	}, nil, nil)

	h := newHarness(t, newFakeClient(t, newZone("example-com-abc123", "example.com")))
	if err := h.run("zone", "nameservers", "example.com", "--check"); err != nil {
		t.Fatalf("zone nameservers --check: %v", err)
	}
	if !strings.Contains(h.out.String(), "Set these nameservers at your domain registrar:") {
		t.Errorf("a domain with no delegation at all got no instruction:\n%s", h.out.String())
	}
}

func TestNameserversCheckStaysQuietWhenLiveDNSAgrees(t *testing.T) {
	// Live evidence may earn the instruction block; it must not manufacture one
	// when the delegation is already correct.
	stubProbes(t, map[string]nsProbe{
		"ns1.datum.net.": {State: probeAuthoritative, Detail: "2 NS records for example.com"},
		"ns2.datum.net.": {State: probeAuthoritative, Detail: "2 NS records for example.com"},
	}, []string{"ns1.datum.net.", "ns2.datum.net."}, nil)

	h := newHarness(t, newFakeClient(t, newZone("example-com-abc123", "example.com")))
	if err := h.run("zone", "nameservers", "example.com", "--check"); err != nil {
		t.Fatalf("zone nameservers --check: %v", err)
	}
	out := h.out.String()
	if !strings.Contains(out, "Public DNS delegates example.com to its assigned nameservers.") {
		t.Fatalf("live check did not confirm the delegation:\n%s", out)
	}
	if strings.Contains(out, "Set these nameservers at your domain registrar:") {
		t.Errorf("a correct delegation was told to fix itself:\n%s", out)
	}
}

func TestNameserversWithoutCheckStaysQuietOnUnknown(t *testing.T) {
	// Without --check there is no live evidence, so an unobserved registrar
	// still gets no instruction. This is the fix from the previous round, and
	// the live-evidence gate must not have reopened it.
	h := newHarness(t, newFakeClient(t, newZone("example-com-abc123", "example.com")))
	if err := h.run("zone", "nameservers", "example.com"); err != nil {
		t.Fatalf("zone nameservers: %v", err)
	}
	if strings.Contains(h.out.String(), "Set these nameservers at your domain registrar:") {
		t.Errorf("an unobserved registrar was told to fix itself without any live check:\n%s", h.out.String())
	}
}

func TestParentZone(t *testing.T) {
	tests := []struct{ in, want string }{
		{"example.com", "com"},
		{"sub.example.com", "example.com"},
		{"Example.COM.", "com"},
		{"com", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := parentZone(tc.in); got != tc.want {
			t.Errorf("parentZone(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
