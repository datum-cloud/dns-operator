// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// Condition types and reasons the operator publishes. They are duplicated here
// rather than imported from internal/controller so the CLI does not pull the
// controller's dependency tree into the plugin binary.
// CondAccepted and CondProgrammed are exported so commands can read these
// conditions directly via FindCondition without re-spelling the string.
const (
	CondAccepted   = "Accepted"
	CondProgrammed = "Programmed"
)

const (
	condAccepted   = CondAccepted
	condProgrammed = CondProgrammed

	reasonPending   = "Pending"
	reasonNotOwner  = "NotOwner"
	reasonPDNSError = "PDNSError"
	reasonConflict  = "Conflict"
)

// Status words. The first word of each is the filter token a --status flag
// matches on, so they are exported for the commands that implement one.
const (
	StatusOK         = "OK"
	StatusPending    = "Pending"
	StatusError      = "Error"
	StatusRejected   = "Rejected"
	StatusProgrammed = "Programmed"
	StatusNotOwner   = "Not owner"
	StatusConflict   = "Conflict"
	StatusUnknown    = "Unknown"
)

// pendingDetail is the explanation for a record that has no backend outcome yet.
const pendingDetail = "waiting for the DNS backend"

// FindCondition returns the first condition with the given type, or nil.
func FindCondition(conditions []metav1.Condition, condType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == condType {
			return &conditions[i]
		}
	}
	return nil
}

// ZoneStatus renders a zone's condition set as a status word plus a lowercase
// explanation. The first word is the filter token, as in compute: `--status ok`
// matches "OK", `--status error` matches "Error".
//
// Reasons the operator does not define are passed through raw rather than
// guessed at — a CLI that invents wording for an unknown reason is a CLI that
// lies the first time the server grows one.
func ZoneStatus(z *dnsv1alpha1.DNSZone) (word, detail string) {
	if z == nil {
		return StatusUnknown, "no zone data"
	}

	// Admission rejection is terminal and outranks whatever Programmed says.
	if accepted := FindCondition(z.Status.Conditions, condAccepted); accepted != nil &&
		accepted.Status == metav1.ConditionFalse {
		return StatusRejected, firstNonEmpty(accepted.Message, accepted.Reason, "the zone was rejected")
	}

	programmed := FindCondition(z.Status.Conditions, condProgrammed)
	if programmed == nil {
		return StatusPending, pendingDetail
	}

	switch programmed.Status {
	case metav1.ConditionTrue:
		return StatusOK, fmt.Sprintf("zone programmed, %s live", pluralRecords(z.Status.RecordCount))
	case metav1.ConditionFalse:
		if programmed.Reason == reasonPending || programmed.Reason == "" {
			return StatusPending, firstNonEmpty(programmed.Message, pendingDetail)
		}
		return StatusError, firstNonEmpty(programmed.Message, programmed.Reason)
	default:
		return StatusPending, firstNonEmpty(programmed.Message, pendingDetail)
	}
}

// RecordStatus renders the status of one owner name within a record set.
//
// The word comes from the per-owner-name condition in status.recordSets[], never
// from the rolled-up top-level Programmed condition: the interesting reasons —
// NotOwner, Conflict, PDNSError — only exist per name, and the rollup flattens
// all of them to a generic Pending.
//
// The backend's messages are written for humans already, so they are shown
// verbatim; only the handful of known reasons get CLI wording.
func RecordStatus(rs *dnsv1alpha1.DNSRecordSet, ownerName string) (word, detail string) {
	return recordStatus(rs, ownerName, "")
}

// RecordStatusInZone is RecordStatus with the zone domain supplied, which lets
// it resolve every spelling of an owner name the backend treats as one: "@"
// against "example.com.", and a relative label against its fully qualified
// form. Prefer it wherever the zone is known — RecordStatus can only normalise
// the spellings that do not need it.
func RecordStatusInZone(rs *dnsv1alpha1.DNSRecordSet, ownerName, zone string) (word, detail string) {
	return recordStatus(rs, ownerName, zone)
}

func recordStatus(rs *dnsv1alpha1.DNSRecordSet, ownerName, zone string) (word, detail string) {
	if rs == nil {
		return StatusUnknown, "no record set data"
	}

	// A set the API server rejected never reaches the backend, so no per-name
	// condition will ever appear for it.
	if accepted := FindCondition(rs.Status.Conditions, condAccepted); accepted != nil &&
		accepted.Status == metav1.ConditionFalse {
		return StatusRejected, firstNonEmpty(accepted.Message, accepted.Reason, "the record set was rejected")
	}

	conditions := ownerConditions(rs, ownerName, zone, condProgrammed)
	if len(conditions) == 0 {
		return StatusPending, pendingDetail
	}

	// Reduce worst-first across every spelling of this owner name. Ties keep
	// the earliest entry, so the result is stable for a given status list.
	worstRank := -1
	for _, c := range conditions {
		w, d := classifyProgrammed(c)
		if rank := statusSeverity(w); rank > worstRank {
			worstRank, word, detail = rank, w, d
		}
	}
	return word, detail
}

