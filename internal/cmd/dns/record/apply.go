// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/bind"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// Diff vocabulary, shared with the compute plugin: two-space indent, "+" for an
// addition, "-" for a removal, "→" for a value that stays but changes.
const (
	markAdd    = "+"
	markRemove = "-"
	markChange = "→"
)

func applyCommand() *cobra.Command {
	var (
		file   string
		prune  bool
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:   "apply <domain> -f <zonefile>",
		Short: "Make a zone match a BIND zone file",
		Long: "Apply is the declarative path: it diffs a zone file against the live zone, prints " +
			"what would change, and converges.\n\n" +
			"By default it only adds and updates. Pass --prune to also delete the records the file " +
			"does not mention. Platform-managed records — the zone's SOA, its apex NS records, and " +
			"anything owned by AI Edge — are never pruned or modified, and what was skipped is " +
			"always reported. There is no --force: a zone file is not the place to say \"yes, " +
			"delete my delegation\". Use `dns record delete --force` where the record is named " +
			"explicitly.\n\n" +
			"With --prune, the diff is computed from the zone as it was read. If another writer " +
			"changes the zone while the command runs, the retry converges against the newer " +
			"state and may delete a record the diff did not show — the dry run is a good " +
			"description of the change, not an upper bound on it.",
		Example: "  # Show and apply the difference\n" +
			"  datumctl dns record apply example.com -f example.com.zone\n\n" +
			"  # Make the zone exactly match the file\n" +
			"  datumctl dns record apply example.com -f example.com.zone --prune\n\n" +
			"  # See the diff without touching anything\n" +
			"  datumctl dns record apply example.com -f example.com.zone --dry-run",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: zoneNameArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" {
				return util.UsageErrorf("-f/--file is required").
					WithFix("pass the zone file to apply:\n" +
						"       datumctl dns record apply example.com -f example.com.zone")
			}
			return runApply(cmd, args[0], file, prune, dryRun)
		},
	}

	cmd.Flags().StringVarP(&file, "file", "f", "", "BIND zone file to apply, or \"-\" for standard input")
	cmd.Flags().BoolVar(&prune, "prune", false, "Delete live records the file does not mention")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"Print the diff and validate it against the API server without writing anything")
	return cmd
}

// change is one line of the diff.
type change struct {
	mark   string
	name   string
	rrType dnsv1alpha1.RRType
	// entry is the value the line is about: the desired one for an addition or
	// a change, the live one for a removal.
	entry dnsv1alpha1.RecordEntry
	// oldTTL and newTTL are set only on a change line.
	oldTTL, newTTL *int64
	// reason, when set, names why the change will not be applied.
	reason string
}

func (c change) ttlColumn() string {
	if c.mark == markChange {
		return rdata.FormatTTL(c.oldTTL) + " " + markChange + " " + rdata.FormatTTL(c.newTTL)
	}
	return rdata.FormatTTL(c.entry.TTL)
}

