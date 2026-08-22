// SPDX-License-Identifier: AGPL-3.0-only

// Package dns implements the `datumctl dns` plugin: zone and record management
// presented in the terms users think in, over an API that stores records in
// per-type buckets.
package dns

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"go.datum.net/datumctl/plugin"

	"go.miloapis.com/dns-operator/internal/cmd/dns/record"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
	"go.miloapis.com/dns-operator/internal/cmd/dns/zone"
)

const short = "Manage DNS zones and records on Datum Cloud"

// Commands that never touch the DNS API, named once so the skip list and its
// tests cannot drift apart.
// pluginAPIVersion is the datumctl plugin contract this binary implements. It
// must match the Manifest served by cmd/datumctl-dns.
const pluginAPIVersion = 1

const (
	cmdVersion    = "version"
	cmdCompletion = "completion"
	cmdHelp       = "help"
)

// entitlementSkip names the commands that must run without a project
// entitlement: they either do not touch the API at all, or are the shell's own
// completion hooks, where an error or a prompt would corrupt the command line.
var entitlementSkip = map[string]bool{
	cmdVersion:                      true,
	cmdCompletion:                   true,
	cmdHelp:                         true,
	cobra.ShellCompRequestCmd:       true, // __complete
	cobra.ShellCompNoDescRequestCmd: true, // __completeNoDesc
}

// Command builds the plugin's root command.
func Command() *cobra.Command {
	root := plugin.NewRootCmd("dns", short)

	// Errors are rendered by main through util.RenderExit, which owns the
	// Error/Fix/exit-status format and the exit code. Cobra must not print its
	// own version of either, and a failing command must not dump usage text
	// over the message that explains what went wrong.
	root.SilenceUsage = true
	root.SilenceErrors = true
	root.SuggestionsMinimumDistance = 2

	// The SDK wires -o with a table|json|yaml help string; this plugin also
	// serves wide and name.
	if out := root.PersistentFlags().Lookup("output"); out != nil {
		out.Usage = "Output format. One of: table|wide|json|yaml|name"
	}

	root.PersistentFlags().BoolP("verbose", "v", false, "Show the underlying cause of errors")
	root.PersistentFlags().BoolP("quiet", "q", false, "Suppress progress and footer output")
	root.PersistentFlags().String("color", "auto", "Colorize output. One of: auto|always|never")
	root.PersistentFlags().BoolP("yes", "y", false, "Skip confirmation prompts")

	_ = root.RegisterFlagCompletionFunc("output",
		util.CompleteEnum("table", "wide", "json", "yaml", "name"))
	_ = root.RegisterFlagCompletionFunc("color",
		util.CompleteEnum("auto", "always", "never"))

	// Cobra reports a bad flag as a plain error, which would exit 1. Flags and
	// arguments are usage failures, and scripts branch on that distinction.
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return util.UsageErrorf("%s", err.Error()).
			WithFix(fmt.Sprintf("run `%s --help` to see the available flags.", cmd.CommandPath()))
	})

	// A typo'd subcommand must fail rather than print help and exit 0, which is
	// what a non-runnable root does by default. RunE makes the bare `dns` still
	// print help while giving argument validation somewhere to land.
	root.Args = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return nil
		}
		return unknownSubcommandError(cmd, args[0])
	}
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	}

	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if skipsEntitlement(cmd) {
			return nil
		}
		return util.EnsureDNSEntitlement(cmd.Context(), util.ProjectFromCmd(cmd), cmd.InOrStdin(), cmd.ErrOrStderr())
	}

	root.AddCommand(
		zone.Command(),
		record.Command(),
		versionCommand(),
	)

	// Argument validation is a usage failure everywhere in the tree, not just
	// where an author remembered to say so.
	enforceUsageExit(root)

	return root
}

// skipsEntitlement reports whether the pre-flight must be skipped for this
// invocation: a help request, or one of the commands in entitlementSkip.
func skipsEntitlement(cmd *cobra.Command) bool {
	if help, err := cmd.Flags().GetBool("help"); err == nil && help {
		return true
	}
	// The bare `dns` only prints help; there is nothing to be entitled to.
	if cmd.Parent() == nil {
		return true
	}
	for c := cmd; c != nil; c = c.Parent() {
		if entitlementSkip[c.Name()] {
			return true
		}
	}
	return false
}

// unknownSubcommandError rejects an unrecognized subcommand with a usage exit
// code and a nearest-match suggestion, matching the "did you mean" behaviour
// cobra gives for the noun level.
func unknownSubcommandError(cmd *cobra.Command, name string) *util.CLIError {
	msg := fmt.Sprintf("unknown command %q for %q", name, cmd.CommandPath())
	if suggestions := cmd.SuggestionsFor(name); len(suggestions) > 0 {
		msg += "\n\nDid you mean this?\n\t" + strings.Join(suggestions, "\n\t")
	}
	return util.NewCLIError(util.ExitUsage, msg)
}

// enforceUsageExit makes every argument-validation error in the command tree
// exit with ExitUsage.
//
// Cobra's stock validators — cobra.NoArgs, ExactArgs, and the rest — return a
// plain error, which classifies as a generic ExitError. A script branching on
// exit codes would see a typo'd argument as an unexpected failure rather than
// as the usage error it is. Rather than require every command to remember to
// wrap its own validator, this walks the tree once and does it for all of them,
// so reaching for a stock validator is not a way to get it wrong.
//
// An error that is already a *util.CLIError passes through untouched, so a
// command that built a richer message with its own Fix keeps it.
func enforceUsageExit(cmd *cobra.Command) {
	switch {
	case cmd.Args != nil:
		inner := cmd.Args
		cmd.Args = func(c *cobra.Command, args []string) error {
			return asUsageError(inner(c, args))
		}
	case cmd.HasSubCommands():
		// A nil Args on a parent leaves cobra's legacyArgs to reject an
		// unrecognised subcommand, which is the right check with the wrong exit
		// code and no suggestion.
		cmd.Args = func(c *cobra.Command, args []string) error {
			if len(args) == 0 || c.Runnable() {
				return nil
			}
			return unknownSubcommandError(c, args[0])
		}
	}

	for _, sub := range cmd.Commands() {
		enforceUsageExit(sub)
	}
}

// asUsageError re-labels a validation error as a usage failure, leaving an
// error that already carries an exit code alone.
func asUsageError(err error) error {
	if err == nil {
		return nil
	}
	var already *util.CLIError
	if errors.As(err, &already) {
		return err
	}
	return util.UsageErrorf("%s", err.Error())
}
