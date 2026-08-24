// SPDX-License-Identifier: AGPL-3.0-only

package zone

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// labelWidth lines the detail view's values up in one column.
const labelWidth = 12

// domainColumnWidth positions the "project:" annotation on the Zone line.
const domainColumnWidth = 32

func describeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "describe <domain>",
		Aliases: []string{"show", "get"},
		Short:   "Show a zone's status, delegation, and record summary",
		Long: `Show one zone in detail: its status, whether the registrar has been pointed at
the assigned nameservers, and what it contains.`,
		Example: `  datumctl dns zone describe example.com
  datumctl dns zone describe example.com -o yaml`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: util.CompleteZoneNames,
		RunE:              runDescribe,
	}

	cmd.Flags().StringP("output", "o", "wide", "Output format: wide, json, yaml, name")
	_ = cmd.RegisterFlagCompletionFunc("output", util.CompleteEnum("wide", "json", "yaml", "name"))

	return cmd
}

func runDescribe(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	project := util.ProjectFromCmd(cmd)
	outputFlag, _ := cmd.Flags().GetString("output")

	format, err := util.ParseOutputFormat(outputFlag,
		util.OutputWide, util.OutputJSON, util.OutputYAML, util.OutputName)
	if err != nil {
		return err
	}

	c, err := clientFactory(project)
	if err != nil {
		return err
	}

	z, err := getZone(ctx, c, project, args[0])
	if err != nil {
		return util.ClassifyError(err)
	}

	out := cmd.OutOrStdout()
	switch format {
	case util.OutputJSON, util.OutputYAML:
		// The typed client drops TypeMeta on the way in; the machine formats
		// are worth less without it.
		z.SetGroupVersionKind(dnsv1alpha1.GroupVersion.WithKind("DNSZone"))
	}
	switch format {
	case util.OutputJSON:
		return util.PrintJSON(out, z)
	case util.OutputYAML:
		return util.PrintYAML(out, z)
	case util.OutputName:
		// The root advertises and completes -o name on every command; a
		// describe that rejects it is a trap for a script that adds the flag
		// globally.
		_, _ = fmt.Fprintln(out, zoneDisplayName(z))
		return nil
	}

	// A record-set listing the caller is not entitled to must not sink the
	// whole view: the zone's own status still answers most of the question.
	// The error travels with the data so the view can say why the breakdown is
	// missing rather than omitting the block.
	sets, listErr := zoneRecordSets(ctx, c, z)

	printZoneDetail(out, z, project, sets, listErr)
	return nil
}

// printZoneDetail renders the human view of one zone.
func printZoneDetail(out io.Writer, z *dnsv1alpha1.DNSZone, project string, sets []dnsv1alpha1.DNSRecordSet, listErr error) {
	domain := zoneDisplayName(z)

	_, _ = fmt.Fprintf(out, "%-*s %-*s project: %s\n", labelWidth, "Zone", domainColumnWidth, domain, project)
	_, _ = fmt.Fprintf(out, "%-*s %s\n", labelWidth, "Class", util.OrDash(z.Spec.DNSZoneClassName))
	if desc := description(z); desc != "" {
		_, _ = fmt.Fprintf(out, "%-*s %s\n", labelWidth, "Description", desc)
	}
	_, _ = fmt.Fprintf(out, "%-*s %s\n", labelWidth, "Created", util.RelativeAgeVerbose(z.CreationTimestamp))

	word, detail := util.ZoneStatus(z)
	d := util.DelegationState(z)

	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "%-*s %s — %s\n", labelWidth, "Status", word, detail)
	_, _ = fmt.Fprintf(out, "%-*s %s\n", labelWidth, "Delegation", delegationSummary(d))

	_, _ = fmt.Fprintf(out, "\nNameservers\n")
	printNameserverList(out, d)

	// The block is printed unconditionally. Omitting it while the Status line
	// above still says "12 records live" leaves the reader unable to tell "no
	// records" from "not allowed to look", with two lines of the same view
	// contradicting each other.
	_, _ = fmt.Fprintln(out)
	printRecordSummary(out, z, sets, listErr)

	if delegationNeedsAction(d) {
		_, _ = fmt.Fprintln(out)
		printDelegationInstructions(out, domain, d)
	}

	printZoneNextSteps(out, domain, word)
}

// printZoneNextSteps offers what is worth doing next, which depends on whether
// the zone is working.
//
// A zone the platform has rejected is not serving changes, so "add a record" is
// not a next step — it is an instruction to do something that will not take
// effect, offered at the exact moment the reader is trying to find out what is
// wrong.
func printZoneNextSteps(out io.Writer, domain, status string) {
	_, _ = fmt.Fprintf(out, "\nNext steps:\n")

	if status == util.StatusRejected || status == util.StatusError {
		_, _ = fmt.Fprintf(out, "  The zone is not serving changes while it is %s. The Status line above\n", strings.ToLower(status))
		_, _ = fmt.Fprintf(out, "  carries the reason from the platform.\n\n")
		_, _ = fmt.Fprintf(out, "  See what is there:       datumctl dns record list %s\n", domain)
		_, _ = fmt.Fprintf(out, "  Keep a copy:             datumctl dns zone export %s --file %s.zone\n", domain, domain)
		return
	}

	_, _ = fmt.Fprintf(out, "  List records:            datumctl dns record list %s\n", domain)
	_, _ = fmt.Fprintf(out, "  Add a record:            datumctl dns record create %s www A 203.0.113.10\n", domain)
	_, _ = fmt.Fprintf(out, "  Export as a zone file:   datumctl dns zone export %s\n", domain)
}

