// SPDX-License-Identifier: AGPL-3.0-only

package zone

import (
	"bytes"
	"fmt"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"go.miloapis.com/dns-operator/internal/cmd/dns/bind"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// exportTTL is the $TTL written at the top of an exported file. It is the
// backend's own default for an entry with no TTL, so a record the API stores as
// "Auto" exports as the number it actually resolves with rather than as a blank
// the reader has to look up.
const exportTTL = 300

func exportCommand() *cobra.Command {
	var file string

	cmd := &cobra.Command{
		Use:   "export <domain>",
		Short: "Write a zone's records out as a BIND zone file",
		Long: "Export flattens every record set in the zone back into a BIND zone file.\n\n" +
			"The output is what `datumctl dns record apply` reads, so export, edit and apply is a " +
			"closed loop: exporting and re-applying an untouched file reports no changes.",
		Example: "  # Print the zone to the terminal\n" +
			"  datumctl dns zone export example.com\n\n" +
			"  # Save it, edit it, then apply the difference\n" +
			"  datumctl dns zone export example.com --file example.com.zone\n" +
			"  datumctl dns record apply example.com -f example.com.zone",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: util.CompleteZoneNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runExport(cmd, args[0], file)
		},
	}

	cmd.Flags().StringVarP(&file, "file", "f", "", "Write to this path instead of standard output")
	return cmd
}

func runExport(cmd *cobra.Command, domain, file string) error {
	ctx := cmd.Context()

	c, err := clientFactory(util.ProjectFromCmd(cmd))
	if err != nil {
		return err
	}

	zone, err := getZone(ctx, c, util.ProjectFromCmd(cmd), domain)
	if err != nil {
		return err
	}

	sets, err := zoneRecordSets(ctx, c, zone)
	if err != nil {
		return util.ClassifyError(err)
	}
	sort.Slice(sets, func(i, j int) bool { return sets[i].Name < sets[j].Name })

	var records []bind.Record
	for i := range sets {
		records = append(records, bind.RecordsFromSet(&sets[i])...)
	}

	// The file is built in memory so a failure part-way through leaves neither a
	// truncated file on disk nor half a zone on the terminal.
	var buf bytes.Buffer
	if err := bind.Emit(&buf, zone.Spec.DomainName, exportTTL, records); err != nil {
		return util.NewCLIError(util.ExitError,
			fmt.Sprintf("writing zone file for %s: %v", zone.Spec.DomainName, err)).WithCause(err)
	}

	if file == "" {
		_, err := cmd.OutOrStdout().Write(buf.Bytes())
		return err
	}
	if err := os.WriteFile(file, buf.Bytes(), 0o644); err != nil {
		return util.NewCLIError(util.ExitError,
			fmt.Sprintf("writing %q: %v", file, err)).WithCause(err)
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Wrote %d record(s) to %s\n", len(records), file)
	return nil
}