func runApply(cmd *cobra.Command, domain, file string, prune, dryRun bool) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	// The file is read, parsed and validated before the first API call, so a
	// syntax error on line 40 is a line-numbered usage error rather than a
	// zone-not-found, and so nothing is ever half-written. The zone is only a
	// guess at this point: the positional also accepts a DNSZone object name,
	// and zone-relative rules cannot run against "example-com". zoneGuessFrom
	// gates on a dot, exactly as the other record verbs do.
	source, err := readSource(cmd, file)
	if err != nil {
		return err
	}
	if guess := zoneGuessFrom(domain); guess != "" {
		if _, err := parseAndValidate(source, file, guess, io.Discard); err != nil {
			return err
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

	// Re-run against the zone the server actually returned. The pre-flight above
	// used a guess; this is the authoritative pass, and it is the one whose
	// warnings the user sees.
	desired, err := parseAndValidate(source, file, zone.Spec.DomainName, errOut)
	if err != nil {
		return err
	}

	sets, err := listSets(ctx, c, zone, nil)
	if err != nil {
		return err
	}

	plan := newPlan(zone, desired, sets, prune)
	plan.reportSkipped(errOut)

	if len(plan.changes) == 0 {
		_, _ = fmt.Fprintln(out, "No changes.")
		return nil
	}
	printApplyDiff(out, plan.changes)

	if !dryRun && !util.AssumeYes(cmd) {
		if err := confirmApply(cmd, zone, plan, errOut); err != nil {
			return err
		}
	}
	return plan.converge(ctx, c, dryRun, out, errOut)
}

// confirmApply gates the write. A prune that would delete records is the
// destructive tier: nothing recovers a deleted RRset, so it refuses to run
// unattended rather than proceeding the way an ordinary confirmation does.
func confirmApply(cmd *cobra.Command, zone *dnsv1alpha1.DNSZone, plan *plan, errOut io.Writer) error {
	deletes := countMark(plan.changes, markRemove)
	if deletes > 0 && util.NonInteractive(cmd.InOrStdin()) {
		return util.NewCLIError(util.ExitAborted,
			fmt.Sprintf("refusing to delete %d %s without confirmation",
				deletes, pluralize(deletes, "record"))).
			WithFix("re-run with --yes to confirm, or without --prune to add and update only.")
	}
	ok, err := util.ConfirmYesNo(cmd.InOrStdin(), errOut,
		fmt.Sprintf("Apply %d %s to %s?",
			len(plan.changes), pluralize(len(plan.changes), "change"), zone.Spec.DomainName),
		false)
	if err != nil {
		return err
	}
	if !ok {
		return util.NewCLIError(util.ExitAborted, "apply cancelled")
	}
	return nil
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
		defer f.Close() //nolint:errcheck // opened read-only
		r = f
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, util.NewCLIError(util.ExitUsage,
			fmt.Sprintf("reading zone file %q: %v", displayPath(path), err)).WithCause(err)
	}
	return data, nil
}

// parseAndValidate parses the file against a zone and refuses anything the
// backend would drop. Advisories go to errOut, which the pre-API pass discards
// so the user is not told the same thing twice.
func parseAndValidate(
	source []byte, path, domain string, errOut io.Writer,
) ([]bind.Record, error) {
	res, err := bind.Parse(bytes.NewReader(source), domain, nil)
	if err != nil {
		return nil, fileError(path, err, bind.FixFor(err))
	}
	for _, w := range res.Warnings {
		_, _ = fmt.Fprintf(errOut, "Warning: %s\n", w)
	}
	if len(res.Unsupported) > 0 {
		_, _ = fmt.Fprintf(errOut, "\n%d %s use types Datum DNS does not support and were ignored:\n",
			len(res.Unsupported), pluralize(len(res.Unsupported), "record"))
		for _, u := range res.Unsupported {
			_, _ = fmt.Fprintf(errOut, "  line %d  %s  %s\n", u.Line, u.Type, u.Raw)
		}
		_, _ = fmt.Fprintln(errOut)
	}

	// The API admits an entry whose typed field does not match its record type
	// and the backend then skips it without a condition, so the file is checked
	// before anything at all is written.
	for _, rec := range res.Records {
		if verr := rdata.ValidateInZone(rec.Type, rec.Entry, domain); verr != nil {
			return nil, fileError(fmt.Sprintf("%s line %d", displayPath(path), rec.Line),
				verr, rdata.FixFor(verr))
		}
	}

	// The cross-entry rules only exist over a whole type: a two-value CNAME set
	// and duplicate values within one owner both pass the per-entry pass above,
	// and the backend loses a value to the first and 422s the entire RRset over
	// the second.
	for _, t := range rdata.SupportedTypes() {
		var entries []dnsv1alpha1.RecordEntry
		for _, rec := range res.Records {
			if rec.Type == t {
				entries = append(entries, rec.Entry)
			}
		}
		if len(entries) == 0 {
			continue
		}
		if verr := rdata.ValidateEntriesInZone(t, entries, domain); verr != nil {
			return nil, fileError(fmt.Sprintf("%s (%s records)", displayPath(path), t),
				verr, rdata.FixFor(verr))
		}
	}
	return res.Records, nil
}

func fileError(where string, err error, fix string) error {
	e := util.NewCLIError(util.ExitUsage, fmt.Sprintf("%s: %v", where, err)).WithCause(err)
	if fix != "" {
		e = e.WithFix(fix)
	}
	return e
}

