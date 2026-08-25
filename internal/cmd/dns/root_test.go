// SPDX-License-Identifier: AGPL-3.0-only

package dns

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// falseDefault is the string form of a bool flag's zero default.
const falseDefault = "false"

func TestCommandFlags(t *testing.T) {
	root := Command()

	tests := []struct {
		flag      string
		shorthand string
		want      string
	}{
		{flag: "org"},
		{flag: "project"},
		{flag: "output", shorthand: "o", want: "table"},
		{flag: "verbose", shorthand: "v", want: falseDefault},
		{flag: "quiet", shorthand: "q", want: falseDefault},
		{flag: "color", want: "auto"},
		{flag: "yes", shorthand: "y", want: falseDefault},
	}

	for _, tc := range tests {
		t.Run(tc.flag, func(t *testing.T) {
			f := root.PersistentFlags().Lookup(tc.flag)
			if f == nil {
				t.Fatalf("--%s is not registered", tc.flag)
			}
			if f.Shorthand != tc.shorthand {
				t.Errorf("shorthand = %q, want %q", f.Shorthand, tc.shorthand)
			}
			if tc.want != "" && f.DefValue != tc.want {
				t.Errorf("default = %q, want %q", f.DefValue, tc.want)
			}
		})
	}
}

func TestCommandSilencesCobraOutput(t *testing.T) {
	root := Command()
	if !root.SilenceUsage {
		t.Errorf("SilenceUsage = false; a failing command must not dump usage over the error")
	}
	if !root.SilenceErrors {
		t.Errorf("SilenceErrors = false; RenderExit owns the error format")
	}
	if root.SuggestionsMinimumDistance != 2 {
		t.Errorf("SuggestionsMinimumDistance = %d, want 2", root.SuggestionsMinimumDistance)
	}
}

func TestOutputFlagAdvertisesEveryFormat(t *testing.T) {
	usage := Command().PersistentFlags().Lookup("output").Usage
	if want := "Output format. One of: table|wide|json|yaml|name"; usage != want {
		t.Errorf("usage = %q, want %q", usage, want)
	}
}

func TestSkipsEntitlement(t *testing.T) {
	// A stand-in tree: a leaf under the root, plus the completion hooks cobra
	// installs at Execute time.
	newTree := func() (*cobra.Command, map[string]*cobra.Command) {
		root := Command()
		byName := map[string]*cobra.Command{}
		for _, name := range []string{
			"list", cmdVersion, cmdCompletion,
			cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd,
		} {
			c := &cobra.Command{Use: name, Run: func(*cobra.Command, []string) {}}
			root.AddCommand(c)
			byName[name] = c
		}
		byName["root"] = root
		return root, byName
	}

	tests := []struct {
		name string
		cmd  string
		want bool
	}{
		{name: "a normal subcommand runs the pre-flight", cmd: "list", want: false},
		{name: "version skips", cmd: cmdVersion, want: true},
		{name: "completion skips", cmd: cmdCompletion, want: true},
		{name: "__complete skips", cmd: cobra.ShellCompRequestCmd, want: true},
		{name: "__completeNoDesc skips", cmd: cobra.ShellCompNoDescRequestCmd, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, byName := newTree()
			if got := skipsEntitlement(byName[tc.cmd]); got != tc.want {
				t.Errorf("skipsEntitlement(%s) = %v, want %v", tc.cmd, got, tc.want)
			}
		})
	}

	t.Run("a subcommand of completion skips too", func(t *testing.T) {
		_, byName := newTree()
		bash := &cobra.Command{Use: "bash"}
		byName[cmdCompletion].AddCommand(bash)
		if !skipsEntitlement(bash) {
			t.Errorf("skipsEntitlement(completion bash) = false, want true")
		}
	})

	t.Run("--help skips", func(t *testing.T) {
		root, byName := newTree()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		list := byName["list"]
		list.InitDefaultHelpFlag()
		if err := list.Flags().Set("help", "true"); err != nil {
			t.Fatal(err)
		}
		if !skipsEntitlement(list) {
			t.Errorf("skipsEntitlement with --help = false, want true")
		}
	})
}

