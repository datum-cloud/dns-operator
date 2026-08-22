// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"github.com/spf13/cobra"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// recordInput is a mutation's arguments after parsing and validation: an owner
// name, a type, and the values, in canonical (unencoded) form.
type recordInput struct {
	// rawName is the owner name exactly as the user typed it, kept so the name
	// can be re-derived once the zone's authoritative domain is known.
	rawName   string
	ownerName string
	rrType    dnsv1alpha1.RRType
	entries   []dnsv1alpha1.RecordEntry
	ttl       *int64
	ttlSet    bool
	fromFlags bool
	warnings  []string
}

// parseRecordInput reads the record grammar off the command line.
//
// Three notations reach here and all three end in the same RecordEntry values:
// positional presentation format, named flags, and a whole `dig`-shaped line.
// Mixing the first two for the same value is a usage error rather than a merge,
// because a merge would have to decide which one the user meant.
//
// Every value is validated before this returns. The API server admits a record
// whose typed field does not match spec.recordType and the backend then skips
// it silently, so a record that cannot resolve must be stopped here or it is
// never stopped at all.
// zoneDomain is the zone as the user spelled it on the command line. Parsing
// runs before the zone is fetched — an unparseable type or a malformed address
// is exit 2 whether or not the zone happens to exist, and a script branching on
// the exit code must not see that answer change with unrelated state. rebind
// re-checks against the authoritative domain once it is known.
func parseRecordInput(cmd *cobra.Command, zoneDomain string, args []string, line, ttlFlag string) (*recordInput, error) {
	var (
		nameArg string
		typeArg string
		values  []string
		lineTTL *int64
	)

	if line != "" {
		if len(args) > 1 {
			return nil, util.UsageErrorf("--line carries the whole record; %s was also given positionally", args[1]).
				WithFix("use either `--line \"www 300 IN A 203.0.113.10\"` or `www A 203.0.113.10`, not both.")
		}
		if anyRdataFlagSet(cmd) {
			return nil, util.UsageErrorf("record data was given both with --line and as named flags").
				WithFix("--line already carries the value; drop the type flags.")
		}
		parsed, err := rdata.ParseLine(line)
		if err != nil {
			return nil, usageFromRdata(err)
		}
		nameArg, typeArg, values, lineTTL = parsed.Name, string(parsed.Type), []string{parsed.Rdata}, parsed.TTL
	} else {
		if len(args) < 3 {
			return nil, util.UsageErrorf("a name, a type and at least one value are required").
				WithFix("`... <domain> <name> <TYPE> <value>`, for example:\n" +
					"       datumctl dns record create example.com www A 203.0.113.10")
		}
		nameArg, typeArg, values = args[1], args[2], args[3:]
	}

	t, err := rdata.ParseRRType(typeArg)
	if err != nil {
		return nil, usageFromRdata(err)
	}
	if err := validateRdataFlags(cmd, t); err != nil {
		return nil, err
	}

	in := &recordInput{rawName: nameArg, rrType: t}

	// --ttl wins over a TTL spelled inside --line, so the flag stays the one
	// place a script sets it regardless of how the value was supplied.
	switch {
	case cmd.Flags().Changed("ttl"):
		ttl, terr := rdata.ParseTTL(ttlFlag)
		if terr != nil {
			return nil, usageFromRdata(terr)
		}
		in.ttl, in.ttlSet = ttl, true
	case lineTTL != nil:
		in.ttl, in.ttlSet = lineTTL, true
	}

	flagEntry, fromFlags, err := rdata.FromFlags(cmd.Flags(), t)
	if err != nil {
		return nil, usageFromRdata(err)
	}
	switch {
	case fromFlags && len(values) > 0:
		fix := "use one notation or the other."
		// fromFlags implies the type has flags, but the fix line reads off
		// FlagNames rather than assuming that, so a type that loses its flags
		// later degrades to a duller message instead of a panic.
		if names := rdata.FlagNames(t); len(names) > 0 {
			fix = "use one notation or the other — `" + string(t) + " " + values[0] +
				"` or the --" + names[0] + " form."
		}
		return nil, util.UsageErrorf("record data was given both positionally and as named flags").WithFix(fix)

	case fromFlags:
		in.fromFlags = true
		if t == dnsv1alpha1.RRTypeTXT && flagEntry.TXT != nil {
			// --data @file and --data - are the answer to TXT's shell-quoting
			// problem, which is where SPF and DKIM values live.
			data, derr := rdata.ResolveTXTData(flagEntry.TXT.Content, cmd.InOrStdin())
			if derr != nil {
				return nil, usageFromRdata(derr)
			}
			flagEntry.TXT = &dnsv1alpha1.TXTRecordSpec{Content: data}
		}
		in.entries = []dnsv1alpha1.RecordEntry{flagEntry}

	case len(values) == 0:
		return nil, util.UsageErrorf("a value is required for a %s record", t).
			WithFix(valueFix(t))

	default:
		for _, v := range values {
			e, perr := rdata.ParseValue(t, v)
			if perr != nil {
				return nil, usageFromRdata(perr)
			}
			in.entries = append(in.entries, e)
		}
	}

	for i := range in.entries {
		in.entries[i].TTL = in.ttl
	}

	if err := in.precheck(zoneGuessFrom(zoneDomain)); err != nil {
		return nil, err
	}
	return in, nil
}

