// SPDX-License-Identifier: AGPL-3.0-only

package zone

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/bind"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// discoveryTypes is what the DNSZoneDiscovery controller queries. It is
// deliberately narrower than the API's RRType enum, and a migration that does
// not say so out loud leaves the user believing their NS and SOA came across.
var discoveryTypes = []dnsv1alpha1.RRType{
	dnsv1alpha1.RRTypeA, dnsv1alpha1.RRTypeAAAA, dnsv1alpha1.RRTypeCNAME,
	dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RRTypeMX, dnsv1alpha1.RRTypeSRV,
	dnsv1alpha1.RRTypeCAA, dnsv1alpha1.RRTypeTLSA, dnsv1alpha1.RRTypeHTTPS,
	dnsv1alpha1.RRTypeSVCB,
}

// Per-record outcomes, in the order the summary tallies them.
const (
	outcomeCreated = "created"
	outcomeUpdated = "updated"
	outcomeSkipped = "skipped"
	// outcomeKept is a record already in the zone that the import preserved
	// rather than touched. It is reported for the same reason a skip is: a
	// platform record named in the FILE is announced as skipped, so a platform
	// record already LIVE and carried through --replace has to be announced
	// too. They are the same decision about the same records, and reporting
	// only one of them is what let two separate delegation bugs print a clean
	// "1 record — 1 created" while the apex NS records were being removed.
	outcomeKept   = "kept"
	outcomeFailed = "failed"
)

func importCommand() *cobra.Command {
	var (
		file     string
		discover bool
		replace  bool
		dryRun   bool
		timeout  time.Duration
	)

	cmd := &cobra.Command{
		Use:   "import <domain>",
		Short: "Import records from a BIND zone file or by discovering the live zone",
		Long: "Import bulk-loads records into a zone, either from a BIND zone file or from a " +
			"snapshot of what the domain resolves to today.\n\n" +
			"Records are grouped by type before writing, so each record type costs one API call " +
			"regardless of how many records it holds. TTLs are taken from the file as written — " +
			"unlike the portal, nothing is rounded onto a preset ladder.",
		Example: "  # Load a zone file exported from another provider\n" +
			"  datumctl dns zone import example.com --file example.com.zone\n\n" +
			"  # Snapshot what the domain resolves to today and import that\n" +
			"  datumctl dns zone import example.com --discover\n\n" +
			"  # Replace each type present in the file rather than merging into it\n" +
			"  datumctl dns zone import example.com --file example.com.zone --replace",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: util.CompleteZoneNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch {
			case file == "" && !discover:
				return util.UsageErrorf("one of --file or --discover is required").
					WithFix("pass --file <zonefile> to import a BIND file, or --discover to snapshot the live zone.")
			case file != "" && discover:
				return util.UsageErrorf("--file and --discover are mutually exclusive").
					WithFix("import a file, or discover the live zone — not both in one command.")
			}
			return runImport(cmd, args[0], importOptions{
				file:    file,
				replace: replace,
				dryRun:  dryRun,
				timeout: timeout,
			})
		},
	}

	cmd.Flags().StringVarP(&file, "file", "f", "", "BIND zone file to import, or \"-\" for stdin")
	cmd.Flags().BoolVar(&discover, "discover", false,
		"Snapshot the records the domain resolves to today and import those")
	cmd.Flags().BoolVar(&replace, "replace", false,
		"Replace the existing records of each type in the input instead of merging into them")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"Validate against the API server without writing anything")
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Minute,
		"How long to wait for --discover to finish")
	return cmd
}

type importOptions struct {
	file    string
	replace bool
	dryRun  bool
	timeout time.Duration
}

// outcome is one input record and what happened to it.
type outcome struct {
	rec    bind.Record
	result string
	detail string
}

