// SPDX-License-Identifier: AGPL-3.0-only

package zone

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// statusFilters are the values --status accepts. The first word of the rendered
// status is the filter token, as in compute.
var statusFilters = []string{
	strings.ToLower(util.StatusOK),
	strings.ToLower(util.StatusPending),
	strings.ToLower(util.StatusError),
	strings.ToLower(util.StatusRejected),
}

func listCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List DNS zones",
		Long:    "List the DNS zones in the project, with their delegation state.",
		Example: `  # Every zone
  datumctl dns zone list

  # Only the ones that are not working yet
  datumctl dns zone list --status error

  # Raw API objects
  datumctl dns zone list -o json`,
		Args: cobra.NoArgs,
		RunE: runList,
	}
	addListFlags(cmd)
	return cmd
}

// addListFlags registers the list flags. It is shared with the bare `zone`
// group, which lists.
func addListFlags(cmd *cobra.Command) {
	cmd.Flags().String("status", "", "Filter by status: ok, pending, error")
	cmd.Flags().StringP("output", "o", "table", "Output format: table, wide, json, yaml, name")
	cmd.Flags().Bool("no-headers", false, "Omit the table header row (table and wide only)")

	_ = cmd.RegisterFlagCompletionFunc("status", util.CompleteEnum(statusFilters...))
	_ = cmd.RegisterFlagCompletionFunc("output",
		util.CompleteEnum("table", "wide", "json", "yaml", "name"))
}

// zoneRow is one rendered table line, computed once so the filter, the tally,
// and the table all read the same values.
type zoneRow struct {
	name        string
	status      string
	records     string
	nameservers string
	delegated   string
	age         string
	class       string
	domain      string
}

func runList(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	project := util.ProjectFromCmd(cmd)

	outputFlag, _ := cmd.Flags().GetString("output")
	statusFilter, _ := cmd.Flags().GetString("status")
	noHeaders, _ := cmd.Flags().GetBool("no-headers")

	format, err := util.ParseOutputFormat(outputFlag)
	if err != nil {
		return err
	}
	if statusFilter != "" && !isKnownStatusFilter(statusFilter) {
		return util.UsageErrorf("invalid status filter %q — must be one of: %s",
			statusFilter, strings.Join(statusFilters, ", "))
	}

	c, err := clientFactory(project)
	if err != nil {
		return err
	}

	var list dnsv1alpha1.DNSZoneList
	if err := c.List(ctx, &list, client.InNamespace(util.ResourceNamespace)); err != nil {
		return util.ClassifyError(fmt.Errorf("listing zones: %w", err))
	}

	// The machine contract is the raw API list, dispatched before any
	// client-side enrichment: a script that parses -o json must see exactly
	// what the API served.
	out := cmd.OutOrStdout()
	switch format {
	case util.OutputJSON, util.OutputYAML:
		// The typed client drops TypeMeta on the way in. Putting it back is
		// what makes the output round-trip through `datumctl apply -f -`.
		list.SetGroupVersionKind(dnsv1alpha1.GroupVersion.WithKind("DNSZoneList"))
	}
	switch format {
	case util.OutputJSON:
		return util.PrintJSON(out, &list)
	case util.OutputYAML:
		return util.PrintYAML(out, &list)
	}

	rows := make([]zoneRow, 0, len(list.Items))
	for i := range list.Items {
		row := newZoneRow(&list.Items[i])
		if statusFilter != "" && !strings.EqualFold(row.status, statusFilter) {
			continue
		}
		rows = append(rows, row)
	}

	// The API returns objects in object-name order, which is the generated name
	// nobody sees. Sorting by domain makes the table order the one the reader
	// is looking at.
	sort.Slice(rows, func(i, j int) bool { return rows[i].name < rows[j].name })

	if format == util.OutputName {
		for _, r := range rows {
			_, _ = fmt.Fprintln(out, r.name)
		}
		return nil
	}

	if len(rows) == 0 {
		printEmpty(out, project, statusFilter)
		return nil
	}

	wide := format == util.OutputWide
	tw := util.NewTabWriter(out)
	if !noHeaders {
		if wide {
			_, _ = fmt.Fprintf(tw, "NAME\tSTATUS\tRECORDS\tNAMESERVERS\tDELEGATED\tAGE\tCLASS\tDOMAIN\n")
		} else {
			_, _ = fmt.Fprintf(tw, "NAME\tSTATUS\tRECORDS\tNAMESERVERS\tDELEGATED\tAGE\n")
		}
	}
	for _, r := range rows {
		if wide {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				r.name, r.status, r.records, r.nameservers, r.delegated, r.age, r.class, r.domain)
		} else {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
				r.name, r.status, r.records, r.nameservers, r.delegated, r.age)
		}
	}
	_ = tw.Flush()

	t := tally(rows)
	noun := "zones"
	if len(rows) == 1 {
		noun = "zone"
	}
	_, _ = fmt.Fprintf(out, "\n%d %s — %d OK, %d Pending, %d Rejected, %d Error\n",
		len(rows), noun, t.ok, t.pending, t.rejected, t.failed)
	return nil
}