// precheck is the pre-API validation pass; see rebind for why there are two and
// why this one is handed a guess rather than the zone.
func (in *recordInput) precheck(zoneGuess string) error {
	return in.rebind(zoneGuess)
}

// rebind resolves the owner name and re-validates every value against a zone
// domain. It runs twice: once before any API call, against the zone as the user
// spelled it, and again once resolveZone has produced the authoritative domain.
//
// It runs twice because the zone positional accepts two different things. Users
// type the domain, but the DNSZone object's name is accepted as a convenience,
// and the owner-name and trailing-dot rules are only correct against the real
// domain — so whatever the first pass concluded about the name is re-derived
// from rawName once the domain is known. Do not collapse this to a single
// post-resolve call: validating after the fetch is what used to mask a bad
// record type behind a zone-not-found and make the exit code for identical
// input depend on whether the zone happened to exist.
//
// The first pass is handed zoneGuessFrom's answer rather than the positional
// itself, and that gate is load-bearing. rdata's zone-aware validation makes an
// out-of-zone owner name a hard error, so pre-checking an absolute name against
// a literal object name like "example-com" would reject something perfectly
// valid. An undotted positional therefore means "zone not known yet" and the
// zone-relative rules wait for the second pass, while type, arity, rdata
// syntax, TTL and notation exclusivity are all still caught before the API
// call. That is the most a pre-API check can honestly claim without inventing
// a false rejection.
//
// Validation is a whole-slice call, never a loop over Validate: single-valued
// types are a property of the set, and a two-value CNAME passes entry by entry
// while the backend keeps one value and discards the other without a word.
func (in *recordInput) rebind(zoneDomain string) error {
	ownerName, warnings, err := rdata.NormalizeNameWithWarnings(in.rawName, zoneDomain)
	if err != nil {
		return usageFromRdata(err)
	}
	in.ownerName = ownerName
	in.warnings = warnings

	for i := range in.entries {
		in.entries[i].Name = ownerName
	}
	if err := rdata.ValidateEntriesInZone(in.rrType, canonicalEntries(in.rrType, in.entries), zoneDomain); err != nil {
		return usageFromRdata(err)
	}
	in.warnings = append(in.warnings, rdata.WarningsInZone(in.rrType, zoneDomain, in.entries...)...)
	return nil
}

// valueFix names the grammar for a type when no value was given.
func valueFix(t dnsv1alpha1.RRType) string {
	if names := rdata.FlagNames(t); len(names) > 0 {
		return "give the value positionally, or with --" + names[0] + " and its companions."
	}
	return "give the value positionally, for example `" + string(t) + " " + exampleRdata(t) + "`."
}

func exampleRdata(t dnsv1alpha1.RRType) string {
	switch t {
	case dnsv1alpha1.RRTypeA:
		return "203.0.113.10"
	case dnsv1alpha1.RRTypeAAAA:
		return "2001:db8::1"
	default:
		return "target.example.net."
	}
}

// anyRdataFlagSet reports whether the user supplied any named rdata flag.
func anyRdataFlagSet(cmd *cobra.Command) bool {
	for _, n := range allRdataFlagNames() {
		if f := cmd.Flags().Lookup(n); f != nil && f.Changed {
			return true
		}
	}
	return false
}

// canonicalEntries maps a slice through canonicalEntry, for validation and
// comparison against freshly parsed input.
func canonicalEntries(t dnsv1alpha1.RRType, entries []dnsv1alpha1.RecordEntry) []dnsv1alpha1.RecordEntry {
	out := make([]dnsv1alpha1.RecordEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, canonicalEntry(t, e))
	}
	return out
}
