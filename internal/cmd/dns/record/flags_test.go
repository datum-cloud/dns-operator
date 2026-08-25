// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"testing"

	"github.com/spf13/cobra"

	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
)

// TestMutationCommandsCarryTheUnionOfRdataFlags — the record type is a
// positional, and cobra has no hook between resolving the command and parsing
// its flags, so the flag set has to hold every type's flags at once.
func TestMutationCommandsCarryTheUnionOfRdataFlags(t *testing.T) {
	for _, cmd := range []*cobra.Command{createCommand(), setCommand()} {
		t.Run(cmd.Name(), func(t *testing.T) {
			for _, name := range allRdataFlagNames() {
				if cmd.Flags().Lookup(name) == nil {
					t.Errorf("--%s is not registered", name)
				}
			}
			for _, name := range []string{"ttl", "line", "wait", "timeout", "dry-run", "force"} {
				if cmd.Flags().Lookup(name) == nil {
					t.Errorf("--%s is not registered", name)
				}
			}
		})
	}
}

// TestUnionFlagsAgreeOnTypeWhereTheyShareAName — the union is only well defined
// because a shared flag name means the same thing everywhere it appears.
func TestUnionFlagsAgreeOnTypeWhereTheyShareAName(t *testing.T) {
	cmd := createCommand()
	want := map[string]string{
		rdata.FlagPriority: "uint16",
		rdata.FlagTarget:   "string",
		rdata.FlagParam:    "stringArray",
		rdata.FlagFlag:     "uint8",
		rdata.FlagValue:    "string",
	}
	for name, kind := range want {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			t.Fatalf("--%s is not registered", name)
		}
		if f.Value.Type() != kind {
			t.Errorf("--%s is a %s, want %s", name, f.Value.Type(), kind)
		}
	}
}

// TestHelpHidesTheFlagsOfOtherTypes trims the union back once the command line
// names a type, so `record create example.com @ MX --help` is readable.
func TestHelpHidesTheFlagsOfOtherTypes(t *testing.T) {
	h := newHarness(t, testZone())
	requireNoError(t, h.run("record", "create", testDomain, "@", "MX", "--help"))

	out := h.stdout()
	mustContain(t, out, "--preference")
	mustContain(t, out, "--exchange")
	mustNotContain(t, out, "--cert-data")
	mustNotContain(t, out, "--mname")
}

// TestHelpWithoutATypeShowsEverything keeps the flags discoverable.
func TestHelpWithoutATypeShowsEverything(t *testing.T) {
	h := newHarness(t, testZone())
	requireNoError(t, h.run("record", "create", "--help"))

	out := h.stdout()
	mustContain(t, out, "--preference")
	mustContain(t, out, "--cert-data")
	mustContain(t, out, "--data")
}

func TestTypeCompletionOffersEverySupportedType(t *testing.T) {
	got, directive := completeRRTypes(nil, nil, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want NoFileComp", directive)
	}
	if len(got) != len(rdata.SupportedTypes()) {
		t.Errorf("completions = %d, want %d", len(got), len(rdata.SupportedTypes()))
	}
}

// TestHelpShowsEachTypesOwnWordingForSharedFlags.
//
// pflag keeps the FIRST registration of a name, so the union gave --priority and
// --target SRV's help text everywhere — and SRV's text is wrong for HTTPS, where
// priority 0 selects alias mode and "." is a legal target. The help contradicted
// what the command accepts.
func TestHelpShowsEachTypesOwnWordingForSharedFlags(t *testing.T) {
	tests := []struct {
		rrType  string
		want    []string
		notWant []string
	}{
		{
			rrType:  "SRV",
			want:    []string{"service priority (0-65535, lower wins)", "service host, fully qualified"},
			notWant: []string{"alias mode"},
		},
		{
			rrType:  "HTTPS",
			want:    []string{"0 selects alias mode", "or . for this name"},
			notWant: []string{"lower wins"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.rrType, func(t *testing.T) {
			h := newHarness(t, testZone())
			requireNoError(t, h.run("record", "create", testDomain, "api", tc.rrType, "--help"))
			for _, w := range tc.want {
				mustContain(t, h.stdout(), w)
			}
			for _, w := range tc.notWant {
				mustNotContain(t, h.stdout(), w)
			}
		})
	}
}
