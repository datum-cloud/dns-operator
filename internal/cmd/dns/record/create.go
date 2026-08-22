// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

type mutateOptions struct {
	ttl     string
	line    string
	wait    bool
	timeout time.Duration
	dryRun  bool
	force   bool
	// replace distinguishes `set` from `create`: replace overwrites every value
	// at the (name, type), create appends one.
	replace bool
}

func createCommand() *cobra.Command {
	opts := &mutateOptions{}

	cmd := &cobra.Command{
		Use:   "create <domain> <name> <TYPE> [<value>...]",
		Short: "Add a record, keeping the values already at that name",
		Long: `Add one or more values to a record.

create appends: the values already at that (name, type) stay, and an exact
duplicate is refused. To replace every value at a name instead, use
` + "`datumctl dns record set`" + `.

Flat types take their value positionally; structured types are taught with
named flags, and also accept presentation format. Mixing the two notations for
one value is an error, not a merge.`,
		Example: `  # One address, then a second at the same name
  datumctl dns record create example.com www A 203.0.113.10
  datumctl dns record create example.com www A 203.0.113.11 --ttl 300

  # Structured types, by name
  datumctl dns record create example.com @ MX --preference 10 --exchange mail.example.com.

  # ...or pasted from a provider export
  datumctl dns record create example.com _sip._tcp SRV "10 5 5060 sip.example.com."

  # A whole line, as dig prints it
  datumctl dns record create example.com --line "www 300 IN A 203.0.113.10"

  # A DKIM key that will not survive shell quoting
  datumctl dns record create example.com selector1._domainkey TXT --data @dkim.txt`,
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: zoneNameArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMutate(cmd, args, opts)
		},
	}

	registerMutateFlags(cmd, opts)
	return cmd
}

func setCommand() *cobra.Command {
	opts := &mutateOptions{replace: true}

	cmd := &cobra.Command{
		Use:   "set <domain> <name> <TYPE> [<value>...]",
		Short: "Replace every value at a name with the ones given",
		Long: `Replace the values of a record.

set overwrites: every value already at that (name, type) is removed and the
ones given take their place. To add a value while keeping the others, use
` + "`datumctl dns record create`" + `.

This is the "change my A record" verb, and create is the "add a second A
record" verb. They are separate because a single command cannot express both
intents safely.`,
		Example: `  # Repoint a name, discarding whatever was there
  datumctl dns record set example.com www A 203.0.113.20

  # Two values at once
  datumctl dns record set example.com www A 203.0.113.20 203.0.113.21

  # Replace the SPF record
  datumctl dns record set example.com @ TXT "v=spf1 include:_spf.example.com ~all"`,
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: zoneNameArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMutate(cmd, args, opts)
		},
	}

	registerMutateFlags(cmd, opts)
	return cmd
}

func registerMutateFlags(cmd *cobra.Command, opts *mutateOptions) {
	cmd.Flags().StringVar(&opts.ttl, "ttl", "", "Time to live, in seconds or as a duration (300, 5m, 1h). Omitted means Auto")
	cmd.Flags().StringVar(&opts.line, "line", "", `A whole record line, as dig prints it: "www 300 IN A 203.0.113.10"`)
	cmd.Flags().BoolVar(&opts.wait, "wait", false, "Wait until the record is programmed by the DNS backend")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", defaultWaitTimeout, "How long --wait waits before giving up")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Validate and show the change without applying it")
	cmd.Flags().BoolVar(&opts.force, "force", false, "Allow editing a platform-managed record (SOA, apex NS)")

	registerRdataFlags(cmd)
	helpFilteredByType(cmd)
}

