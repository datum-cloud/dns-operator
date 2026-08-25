// SPDX-License-Identifier: AGPL-3.0-only

package zone

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

func deleteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "delete <domain>",
		Aliases: []string{"rm"},
		Short:   "Delete a DNS zone and every record in it",
		Long: `Delete a DNS zone.

Deleting a zone cascades: the operator owns every record set in the zone through
a controller ownerReference, so all of the zone's records are garbage-collected
with it and the domain stops resolving.

The confirmation asks for the zone name typed in full, and the command refuses
to run non-interactively without --yes.`,
		Example: `  datumctl dns zone delete example.com

  # In a script
  datumctl dns zone delete example.com --yes

  # Validate the deletion without performing it
  datumctl dns zone delete example.com --dry-run`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: util.CompleteZoneNames,
		RunE:              runDelete,
	}

	cmd.Flags().Bool("dry-run", false, "Submit the deletion for server-side validation without performing it")

	return cmd
}

func runDelete(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	project := util.ProjectFromCmd(cmd)

	dryRun, _ := cmd.Flags().GetBool("dry-run")
	// --yes is a root persistent flag, shared with every other gated command.
	assumeYes := util.AssumeYes(cmd)

	c, err := clientFactory(project)
	if err != nil {
		return err
	}

	z, err := getZone(ctx, c, project, args[0])
	if err != nil {
		return util.ClassifyError(err)
	}
	domain := zoneDisplayName(z)

	// The count comes from the record sets themselves rather than
	// status.recordCount, so the number in the prompt is what is there now and
	// not what the last reconcile saw.
	//
	// A failure to count is never treated as a count of zero. This number is
	// the entire quantification of the blast radius the user is consenting to,
	// and RBAC that grants delete on DNSZone without list on DNSRecordSet is
	// ordinary — failing open here would ask for consent to destroy records
	// while reporting that there are none.
	records := z.Status.RecordCount
	sets, countErr := zoneRecordSets(ctx, c, z)
	if countErr == nil {
		records = countRecordEntries(sets)
	}

	out := cmd.OutOrStdout()

	if !dryRun && !assumeYes {
		prompt := cascadeWarning(domain, records, countErr) + objectNameNote(args[0], domain, z.Name)
		confirmed, err := util.ConfirmTyped(cmd.InOrStdin(), cmd.ErrOrStderr(), prompt, domain)
		if err != nil {
			return err
		}
		if !confirmed {
			return util.NewCLIError(util.ExitAborted, "aborted")
		}
	}

	var opts []client.DeleteOption
	if dryRun {
		opts = append(opts, client.DryRunAll)
	}
	if err := c.Delete(ctx, z, opts...); err != nil {
		return util.ClassifyError(fmt.Errorf("deleting zone: %w", err))
	}

	if dryRun {
		if countErr != nil {
			_, _ = fmt.Fprintf(out,
				"zone/%s would be deleted, along with every DNS record it contains — dry run, nothing was deleted\n",
				domain)
			return nil
		}
		_, _ = fmt.Fprintf(out, "zone/%s would be deleted, along with %s — dry run, nothing was deleted\n",
			domain, pluralize(records, "DNS record", "DNS records"))
		return nil
	}

	// The receipt reports what was destroyed with the same honesty as the
	// prompt: an uncounted cascade is not a cascade of nothing.
	switch {
	case countErr != nil:
		_, _ = fmt.Fprintf(out, "zone/%s deleted — any DNS records it contained were deleted with it\n", domain)
	case records > 0:
		_, _ = fmt.Fprintf(out, "zone/%s deleted — %s deleted with it\n",
			domain, pluralize(records, "DNS record was", "DNS records were"))
	default:
		_, _ = fmt.Fprintf(out, "zone/%s deleted\n", domain)
	}
	return nil
}

// cascadeWarning is the text above the typed confirmation. It states the
// cascade explicitly, because "delete the zone" reads as reversible and
// deleting every record in it is not.
//
// countErr non-nil means the records could not be counted. That case keeps the
// strong wording and names the reason: "removes it permanently" on its own
// reads as "there is nothing else in here", which is precisely the claim an
// uncounted zone does not support.
func cascadeWarning(domain string, records int, countErr error) string {
	const consequence = "This cannot be undone, and the domain will stop resolving.\n"

	if countErr != nil {
		return fmt.Sprintf(
			"Deleting zone %s will also delete every DNS record it contains.\n"+
				"The record count is unavailable — %s — so this zone may hold records that are not listed here.\n"+
				consequence,
			domain, listFailureReason(countErr))
	}
	if records == 0 {
		return fmt.Sprintf("Deleting zone %s removes it permanently.\n"+consequence, domain)
	}
	return fmt.Sprintf(
		"Deleting zone %s will also delete all %s it contains.\n"+consequence,
		domain, pluralize(records, "DNS record", "DNS records"))
}

// objectNameNote warns when the user identified the zone by its Kubernetes
// object name, because the confirmation then asks them to type something they
// did not write.
func objectNameNote(typed, domain, objectName string) string {
	if strings.EqualFold(strings.TrimSpace(typed), domain) || typed != objectName {
		return ""
	}
	return fmt.Sprintf("You named the zone by its object name %q; confirm with its domain.\n", objectName)
}