// ---------------------------------------------------------------------------
// The plan
// ---------------------------------------------------------------------------

// plan is the whole diff, worked out before anything is written. The unit of
// work is one API object and the unit the user reads is a record.
//
// A type is NOT one object. Nothing forbids several DNSRecordSets of the same
// type in a zone and a live zone routinely has them — the CLI writes to its
// own, a Gateway gets one per listener — so the desired records of one type are
// partitioned across the sets that already hold their names. Treating a type as
// a single object wrote every name into whichever set sorted first, which left
// the set that really held the name stale and gave the zone two entries for one
// key: the shape the backend reports back as Conflict and Not owner.
type plan struct {
	zone    *dnsv1alpha1.DNSZone
	prune   bool
	units   []*typePlan
	changes []change
	skipped []change
}

// typePlan is one type's work against ONE record set.
type typePlan struct {
	rrType dnsv1alpha1.RRType
	// set is the object this unit writes; nil means one must be created.
	set *dnsv1alpha1.DNSRecordSet
	// file holds the records the zone file declares for this type.
	file []dnsv1alpha1.RecordEntry
	// next is what spec.records becomes, with platform-managed entries kept.
	next []dnsv1alpha1.RecordEntry
	// changes are the lines that will be applied.
	changes []change
}

func newPlan(
	zone *dnsv1alpha1.DNSZone, desired []bind.Record,
	sets []dnsv1alpha1.DNSRecordSet, prune bool,
) *plan {
	p := &plan{zone: zone, prune: prune}

	setsByType := map[dnsv1alpha1.RRType][]*dnsv1alpha1.DNSRecordSet{}
	for i := range sets {
		t := sets[i].Spec.RecordType
		setsByType[t] = append(setsByType[t], &sets[i])
	}

	present := map[dnsv1alpha1.RRType]bool{}
	for t := range setsByType {
		present[t] = true
	}
	for _, r := range desired {
		present[r.Type] = true
	}

	for _, t := range rdata.SupportedTypes() {
		if !present[t] {
			continue
		}
		var file []dnsv1alpha1.RecordEntry
		for _, r := range desired {
			if r.Type == t {
				file = append(file, r.Entry)
			}
		}

		for _, tp := range p.partition(t, setsByType[t], file) {
			current := currentEntries(tp.set, t)
			tp.next = p.resolve(tp, current, true)
			tp.changes = diffEntries(t, current, tp.next, zone.Spec.DomainName)

			// The same resolution with protection switched off says what the
			// file asked for; the difference between the two is exactly what
			// was held back, which is the only honest way to report it.
			wanted := diffEntries(t, current, p.resolve(tp, current, false), zone.Spec.DomainName)
			p.skipped = append(p.skipped, withheld(wanted, tp.changes, p.reasonFor(tp))...)

			if len(tp.changes) > 0 {
				p.units = append(p.units, tp)
				p.changes = append(p.changes, tp.changes...)
			}
		}
	}

	sortChanges(p.changes)
	sortChanges(p.skipped)
	return p
}

// resolve computes what spec.records becomes for one type.
//
// partition splits one type's desired records across the sets that already
// hold their owner names, so a record is written where its siblings live rather
// than duplicated into whichever object happened to sort first.
//
// Names no set holds yet go to one unit together — the first set this command
// may write to, or a new object when a controller owns them all — so a zone
// gains one set per type rather than one per record.
func (p *plan) partition(
	t dnsv1alpha1.RRType, sets []*dnsv1alpha1.DNSRecordSet, file []dnsv1alpha1.RecordEntry,
) []*typePlan {
	units := make([]*typePlan, 0, len(sets)+1)
	index := map[*dnsv1alpha1.DNSRecordSet]*typePlan{}
	for _, rs := range sets {
		tp := &typePlan{rrType: t, set: rs}
		units = append(units, tp)
		index[rs] = tp
	}

	// Where a name nothing holds goes. A controller's set is never it: the
	// controller owns the names inside its set, not the type, and an entry
	// added there is reverted on the next reconcile.
	var fresh *typePlan
	for _, tp := range units {
		if !isMachineOwned(tp.set) {
			fresh = tp
			break
		}
	}
	if fresh == nil {
		fresh = &typePlan{rrType: t}
		units = append(units, fresh)
	}

	for _, e := range file {
		target := fresh
		if holder := holderOf(sets, e.Name, p.zone.Spec.DomainName); holder != nil {
			target = index[holder]
		}
		target.file = append(target.file, e)
	}
	return units
}