func runMutate(cmd *cobra.Command, args []string, opts *mutateOptions) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	// Everything that does not need the server is checked first. Resolving the
	// zone before validating the arguments would mask a bad record type behind
	// a zone-not-found, hand the same malformed input two different exit codes
	// depending on whether the zone exists, and spend a round trip on input
	// that could never have been valid.
	in, err := parseRecordInput(cmd, normalizeDomain(args[0]), args, opts.line, opts.ttl)
	if err != nil {
		return err
	}

	c, err := clientFactory(util.ProjectFromCmd(cmd))
	if err != nil {
		return err
	}
	zone, err := resolveZone(ctx, c, args[0])
	if err != nil {
		return err
	}
	// The positional may have named the DNSZone object rather than the domain,
	// so the name and trailing-dot rules are re-checked against the real one.
	if err := in.rebind(zone.Spec.DomainName); err != nil {
		return err
	}

	existing, err := findSet(ctx, c, zone, in.rrType, in.ownerName)
	if err != nil {
		return err
	}
	if err := guardMutation(errOut, existing, zone, in.rrType, in.ownerName, opts.force); err != nil {
		return err
	}
	printWarnings(errOut, in.warnings)

	hadEntries := existing != nil && setHasOwner(existing, in.ownerName, zone.Spec.DomainName)

	edit := appendEdit(in, zone)
	if opts.replace {
		edit = replaceEdit(in, zone)
	}

	result, err := applyEdit(ctx, c, zone, in.rrType, in.ownerName, existing, opts.dryRun, edit)
	if err != nil {
		return err
	}

	if opts.dryRun {
		printMutationDiff(out, zone, in.rrType, in.ownerName, result)
		return nil
	}

	action := actionCreated
	if opts.replace && hadEntries {
		action = actionUpdated
		// `set` run twice with the same values changes nothing, and saying
		// "updated" would be a small lie in exactly the register a tool people
		// script has to be trusted in.
		if sameRecords(in.rrType, result.before, result.after, in.ownerName, zone.Spec.DomainName) {
			action = actionUnchanged
		}
	}
	// Echo what was written rather than what was typed: a value that inherited
	// the name's existing TTL must not be reported back with the "Auto" it was
	// parsed with.
	printEcho(out, zone, in.rrType, in.ownerName, action, writtenEntries(in, zone, result, opts.replace), in.fromFlags)

	if opts.wait {
		return waitForProgrammed(ctx, c, zone, in.rrType, in.ownerName, out, opts.timeout)
	}
	return nil
}

// sameRecords reports whether a write left the values at an owner name exactly
// as they were, comparing by value and effective TTL so an Auto entry and an
// explicit DefaultTTL do not read as a change.
func sameRecords(t dnsv1alpha1.RRType, before, after []dnsv1alpha1.RecordEntry, ownerName, zoneDomain string) bool {
	was := entriesForOwner(before, ownerName, zoneDomain)
	now := entriesForOwner(after, ownerName, zoneDomain)
	if len(was) != len(now) {
		return false
	}
	for _, e := range now {
		if !containsEntry(was, t, e) {
			return false
		}
	}
	return true
}

// writtenEntries is the state the command put at the owner name: everything
// there for a replace, and only the arrivals for an append.
func writtenEntries(in *recordInput, zone *dnsv1alpha1.DNSZone, result *writeResult, replace bool) []dnsv1alpha1.RecordEntry {
	zoneDomain := zone.Spec.DomainName
	after := entriesForOwner(result.after, in.ownerName, zoneDomain)
	if replace {
		return after
	}
	// Membership is by VALUE only. containsEntry also compares TTL, which is
	// right for a diff and wrong here: --ttl retimes every value at the name, so
	// a neighbour the command did not add would differ by TTL and be echoed back
	// under "created".
	before := entriesForOwner(result.before, in.ownerName, zoneDomain)
	var added []dnsv1alpha1.RecordEntry
	for _, e := range after {
		if !containsValue(before, in.rrType, e) {
			added = append(added, e)
		}
	}
	return added
}

// containsValue reports whether entries holds the same value as want, ignoring
// TTL.
func containsValue(entries []dnsv1alpha1.RecordEntry, t dnsv1alpha1.RRType, want dnsv1alpha1.RecordEntry) bool {
	for _, e := range entries {
		if entriesEqual(t, e, want) {
			return true
		}
	}
	return false
}

