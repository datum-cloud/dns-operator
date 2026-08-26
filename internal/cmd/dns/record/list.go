// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

type listOptions struct {
	rrTypes   []string
	name      string
	status    string
	managed   bool
	noManaged bool
	// wantManaged is the resolved answer to "whose records?", or nil for no
	// filter at all. It is what separates --managed=false, which asks for your
	// own records, from an absent flag, which asks for everything.
	wantManaged *bool
	noHeaders   bool
}

// resolveManaged folds the two spellings of the same question into one answer.
//
// --no-managed exists because that is how people reach for it, and because a
// negated flag reads better in a script than a value on a positive one.
// --managed=false keeps working and means the same thing: it already parsed
// before this filter existed, where it silently meant "no filter", so leaving
// it inert would be the more surprising choice.
func (o *listOptions) resolveManaged(cmd *cobra.Command) error {
	managedSet := cmd.Flags().Changed("managed")
	noManagedSet := cmd.Flags().Changed("no-managed")

	if managedSet && noManagedSet {
		return util.UsageErrorf("--managed and --no-managed ask opposite questions; pass only one").
			WithFix("use --managed for the records Datum manages, or --no-managed for your own.")
	}

	switch {
	case managedSet:
		o.wantManaged = &o.managed
	case noManagedSet:
		// --no-managed asks for the records Datum does NOT manage, so the
		// wanted value is the negation.
		want := !o.noManaged
		o.wantManaged = &want
	}
	return nil
}

// managedFlagName reports which spelling the user actually typed, so a warning
// quotes their own command line back at them.
func (o *listOptions) managedFlagName() string {
	if o.noManaged {
		return "--no-managed"
	}
	return "--managed"
}

func listCommand() *cobra.Command {
	opts := &listOptions{}

	cmd := &cobra.Command{
		Use:     "list <domain>",
		Aliases: []string{"ls"},
		Short:   "List the records in a zone",
		Long: `List every record in a zone, one row per value.

The rows are flattened from the zone's DNSRecordSet objects, which store one
bucket per record type. STATUS is the per-owner-name condition, not the
rolled-up one on the set: the interesting outcomes — Conflict, Not owner,
Error — only exist per name.

-o json and -o yaml emit the raw DNSRecordSet objects. The flat view is a
presentation, and scripts that need the objects should not have to reconstruct
them.`,
		Example: `  # Everything in the zone
  datumctl dns record list example.com

  # Only the mail records
  datumctl dns record list example.com --type MX,TXT

  # Only what is not working
  datumctl dns record list example.com --status conflict

  # Only what the platform manages
  datumctl dns record list example.com --managed`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: util.CompleteZoneNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runList(cmd, args[0], opts)
		},
	}

	cmd.Flags().StringSliceVar(&opts.rrTypes, "type", nil, "Filter to these record types (comma separated, e.g. A,MX)")
	cmd.Flags().StringVar(&opts.name, "name", "", "Filter to one owner name, relative to the zone (e.g. www, @)")
	cmd.Flags().StringVar(&opts.status, "status", "", statusFilterUsage())
	cmd.Flags().BoolVar(&opts.managed, "managed", false, "Show only the records Datum manages for you")
	cmd.Flags().BoolVar(&opts.noManaged, "no-managed", false,
		"Show only your own records, excluding the ones Datum manages (same as --managed=false)")
	cmd.Flags().BoolVar(&opts.noHeaders, "no-headers", false, "Omit the table header row (table and wide only)")

	_ = cmd.RegisterFlagCompletionFunc("type", completeRRTypes)
	_ = cmd.RegisterFlagCompletionFunc("status", util.CompleteEnum(statusFilterTokens()...))

	return cmd
}

