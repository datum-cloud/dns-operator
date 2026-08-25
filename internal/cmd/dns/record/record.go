// SPDX-License-Identifier: AGPL-3.0-only

// Package record implements `datumctl dns record`: a record-level view over an
// API that stores one DNSRecordSet per (zone, type).
//
// The bucketing is the thing this package hides. `www`, `api` and `@` all live
// in the zone's single A object, so every read flattens spec.records[] into one
// row per entry and every write is a read-modify-write on the bucket, sent back
// with a resourceVersion precondition so two concurrent writers cannot silently
// clobber each other.
package record

import (
	"github.com/spf13/cobra"
)

// Command builds the `dns record` noun and its verbs.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "record",
		Aliases: []string{"records", "rr"},
		Short:   "Manage DNS records in a zone",
		Long: `Manage the records in a Datum DNS zone.

Records are presented one per value. The API stores them bucketed by
(zone, type) in a DNSRecordSet; this command hides that: it flattens the
buckets on read and rebuilds them on write.

Values are entered in zone-file presentation format for the flat types and
with named flags for the structured ones. Both notations work for every type,
so a value pasted out of a provider export needs no translation.`,
		Example: `  # Everything in the zone
  datumctl dns record list example.com

  # Add an address record, then a second one at the same name
  datumctl dns record create example.com www A 203.0.113.10
  datumctl dns record create example.com www A 203.0.113.11

  # Replace every value at a name
  datumctl dns record set example.com www A 203.0.113.20

  # Structured types are taught with named flags
  datumctl dns record create example.com @ MX --preference 10 --exchange mail.example.com.

  # Remove one value, or every value at a name
  datumctl dns record delete example.com www A 203.0.113.11
  datumctl dns record delete example.com www A`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	cmd.AddCommand(
		listCommand(),
		createCommand(),
		setCommand(),
		deleteCommand(),
		describeCommand(),
		applyCommand(),
	)

	return cmd
}
