// SPDX-License-Identifier: AGPL-3.0-only

package zone

import (
	"fmt"

	"github.com/spf13/cobra"

	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

func nameserversCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "nameservers <domain>",
		Aliases: []string{"ns"},
		Short:   "Show a zone's nameservers and delegation state",
		Long: `Show the nameservers Datum assigned to a zone and whether the registrar
publishes them.

--check additionally queries the assigned nameservers directly and asks the
public DNS what the domain currently delegates to, which answers "is it
actually working" rather than "is the control plane happy".`,
		Example: `  datumctl dns zone nameservers example.com
  datumctl dns zone nameservers example.com --check`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: util.CompleteZoneNames,
		RunE:              runNameservers,
	}

	cmd.Flags().Bool("check", false, "Resolve the zone live against its assigned nameservers")
	cmd.Flags().Duration("timeout", defaultCheckTimeout, "Per-query timeout for --check")

	return cmd
}

func runNameservers(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	project := util.ProjectFromCmd(cmd)

	check, _ := cmd.Flags().GetBool("check")
	timeout, _ := cmd.Flags().GetDuration("timeout")

	c, err := clientFactory(project)
	if err != nil {
		return err
	}

	z, err := getZone(ctx, c, project, args[0])
	if err != nil {
		return util.ClassifyError(err)
	}
	domain := zoneDisplayName(z)

	d := util.DelegationState(z)
	out := cmd.OutOrStdout()

	_, _ = fmt.Fprintf(out, "Nameservers for %s\n", domain)
	printNameserverList(out, d)
	_, _ = fmt.Fprintf(out, "\n%-*s %s\n", labelWidth, "Delegation", delegationSummary(d))

	live := liveResult{verdict: liveInconclusive}
	if check {
		_, _ = fmt.Fprintln(out)
		live = printLiveCheck(ctx, out, domain, d, timeout)
	}

	// The live check is a second, independent source of truth about the
	// registrar, and a stronger one: it observes the public DNS directly rather
	// than waiting for a Domain object to be reconciled. When it establishes
	// that the delegation is wrong, that is enough on its own to earn the
	// instruction block — otherwise --check reports the problem and withholds
	// the remedy, on the very flow `zone create` sends people to.
	// The asymmetry is deliberate and must stay: live evidence may only ADD the
	// instruction block, never suppress one the control plane earned. The two
	// sources answer slightly different questions — the Domain records what the
	// registrar published when it was last reconciled, the live query records
	// what public DNS returns right now — so a passing live check does not
	// disprove an observed mismatch, and collapsing this into a single
	// condition would let one silence the other.
	if live.verdict == liveNotDelegated && len(d.Observed) == 0 {
		// Say what the live query actually saw rather than "unknown".
		d.Observed = live.public
	}
	if delegationNeedsAction(d) || live.verdict == liveNotDelegated {
		_, _ = fmt.Fprintln(out)
		printDelegationInstructions(out, domain, d)
	}

	return nil
}
