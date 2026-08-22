// SPDX-License-Identifier: AGPL-3.0-only

package zone

import (
	"strings"
	"testing"

	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// The delegation views make a claim about a third party's configuration, so
// every state is asserted as the whole rendered block rather than as a state
// word. The bug these tests exist to prevent was not a wrong state value — the
// state was right — it was three sentences of rendering that asserted facts the
// state did not support.

// linkedButUnobserved gives the zone a Domain object that publishes no
// nameservers. This is the ordinary condition of a zone in the minutes after it
// is created: the Domain exists, nobody has looked at the registrar yet.
func linkedButUnobserved() zoneOption { return delegated() }

func TestDelegationRendering(t *testing.T) {
	tests := []struct {
		name string
		opts []zoneOption
		want string
	}{
		{
			name: "complete — every nameserver observed at the registrar",
			opts: []zoneOption{delegated("ns1.datum.net.", "ns2.datum.net.")},
			want: strings.Join([]string{
				"Nameservers for example.com",
				"  ns1.datum.net.   set at registrar",
				"  ns2.datum.net.   set at registrar",
				"",
				"Delegation   Complete — all 2 nameservers set at the registrar",
				"",
			}, "\n"),
		},
		{
			name: "partial — one of two observed",
			opts: []zoneOption{delegated("ns1.datum.net.", "ns-cloud-a1.googledomains.com.")},
			want: strings.Join([]string{
				"Nameservers for example.com",
				"  ns1.datum.net.   set at registrar",
				"  ns2.datum.net.   not set at registrar",
				"",
				"Delegation   Partial — 1 of 2 nameservers set at the registrar",
				"",
				"Set these nameservers at your domain registrar:",
				"  ns1.datum.net.",
				"  ns2.datum.net.",
				"",
				"Currently delegated to:",
				"  ns1.datum.net.",
				"  ns-cloud-a1.googledomains.com.",
				"",
				"Re-check with: datumctl dns zone nameservers example.com --check",
				"",
			}, "\n"),
		},
		{
			// The case that distinguishes Incomplete from Unknown: the
			// registrar WAS observed, and it points somewhere else. Here the
			// instruction is earned.
			name: "incomplete — the registrar was observed pointing elsewhere",
			opts: []zoneOption{delegated("ns-cloud-a1.googledomains.com.", "ns-cloud-a2.googledomains.com.")},
			want: strings.Join([]string{
				"Nameservers for example.com",
				"  ns1.datum.net.   not set at registrar",
				"  ns2.datum.net.   not set at registrar",
				"",
				"Delegation   Incomplete — 0 of 2 nameservers set at the registrar",
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
				"",
			}, "\n"),
		},
		{
			// A Domain exists and nobody has checked it. Nothing here may
			// claim the registrar is wrong, and nothing may tell the user to
			// go change it.
			name: "unknown — linked domain, registrar not observed yet",
			opts: []zoneOption{linkedButUnobserved()},
			want: strings.Join([]string{
				"Nameservers for example.com",
				"  ns1.datum.net.   unknown",
				"  ns2.datum.net.   unknown",
				"",
				"Delegation   Unknown — the registrar's nameservers have not been checked yet",
				"",
			}, "\n"),
		},
		{
			name: "unknown — no linked domain at all",
			opts: nil,
			want: strings.Join([]string{
				"Nameservers for example.com",
				"  ns1.datum.net.   unknown",
				"  ns2.datum.net.   unknown",
				"",
				"Delegation   Unknown — no linked domain to check the registrar against",
				"",
			}, "\n"),
		},
		{
			name: "unknown — no nameservers assigned yet",
			opts: []zoneOption{pending()},
			want: strings.Join([]string{
				"Nameservers for example.com",
				"  none assigned yet",
				"",
				"Delegation   Unknown — no nameservers assigned yet",
				"",
			}, "\n"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, newFakeClient(t, newZone("example-com-abc123", "example.com", tc.opts...)))
			if err := h.run("zone", "nameservers", "example.com"); err != nil {
				t.Fatalf("zone nameservers: %v", err)
			}
			if got := h.out.String(); got != tc.want {
				t.Errorf("output =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// TestDelegationNeverInventsRegistrarFacts is the regression guard stated as a
// rule rather than as a layout: in any state where the registrar has not been
// observed, no output may say the nameservers are missing from it, and no
// output may tell the user to go and change it.
func TestDelegationNeverInventsRegistrarFacts(t *testing.T) {
	unobserved := map[string][]zoneOption{
		"linked but unobserved": {linkedButUnobserved()},
		"no linked domain":      nil,
		"no nameservers":        {pending()},
	}

	forbidden := []string{
		nsNotSetAtRegistrar,
		"Set these nameservers at your domain registrar:",
		"Currently delegated to:",
	}

	for name, opts := range unobserved {
		for _, cmd := range []string{"nameservers", "describe"} {
			t.Run(name+"/"+cmd, func(t *testing.T) {
				h := newHarness(t, newFakeClient(t,
					newZone("example-com-abc123", "example.com", opts...)))
				if err := h.run("zone", cmd, "example.com"); err != nil {
					t.Fatalf("zone %s: %v", cmd, err)
				}
				out := h.out.String()
				for _, phrase := range forbidden {
					if strings.Contains(out, phrase) {
						t.Errorf("output claims %q with an unobserved registrar:\n%s", phrase, out)
					}
				}
			})
		}
	}
}

// TestDescribeDelegationBlock covers the same states through describe, which
// renders the summary and the nameserver list through the same helpers but
// gates the instruction block separately.
func TestDescribeDelegationBlock(t *testing.T) {
	tests := []struct {
		name            string
		opts            []zoneOption
		wantSummary     string
		wantAnnotation  string
		wantInstruction bool
	}{
		{
			name:            "complete",
			opts:            []zoneOption{delegated("ns1.datum.net.", "ns2.datum.net.")},
			wantSummary:     "Delegation   Complete — all 2 nameservers set at the registrar",
			wantAnnotation:  "  ns1.datum.net.   set at registrar",
			wantInstruction: false,
		},
		{
			name:            "incomplete",
			opts:            []zoneOption{delegated("ns-cloud-a1.googledomains.com.")},
			wantSummary:     "Delegation   Incomplete — 0 of 2 nameservers set at the registrar",
			wantAnnotation:  "  ns1.datum.net.   not set at registrar",
			wantInstruction: true,
		},
		{
			name:            "unknown, linked but unobserved",
			opts:            []zoneOption{linkedButUnobserved()},
			wantSummary:     "Delegation   Unknown — the registrar's nameservers have not been checked yet",
			wantAnnotation:  "  ns1.datum.net.   unknown",
			wantInstruction: false,
		},
		{
			name:            "unknown, not linked",
			opts:            nil,
			wantSummary:     "Delegation   Unknown — no linked domain to check the registrar against",
			wantAnnotation:  "  ns1.datum.net.   unknown",
			wantInstruction: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, newFakeClient(t,
				newZone("example-com-abc123", "example.com", tc.opts...)))
			if err := h.run("zone", "describe", "example.com"); err != nil {
				t.Fatalf("zone describe: %v", err)
			}
			out := h.out.String()

			if !strings.Contains(out, tc.wantSummary) {
				t.Errorf("summary line missing %q:\n%s", tc.wantSummary, out)
			}
			if !strings.Contains(out, tc.wantAnnotation) {
				t.Errorf("nameserver annotation missing %q:\n%s", tc.wantAnnotation, out)
			}
			gotInstruction := strings.Contains(out, "Set these nameservers at your domain registrar:")
			if gotInstruction != tc.wantInstruction {
				t.Errorf("instruction block present = %v, want %v:\n%s", gotInstruction, tc.wantInstruction, out)
			}
		})
	}
}

// TestListDelegatedColumn checks the same distinction survives into the table,
// where "no" and "unknown" are one word apart and mean opposite things.
func TestListDelegatedColumn(t *testing.T) {
	tests := []struct {
		name string
		opts []zoneOption
		want string
	}{
		{name: "complete", opts: []zoneOption{delegated("ns1.datum.net.", "ns2.datum.net.")}, want: "yes"},
		{name: "incomplete", opts: []zoneOption{delegated("ns-cloud-a1.googledomains.com.")}, want: "no"},
		{name: "partial", opts: []zoneOption{delegated("ns1.datum.net.", "other.example.net.")}, want: "partial (1/2)"},
		{name: "linked but unobserved", opts: []zoneOption{linkedButUnobserved()}, want: "unknown"},
		{name: "not linked", opts: nil, want: "unknown"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			z := newZone("example-com-abc123", "example.com", tc.opts...)
			if got := delegatedCell(util.DelegationState(z)); got != tc.want {
				t.Errorf("DELEGATED cell = %q, want %q", got, tc.want)
			}
		})
	}
}