// holderOf returns the set already carrying an owner name, preferring one the
// user may write to when several do.
//
// The preference is the opposite of findSet's, and deliberately. findSet picks
// for a single-record write, where resolving to the controller's copy makes the
// guard refuse — the safe answer for a command that would otherwise write into
// a set the controller reverts. apply reconciles a whole file, so refusing to
// update the copy the user does own would block legitimate work; the
// controller's copy is left alone by resolve's own protection instead, and
// reported as skipped.
func holderOf(sets []*dnsv1alpha1.DNSRecordSet, ownerName, zoneDomain string) *dnsv1alpha1.DNSRecordSet {
	var managed *dnsv1alpha1.DNSRecordSet
	for _, rs := range sets {
		if !setHasOwner(rs, ownerName, zoneDomain) {
			continue
		}
		if !isMachineOwned(rs) {
			return rs
		}
		if managed == nil {
			managed = rs
		}
	}
	return managed
}

// With protection on, entries the platform owns survive untouched and the file
// cannot add to a set a Gateway controls — writing there would be reverted, so
// reporting success would be a lie. Without --prune the live entries stay and
// the file's are merged in; with it, the file is the whole truth for the type,
// minus what the platform owns.
func (p *plan) resolve(tp *typePlan, current []dnsv1alpha1.RecordEntry, protect bool) []dnsv1alpha1.RecordEntry {
	zone := p.zone.Spec.DomainName
	if protect && tp.set != nil && classify(tp.set, dnsv1alpha1.RecordEntry{}, p.zone.Spec.DomainName) == provGateway {
		return current
	}

	keep := make([]dnsv1alpha1.RecordEntry, 0, len(current))
	editable := make([]dnsv1alpha1.RecordEntry, 0, len(current))
	for _, e := range current {
		if protect && p.protectedEntry(tp.set, tp.rrType, e) {
			keep = append(keep, e)
			continue
		}
		editable = append(editable, e)
	}

	// A file entry aimed at an owner name the platform owns is dropped rather
	// than merged in beside it. The test is the owner name, not the value: the
	// harm in "@ IN NS ns9.example." is that it joins the delegation, and a
	// value-level check would let it through. A different owner name in the same
	// object — a subdomain delegation living in <zone>-ns — is still editable.
	//
	// Both tests are needed. ownedByPlatform catches a file entry aimed at an
	// entry that is live and protected; protectedEntry catches one whose SHAPE
	// is the platform's when there is nothing live to compare against. The
	// operator creates the SOA and apex-NS sets only once a zone's nameservers
	// are assigned, so a zone that has not reached that point has no protected
	// entry for the owner-name test to match — and applying a provider export to
	// it would create <zone>-soa from the old provider's record, under exactly
	// the name the operator later looks for, making it the zone's SOA
	// permanently. `zone import` closes the same race the same way.
	//
	// That window is bounded by nameserver assignment, not by controller
	// latency: ensureSOARecordSet returns early while status.nameservers is
	// empty. A zone stuck in Pending has an unbounded window, and it is the
	// state a user is most likely to be impatiently applying against — so the
	// window is correlated with haste rather than merely possible.
	file := make([]dnsv1alpha1.RecordEntry, 0, len(tp.file))
	for _, w := range tp.file {
		if protect && (ownedByPlatform(keep, w.Name, zone) || p.protectedEntry(tp.set, tp.rrType, w)) {
			continue
		}
		file = append(file, w)
	}

	next := keep
	if p.prune {
		return append(next, file...)
	}
	next = append(next, editable...)
	for _, w := range file {
		// A single-valued type holds one value per owner name, so the file's
		// entry REPLACES whatever is at that name rather than joining it.
		// Matching by value instead — as the multi-valued merge below does —
		// appended the new value beside the old one, and `record apply -f` on a
		// repointed CNAME failed with "2 values but is single-valued" unless
		// --prune happened to be passed. Entries the platform owns are never
		// reached: a file entry aimed at a protected owner was dropped above.
		if rdata.IsSingleValued(tp.rrType) {
			next = append(withoutOwner(next, w.Name, zone), w)
			continue
		}
		if i := indexOfEntry(next, tp.rrType, w, zone); i >= 0 {
			if indexOfEntry(keep, tp.rrType, w, zone) < 0 {
				next[i] = w
			}
			continue
		}
		next = append(next, w)
	}
	return next
}