// newZoneRow renders one zone into its table cells.
func newZoneRow(z *dnsv1alpha1.DNSZone) zoneRow {
	word, _ := util.ZoneStatus(z)
	d := util.DelegationState(z)

	name := zoneDisplayName(z)

	domain := ""
	if z.Status.DomainRef != nil {
		domain = z.Status.DomainRef.Name
	}

	return zoneRow{
		name:   name,
		status: word,
		// status.recordCount counts record entries, not DNSRecordSet objects.
		records:     fmt.Sprintf("%d", z.Status.RecordCount),
		nameservers: util.OrDash(strings.Join(z.Status.Nameservers, ", ")),
		delegated:   delegatedCell(d),
		age:         util.RelativeAge(z.CreationTimestamp),
		class:       util.OrDash(z.Spec.DNSZoneClassName),
		domain:      util.OrDash(domain),
	}
}

// delegatedCell compresses the delegation state into the yes/no a table column
// has room for. "unknown" is its own answer: with no linked Domain there is
// nothing to compare, and reporting "no" there would accuse a registrar that
// may well be configured correctly.
func delegatedCell(d util.Delegation) string {
	switch d.State {
	case util.DelegationComplete:
		return "yes"
	case util.DelegationIncomplete:
		return "no"
	case util.DelegationPartial:
		return fmt.Sprintf("partial (%d/%d)", d.SetCount, d.Total)
	default:
		return "unknown"
	}
}

// statusTally is the footer's per-status count of the rows that survived
// filtering.
type statusTally struct{ ok, pending, rejected, failed int }

// tally counts the filtered rows by status.
//
// Rejected has its own bucket rather than being folded into Error. They are
// different states needing different actions — admission refused a Rejected
// zone and the object has to be deleted, while an Error zone failed to program
// and may yet recover — and a row that reads "Rejected" being counted under
// "Error" is a footer that contradicts the table above it. Anything else
// unrecognised still counts as an error, since an unknown state is not a
// working one.
func tally(rows []zoneRow) statusTally {
	var t statusTally
	for _, r := range rows {
		switch r.status {
		case util.StatusOK:
			t.ok++
		case util.StatusPending:
			t.pending++
		case util.StatusRejected:
			t.rejected++
		default:
			t.failed++
		}
	}
	return t
}

// printEmpty renders the empty state. An empty list is never an error: it is
// either a new project, which gets a starting point, or a filter that matched
// nothing, which names the filter.
func printEmpty(out io.Writer, project, statusFilter string) {
	if statusFilter != "" {
		_, _ = fmt.Fprintf(out, "No DNS zones in project %s match status=%s.\n", project, strings.ToLower(statusFilter))
		return
	}
	_, _ = fmt.Fprintf(out, "No DNS zones found in project %s.\n\n", project)
	_, _ = fmt.Fprintf(out, "Get started:\n")
	_, _ = fmt.Fprintf(out, "  datumctl dns zone create example.com\n")
}

func isKnownStatusFilter(s string) bool {
	for _, f := range statusFilters {
		if strings.EqualFold(f, s) {
			return true
		}
	}
	return false
}