// appendEdit implements create: the values given are added to whatever is
// already at the name, and an exact duplicate is refused.
func appendEdit(in *recordInput, zone *dnsv1alpha1.DNSZone) editFunc {
	zoneDomain := zone.Spec.DomainName

	return func(existing []dnsv1alpha1.RecordEntry) ([]dnsv1alpha1.RecordEntry, error) {
		out := append([]dnsv1alpha1.RecordEntry(nil), existing...)
		ttl := effectiveTTL(in, existing, zoneDomain)

		for _, e := range in.entries {
			e.Name, e.TTL = in.ownerName, ttl
			for _, ex := range existing {
				if sameOwner(ex.Name, in.ownerName, zoneDomain) && entriesEqual(in.rrType, ex, e) {
					return nil, duplicateError(in.rrType, in.ownerName, zoneDomain, e)
				}
			}
			out = append(out, e)
		}

		// TTL is per-entry in the API but per-RRset in DNS, and the backend
		// applies the first entry's to the whole set. An explicit --ttl
		// therefore has to reach every value at the name, or it would appear
		// to have been ignored.
		if in.ttlSet {
			out = retimeOwner(out, in.ownerName, zoneDomain, ttl)
		}
		return out, validateOwner(in.rrType, out, in.ownerName, zoneDomain)
	}
}

// replaceEdit implements set: every value at the name goes, the given ones
// take their place, and other names in the same type bucket are untouched.
func replaceEdit(in *recordInput, zone *dnsv1alpha1.DNSZone) editFunc {
	zoneDomain := zone.Spec.DomainName

	return func(existing []dnsv1alpha1.RecordEntry) ([]dnsv1alpha1.RecordEntry, error) {
		ttl := effectiveTTL(in, existing, zoneDomain)

		var out []dnsv1alpha1.RecordEntry
		for _, e := range existing {
			if !sameOwner(e.Name, in.ownerName, zoneDomain) {
				out = append(out, e)
			}
		}
		for _, e := range in.entries {
			e.Name, e.TTL = in.ownerName, ttl
			out = append(out, e)
		}
		return out, validateOwner(in.rrType, out, in.ownerName, zoneDomain)
	}
}

// effectiveTTL is --ttl when it was given, and otherwise whatever the name
// already resolves with. Inheriting matters because the backend takes the first
// entry's TTL for the whole RRset: a new value written with a nil TTL next to
// an existing 3600 would look like it had been given Auto and would not have
// been.
func effectiveTTL(in *recordInput, existing []dnsv1alpha1.RecordEntry, zoneDomain string) *int64 {
	if in.ttlSet {
		return in.ttl
	}
	if ttl, found := ownerTTL(existing, in.ownerName, zoneDomain); found {
		return ttl
	}
	return nil
}

func retimeOwner(entries []dnsv1alpha1.RecordEntry, ownerName, zoneDomain string, ttl *int64) []dnsv1alpha1.RecordEntry {
	out := make([]dnsv1alpha1.RecordEntry, len(entries))
	copy(out, entries)
	for i := range out {
		if sameOwner(out[i].Name, ownerName, zoneDomain) {
			out[i].TTL = ttl
		}
	}
	return out
}

// validateOwner runs the cross-value checks — single-valued types, duplicate
// values — over the entries at the affected name only.
//
// Validating the whole bucket would be stricter, and wrong: a record written by
// the portal for some other name that no longer validates would then block
// every unrelated write to the same type, and the user would have no way to fix
// it from here.
func validateOwner(t dnsv1alpha1.RRType, entries []dnsv1alpha1.RecordEntry, ownerName, zoneDomain string) error {
	owned := entriesForOwner(entries, ownerName, zoneDomain)
	if err := rdata.ValidateEntriesInZone(t, canonicalEntries(t, owned), zoneDomain); err != nil {
		return usageFromRdata(err)
	}
	return nil
}

func duplicateError(t dnsv1alpha1.RRType, ownerName, zoneDomain string, e dnsv1alpha1.RecordEntry) error {
	return util.NewCLIError(util.ExitConflict,
		fmt.Sprintf("%s already has the %s value %q",
			ownerDisplay(ownerName, zoneDomain), t, rdata.Render(t, canonicalEntry(t, e)))).
		WithFix("nothing to do — use `dns record set` to replace the values at this name, " +
			"or `dns record list` to see them.")
}
