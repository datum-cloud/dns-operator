// SPDX-License-Identifier: AGPL-3.0-only

package bind

import (
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
)

// RecordsFromSet flattens one DNSRecordSet into the Record shape the diff, the
// summary table and the emitter all consume, with every entry in its logical
// form so it compares equal to the same record read out of a zone file.
func RecordsFromSet(set *dnsv1alpha1.DNSRecordSet) []Record {
	out := make([]Record, 0, len(set.Spec.Records))
	for _, entry := range set.Spec.Records {
		// rdata owns the wire/logical conversion for every type; nothing here
		// knows or needs to know that TXT is currently the only one with a wire
		// form that differs.
		e := rdata.EntryFromAPI(set.Spec.RecordType, entry)
		name := e.Name
		if name == "" {
			name = "@"
		}
		e.Name = name
		out = append(out, Record{
			Name:  name,
			TTL:   e.TTL,
			Type:  set.Spec.RecordType,
			Entry: e,
		})
	}
	return out
}