func runImport(cmd *cobra.Command, domain string, opts importOptions) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	// A zone file is read and parsed before the first API call, so a syntax
	// error on line 40 fails as a line-numbered usage error rather than as a
	// zone-not-found, and the user is not told to fix the wrong thing. The
	// positional is only a guess here — it also accepts a DNSZone object name,
	// and zone-relative rules cannot run against "example-com" — so the pass is
	// skipped when it carries no dot, exactly as the record verbs do.
	var source []byte
	if opts.file != "" {
		var rerr error
		if source, rerr = readSource(cmd, opts.file); rerr != nil {
			return rerr
		}
		if guess := normalizeDomain(domain); strings.Contains(guess, ".") {
			if _, _, perr := parseZoneFile(source, opts.file, guess, io.Discard); perr != nil {
				return perr
			}
		}
	}

	c, err := clientFactory(util.ProjectFromCmd(cmd))
	if err != nil {
		return err
	}

	zone, err := getZone(ctx, c, util.ProjectFromCmd(cmd), domain)
	if err != nil {
		return err
	}

	var (
		records  []bind.Record
		warnings []string
	)
	if opts.file != "" {
		// The authoritative pass, against the zone the server returned.
		records, warnings, err = parseZoneFile(source, opts.file, zone.Spec.DomainName, errOut)
	} else {
		// Discovery reads structured status, not text, so it has no advisories
		// of its own to report.
		records, err = importFromDiscovery(ctx, c, zone, opts.timeout, errOut)
	}
	if err != nil {
		return err
	}
	for _, w := range warnings {
		_, _ = fmt.Fprintf(errOut, "Warning: %s\n", w)
	}

	records = rewriteApexCNAME(records, zone.Spec.DomainName, errOut)
	if len(records) == 0 {
		_, _ = fmt.Fprintln(out, "Nothing to import.")
		return nil
	}

	if opts.replace {
		if !util.AssumeYes(cmd) {
			prompt := fmt.Sprintf(
				"--replace will discard the existing records of every type in the input for %s. Continue?",
				zone.Spec.DomainName)
			ok, cerr := util.ConfirmYesNo(cmd.InOrStdin(), errOut, prompt, false)
			if cerr != nil {
				return cerr
			}
			if !ok {
				return util.NewCLIError(util.ExitAborted, "import cancelled")
			}
		}
	}

	outcomes := convergeImport(ctx, c, zone, records, opts, errOut)
	return reportImport(out, outcomes, opts.dryRun)
}

// readSource slurps the zone file. It is read once, into memory, because the
// file is parsed twice — before the API call against the guessed zone and again
// against the resolved one — and standard input cannot be rewound.
func readSource(cmd *cobra.Command, path string) ([]byte, error) {
	r := cmd.InOrStdin()
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, util.NewCLIError(util.ExitUsage,
				fmt.Sprintf("reading zone file %q: %v", path, err)).WithCause(err)
		}
		defer f.Close() //nolint:errcheck // read-only
		r = f
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, util.NewCLIError(util.ExitUsage,
			fmt.Sprintf("reading zone file %q: %v", displayPath(path), err)).WithCause(err)
	}
	return data, nil
}

// parseZoneFile parses a zone file against a zone and reports what it could not
// carry across. Advisories go to errOut, which the pre-API pass discards so the
// user is not told the same thing twice.
func parseZoneFile(
	source []byte, path, domain string, errOut io.Writer,
) ([]bind.Record, []string, error) {
	res, err := bind.Parse(bytes.NewReader(source), domain, nil)
	if err != nil {
		e := util.NewCLIError(util.ExitUsage, fmt.Sprintf("%s: %v", displayPath(path), err)).WithCause(err)
		if fix := bind.FixFor(err); fix != "" {
			e = e.WithFix(fix)
		}
		return nil, nil, e
	}
	reportUnsupported(errOut, res.Unsupported)
	return res.Records, res.Warnings, nil
}

// reportUnsupported names every record the API cannot store. Listing them is
// the whole difference between a migration the user can finish elsewhere and
// one that quietly loses records.
func reportUnsupported(w io.Writer, unsupported []bind.Unsupported) {
	if len(unsupported) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "\n%s use types Datum DNS does not support and were not imported:\n",
		pluralize(len(unsupported), "record", "records"))
	for _, u := range unsupported {
		_, _ = fmt.Fprintf(w, "  line %d  %s  %s\n", u.Line, u.Type, u.Raw)
	}
	_, _ = fmt.Fprintf(w, "Supported types: %s\n\n", typeNames(rdata.SupportedTypes()))
}