func runList(cmd *cobra.Command, domain string, opts *listOptions) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	format, err := util.ParseOutputFormat(outputFlag(cmd))
	if err != nil {
		return err
	}

	if err := opts.resolveManaged(cmd); err != nil {
		return err
	}

	rrTypes, err := parseTypeFilter(opts.rrTypes)
	if err != nil {
		return err
	}

	// --name's shape is checked before the API call, so a malformed filter is
	// exit 2 whether or not the zone exists.
	if opts.name != "" {
		if nerr := precheckName(opts.name, normalizeDomain(domain)); nerr != nil {
			return nerr
		}
	}

	c, err := clientFactory(util.ProjectFromCmd(cmd))
	if err != nil {
		return err
	}
	zone, err := resolveZone(ctx, c, domain)
	if err != nil {
		return err
	}

	// --type is a server-side selector, so it is applied before the objects
	// leave the API server and therefore before the raw-object output too.
	sets, err := listSets(ctx, c, zone, rrTypes)
	if err != nil {
		return err
	}

	// json and yaml are the raw object contract and are dispatched before
	// flattening: the row view is a presentation, not the data.
	switch format {
	case util.OutputJSON:
		warnUnfilterable(cmd.ErrOrStderr(), opts)
		return util.PrintJSON(out, recordSetList(sets))
	case util.OutputYAML:
		warnUnfilterable(cmd.ErrOrStderr(), opts)
		return util.PrintYAML(out, recordSetList(sets))
	}

	rows := flatten(sets, zone)
	// Kept so the empty state can tell "this zone holds nothing of the type you
	// asked for" from "the row filters excluded everything", which want
	// opposite advice.
	beforeRowFilters := len(rows)
	rows, err = filterRows(rows, opts, zone.Spec.DomainName)
	if err != nil {
		return err
	}

	// This check runs AFTER the fetch, which looks like a violation of the rule
	// every other argument in this package follows — validate before the first
	// API call, so an argument error is never masked by a missing zone. It is an
	// exception, not an oversight, and the distinction is worth stating: the
	// ordering rule applies to inputs whose validity is a function of the
	// ARGUMENTS, and does not apply to inputs whose validity is a function of
	// SERVER DATA. A record type is knowable client-side; a status word is not,
	// because RecordStatus passes an unrecognised server reason through raw, so
	// "Throttled" is a legitimate status the moment the operator grows the
	// reason. The cost is that `--status bogus` against a missing zone reports
	// the missing zone — which is genuinely the first real problem.
	//
	// A KNOWN token matching nothing is deliberately not an error. "Nothing is
	// in Conflict" is a real answer to a real question, and turning it into a
	// non-zero exit would break every monitor that asks it.
	if opts.status != "" && len(rows) == 0 && !knownStatusToken(opts.status) {
		return unknownStatusError(opts.status)
	}

	if format == util.OutputName {
		printNames(out, rows)
		return nil
	}

	if len(rows) == 0 {
		printEmpty(out, zone.Spec.DomainName, opts, rrTypes, beforeRowFilters > 0)
		return nil
	}

	truncated := printTable(out, rows, format == util.OutputWide, opts.noHeaders)
	if !boolFlag(cmd, "quiet") {
		_, _ = fmt.Fprintf(out, "\n%s — %s\n", countOf(len(rows), pluralize(len(rows), "record")), strings.Join(tally(rows), ", "))
		// A shortened value is only honest if the reader is told where the rest
		// of it went.
		if truncated > 0 {
			_, _ = fmt.Fprintf(out, "%s shortened to fit — see them in full with -o wide or -o json\n",
				countOf(truncated, pluralize(truncated, "value")))
		}
	}
	return nil
}

// recordSetList wraps the fetched objects in a List so `-o json` emits one
// document rather than a bare array, matching what the API server would return.
//
// The items are stamped with their own apiVersion and kind as well. A typed
// client leaves those blank, and without them the output cannot be piped back
// through `kubectl apply -f` — which is most of the reason to ask for it.
func recordSetList(sets []dnsv1alpha1.DNSRecordSet) *dnsv1alpha1.DNSRecordSetList {
	items := make([]dnsv1alpha1.DNSRecordSet, len(sets))
	copy(items, sets)
	for i := range items {
		items[i].SetGroupVersionKind(dnsv1alpha1.GroupVersion.WithKind("DNSRecordSet"))
	}

	list := &dnsv1alpha1.DNSRecordSetList{Items: items}
	list.SetGroupVersionKind(dnsv1alpha1.GroupVersion.WithKind("DNSRecordSetList"))
	return list
}