// withoutOwner drops every entry at one owner name.
//
// It does not need to spare the preserved platform entries: a file entry aimed
// at an owner the platform holds was dropped before this point, so no name
// reaching here appears in the keep set.
func withoutOwner(entries []dnsv1alpha1.RecordEntry, name, zone string) []dnsv1alpha1.RecordEntry {
	out := entries[:0:0]
	for _, e := range entries {
		if sameOwnerName(e.Name, name, zone) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// ownedByPlatform reports whether one of the preserved entries carries the same
// owner name as w.
func ownedByPlatform(keep []dnsv1alpha1.RecordEntry, name, zone string) bool {
	for _, e := range keep {
		if sameOwnerName(e.Name, name, zone) {
			return true
		}
	}
	return false
}

// protectedEntry reports whether an entry must survive --prune.
//
// It asks TWICE, on purpose. classify answers "who owns this", which is the
// right question for `list`'s marker and `describe`'s header and the wrong one
// here: apply is the only caller that turns the answer into a DELETE, so it is
// the only caller where a false negative loses data rather than a label. The
// second test is the shape-based rule the targeted verbs gate on, so a record
// create/set/delete all refuse to touch can never be silently pruned by the
// bulk path.
//
// The belt and the braces are worth it specifically here: silent loss of a
// zone's delegation is the failure this codebase has now found six separate
// routes to, and every one of them looked individually implausible first.
func (p *plan) protectedEntry(set *dnsv1alpha1.DNSRecordSet, t dnsv1alpha1.RRType, e dnsv1alpha1.RecordEntry) bool {
	if classify(set, e, p.zone.Spec.DomainName) != provUser {
		return true
	}
	return platformRisk(set, p.zone, t, e.Name) != ""
}

// reasonFor names why a type's changes were withheld.
//
// A nil set is not "no reason": a change can be withheld precisely because the
// type is the platform's and no set exists yet, which is the shape that would
// otherwise let a provider export create the zone's SOA. Only the Gateway test
// needs a set to consult.
func (p *plan) reasonFor(tp *typePlan) string {
	if tp.set != nil && classify(tp.set, dnsv1alpha1.RecordEntry{}, p.zone.Spec.DomainName) == provGateway {
		if owner := gatewayOwner(tp.set); owner != "" {
			return "managed by AI Edge — Gateway " + owner
		}
		return "managed by AI Edge"
	}
	switch tp.rrType {
	case dnsv1alpha1.RRTypeSOA:
		return "the zone's SOA record"
	case dnsv1alpha1.RRTypeNS:
		return "the zone's apex NS records"
	}
	return "platform-managed"
}

// converge writes the plan, one API call per record type.
func (p *plan) converge(ctx context.Context, c client.Client, dryRun bool, out, warnTo io.Writer) error {
	var failures []string
	applied := 0

	for _, tp := range p.units {
		t := tp.rrType

		// The backend applies the FIRST entry's TTL to a whole owner name and
		// drops the rest silently, and a file merged into live records is how an
		// owner ends up with two.
		for _, w := range rdata.WarningsInZone(t, p.zone.Spec.DomainName, tp.next...) {
			_, _ = fmt.Fprintf(warnTo, "Warning: %s %s\n", t, w)
		}
		// applyEdit carries the resourceVersion precondition and re-applies once
		// against fresh state, so a concurrent writer to the same type cannot be
		// clobbered. The closure recomputes from whatever it is handed, which is
		// what makes the retry correct rather than merely repeated.
		//
		// Validation lives INSIDE the closure for exactly that reason. Checking
		// tp.next — computed from prefetched state — would validate a result the
		// retry then discards: a concurrent writer adding a second CNAME at the
		// same owner produced a stored two-value CNAME set at exit 0, because
		// the recomputation was never checked. The mutate paths put
		// validateOwner inside their closures for the same reason.
		edit := func(current []dnsv1alpha1.RecordEntry) ([]dnsv1alpha1.RecordEntry, error) {
			resolved := p.resolve(tp, canonicalEntries(t, current), true)

			// An empty result is not an invalid set, it is a deletion:
			// spec.records has MinItems=1, so the object goes rather than being
			// written back.
			//
			// This is the whole-slice check, and it is not redundant with the
			// per-entry pass readZoneFile already ran: only ValidateEntriesInZone
			// catches a two-value CNAME set and duplicates within one owner,
			// which PowerDNS 422s the entire RRset over.
			if len(resolved) > 0 {
				if err := rdata.ValidateEntriesInZone(t, resolved, p.zone.Spec.DomainName); err != nil {
					return nil, err
				}
			}
			return resolved, nil
		}
		if _, err := applyEdit(ctx, c, p.zone, t, "", tp.set, dryRun, edit); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", t, err))
			continue
		}
		applied += len(tp.changes)
	}

	if dryRun {
		_, _ = fmt.Fprintf(out, "Dry run — %d %s validated, nothing was written.\n",
			applied, pluralize(applied, "change"))
	} else {
		_, _ = fmt.Fprintf(out, "Applied %d %s to %s.\n",
			applied, pluralize(applied, "change"), p.zone.Spec.DomainName)
	}
	if len(failures) > 0 {
		// A type that failed must not hide the ones that succeeded, so the error
		// lands after the summary rather than instead of it.
		return util.NewCLIError(util.ExitError,
			"some record types could not be written:\n  "+strings.Join(failures, "\n  ")).
			WithFix("re-run the command — the record types that succeeded are already written.")
	}
	return nil
}

// reportSkipped names what the command declined to touch. Silence here would
// read as "the file was applied in full", which is the one thing it was not.
func (p *plan) reportSkipped(w io.Writer) {
	if len(p.skipped) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "%d %s in the file %s not applied — the records belong to the platform:\n",
		len(p.skipped), pluralize(len(p.skipped), "change"), wasWere(len(p.skipped)))
	tw := util.NewTabWriter(w)
	for _, c := range p.skipped {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t(%s)\n",
			c.mark, c.name, c.rrType, rdata.Render(c.rrType, c.entry), c.reason)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintln(w)
}