// importFromDiscovery snapshots the zone as it resolves today.
func importFromDiscovery(
	ctx context.Context, c client.Client, zone *dnsv1alpha1.DNSZone,
	timeout time.Duration, errOut io.Writer,
) ([]bind.Record, error) {
	disc, err := ensureDiscovery(ctx, c, zone)
	if err != nil {
		return nil, err
	}

	if cond := util.FindCondition(disc.Status.Conditions, "Discovered"); cond == nil || cond.Status != metav1.ConditionTrue {
		_, _ = fmt.Fprintf(errOut, "Discovering the records %s resolves to today…\n", zone.Spec.DomainName)
		disc, err = waitForDiscovery(ctx, c, disc, timeout)
		if err != nil {
			return nil, err
		}
	}

	var records []bind.Record
	for _, rs := range disc.Status.RecordSets {
		for _, entry := range rs.Records {
			e := rdata.EntryFromAPI(rs.RecordType, entry)
			// Discovery's records come from the OLD PROVIDER'S zone data, and
			// the operator relativizes them only as far as mapping "" to "@".
			// A file gets its owner names canonicalised by the parser; giving
			// discovery the same treatment means one spelling reaches the rest
			// of the command whichever way the records arrived, and the summary
			// table shows "@" rather than "example.com.".
			//
			// This is convenience, not the guard. platformOwned and ownerEqual
			// are zone-aware in their own right, so a spelling that slips past
			// here is still recognised where it matters.
			name, warns, err := rdata.NormalizeNameWithWarnings(e.Name, zone.Spec.DomainName)
			if err != nil {
				// Unnormalizable is not this function's error to raise: keep the
				// name as discovered and let per-record validation report it
				// against the record it belongs to.
				name = e.Name
			}
			for _, w := range warns {
				_, _ = fmt.Fprintf(errOut, "Warning: discovered %s record: %s\n", rs.RecordType, w)
			}
			e.Name = name
			records = append(records, bind.Record{
				Name: name, TTL: e.TTL, Type: rs.RecordType, Entry: e,
			})
		}
	}

	_, _ = fmt.Fprintf(errOut, "Discovered %s across %s.\n",
		pluralize(len(records), "record", "records"),
		pluralize(len(disc.Status.RecordSets), "type", "types"))
	_, _ = fmt.Fprintf(errOut,
		"Discovery queries %s only — NS, SOA, PTR and ALIAS records are never returned and must be added by hand.\n\n",
		typeNames(discoveryTypes))
	return records, nil
}

// ensureDiscovery reuses a discovery that already ran for this zone rather than
// littering the namespace with one object per import. DNSZoneDiscovery is
// write-once and has no lifecycle beyond its snapshot, so an old one is only
// worth reusing when it actually finished.
func ensureDiscovery(ctx context.Context, c client.Client, zone *dnsv1alpha1.DNSZone) (*dnsv1alpha1.DNSZoneDiscovery, error) {
	// Filtered client-side rather than through the selectable field: a project
	// holds a handful of these at most, and the same code then works against a
	// control plane that has not indexed the field.
	var list dnsv1alpha1.DNSZoneDiscoveryList
	if err := c.List(ctx, &list, client.InNamespace(util.ResourceNamespace)); err != nil {
		return nil, util.ClassifyError(fmt.Errorf("listing zone discoveries: %w", err))
	}
	var mine []*dnsv1alpha1.DNSZoneDiscovery
	for i := range list.Items {
		if list.Items[i].Spec.DNSZoneRef.Name == zone.Name {
			mine = append(mine, &list.Items[i])
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].Name < mine[j].Name })
	for _, d := range mine {
		if cond := util.FindCondition(d.Status.Conditions, "Discovered"); cond != nil && cond.Status == metav1.ConditionTrue {
			return d, nil
		}
	}
	if len(mine) > 0 {
		// One is already in flight; wait on it rather than starting a second.
		return mine[0], nil
	}

	disc := &dnsv1alpha1.DNSZoneDiscovery{
		ObjectMeta: metav1.ObjectMeta{
			Name:      zone.Name + "-discovery",
			Namespace: util.ResourceNamespace,
		},
		Spec: dnsv1alpha1.DNSZoneDiscoverySpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
		},
	}
	if err := c.Create(ctx, disc); err != nil {
		return nil, util.ClassifyError(err)
	}
	return disc, nil
}

// waitForDiscovery polls until the snapshot is ready. The timeout is bounded
// because a discovery that never reports is indistinguishable from one that is
// slow, and a CLI that hangs forever is worse than one that says so.
func waitForDiscovery(
	ctx context.Context, c client.Client, disc *dnsv1alpha1.DNSZoneDiscovery, timeout time.Duration,
) (*dnsv1alpha1.DNSZoneDiscovery, error) {
	key := client.ObjectKeyFromObject(disc)
	out := disc

	err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true,
		func(ctx context.Context) (bool, error) {
			var got dnsv1alpha1.DNSZoneDiscovery
			if err := c.Get(ctx, key, &got); err != nil {
				return false, err
			}
			out = &got
			if cond := util.FindCondition(got.Status.Conditions, "Discovered"); cond != nil {
				if cond.Status == metav1.ConditionTrue {
					return true, nil
				}
				if cond.Status == metav1.ConditionFalse {
					return false, util.NewCLIError(util.ExitError,
						"discovering zone records: "+strings.ToLower(cond.Message))
				}
			}
			if cond := util.FindCondition(got.Status.Conditions, "Accepted"); cond != nil &&
				cond.Status == metav1.ConditionFalse {
				return false, util.NewCLIError(util.ExitInvalid,
					"the discovery request was rejected: "+strings.ToLower(cond.Message))
			}
			return false, nil
		})
	if err != nil {
		if cliErr, ok := err.(*util.CLIError); ok {
			return nil, cliErr
		}
		if wait.Interrupted(err) {
			return nil, util.NewCLIError(util.ExitUnavailable,
				fmt.Sprintf("discovery did not finish within %s", timeout)).
				WithFix("re-run the command — the snapshot is kept and will be reused.").
				WithCause(err)
		}
		return nil, util.ClassifyError(err)
	}
	return out, nil
}

