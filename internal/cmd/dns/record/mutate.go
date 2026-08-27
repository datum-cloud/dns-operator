// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// Actions a write can report, used in the echo line and in the diff header.
const (
	actionCreated = "created"
	actionUpdated = "updated"
	actionDeleted = "deleted"
	// actionUnchanged is reported when a write was a no-op, so `set` run twice
	// does not claim to have changed something the second time.
	actionUnchanged = "unchanged"
)

// editFunc transforms the entries of a (zone, type) bucket. It is called once
// per attempt, always against freshly read state, so a retry after a conflict
// re-applies the intent rather than re-sending a stale object.
//
// An editFunc works in logical values and does not encode anything: applyEdit
// runs every entry it returns through the API-boundary conversion. That is
// deliberate — an encode each editFunc had to remember would be correct until
// the day someone adds one that forgets, and the resulting record would be
// wrong on the wire with no test failing.
type editFunc func(existing []dnsv1alpha1.RecordEntry) ([]dnsv1alpha1.RecordEntry, error)

// writeResult describes what a write did, for the echo and the dry-run diff.
type writeResult struct {
	action     string
	set        *dnsv1alpha1.DNSRecordSet
	before     []dnsv1alpha1.RecordEntry
	after      []dnsv1alpha1.RecordEntry
	setRemoved bool
}

// applyEdit is the read-modify-write every mutation goes through.
//
// The write carries a resourceVersion precondition, which is the difference
// between this and the portal: two people editing different names in the same
// type bucket would otherwise silently overwrite each other's records, because
// the bucket is one object. On a rejected precondition the edit is re-applied
// once against fresh state, and a second conflict is reported rather than
// retried forever — at that point something else is writing continuously and
// the user needs to know.
func applyEdit(
	ctx context.Context,
	c client.Client,
	zone *dnsv1alpha1.DNSZone,
	t dnsv1alpha1.RRType,
	ownerName string,
	prefetched *dnsv1alpha1.DNSRecordSet,
	dryRun bool,
	edit editFunc,
) (*writeResult, error) {
	const attempts = 2

	set := prefetched
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			refetched, err := findSet(ctx, c, zone, t, ownerName)
			if err != nil {
				return nil, err
			}
			set = refetched
		}

		if set == nil {
			result, err := createSet(ctx, c, zone, t, dryRun, edit)
			if err == nil {
				return result, nil
			}
			if isRetryable(err) && attempt < attempts-1 {
				continue
			}
			return nil, writeError(err, zone, t)
		}

		base := set.DeepCopy()
		after, err := edit(base.Spec.Records)
		if err != nil {
			return nil, err
		}
		after = encodeForAPI(t, after)

		updated := set.DeepCopy()
		updated.Spec.Records = after
		result := &writeResult{
			action: actionUpdated,
			set:    updated,
			before: base.Spec.Records,
			after:  after,
		}

		// spec.records has MinItems=1, so a bucket emptied by a delete cannot
		// be written back — the object itself goes.
		if len(after) == 0 {
			result.action = actionDeleted
			result.setRemoved = true
			err = c.Delete(ctx, updated, deleteCallOptions(base.ResourceVersion, dryRun)...)
		} else {
			patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
			err = c.Patch(ctx, updated, patch, patchOptions(dryRun)...)
		}
		if err == nil {
			return result, nil
		}
		if isRetryable(err) && attempt < attempts-1 {
			continue
		}
		return nil, writeError(err, zone, t)
	}

	return nil, conflictError(zone, t)
}