// ---------------------------------------------------------------------------
// Diffing
// ---------------------------------------------------------------------------

// diffEntries compares two entry lists of one type. TTL is not part of a
// record's identity, so a TTL edit shows as one change line rather than as a
// delete and an add.
func diffEntries(t dnsv1alpha1.RRType, before, after []dnsv1alpha1.RecordEntry, zone string) []change {
	var out []change

	for _, a := range after {
		i := indexOfEntry(before, t, a, zone)
		switch {
		case i < 0:
			out = append(out, change{mark: markAdd, name: displayName(a.Name), rrType: t, entry: a})
		case !util.TTLEqual(before[i].TTL, a.TTL):
			out = append(out, change{
				mark: markChange, name: displayName(a.Name), rrType: t, entry: a,
				oldTTL: before[i].TTL, newTTL: a.TTL,
			})
		}
	}
	for _, b := range before {
		if indexOfEntry(after, t, b, zone) < 0 {
			out = append(out, change{mark: markRemove, name: displayName(b.Name), rrType: t, entry: b})
		}
	}
	return out
}

// withheld returns the changes the file asked for that the plan will not make.
func withheld(wanted, applying []change, reason string) []change {
	have := map[string]bool{}
	for _, c := range applying {
		have[changeKey(c)] = true
	}
	out := make([]change, 0, len(wanted))
	for _, c := range wanted {
		if have[changeKey(c)] {
			continue
		}
		c.reason = reason
		out = append(out, c)
	}
	return out
}