// summaryTypeOrder is the order the per-type breakdown reads in: the zone's own
// metadata first, then addresses, then the rest. It is the portal's
// DNS_RECORD_TYPES order, so the two surfaces list a zone's contents the same
// way; alphabetical would put AAAA before A and bury SOA in the middle.
var summaryTypeOrder = []dnsv1alpha1.RRType{
	dnsv1alpha1.RRTypeSOA,
	dnsv1alpha1.RRTypeNS,
	dnsv1alpha1.RRTypeA,
	dnsv1alpha1.RRTypeAAAA,
	dnsv1alpha1.RRTypeCNAME,
	dnsv1alpha1.RRTypeALIAS,
	dnsv1alpha1.RRTypeMX,
	dnsv1alpha1.RRTypeTXT,
	dnsv1alpha1.RRTypeSRV,
	dnsv1alpha1.RRTypeCAA,
	dnsv1alpha1.RRTypePTR,
	dnsv1alpha1.RRTypeTLSA,
	dnsv1alpha1.RRTypeHTTPS,
	dnsv1alpha1.RRTypeSVCB,
}

// printRecordSummary renders the record count and its per-type breakdown.
//
// The breakdown is the point of the block: a bare total tells a reader nothing
// they did not already know, while "no MX" or "SOA 2" is visible at a glance.
//
// Every number counts record entries. status.recordCount is the operator's own
// sum of len(spec.records) across the sets referencing the zone, and the
// per-type figures are that same sum partitioned by type — so they add up.
// Counting DNSRecordSet objects instead would report 1 for a type holding
// thirty records.
func printRecordSummary(out io.Writer, z *dnsv1alpha1.DNSZone, sets []dnsv1alpha1.DNSRecordSet, listErr error) {
	// A listing that failed says so. status.recordCount is still worth showing
	// — it is the same number the Status line and the table use — but the
	// per-type figures are simply not available, and a silent omission would
	// read as "this zone has no records".
	if listErr != nil {
		_, _ = fmt.Fprintf(out, "%-*s %d\n", labelWidth, "Records", z.Status.RecordCount)
		_, _ = fmt.Fprintf(out, "  the per-type breakdown is unavailable — %s\n", listFailureReason(listErr))
		return
	}

	byType := map[dnsv1alpha1.RRType]int{}
	for i := range sets {
		byType[sets[i].Spec.RecordType] += len(sets[i].Spec.Records)
	}
	counted := countRecordEntries(sets)
	reported := z.Status.RecordCount

	if reported == 0 && counted == 0 {
		_, _ = fmt.Fprintf(out, "%-*s none yet\n", labelWidth, "Records")
		return
	}

	// status.recordCount is the headline because it is what the rest of the CLI
	// shows; the sets are only the partition of it. A zone reconciled moments
	// ago can report 0 while its sets already exist, so fall back rather than
	// claiming the zone is empty.
	total := reported
	if total == 0 {
		total = counted
	}

	// The cells are the source of the type count, not the map: a set carrying
	// no entries contributes a key and no column, and a header that counts it
	// disagrees with the columns printed under it.
	cells := typeCells(byType)
	if len(cells) == 0 {
		_, _ = fmt.Fprintf(out, "%-*s %d\n", labelWidth, "Records", total)
		_, _ = fmt.Fprintf(out, "  no record sets came back for this zone, so there is no per-type breakdown\n")
		return
	}

	_, _ = fmt.Fprintf(out, "%-*s %d across %s\n", labelWidth, "Records", total,
		pluralize(len(cells), "type", "types"))
	_, _ = fmt.Fprintf(out, "  %s\n", strings.Join(cells, "    "))

	// Two figures that do not add up are worse than one figure plus an
	// explanation: status.recordCount trails the sets while the operator
	// reconciles, and a reader who spots the gap deserves to know which number
	// is which rather than assuming the CLI cannot count.
	if counted != total {
		_, _ = fmt.Fprintf(out,
			"  the per-type counts add up to %d, not the %d the zone reports — the operator is still catching up\n",
			counted, total)
	}
}

// typeCells renders the breakdown cells in reading order, omitting types with
// no records. A type outside summaryTypeOrder can only appear if the CRD enum
// grows; those follow, sorted, rather than vanishing from the total.
func typeCells(byType map[dnsv1alpha1.RRType]int) []string {
	cells := make([]string, 0, len(byType))
	seen := make(map[dnsv1alpha1.RRType]bool, len(byType))

	for _, t := range summaryTypeOrder {
		if n := byType[t]; n > 0 {
			cells = append(cells, fmt.Sprintf("%s %d", t, n))
			seen[t] = true
		}
	}

	rest := make([]string, 0)
	for t, n := range byType {
		if n > 0 && !seen[t] {
			rest = append(rest, fmt.Sprintf("%s %d", t, n))
		}
	}
	sort.Strings(rest)
	return append(cells, rest...)
}