// parseTypeFilter resolves --type into RR types, rejecting an unknown one
// rather than quietly returning nothing for it.
func parseTypeFilter(values []string) ([]dnsv1alpha1.RRType, error) {
	var out []dnsv1alpha1.RRType
	seen := map[dnsv1alpha1.RRType]bool{}
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		t, err := rdata.ParseRRType(v)
		if err != nil {
			return nil, usageFromRdata(err)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// statusFilterTokens is the canonical --status token for each status a record
// can actually have, generated from statusOrder so the flag's help, its
// completion and its error message cannot say different things. The bare first
// word of a two-word status is also accepted; that is stated once, in the help,
// rather than doubling the length of every list.
func statusFilterTokens() []string {
	out := make([]string, 0, len(statusOrder))
	for _, s := range statusOrder {
		out = append(out, strings.ToLower(strings.ReplaceAll(s, " ", "-")))
	}
	return out
}

// statusFilterUsage is the flag's help string. Hand-writing it is how it came to
// advertise six tokens where eight were accepted.
//
// No backquotes: pflag reads the first backquoted word in a usage string as the
// flag's value placeholder, so "e.g. `not`" rendered the flag as `--status not`
// instead of `--status string`.
func statusFilterUsage() string {
	return "Filter to one status: " + strings.Join(statusFilterTokens(), "|") +
		` (the first word alone also works, so "not" selects "Not owner")`
}

// knownStatusToken reports whether a token names a status the CLI itself
// defines.
func knownStatusToken(status string) bool {
	for _, s := range statusOrder {
		if statusMatches(s, status) {
			return true
		}
	}
	return false
}

// unknownStatusError is returned when a --status token matched neither a status
// the CLI defines nor any row that came back.
//
// Validating a typo matters because without it `--status programed` exits 0 with
// "No records match", indistinguishable from a genuinely empty result, so a
// monitor asking "is anything in Conflict" gets a clean bill of health from a
// misspelling.
//
// But the check cannot be a closed list. RecordStatus passes a reason the CLI
// does not recognise through RAW as the status word — deliberately, so a reason
// the operator grows later is never mistaken for something else — which means a
// row can legitimately read "Throttled" and `--status throttled` must select it.
// So the token is only rejected once the rows are in hand and none of them
// matched either. A known token matching nothing is still a clean exit 0: that
// is a real answer to a real question.
func unknownStatusError(status string) error {
	return util.UsageErrorf("unknown status %q", status).
		WithFix("--status takes one of: " + strings.Join(statusFilterTokens(), ", ") +
			"\n       or any reason the server reports, as shown in the STATUS column.")
}

func filterRows(rows []row, opts *listOptions, zoneDomain string) ([]row, error) {
	var wantName string
	if opts.name != "" {
		n, err := rdata.NormalizeName(opts.name, zoneDomain)
		if err != nil {
			return nil, usageFromRdata(err)
		}
		wantName = n
	}

	out := rows[:0:0]
	for _, r := range rows {
		if wantName != "" && !sameOwner(r.entry.Name, wantName, zoneDomain) {
			continue
		}
		if opts.status != "" && !statusMatches(r.status, opts.status) {
			continue
		}
		if opts.wantManaged != nil && *opts.wantManaged != r.prov.managed() {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// statusMatches compares against the first word of the status, which is the
// filter token the whole plugin uses.
//
// The whole status word is accepted too, punctuation and spacing folded, because
// the first word of "Not owner" is the useless token `not`. `--status not`,
// `--status not-owner` and `--status notowner` all mean the same thing; the
// alias lives here rather than in util, which is frozen and shared.
func statusMatches(status, token string) bool {
	want := foldToken(token)
	return want == foldToken(firstWord(status)) || want == foldToken(status)
}

// foldToken reduces a status word to its comparison form: lowercase, with the
// spaces, hyphens and underscores that separate a two-word status removed.
func foldToken(s string) string {
	return strings.ToLower(tokenFolder.Replace(strings.TrimSpace(s)))
}

var tokenFolder = strings.NewReplacer(" ", "", "-", "", "_", "")

func firstWord(s string) string {
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i]
	}
	return s
}

// maxValueWidth is how much of a record's value the default table shows.
//
// The number is measured, not guessed. Across real zones the ordinary values —
// addresses, hostnames, SPF records, ACME challenge tokens — bunch up below the
// mid-fifties, and there is a clean gap between them and the values nobody
// reads in a table anyway. Dropping to 48 starts cutting a third of a typical
// zone's rows; 56 cuts one in a hundred while still shortening the long TXT
// values this exists for.
const maxValueWidth = 56

// printTable renders the rows and reports how many values it had to shorten, so
// the caller can say so rather than leaving a silent ellipsis.
func printTable(out io.Writer, rows []row, wide, noHeaders bool) int {
	tw := util.NewTabWriter(out)
	if !noHeaders {
		if wide {
			_, _ = fmt.Fprintf(tw, "NAME\tTYPE\tTTL\tVALUE\tSTATUS\tRECORD SET\tAGE\n")
		} else {
			_, _ = fmt.Fprintf(tw, "NAME\tTYPE\tTTL\tVALUE\tSTATUS\n")
		}
	}
	truncated := 0
	for _, r := range rows {
		status := r.status
		if m := r.prov.marker(); m != "" {
			status += "  " + m
		}
		if wide {
			// -o wide is the escape hatch, so it never shortens anything.
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				r.name, r.rrType, rdata.FormatTTL(r.ttl), util.OrDash(r.value), status,
				r.set.Name, util.RelativeAge(r.set.CreationTimestamp))
			continue
		}
		value, cut := util.TruncateCell(util.OrDash(r.value), maxValueWidth)
		if cut {
			truncated++
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			r.name, r.rrType, rdata.FormatTTL(r.ttl), value, status)
	}
	_ = tw.Flush()
	return truncated
}

// printNames emits the (name, type) pairs that address a record in every other
// verb of this command, deduplicated: `record delete` and `record describe`
// take a name and a type, not a value, so that is what -o name is good for.
func printNames(out io.Writer, rows []row) {
	seen := map[string]bool{}
	for _, r := range rows {
		id := fmt.Sprintf("%s/%s", r.name, r.rrType)
		if seen[id] {
			continue
		}
		seen[id] = true
		_, _ = fmt.Fprintln(out, id)
	}
}

// printEmpty is never an error. An empty zone is the normal first state, and a
// filter that matches nothing is a question that got an answer.
// printEmpty explains an empty result. excludedByFilters reports that the zone
// did hold matching records until the row filters removed them, which is a
// different situation from a zone with nothing to show and wants the opposite
// advice: offering to populate a zone that already holds a thousand records
// reads as though they have been lost.
func printEmpty(out io.Writer, domain string, opts *listOptions, rrTypes []dnsv1alpha1.RRType, excludedByFilters bool) {
	// The headline reports what was asked; the advice below reports what to do
	// about it, and the two turn on different questions.
	if listIsFiltered(opts) {
		_, _ = fmt.Fprintf(out, "No records in zone %s match the given filters.\n", domain)
	} else {
		_, _ = fmt.Fprintf(out, "No records found in zone %s.\n", domain)
	}

	if excludedByFilters {
		_, _ = fmt.Fprintf(out, "\nNext steps:\n")
		_, _ = fmt.Fprintf(out, "  Show every record:  datumctl dns record list %s\n", domain)
		_, _ = fmt.Fprintf(out, "  Widen the filter:   datumctl dns record list %s --help\n", domain)
		return
	}

	example := "www A 203.0.113.10"
	if len(rrTypes) == 1 {
		if ex := exampleValue(rrTypes[0]); ex != "" {
			example = ex
		}
	}
	_, _ = fmt.Fprintf(out, "\nGet started:\n")
	_, _ = fmt.Fprintf(out, "  Add a record:   datumctl dns record create %s %s\n", domain, example)
	_, _ = fmt.Fprintf(out, "  Import a zone:  datumctl dns zone import %s --file zone.txt\n", domain)
}

// warnUnfilterable says so when a row filter was given alongside raw-object
// output.
//
// --type is a server-side selector and narrows the objects themselves, but
// --name, --status and --managed select rows out of a flattened view that
// -o json never builds. Applying them would mean returning a DNSRecordSet with
// some of its records removed, which is not an object the API ever served and
// not one that could be applied back. Ignoring them silently is worse than
// saying so, so this says so — on stderr, where it cannot corrupt the data
// contract on stdout.
func warnUnfilterable(errOut io.Writer, opts *listOptions) {
	var ignored []string
	if opts.name != "" {
		ignored = append(ignored, "--name")
	}
	if opts.status != "" {
		ignored = append(ignored, "--status")
	}
	if opts.wantManaged != nil {
		ignored = append(ignored, opts.managedFlagName())
	}
	if len(ignored) == 0 {
		return
	}
	printWarnings(errOut, []string{fmt.Sprintf(
		"%s %s and %s not apply to -o json or -o yaml, which emit whole DNSRecordSet objects — only --type narrows those",
		strings.Join(ignored, ", "), rowFilterPhrase(len(ignored)), isAre(len(ignored)))})
}

// rowFilterPhrase and isAre keep the subject, its article and the following
// verb agreeing: one flag "is a row filter and does not apply", several "are row
// filters and do not apply".
func rowFilterPhrase(n int) string {
	if n == 1 {
		return "is a row filter"
	}
	return "are row filters"
}

func isAre(n int) string {
	if n == 1 {
		return "does"
	}
	return "do"
}

func listIsFiltered(opts *listOptions) bool {
	return len(opts.rrTypes) > 0 || opts.name != "" || opts.status != "" || opts.wantManaged != nil
}

// exampleValue is the "Get started" line for a type, so a filtered-empty
// listing suggests the record the user was looking for rather than an A record.
func exampleValue(t dnsv1alpha1.RRType) string {
	switch t {
	case dnsv1alpha1.RRTypeA:
		return "www A 203.0.113.10"
	case dnsv1alpha1.RRTypeAAAA:
		return "www AAAA 2001:db8::1"
	case dnsv1alpha1.RRTypeCNAME:
		return "cdn CNAME lb.example.net."
	case dnsv1alpha1.RRTypeALIAS:
		return "@ ALIAS lb.example.net."
	case dnsv1alpha1.RRTypeTXT:
		return `@ TXT "v=spf1 -all"`
	case dnsv1alpha1.RRTypeMX:
		return "@ MX --preference 10 --exchange mail.example.com."
	case dnsv1alpha1.RRTypeSRV:
		return "_sip._tcp SRV --priority 10 --weight 5 --port 5060 --target sip.example.com."
	case dnsv1alpha1.RRTypeCAA:
		return "@ CAA --flag 0 --tag issue --value letsencrypt.org"
	case dnsv1alpha1.RRTypeNS:
		return "sub NS ns1.example.net."
	case dnsv1alpha1.RRTypePTR:
		return "1 PTR host.example.com."
	case dnsv1alpha1.RRTypeTLSA:
		return "_443._tcp TLSA --usage 3 --selector 1 --matching-type 1 --cert-data <hex>"
	case dnsv1alpha1.RRTypeHTTPS, dnsv1alpha1.RRTypeSVCB:
		return fmt.Sprintf("api %s --priority 1 --target . --param alpn=h3,h2", t)
	default:
		return ""
	}
}