// changeKey identifies one change for deduplication.
//
// The TTL goes in as its EFFECTIVE value, not its formatted one. A nil TTL and
// an explicit DefaultTTL are the same record, and the formatted forms differ
// ("Auto" vs "300") — the same comparison class that made every Auto record
// report as changed elsewhere. It is currently unreachable, because both
// callers derive the TTL from one file entry and diffEntries folds Auto against
// the default first, but that safety is a property of the callers rather than
// of this function.
func changeKey(c change) string {
	ttl := util.DefaultTTL
	if c.newTTL != nil {
		ttl = *c.newTTL
	}
	return c.mark + "\x00" + strings.ToLower(c.name) + "\x00" + rdata.Key(c.rrType, c.entry) +
		"\x00" + strconv.FormatInt(ttl, 10)
}

func printApplyDiff(w io.Writer, changes []change) {
	tw := util.NewTabWriter(w)
	for _, c := range changes {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n",
			c.mark, c.name, c.rrType, c.ttlColumn(), rdata.Render(c.rrType, c.entry))
	}
	_ = tw.Flush()

	var parts []string
	if n := countMark(changes, markAdd); n > 0 {
		parts = append(parts, fmt.Sprintf("%d to add", n))
	}
	if n := countMark(changes, markChange); n > 0 {
		parts = append(parts, fmt.Sprintf("%d to change", n))
	}
	if n := countMark(changes, markRemove); n > 0 {
		parts = append(parts, fmt.Sprintf("%d to delete", n))
	}
	_, _ = fmt.Fprintf(w, "\n%d %s — %s\n\n",
		len(changes), pluralize(len(changes), "change"), strings.Join(parts, ", "))
}

// sortChanges groups by type and name, then puts additions before changes
// before removals, so a record's fate reads top to bottom.
func sortChanges(changes []change) {
	rank := map[string]int{markAdd: 0, markChange: 1, markRemove: 2}
	sort.SliceStable(changes, func(i, j int) bool {
		a, b := changes[i], changes[j]
		if a.rrType != b.rrType {
			return a.rrType < b.rrType
		}
		if a.name != b.name {
			return lessName(a.name, b.name)
		}
		if a.mark != b.mark {
			return rank[a.mark] < rank[b.mark]
		}
		return rdata.Render(a.rrType, a.entry) < rdata.Render(b.rrType, b.entry)
	})
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// currentEntries reads a set's records into the canonical form everything else
// compares against.
func currentEntries(set *dnsv1alpha1.DNSRecordSet, t dnsv1alpha1.RRType) []dnsv1alpha1.RecordEntry {
	if set == nil {
		return nil
	}
	return canonicalEntries(t, set.Spec.Records)
}

// indexOfEntry finds an entry with the same owner name and the same value,
// ignoring TTL — which is what makes re-applying an unchanged file a no-op.
func indexOfEntry(
	entries []dnsv1alpha1.RecordEntry, t dnsv1alpha1.RRType, want dnsv1alpha1.RecordEntry, zone string,
) int {
	for i, e := range entries {
		if sameOwnerName(e.Name, want.Name, zone) && entriesEqual(t, e, want) {
			return i
		}
	}
	return -1
}

// sameOwnerName compares two owner names the way the backend will.
//
// pdns.QualifyOwner keys an RRset on the qualified name, so "www" and
// "www.example.com." are one owner, as are "@", "" and "example.com.". The CRD
// name pattern admits every one of those spellings, and comparing the literal
// strings sees two owners where the backend sees one: the diff then shows an
// add for a record that already exists, --prune shows a delete for the record
// it is about to re-add, and a platform entry stored under an absolute name is
// not recognised as one. sameOwner is the shared implementation, in client.go.
func sameOwnerName(a, b, zone string) bool { return sameOwner(a, b, zone) }

func countMark(changes []change, mark string) int {
	n := 0
	for _, c := range changes {
		if c.mark == mark {
			n++
		}
	}
	return n
}

func wasWere(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

func displayPath(path string) string {
	if path == "-" {
		return "standard input"
	}
	return path
}