// rewriteApexCNAME turns an apex CNAME into an ALIAS. A CNAME at the apex is
// invalid DNS — it cannot coexist with the SOA and NS records every zone must
// have — and ALIAS is the record that does what the author meant. Each rewrite
// is announced, because it changes the record type the user asked for.
//
// The apex test is zone-aware. An apex CNAME spelled "example.com." rather than
// "@" is the same record, and a literal test would skip the rewrite and leave
// ValidateInZone to reject it with "a CNAME record may not exist at the zone
// apex" — safe, but the user loses the fix and gets an error instead, on the
// migration path where a provider export is the expected input and an apex
// CNAME is among the more common things one carries.
func rewriteApexCNAME(records []bind.Record, domain string, errOut io.Writer) []bind.Record {
	rewritten := 0
	for i := range records {
		r := &records[i]
		if r.Type != dnsv1alpha1.RRTypeCNAME || !rdata.IsApexIn(r.Name, domain) || r.Entry.CNAME == nil {
			continue
		}
		content := r.Entry.CNAME.Content
		r.Type = dnsv1alpha1.RRTypeALIAS
		r.Entry.CNAME = nil
		r.Entry.ALIAS = &dnsv1alpha1.ALIASRecordSpec{Content: content}
		rewritten++
		_, _ = fmt.Fprintf(errOut,
			"Rewrote the apex CNAME to %s as an ALIAS record — a CNAME at the zone apex is invalid DNS.\n",
			content)
	}
	if rewritten > 0 {
		_, _ = fmt.Fprintln(errOut)
	}
	return records
}

// typePlan is one record type's write, worked out before any of them are sent.
type typePlan struct {
	rrType   dnsv1alpha1.RRType
	existing *dnsv1alpha1.DNSRecordSet
	next     []dnsv1alpha1.RecordEntry
	// accepted are the input records this write covers, and results are their
	// outcomes if it succeeds, positionally aligned.
	accepted []bind.Record
	results  []string
}

// convergeImport plans every record type, and only then writes any of them.
//
// The two-phase shape is the point. Everything knowable without the API —
// type/field pairing, rdata syntax, single-valued types, duplicates within an
// owner — is decided across the whole input first, and a failure there writes
// nothing at all: half an imported zone file is worse than none of it, because
// the user cannot tell which half. Once the writes start, a failure is the
// server's and is reported per type, since by then some types are already
// committed and pretending otherwise would be a lie.
func convergeImport(
	ctx context.Context, c client.Client, zone *dnsv1alpha1.DNSZone,
	records []bind.Record, opts importOptions, warnTo io.Writer,
) []outcome {
	existing, listErr := bulkSetsByType(ctx, c, zone)
	if listErr != nil {
		return failAll(records, listErr.Error())
	}

	var (
		outcomes []outcome
		plans    []typePlan
		rejected bool
	)
	for _, t := range orderTypes(records) {
		group := recordsOfType(records, t)
		plan, decided := planType(zone, t, existing[t], group, opts, warnTo)
		outcomes = append(outcomes, decided...)
		// Only a failure aborts. A skip is a deliberate outcome — a platform
		// record the import declined to touch — and must not take the rest of
		// the file down with it, or no provider export would ever import.
		for _, o := range decided {
			if o.result == outcomeFailed {
				rejected = true
			}
		}
		if plan != nil {
			plans = append(plans, *plan)
		}
	}
	if rejected {
		// Report what the input got wrong and stop. Nothing has been written.
		for _, p := range plans {
			for _, r := range p.accepted {
				outcomes = append(outcomes, outcome{
					rec: r, result: outcomeSkipped, detail: "not attempted — the input has errors",
				})
			}
		}
		return outcomes
	}

	for _, p := range plans {
		if err := bulkWriteSet(ctx, c, zone, p.rrType, p.existing, p.next, opts.dryRun); err != nil {
			outcomes = append(outcomes, failAll(p.accepted, err.Error())...)
			continue
		}
		for i, r := range p.accepted {
			outcomes = append(outcomes, outcome{rec: r, result: p.results[i]})
		}
	}
	return outcomes
}

