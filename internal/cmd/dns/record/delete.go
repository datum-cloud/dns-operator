// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"fmt"

	"github.com/spf13/cobra"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

type deleteOptions struct {
	dryRun bool
	force  bool
}

func deleteCommand() *cobra.Command {
	opts := &deleteOptions{}

	cmd := &cobra.Command{
		Use:     "delete <domain> <name> <TYPE> [<value>]",
		Aliases: []string{"rm"},
		Short:   "Remove one value, or every value at a name",
		Long: `Delete records.

With a value, only that value is removed. Without one, every value at that
(name, type) goes — the prompt says how many, so the difference is never a
surprise.

When the last value of a type leaves a zone, the DNSRecordSet holding it is
deleted rather than left empty: the API requires at least one entry, so an
empty set is not a state that can be written.`,
		Example: `  # One value
  datumctl dns record delete example.com www A 203.0.113.11

  # Every A record at that name
  datumctl dns record delete example.com www A

  # No prompt, for scripts
  datumctl dns record delete example.com www A --yes`,
		Args:              cobra.RangeArgs(3, 4),
		ValidArgsFunction: zoneNameArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDelete(cmd, args, opts)
		},
	}

	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Show what would be deleted without deleting it")
	cmd.Flags().BoolVar(&opts.force, "force", false, "Allow deleting a platform-managed record (SOA, apex NS)")

	return cmd
}

func runDelete(cmd *cobra.Command, args []string, opts *deleteOptions) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	// Arguments are checked before the zone is fetched, so a bad type or a
	// malformed value is exit 2 whether or not the zone exists.
	t, err := rdata.ParseRRType(args[2])
	if err != nil {
		return usageFromRdata(err)
	}
	if err := precheckName(args[1], normalizeDomain(args[0])); err != nil {
		return err
	}

	// A value narrows the deletion to one record; without it the whole RRset at
	// the name goes.
	var target *dnsv1alpha1.RecordEntry
	if len(args) == 4 {
		parsed, perr := rdata.ParseValue(t, args[3])
		if perr != nil {
			return usageFromRdata(perr)
		}
		target = &parsed
	}

	c, err := clientFactory(util.ProjectFromCmd(cmd))
	if err != nil {
		return err
	}
	zone, err := resolveZone(ctx, c, args[0])
	if err != nil {
		return err
	}
	zoneDomain := zone.Spec.DomainName

	// Re-derived against the authoritative domain: the positional may have
	// named the DNSZone object rather than the domain it serves.
	ownerName, warnings, err := rdata.NormalizeNameWithWarnings(args[1], zoneDomain)
	if err != nil {
		return usageFromRdata(err)
	}
	printWarnings(errOut, warnings)

	set, err := findSet(ctx, c, zone, t, ownerName)
	if err != nil {
		return err
	}
	if set == nil {
		return notFoundError(t, ownerName, zoneDomain)
	}
	// What is being deleted is established before the managed-record guard, so
	// a name or value that is simply not there says so rather than being told
	// it is platform-managed. The guard still runs before anything is written.
	doomed := matchingEntries(set.Spec.Records, t, ownerName, zoneDomain, target)
	if len(doomed) == 0 {
		if target != nil {
			return util.NewCLIError(util.ExitNotFound,
				fmt.Sprintf("%s has no %s value %q", ownerDisplay(ownerName, zoneDomain), t, rdata.Render(t, *target))).
				WithFix(fmt.Sprintf("list what is there: datumctl dns record list %s --name %s --type %s",
					zoneDomain, displayName(ownerName), t))
		}
		return notFoundError(t, ownerName, zoneDomain)
	}

	if err := guardMutation(errOut, set, zone, t, ownerName, opts.force); err != nil {
		return err
	}

	if !opts.dryRun && !util.AssumeYes(cmd) {
		ok, cerr := util.ConfirmYesNo(cmd.InOrStdin(), errOut, deletePrompt(doomed, t, ownerName, zoneDomain, target), false)
		if cerr != nil {
			return cerr
		}
		if !ok {
			return util.NewCLIError(util.ExitAborted, "aborted; nothing was deleted")
		}
	}

	edit := func(existing []dnsv1alpha1.RecordEntry) ([]dnsv1alpha1.RecordEntry, error) {
		var out []dnsv1alpha1.RecordEntry
		for _, e := range existing {
			if matches(e, t, ownerName, zoneDomain, target) {
				continue
			}
			out = append(out, e)
		}
		return out, nil
	}

	result, err := applyEdit(ctx, c, zone, t, ownerName, set, opts.dryRun, edit)
	if err != nil {
		return err
	}

	if opts.dryRun {
		printMutationDiff(out, zone, t, ownerName, result)
		return nil
	}

	_, _ = fmt.Fprintf(out, "  record/%s %s %s %s\n", zoneDomain, t, displayName(ownerName), actionDeleted)
	for _, e := range doomed {
		_, _ = fmt.Fprintf(out, "  - %s\n", presentationLine(t, e))
	}
	if result.setRemoved {
		_, _ = fmt.Fprintf(out, "  record set %s removed — no %s records remain in the zone\n", result.set.Name, t)
	}
	return nil
}

// matchingEntries returns the entries a delete would remove.
func matchingEntries(entries []dnsv1alpha1.RecordEntry, t dnsv1alpha1.RRType, ownerName, zoneDomain string, target *dnsv1alpha1.RecordEntry) []dnsv1alpha1.RecordEntry {
	var out []dnsv1alpha1.RecordEntry
	for _, e := range entries {
		if matches(e, t, ownerName, zoneDomain, target) {
			out = append(out, e)
		}
	}
	return out
}

// matches reports whether one stored entry is in scope for the delete. A nil
// target means "every value at this name".
func matches(e dnsv1alpha1.RecordEntry, t dnsv1alpha1.RRType, ownerName, zoneDomain string, target *dnsv1alpha1.RecordEntry) bool {
	if !sameOwner(e.Name, ownerName, zoneDomain) {
		return false
	}
	return target == nil || entriesEqual(t, e, *target)
}

// deletePrompt states the blast radius. Deleting every value at a name is the
// case that needs the count: "delete all 3 A records" is a different decision
// from "delete the A record".
func deletePrompt(doomed []dnsv1alpha1.RecordEntry, t dnsv1alpha1.RRType, ownerName, zoneDomain string, target *dnsv1alpha1.RecordEntry) string {
	who := ownerDisplay(ownerName, zoneDomain)
	if target != nil {
		return fmt.Sprintf("Delete the %s record %s for %s?", t, rdata.Render(t, canonicalEntry(t, *target)), who)
	}
	if len(doomed) == 1 {
		return fmt.Sprintf("Delete the 1 %s record for %s?", t, who)
	}
	return fmt.Sprintf("Delete all %d %s records for %s?", len(doomed), t, who)
}

func notFoundError(t dnsv1alpha1.RRType, ownerName, zoneDomain string) error {
	return util.NewCLIError(util.ExitNotFound,
		fmt.Sprintf("no %s records for %s", t, ownerDisplay(ownerName, zoneDomain))).
		WithFix(fmt.Sprintf("list what is there: datumctl dns record list %s", zoneDomain))
}