func TestUnknownSubcommandError(t *testing.T) {
	root := Command()
	root.AddCommand(&cobra.Command{Use: "zone", Run: func(*cobra.Command, []string) {}})

	t.Run("a typo is a usage error", func(t *testing.T) {
		err := unknownSubcommandError(root, "zne")
		if err.Code() != util.ExitUsage {
			t.Errorf("code = %d, want %d", err.Code(), util.ExitUsage)
		}
		if !strings.Contains(err.Error(), `unknown command "zne" for "dns"`) {
			t.Errorf("message = %q", err.Error())
		}
		if !strings.Contains(err.Error(), "Did you mean this?") {
			t.Errorf("a near miss should suggest the real command, got %q", err.Error())
		}
	})

	t.Run("a distant name gets no suggestion", func(t *testing.T) {
		err := unknownSubcommandError(root, "frobnicate")
		if strings.Contains(err.Error(), "Did you mean") {
			t.Errorf("message = %q, want no suggestion", err.Error())
		}
	})
}

func TestBareRootSkipsEntitlement(t *testing.T) {
	if !skipsEntitlement(Command()) {
		t.Errorf("skipsEntitlement(root) = false; the bare command only prints help")
	}
}

// TestArityErrorNamesTheArgument covers the first thing a new user sees. Cobra's
// own text ("accepts 1 arg(s), received 0") names a count and nothing else, and
// SilenceUsage means it is the whole output — so the reader is told a command
// is wrong without being told what it takes.
func TestArityErrorNamesTheArgument(t *testing.T) {
	cases := []struct {
		args    []string
		wantMsg string
		wantFix string
	}{
		{[]string{"record", "list"}, "datumctl dns record list takes <domain>", "datumctl dns record list example.com"},
		{[]string{"record", "describe"}, "takes <domain> <name> [<TYPE>]", "datumctl dns record describe example.com www"},
		{[]string{"zone", "export"}, "datumctl dns zone export takes <domain>", "datumctl dns zone export example.com"},
		{[]string{"zone", "list", "unexpected"}, "datumctl dns zone list takes no arguments", ""},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			err := runArgs(t, tc.args...)
			if err == nil {
				t.Fatalf("%v: want a usage error", tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", err.Error(), tc.wantMsg)
			}
			var ce *util.CLIError
			if !errors.As(err, &ce) {
				t.Fatalf("error is %T, want *util.CLIError", err)
			}
			if ce.Code() != util.ExitUsage {
				t.Errorf("exit code = %d, want %d", ce.Code(), util.ExitUsage)
			}
			if tc.wantFix != "" && !strings.Contains(ce.Fix(), tc.wantFix) {
				t.Errorf("fix = %q, want it to contain %q", ce.Fix(), tc.wantFix)
			}
		})
	}
}

// TestTypoSuggestionsUseEditDistance pins the fix for suggestions firing only on
// a prefix match. Cobra defaults SuggestionsMinimumDistance on the root alone, so
// every subcommand sat at 0, where "desc" found "describe" but "lst" never found
// "list".
func TestTypoSuggestionsUseEditDistance(t *testing.T) {
	for typo, want := range map[string]string{
		"lst":        "list",
		"exprt":      "export",
		"delet":      "delete",
		"nameservrs": "nameservers",
	} {
		t.Run(typo, func(t *testing.T) {
			err := runArgs(t, "zone", typo)
			if err == nil {
				t.Fatalf("want an error for %q", typo)
			}
			if !strings.Contains(err.Error(), "Did you mean") || !strings.Contains(err.Error(), want) {
				t.Errorf("error for %q = %q, want a suggestion of %q", typo, err.Error(), want)
			}
		})
	}
}

// runArgs executes the real command tree with args and returns the error the
// user would be shown. Output is discarded: these tests are about the error.
func runArgs(t *testing.T, args ...string) error {
	t.Helper()
	root := Command()
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return root.Execute()
}
