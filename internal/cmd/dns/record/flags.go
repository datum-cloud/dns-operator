// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// outputFlag reads -o wherever it was declared: locally on the command, or as
// the root's persistent flag injected by the plugin SDK.
func outputFlag(cmd *cobra.Command) string {
	if v := stringFlag(cmd, "output"); v != "" {
		return v
	}
	return string(util.OutputTable)
}

func stringFlag(cmd *cobra.Command, name string) string {
	if f := cmd.Flags().Lookup(name); f != nil {
		v, _ := cmd.Flags().GetString(name)
		return v
	}
	if root := cmd.Root(); root != nil {
		if f := root.PersistentFlags().Lookup(name); f != nil {
			v, _ := root.PersistentFlags().GetString(name)
			return v
		}
	}
	return ""
}

func boolFlag(cmd *cobra.Command, name string) bool {
	if f := cmd.Flags().Lookup(name); f != nil {
		v, _ := cmd.Flags().GetBool(name)
		return v
	}
	if root := cmd.Root(); root != nil {
		if f := root.PersistentFlags().Lookup(name); f != nil {
			v, _ := root.PersistentFlags().GetBool(name)
			return v
		}
	}
	return false
}

// usageFromRdata turns a validation failure into a usage exit, carrying the
// package's suggested remedy onto the "Fix:" line. Client-side rdata validation
// is exit 2 by the plugin's contract: the input was wrong, not the server.
func usageFromRdata(err error) error {
	if err == nil {
		return nil
	}
	var already *util.CLIError
	if errors.As(err, &already) {
		return already
	}
	ce := util.UsageErrorf("%s", err.Error()).WithCause(err)
	if fix := rdata.FixFor(err); fix != "" {
		ce = ce.WithFix(fix)
	}
	return ce
}

// completeRRTypes completes a type positional or --type value from the types
// the API accepts. Completion never errors and never falls back to filenames.
func completeRRTypes(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	types := rdata.SupportedTypes()
	out := make([]string, 0, len(types))
	for _, t := range types {
		out = append(out, string(t))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// registerRdataFlags adds the union of every type's named rdata flags to a
// mutation command.
//
// The record type is a positional argument, and cobra has no hook between
// resolving the command and parsing its flags — so the flag set cannot be built
// from the type the user typed. The alternative, pre-scanning os.Args before
// Cobra runs, would put the parser's job in two places and would not survive a
// test that drives RunE with its own argument slice.
//
// Registering the union instead keeps one parser and moves the type check one
// step later: validateRdataFlags rejects a flag that does not belong to the
// type, with a better message than pflag's "unknown flag" would have been. The
// flag names collide only where the meaning and the type already agree
// (--priority is a uint16 for both SRV and HTTPS, --target a string for both),
// so the union is well defined.
//
// The cost is a long --help, which helpFilteredByType then trims back to the
// flags that apply once a type is on the command line.
func registerRdataFlags(cmd *cobra.Command) {
	union := pflag.NewFlagSet("rdata", pflag.ContinueOnError)
	for _, t := range rdata.SupportedTypes() {
		per := pflag.NewFlagSet(string(t), pflag.ContinueOnError)
		rdata.RegisterFlags(per, t)
		union.AddFlagSet(per)
	}
	cmd.Flags().AddFlagSet(union)
}

// allRdataFlagNames is every name registerRdataFlags may have added.
func allRdataFlagNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range rdata.SupportedTypes() {
		for _, n := range rdata.FlagNames(t) {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}

// validateRdataFlags rejects named rdata flags that belong to a different type.
// Without it, `record create example.com www A --preference 10` would parse
// cleanly and write an A record that ignored half the command.
func validateRdataFlags(cmd *cobra.Command, t dnsv1alpha1.RRType) error {
	allowed := map[string]bool{}
	for _, n := range rdata.FlagNames(t) {
		allowed[n] = true
	}

	var bad []string
	for _, n := range allRdataFlagNames() {
		if allowed[n] {
			continue
		}
		if f := cmd.Flags().Lookup(n); f != nil && f.Changed {
			bad = append(bad, "--"+n)
		}
	}
	if len(bad) == 0 {
		return nil
	}

	verb := "is not a flag"
	if len(bad) > 1 {
		verb = "are not flags"
	}
	err := util.UsageErrorf("%s %s for %s records", strings.Join(bad, ", "), verb, t)

	if names := rdata.FlagNames(t); len(names) > 0 {
		return err.WithFix(fmt.Sprintf("%s records take --%s.", t, strings.Join(names, ", --")))
	}
	return err.WithFix(fmt.Sprintf("%s records take their value positionally: `... %s <value>`.", t, t))
}

// retypeRdataHelp narrows the union flag set to one record type for the help
// output: the flags of other types are hidden, and the flags this type shares
// with another get their own type's wording back.
//
// The re-description matters as much as the hiding. pflag keeps the FIRST
// registration of a name, so --priority and --target carried SRV's help text
// everywhere — and SRV's text is wrong for HTTPS, where priority 0 selects
// alias mode and "." is a legal target. The displayed sentence contradicted
// what the command accepts.
func retypeRdataHelp(cmd *cobra.Command, t dnsv1alpha1.RRType) {
	own := pflag.NewFlagSet(string(t), pflag.ContinueOnError)
	rdata.RegisterFlags(own, t)

	allowed := map[string]bool{}
	for _, n := range rdata.FlagNames(t) {
		allowed[n] = true
	}
	for _, n := range allRdataFlagNames() {
		f := cmd.Flags().Lookup(n)
		if f == nil {
			continue
		}
		f.Hidden = !allowed[n]
		if mine := own.Lookup(n); mine != nil {
			f.Usage = mine.Usage
		}
	}
}

// helpFilteredByType hides the rdata flags that do not apply once the command
// line names a record type, so `record create example.com @ MX --help` shows
// --preference and --exchange rather than all twenty-one.
func helpFilteredByType(cmd *cobra.Command) {
	fallback := cmd.HelpFunc()
	cmd.SetHelpFunc(func(c *cobra.Command, args []string) {
		// Flags are parsed before help is printed, so the leftover positionals
		// are the reliable place to look for the type.
		if positional := c.Flags().Args(); len(positional) >= 3 {
			if t, err := rdata.ParseRRType(positional[2]); err == nil {
				retypeRdataHelp(c, t)
			}
		}
		fallback(c, args)
	})
}

// zoneGuessFrom returns the zone domain the pre-API validation pass should use.
//
// The zone positional is normally the domain, and validating against it before
// any API call is what keeps a bad argument from being masked by a missing
// zone. But the positional also accepts the DNSZone object's name as a
// convenience, and validating an absolute owner name against `example-com`
// would reject something perfectly correct. Every domain has a dot in it and
// the object names this convention produces do not, so an undotted positional
// means "zone not known yet" — the zone-relative rules then wait for rebind,
// which runs against the authoritative domain either way.
func zoneGuessFrom(positional string) string {
	if strings.Contains(positional, ".") {
		return positional
	}
	return ""
}

// precheckName is zoneGuessFrom applied to a bare owner name, for the commands
// that have no rdata to parse.
func precheckName(name, positional string) error {
	if _, _, err := rdata.NormalizeNameWithWarnings(name, zoneGuessFrom(positional)); err != nil {
		return usageFromRdata(err)
	}
	return nil
}
