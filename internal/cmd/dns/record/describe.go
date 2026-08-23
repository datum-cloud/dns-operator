// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

func describeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "describe <domain> <name> [<TYPE>]",
		Aliases: []string{"show", "get"},
		Short:   "Show a record's values, fields, and why it is or is not live",
		Long: `Show everything known about the records at one name.

The values are shown both ways: in presentation format, and broken out into
the named fields the structured types are taught with. The status detail is the
backend's own sentence, reproduced verbatim — those messages are written for
people, and rewording them would only add a translation layer to be wrong.

Omit the type to see every type at that name.`,
		Example: `  # Every record at a name
  datumctl dns record describe example.com www

  # One type
  datumctl dns record describe example.com @ MX`,
		Args:              cobra.RangeArgs(2, 3),
		ValidArgsFunction: zoneNameArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDescribe(cmd, args)
		},
	}

	return cmd
}

func runDescribe(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	format, err := util.ParseOutputFormat(outputFlag(cmd),
		util.OutputTable, util.OutputWide, util.OutputJSON, util.OutputYAML)
	if err != nil {
		return err
	}

	// Arguments before the API call, as everywhere else: a bad type is exit 2
	// whether or not the zone turns out to exist.
	var rrTypes []dnsv1alpha1.RRType
	if len(args) == 3 {
		t, terr := rdata.ParseRRType(args[2])
		if terr != nil {
			return usageFromRdata(terr)
		}
		rrTypes = []dnsv1alpha1.RRType{t}
	}
	if err := precheckName(args[1], normalizeDomain(args[0])); err != nil {
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
	zoneDomain := zone.Spec.DomainName

	ownerName, warnings, err := rdata.NormalizeNameWithWarnings(args[1], zoneDomain)
	if err != nil {
		return usageFromRdata(err)
	}
	printWarnings(cmd.ErrOrStderr(), warnings)

	sets, err := listSets(ctx, c, zone, rrTypes)
	if err != nil {
		return err
	}

	// Only the sets that actually hold this name matter; the rest of the zone's
	// buckets are not this record.
	var owning []dnsv1alpha1.DNSRecordSet
	for i := range sets {
		if setHasOwner(&sets[i], ownerName, zoneDomain) {
			owning = append(owning, sets[i])
		}
	}
	if len(owning) == 0 {
		return describeNotFound(ownerName, zoneDomain, rrTypes)
	}

	switch format {
	case util.OutputJSON:
		return util.PrintJSON(out, recordSetList(owning))
	case util.OutputYAML:
		return util.PrintYAML(out, recordSetList(owning))
	}

	sort.Slice(owning, func(i, j int) bool { return owning[i].Spec.RecordType < owning[j].Spec.RecordType })
	for i := range owning {
		if i > 0 {
			_, _ = fmt.Fprintln(out)
		}
		describeSet(out, &owning[i], zone, ownerName)
	}

	printNextSteps(out, zone, ownerName, owning)
	return nil
}

func describeSet(out io.Writer, rs *dnsv1alpha1.DNSRecordSet, zone *dnsv1alpha1.DNSZone, ownerName string) {
	t := rs.Spec.RecordType
	zoneDomain := zone.Spec.DomainName
	entries := entriesForOwner(rs.Spec.Records, ownerName, zoneDomain)

	_, _ = fmt.Fprintf(out, "%-13s %s\n", "Record", ownerDisplay(ownerName, zoneDomain))
	_, _ = fmt.Fprintf(out, "%-13s %s\n", "Zone", zoneDomain)
	_, _ = fmt.Fprintf(out, "%-13s %s\n", "Type", t)
	_, _ = fmt.Fprintf(out, "%-13s %s\n", "TTL", describeTTL(entries))

	if prov := classify(rs, dnsv1alpha1.RecordEntry{Name: ownerName}, zone.Spec.DomainName); prov != provUser {
		_, _ = fmt.Fprintf(out, "%-13s %s\n", "Managed by", managedBy(prov, rs))
	}
	_, _ = fmt.Fprintf(out, "%-13s %s\n", "Record set", rs.Name)
	_, _ = fmt.Fprintf(out, "%-13s %s\n", "Created", util.RelativeAgeVerbose(rs.CreationTimestamp))
	_, _ = fmt.Fprintln(out)

	_, _ = fmt.Fprintf(out, "Values\n")
	for _, raw := range entries {
		e := canonicalEntry(t, raw)
		_, _ = fmt.Fprintf(out, "  %s\n", rdata.Render(t, e))
		// The named fields are shown because presentation format is what most
		// records were entered as, and the flags are the notation that stays
		// readable six months later. Flat types have a single field that
		// repeats the value, so they are left alone.
		if fields := rdata.Fields(t, e); len(fields) > 1 {
			for _, f := range fields {
				_, _ = fmt.Fprintf(out, "      %-12s %s\n", f[0]+":", f[1])
			}
		}
	}
	_, _ = fmt.Fprintln(out)

	word, detail := util.RecordStatusInZone(rs, ownerName, zoneDomain)
	_, _ = fmt.Fprintf(out, "%-13s %s\n", "Status", word)
	if detail != "" {
		_, _ = fmt.Fprintf(out, "%-13s %s\n", "", detail)
	}
}

// describeTTL spells out what Auto resolves to, so the number is never hidden,
// and reports honestly when the entries disagree.
func describeTTL(entries []dnsv1alpha1.RecordEntry) string {
	if len(entries) == 0 {
		return rdata.FormatTTL(nil)
	}
	first := rdata.FormatTTL(entries[0].TTL)
	for _, e := range entries[1:] {
		// Effective TTLs, so an entry left Auto beside an explicit 300 is not
		// reported as a disagreement — they resolve to the same number.
		if !util.TTLEqual(e.TTL, entries[0].TTL) {
			return fmt.Sprintf("%s (values disagree; the backend applies the first)", first)
		}
	}
	if entries[0].TTL == nil {
		// Spelled out so "Auto" is never a mystery number.
		return fmt.Sprintf("Auto (%s)", rdata.FormatSeconds(util.DefaultTTL))
	}
	return first
}

func managedBy(p provenance, rs *dnsv1alpha1.DNSRecordSet) string {
	if p == provGateway {
		if owner := gatewayOwner(rs); owner != "" {
			return fmt.Sprintf("AI Edge — %s %q; this record is read-only", sourceKind(rs), owner)
		}
		return "AI Edge; this record is read-only"
	}
	return "the platform; editing requires --force"
}

func printNextSteps(out io.Writer, zone *dnsv1alpha1.DNSZone, ownerName string, owning []dnsv1alpha1.DNSRecordSet) {
	domain := zone.Spec.DomainName
	name := displayName(ownerName)
	t := string(owning[0].Spec.RecordType)

	_, _ = fmt.Fprintf(out, "\nNext steps:\n")
	_, _ = fmt.Fprintf(out, "  Change the value:    datumctl dns record set %s %s %s <value>\n", domain, name, t)
	_, _ = fmt.Fprintf(out, "  Add another value:   datumctl dns record create %s %s %s <value>\n", domain, name, t)
	_, _ = fmt.Fprintf(out, "  Remove it:           datumctl dns record delete %s %s %s\n", domain, name, t)
	_, _ = fmt.Fprintf(out, "  See the whole zone:  datumctl dns record list %s\n", domain)
}

func describeNotFound(ownerName, zoneDomain string, rrTypes []dnsv1alpha1.RRType) error {
	if len(rrTypes) == 1 {
		return notFoundError(rrTypes[0], ownerName, zoneDomain)
	}
	return util.NewCLIError(util.ExitNotFound,
		fmt.Sprintf("no records for %s", ownerDisplay(ownerName, zoneDomain))).
		WithFix(fmt.Sprintf("list what is there: datumctl dns record list %s", zoneDomain))
}