// planType works out one type's final entry list without writing anything. It
// returns the plan and the input records it had to reject; either may be empty.
func planType(
	zone *dnsv1alpha1.DNSZone, t dnsv1alpha1.RRType,
	existing *dnsv1alpha1.DNSRecordSet, group []bind.Record, opts importOptions, warnTo io.Writer,
) (*typePlan, []outcome) {
	domain := zone.Spec.DomainName

	// A Gateway controller reverts anything written to a set it owns, so a
	// success here would be a lie. That is the one managed case that fails
	// rather than being skipped, matching the tier `record create` uses.
	if gw, reason := gatewayOwned(existing); gw {
		return nil, failAll(group, "the "+string(t)+" records for this zone are "+reason)
	}

	outcomes := make([]outcome, 0, len(group))
	accepted := make([]bind.Record, 0, len(group))
	for _, r := range group {
		// The zone's own SOA and apex NS records are skipped, not imported and
		// not failed. Every provider zone-file export contains both — that is
		// what a zone file is — and "migrate my zone off the old provider" is
		// the case this command exists for, so failing on them would reject the
		// flagship input. Importing them is worse still: merged, the zone would
		// advertise both Datum's nameservers and the old provider's and resolve
		// inconsistently; replaced, delegation to Datum is destroyed and the
		// zone stops resolving. Either way the user's own import did it.
		if managed, reason := platformOwned(t, r.Name, domain); managed {
			outcomes = append(outcomes, outcome{rec: r, result: outcomeSkipped, detail: reason})
			continue
		}
		// Validation is not a convenience here. buildRRSets emits an rrset with
		// no records for an entry whose typed field does not match its record
		// type, which the client turns into a DELETE — so one malformed entry
		// can remove a correct RRset that already exists at the same name.
		if err := rdata.ValidateInZone(t, r.Entry, domain); err != nil {
			outcomes = append(outcomes, outcome{rec: r, result: outcomeFailed, detail: at(r, err.Error())})
			continue
		}
		accepted = append(accepted, r)
	}
	if len(accepted) == 0 {
		return nil, outcomes
	}

	var current, keep []dnsv1alpha1.RecordEntry
	if existing != nil {
		for _, e := range existing.Spec.Records {
			le := rdata.EntryFromAPI(t, e)
			current = append(current, le)
			if managed, _ := platformOwned(t, le.Name, domain); managed {
				keep = append(keep, le)
			}
		}
	}

	// next is a copy, never an alias of current. The merge path below assigns
	// into next[idx] and then reads current to decide created-versus-updated;
	// sharing one backing array would make the second read see the first write.
	// It happens to be harmless today — the lookup is value-keyed and the write
	// only changes a TTL — but the two are twenty lines apart and nothing
	// enforces that they stay that way.
	next := append([]dnsv1alpha1.RecordEntry(nil), current...)
	if opts.replace {
		// Everything preserved here is reported, using the owner spelling the
		// record is actually STORED under rather than a normalised one — the
		// point of the line is to show the user what survived, and an
		// absolutely-spelled entry appearing in it is a useful signal now that
		// the guard recognises those.
		for _, e := range keep {
			outcomes = append(outcomes, outcome{
				rec:    bind.Record{Name: e.Name, TTL: e.TTL, Type: t, Entry: e},
				result: outcomeKept, detail: keptReason(t),
			})
		}
		// --replace means "replace the records I am giving you", not "dismantle
		// the zone". A subdomain delegation shares the <zone>-ns object with the
		// platform's apex NS records, so replacing that type outright would take
		// the delegation with it; the platform's own entries survive either way.
		next = append([]dnsv1alpha1.RecordEntry(nil), keep...)
	}
	results := make([]string, len(accepted))
	for i, r := range accepted {
		idx := indexOfEntry(next, t, r.Entry, domain)
		switch {
		case idx >= 0 && util.TTLEqual(next[idx].TTL, r.Entry.TTL):
			results[i] = outcomeSkipped
		case idx >= 0:
			next[idx] = r.Entry
			results[i] = outcomeUpdated
		default:
			next = append(next, r.Entry)
			if indexOfEntry(current, t, r.Entry, domain) >= 0 {
				results[i] = outcomeUpdated
			} else {
				results[i] = outcomeCreated
			}
		}
	}

	// Cross-entry rules — single-valued types, and duplicate values PowerDNS
	// rejects the whole RRset over — can only be checked once the final set is
	// known. Validate-in-a-loop above passes a two-value CNAME set and the user
	// silently loses a value, so this call is not optional.
	if err := rdata.ValidateEntriesInZone(t, next, domain); err != nil {
		return nil, append(outcomes, failAll(accepted, err.Error())...)
	}

	// The backend applies the FIRST entry's TTL to a whole owner name and drops
	// the rest without a word. Merging a file into existing records is exactly
	// how an owner ends up with two TTLs, so the advisory belongs here.
	for _, w := range rdata.WarningsInZone(t, domain, next...) {
		_, _ = fmt.Fprintf(warnTo, "Warning: %s %s\n", t, w)
	}

	return &typePlan{
		rrType: t, existing: existing, next: next, accepted: accepted, results: results,
	}, outcomes
}