// createSet writes the first bucket for a (zone, type) pair. The object name
// follows the convention the operator and the portal already use, so a zone's
// objects stay recognisable however they were created.
func createSet(
	ctx context.Context,
	c client.Client,
	zone *dnsv1alpha1.DNSZone,
	t dnsv1alpha1.RRType,
	dryRun bool,
	edit editFunc,
) (*writeResult, error) {
	after, err := edit(nil)
	if err != nil {
		return nil, err
	}
	after = encodeForAPI(t, after)
	if len(after) == 0 {
		return nil, util.NewCLIError(util.ExitError, "refusing to create an empty record set")
	}

	obj := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      setObjectName(zone, t),
			Namespace: zone.Namespace,
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
			RecordType: t,
			Records:    after,
		},
	}
	err = c.Create(ctx, obj, createOptions(dryRun)...)
	if apierrors.IsAlreadyExists(err) {
		// The conventional name is taken, and by a set this command may not
		// write to: one it could have written to would have been resolved
		// instead of arriving here. Let the server pick the suffix rather than
		// guessing one, so two creates racing for the same type cannot choose
		// the same name.
		obj.Name = ""
		obj.GenerateName = setObjectName(zone, t) + "-"
		err = c.Create(ctx, obj, createOptions(dryRun)...)
	}
	if err != nil {
		return nil, err
	}
	return &writeResult{action: actionCreated, set: obj, after: after}, nil
}

// encodeForAPI converts every entry in a bucket to the form the API must store,
// immediately before it is written. It is the single chokepoint for the
// conversion: no editFunc encodes, and nothing reaches spec.Records without
// passing through here.
//
// It runs over the whole slice, untouched neighbours included. rdata's encode is
// idempotent, so an entry the CLI wrote is unchanged; an entry some other client
// stored in its logical form is corrected, which is the outcome the encode
// exists for — the alternative is leaving a TXT value that internal/pdns will
// mangle on the next reconcile.
func encodeForAPI(t dnsv1alpha1.RRType, entries []dnsv1alpha1.RecordEntry) []dnsv1alpha1.RecordEntry {
	if entries == nil {
		return nil
	}
	out := make([]dnsv1alpha1.RecordEntry, len(entries))
	for i, e := range entries {
		out[i] = apiEntry(t, e)
	}
	return out
}

func createOptions(dryRun bool) []client.CreateOption {
	if dryRun {
		return []client.CreateOption{client.DryRunAll}
	}
	return nil
}

func patchOptions(dryRun bool) []client.PatchOption {
	if dryRun {
		return []client.PatchOption{client.DryRunAll}
	}
	return nil
}

func deleteCallOptions(resourceVersion string, dryRun bool) []client.DeleteOption {
	opts := []client.DeleteOption{client.Preconditions{ResourceVersion: &resourceVersion}}
	if dryRun {
		opts = append(opts, client.DryRunAll)
	}
	return opts
}

// isRetryable reports whether a failed write is worth one more attempt against
// fresh state: a rejected precondition, or a create that lost a race.
func isRetryable(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err)
}

// writeError turns an API failure into the plugin's error contract, with the
// conflict case spelled out in the user's own vocabulary.
func writeError(err error, zone *dnsv1alpha1.DNSZone, t dnsv1alpha1.RRType) error {
	if isRetryable(err) {
		return conflictError(zone, t).WithCause(err)
	}
	return util.ClassifyError(err)
}

func conflictError(zone *dnsv1alpha1.DNSZone, t dnsv1alpha1.RRType) *util.CLIError {
	return util.NewCLIError(util.ExitConflict,
		fmt.Sprintf("the %s records for %s changed while this command was running", t, zone.Spec.DomainName)).
		WithFix("re-run the command — someone else modified the same record type.")
}

// entriesForOwner returns the entries of a bucket that belong to one owner name.
func entriesForOwner(entries []dnsv1alpha1.RecordEntry, ownerName, zoneDomain string) []dnsv1alpha1.RecordEntry {
	var out []dnsv1alpha1.RecordEntry
	for _, e := range entries {
		if sameOwner(e.Name, ownerName, zoneDomain) {
			out = append(out, e)
		}
	}
	return out
}

// ownerTTL is the TTL already in force for an owner name: the first entry's,
// which is the one internal/pdns applies to the whole RRset.
func ownerTTL(entries []dnsv1alpha1.RecordEntry, ownerName, zoneDomain string) (*int64, bool) {
	for _, e := range entries {
		if sameOwner(e.Name, ownerName, zoneDomain) {
			return e.TTL, true
		}
	}
	return nil, false
}