// ownerConditions returns the named condition from EVERY per-name status entry
// whose owner resolves to the same RRset, in status order. A nil element means
// that entry exists but carries no such condition yet.
//
// Matching is on the qualified owner name, not the literal spelling. The DNS
// backend collapses several spellings onto one RRset — "www", "WWW", and
// "www.example.com." in zone example.com are all the same name, as are "@" and
// "example.com." — so comparing raw strings reports "no status" for a record
// that has one. That failure is silent and reassuring in the worst way: a
// Conflict or a NotOwner renders as a placid "Pending".
//
// Returning every match rather than the first is the other half of the same
// problem. One bucket can hold both "www" and "www.example.com.", which is two
// status entries for one RRset, and they can disagree — one Programmed, the
// other in Conflict. Stopping at the first would report whichever the server
// happened to list first, so a caller asking "is this record live" could be
// told yes while a second spelling of it is failing.
//
// zone is optional. Without it the apex and zone-qualified spellings cannot be
// resolved, so case and the trailing dot are still normalised and the rest is
// left alone.
func ownerConditions(rs *dnsv1alpha1.DNSRecordSet, ownerName, zone, condType string) []*metav1.Condition {
	want := qualifyOwner(ownerName, zone)
	found := make([]*metav1.Condition, 0, len(rs.Status.RecordSets))
	for i := range rs.Status.RecordSets {
		if qualifyOwner(rs.Status.RecordSets[i].Name, zone) != want {
			continue
		}
		found = append(found, FindCondition(rs.Status.RecordSets[i].Conditions, condType))
	}
	return found
}

// statusSeverity ranks the status words so a fold across several entries can
// pick the one the user most needs to see.
//
// Two properties matter. Anything unhealthy outranks Programmed, so a record is
// never called live while one spelling of it is failing. And an unrecognised
// reason — a word this package has no mapping for — outranks both Pending and
// Programmed, because a reason the server invented after this code was written
// is far more likely to be a new failure than a new kind of success.
func statusSeverity(word string) int {
	switch word {
	case StatusRejected:
		return 70
	case StatusError:
		return 60
	case StatusConflict:
		return 50
	case StatusNotOwner:
		return 40
	case StatusPending:
		return 20
	case StatusProgrammed:
		return 10
	case StatusUnknown:
		return 5
	default:
		// An unrecognised reason, passed through raw.
		return 30
	}
}

// classifyProgrammed maps one Programmed condition to its status word and
// detail. A nil condition is an owner the backend has not reported on.
func classifyProgrammed(c *metav1.Condition) (word, detail string) {
	if c == nil {
		return StatusPending, pendingDetail
	}
	if c.Status == metav1.ConditionTrue {
		return StatusProgrammed, firstNonEmpty(c.Message, "live in the DNS backend")
	}

	switch c.Reason {
	case reasonNotOwner:
		return StatusNotOwner, firstNonEmpty(c.Message, "another record set owns this name")
	case reasonConflict:
		return StatusConflict, firstNonEmpty(c.Message, "the backend reported a conflict")
	case reasonPDNSError:
		return StatusError, firstNonEmpty(c.Message, "the backend reported an error")
	case reasonPending, "":
		return StatusPending, firstNonEmpty(c.Message, pendingDetail)
	default:
		// An unknown reason: show the server's own words on both halves.
		return c.Reason, c.Message
	}
}

// qualifyOwner reduces an owner name to the form the DNS backend keys an RRset
// by, so two spellings of one name compare equal.
//
// It mirrors the operator's own QualifyOwner (internal/pdns/client.go), with
// the addition of case folding: DNS names are case-insensitive, and the backend
// treats "WWW" and "www" as one name even though a byte comparison does not.
// With no zone, only the parts that do not need one are normalised.
func qualifyOwner(owner, zone string) string {
	owner = strings.ToLower(strings.TrimSpace(owner))
	zone = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(zone), "."))

	if owner == "@" || owner == "" {
		if zone == "" {
			return "@"
		}
		return zone + "."
	}
	if strings.HasSuffix(owner, ".") {
		if zone == "" {
			// Cannot tell whether this is the apex; leave it as written.
			return owner
		}
		return owner
	}
	if zone == "" {
		return owner
	}
	return owner + "." + zone + "."
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func pluralRecords(n int) string {
	if n == 1 {
		return "1 record"
	}
	return fmt.Sprintf("%d records", n)
}