func reportImport(w io.Writer, outcomes []outcome, dryRun bool) error {
	sortOutcomes(outcomes)

	tw := util.NewTabWriter(w)
	_, _ = fmt.Fprintln(tw, "NAME\tTYPE\tTTL\tVALUE\tRESULT")
	for _, o := range outcomes {
		result := o.result
		if o.detail != "" {
			result += " — " + o.detail
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			o.rec.Name, o.rec.Type, rdata.FormatTTL(o.rec.TTL),
			truncate(rdata.Render(o.rec.Type, o.rec.Entry), 48), result)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	counts := map[string]int{}
	for _, o := range outcomes {
		counts[o.result]++
	}

	// A kept record was never in the input, so counting it in the input total
	// would report more records than the file holds. The two accountings are
	// separate sentences for that reason.
	var parts []string
	for _, k := range []string{outcomeCreated, outcomeUpdated, outcomeSkipped, outcomeFailed} {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
		}
	}
	fromInput := len(outcomes) - counts[outcomeKept]
	_, _ = fmt.Fprintf(w, "\n%s — %s\n",
		pluralize(fromInput, "record", "records"), strings.Join(parts, ", "))
	if n := counts[outcomeKept]; n > 0 {
		_, _ = fmt.Fprintf(w, "%s already in the zone kept — the platform manages them.\n",
			pluralize(n, "record", "records"))
	}
	if dryRun {
		_, _ = fmt.Fprintln(w, "\nDry run — nothing was written.")
	}

	if counts[outcomeFailed] > 0 {
		return util.NewCLIError(util.ExitError,
			fmt.Sprintf("%d of %d records could not be imported", counts[outcomeFailed], len(outcomes))).
			WithFix("fix the records marked \"failed\" above and re-run — the ones that succeeded are already written.")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers shared by import and export.
// ---------------------------------------------------------------------------

// bulkSetsByType indexes a zone's record sets by type, which is the unit every
// write works in.
func bulkSetsByType(
	ctx context.Context, c client.Client, zone *dnsv1alpha1.DNSZone,
) (map[dnsv1alpha1.RRType]*dnsv1alpha1.DNSRecordSet, error) {
	items, err := zoneRecordSets(ctx, c, zone)
	if err != nil {
		return nil, util.ClassifyError(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	out := map[dnsv1alpha1.RRType]*dnsv1alpha1.DNSRecordSet{}
	for i := range items {
		t := items[i].Spec.RecordType
		// Nothing forbids two sets for one type; the first by object name wins,
		// so repeated runs hit the same object.
		if _, seen := out[t]; !seen {
			out[t] = &items[i]
		}
	}
	return out, nil
}

// bulkWriteSet is the single write path: create, update or delete the (zone,
// type) set so that its entries are exactly next.
//
// Update carries the resourceVersion the object was read at, so a concurrent
// writer to the same type loses with a 409 rather than being silently
// clobbered — the precondition the portal omits.
func bulkWriteSet(
	ctx context.Context, c client.Client, zone *dnsv1alpha1.DNSZone, t dnsv1alpha1.RRType,
	existing *dnsv1alpha1.DNSRecordSet, next []dnsv1alpha1.RecordEntry, dryRun bool,
) error {
	var opts []client.DeleteOption
	var writeOpts []client.CreateOption
	var updateOpts []client.UpdateOption
	if dryRun {
		opts = append(opts, client.DryRunAll)
		writeOpts = append(writeOpts, client.DryRunAll)
		updateOpts = append(updateOpts, client.DryRunAll)
	}

	stored := make([]dnsv1alpha1.RecordEntry, 0, len(next))
	for _, e := range next {
		stored = append(stored, rdata.EntryForAPI(t, e))
	}

	switch {
	case existing == nil && len(stored) == 0:
		return nil

	case existing == nil:
		obj := &dnsv1alpha1.DNSRecordSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      recordSetName(zone.Name, t),
				Namespace: util.ResourceNamespace,
			},
			Spec: dnsv1alpha1.DNSRecordSetSpec{
				DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
				RecordType: t,
				Records:    stored,
			},
		}
		if err := c.Create(ctx, obj, writeOpts...); err != nil {
			return classifyWrite(t, zone.Spec.DomainName, err)
		}
		return nil

	case len(stored) == 0:
		// spec.records has MinItems=1, so an empty set is not a legal object;
		// removing the last entry means removing the whole thing.
		rv := existing.ResourceVersion
		opts = append(opts, client.Preconditions{ResourceVersion: &rv})
		if err := c.Delete(ctx, existing, opts...); err != nil {
			return classifyWrite(t, zone.Spec.DomainName, err)
		}
		return nil

	default:
		obj := existing.DeepCopy()
		obj.Spec.Records = stored
		if err := c.Update(ctx, obj, updateOpts...); err != nil {
			return classifyWrite(t, zone.Spec.DomainName, err)
		}
		return nil
	}
}

// classifyWrite turns an API failure into the plugin's error vocabulary, naming
// the record type so a partial bulk failure says which one lost.
func classifyWrite(t dnsv1alpha1.RRType, domain string, err error) error {
	cli := util.ClassifyError(err)
	if cli.Code() == util.ExitConflict {
		return util.NewCLIError(util.ExitConflict,
			fmt.Sprintf("the %s records for %s changed while this command was running", t, domain)).
			WithFix("re-run the command — someone else modified the same record type.").
			WithCause(err)
	}
	return cli
}

// recordSetName is the object name for a (zone, type) set. It matches the
// operator's own scheme for the SOA and NS sets it creates, so the CLI never
// creates a second object competing for the same records.
func recordSetName(zoneName string, t dnsv1alpha1.RRType) string {
	return fmt.Sprintf("%s-%s", zoneName, strings.ToLower(string(t)))
}

// keptReason explains a preserved record in the register of a record that
// already exists, rather than platformOwned's, which is written for a record
// arriving in the file and therefore says "importing them would…".
func keptReason(t dnsv1alpha1.RRType) string {
	switch t {
	case dnsv1alpha1.RRTypeSOA:
		return "the zone's SOA record, managed by the platform"
	case dnsv1alpha1.RRTypeNS:
		return "the zone's delegation, managed by the platform"
	}
	return "managed by the platform"
}

// gatewayOwned reports whether a record set belongs to the Gateway DNS
// controller, which is a fact read off labels rather than a guess.
//
// The three-label rule lives in util.MachineOwned, shared with the record verbs
// so the tier has one definition. This function used to test source-kind alone,
// which was the weaker of the two copies and the one guarding the bulk path: a
// set carrying managed and managed-by but not source-kind was recognised by
// `record apply` and not here, so the import would write, the controller would
// revert it, and the user would get a success report for a change that silently
// disappeared — exactly what the failAll below exists to prevent.
func gatewayOwned(set *dnsv1alpha1.DNSRecordSet) (bool, string) {
	if set == nil {
		return false, ""
	}
	owned, source := util.MachineOwned(set.Labels)
	if !owned {
		return false, ""
	}
	if source != "" {
		return true, "managed by AI Edge (Gateway " + source + ")"
	}
	return true, "managed by AI Edge"
}

// platformOwned reports whether a record is one the platform creates and relies
// on: the zone's SOA, and its apex NS records.
//
// The test is on the record's shape rather than on the object it would land in,
// deliberately. The operator's guard is existence-based — it creates
// <zone>-soa and <zone>-ns only when none exists — so a zone whose nameservers
// have not been assigned yet has no set to classify, and an import would create
// one under exactly the name the operator later looks for. The imported SOA
// would then become the zone's SOA permanently. Classifying by set would miss
// that, which is the same reason `record set` checks the shape too.
//
// A non-apex NS is a subdomain delegation the user owns, and is imported
// normally even though it shares an object with the platform's apex records.
//
// The window this guards is not a controller-latency race, which is what makes
// it worth guarding. ensureSOARecordSet and ensureNSRecordSet both return early
// while status.nameservers is empty, so how long a zone spends without them is
// bounded by NAMESERVER ASSIGNMENT — a different subsystem — and not by how
// quickly the operator reconciles. A zone stuck in Pending has an unbounded
// window, and that is precisely the state a user is most likely to be
// impatiently running an import against: the window is not merely reachable,
// it is correlated with the user being in a hurry.
//
// Measuring the operator's latency alone would therefore report a few hundred
// milliseconds on a healthy day and suggest this guard is dead weight. The
// measurement that answers the question is two-part: time to status.nameservers
// being populated, then time from there to the record sets appearing.
//
// The apex test goes through rdata.FQDN rather than rdata.IsApex, because the
// backend keys an RRset on the qualified name: "@", "" and "example.com." are
// one owner to pdns.QualifyOwner, and the CRD's name pattern admits all three.
// Testing the literal string would let a platform record stored under any
// spelling but "@" walk straight past this guard — and the input most likely to
// carry another spelling is `--discover`, whose records come from the old
// provider's zone data. The design doc's note that discovery is "already
// relativized with @ for apex" describes what the operator does; it is not
// something the CLI can assume.
func platformOwned(t dnsv1alpha1.RRType, name, zone string) (bool, string) {
	switch t {
	case dnsv1alpha1.RRTypeSOA:
		return true, "the zone's SOA record is managed by the platform"
	case dnsv1alpha1.RRTypeNS:
		if rdata.FQDN(name, zone) == rdata.FQDN("@", zone) {
			return true, "the zone's apex NS records are managed by the platform — " +
				"importing them would break delegation"
		}
	}
	return false, ""
}

// ---------------------------------------------------------------------------
// Small shared utilities.
// ---------------------------------------------------------------------------

// indexOfEntry finds an entry with the same owner name and the same value,
// ignoring TTL — that is what makes a re-import a no-op rather than a duplicate.
func indexOfEntry(
	entries []dnsv1alpha1.RecordEntry, t dnsv1alpha1.RRType, want dnsv1alpha1.RecordEntry, zone string,
) int {
	for i, e := range entries {
		if ownerEqual(e.Name, want.Name, zone) && rdata.Equal(t, e, want) {
			return i
		}
	}
	return -1
}

// ownerEqual compares two owner names the way the backend will.
//
// pdns.QualifyOwner keys an RRset on the qualified name, so "www" and
// "www.example.com." are one owner and a literal comparison sees two. Getting
// this wrong does not merely miss a match: the file's record is appended beside
// the stored one, and ValidateEntriesInZone — which does group by FQDN —
// then reports a duplicate value, or a single-valued violation, for a file that
// contains exactly one such record. The user is told the wrong thing about
// input that was correct.
func ownerEqual(a, b, zone string) bool {
	return rdata.FQDN(a, zone) == rdata.FQDN(b, zone)
}

func failAll(records []bind.Record, detail string) []outcome {
	out := make([]outcome, 0, len(records))
	for _, r := range records {
		out = append(out, outcome{rec: r, result: outcomeFailed, detail: at(r, detail)})
	}
	return out
}

// at prefixes a failure with the line it came from. Discovered records have no
// line, so the prefix is omitted rather than reading "line 0".
func at(r bind.Record, detail string) string {
	if r.Line == 0 {
		return detail
	}
	return fmt.Sprintf("line %d: %s", r.Line, detail)
}

// orderTypes returns the types present in records, in the API's declared order,
// so a summary reads the same way twice.
func orderTypes(records []bind.Record) []dnsv1alpha1.RRType {
	present := map[dnsv1alpha1.RRType]bool{}
	for _, r := range records {
		present[r.Type] = true
	}
	var out []dnsv1alpha1.RRType
	for _, t := range rdata.SupportedTypes() {
		if present[t] {
			out = append(out, t)
		}
	}
	return out
}

func recordsOfType(records []bind.Record, t dnsv1alpha1.RRType) []bind.Record {
	var out []bind.Record
	for _, r := range records {
		if r.Type == t {
			out = append(out, r)
		}
	}
	return out
}

func sortOutcomes(outcomes []outcome) {
	sort.SliceStable(outcomes, func(i, j int) bool {
		a, b := outcomes[i].rec, outcomes[j].rec
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		if a.Name != b.Name {
			return nameBefore(a.Name, b.Name)
		}
		return rdata.Render(a.Type, a.Entry) < rdata.Render(b.Type, b.Entry)
	})
}

// nameBefore sorts the apex ahead of named records, as every zone view does.
//
// The apex test here is deliberately the literal one. This orders the summary
// table and nothing else — no behaviour turns on it — and by the time a name
// reaches an outcome it has been canonicalised, by the parser on the file path
// and by NormalizeNameWithWarnings on the discovery path. Threading the zone
// through a comparator to correct the ordering of a spelling that cannot arrive
// would buy nothing. If a third input path is ever added, this is the line that
// quietly stops being right, so it says so.
func nameBefore(a, b string) bool {
	switch {
	case rdata.IsApex(a) && rdata.IsApex(b):
		return false
	case rdata.IsApex(a):
		return true
	case rdata.IsApex(b):
		return false
	}
	return a < b
}

func typeNames(types []dnsv1alpha1.RRType) string {
	names := make([]string, 0, len(types))
	for _, t := range types {
		names = append(names, string(t))
	}
	return strings.Join(names, ", ")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

func displayPath(path string) string {
	if path == "-" {
		return "stdin"
	}
	return path
}