// printEcho writes the confirmation block: what happened, then the records in
// presentation format.
//
// The echo is deliberately in the notation the input was not in. A mutation
// driven by --preference/--exchange prints the zone-file line it produced, so
// the other grammar is learned by seeing it; a mutation driven by presentation
// format additionally prints the named fields of a structured type, for the
// same reason in the other direction.
func printEcho(out io.Writer, zone *dnsv1alpha1.DNSZone, t dnsv1alpha1.RRType, ownerName, action string, entries []dnsv1alpha1.RecordEntry, fromFlags bool) {
	_, _ = fmt.Fprintf(out, "  record/%s %s %s %s\n", zone.Spec.DomainName, t, displayName(ownerName), action)
	for _, e := range entries {
		_, _ = fmt.Fprintf(out, "  %s\n", presentationLine(t, e))
		if fromFlags || !rdata.IsStructured(t) {
			continue
		}
		for _, f := range rdata.Fields(t, canonicalEntry(t, e)) {
			_, _ = fmt.Fprintf(out, "      %-12s %s\n", f[0]+":", f[1])
		}
	}
}

// presentationLine renders one entry the way a zone file or `dig` would.
func presentationLine(t dnsv1alpha1.RRType, e dnsv1alpha1.RecordEntry) string {
	c := canonicalEntry(t, e)
	return fmt.Sprintf("%s  %s  IN  %s  %s", displayName(c.Name), rdata.FormatTTL(c.TTL), t, rdata.Render(t, c))
}

// printMutationDiff renders what a dry run would have changed, in compute's diff
// vocabulary: - for what goes, + for what arrives.
func printMutationDiff(out io.Writer, zone *dnsv1alpha1.DNSZone, t dnsv1alpha1.RRType, ownerName string, result *writeResult) {
	verb := map[string]string{
		actionCreated: "would be created",
		actionUpdated: "would be updated",
		actionDeleted: "would be deleted",
	}[result.action]

	_, _ = fmt.Fprintf(out, "Dry run — no changes were made.\n")
	_, _ = fmt.Fprintf(out, "  record/%s %s %s %s\n", zone.Spec.DomainName, t, displayName(ownerName), verb)

	before := entriesForOwner(result.before, ownerName, zone.Spec.DomainName)
	after := entriesForOwner(result.after, ownerName, zone.Spec.DomainName)

	for _, e := range before {
		if !containsEntry(after, t, e) {
			_, _ = fmt.Fprintf(out, "  - %s\n", presentationLine(t, e))
		}
	}
	for _, e := range after {
		if !containsEntry(before, t, e) {
			_, _ = fmt.Fprintf(out, "  + %s\n", presentationLine(t, e))
		}
	}
	if result.setRemoved {
		_, _ = fmt.Fprintf(out, "  record set %s would be removed — it would hold no records\n", result.set.Name)
	}
}

func containsEntry(entries []dnsv1alpha1.RecordEntry, t dnsv1alpha1.RRType, want dnsv1alpha1.RecordEntry) bool {
	for _, e := range entries {
		// Effective TTLs: an Auto entry and an explicit 300 resolve to the same
		// record, and reporting them as a change would make every dry run of an
		// unchanged Auto record show a spurious -/+ pair.
		if entriesEqual(t, e, want) && util.TTLEqual(e.TTL, want.TTL) {
			return true
		}
	}
	return false
}

// printWarnings emits non-fatal advisories to stderr, where they cannot corrupt
// the -o json contract on stdout.
func printWarnings(errOut io.Writer, warnings []string) {
	for _, w := range warnings {
		_, _ = fmt.Fprintf(errOut, "Warning: %s\n", w)
	}
}

// zoneNameArg is the shared completion for the first positional.
func zoneNameArg(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	switch len(args) {
	case 0:
		return util.CompleteZoneNames(cmd, args, toComplete)
	case 2:
		return completeRRTypes(cmd, args, toComplete)
	default:
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}
