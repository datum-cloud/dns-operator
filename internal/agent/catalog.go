// SPDX-License-Identifier: AGPL-3.0-only

// Package agent turns DNS condition status into answers an assistant can give
// a customer: a lookup table over the condition reasons the operator
// publishes (catalog.go), and a walk over a zone's record sets that turns
// them into one root cause (diagnose.go).
package agent

import (
	"time"

	sharedutil "go.miloapis.com/dns-operator/internal/dns/util"
)

// Actionability says who has to do something about a condition.
type Actionability string

const (
	// ActionabilityUser means the record, zone, or domain configuration is
	// wrong and the customer can fix it.
	ActionabilityUser Actionability = "user"

	// ActionabilityPlatform means the cause is Datum's. No change the customer
	// makes will help, and suggesting one wastes their time.
	ActionabilityPlatform Actionability = "platform"

	// ActionabilityTransient means the resource is in a normal in-flight state
	// and the right advice is to wait.
	ActionabilityTransient Actionability = "transient"

	// ActionabilityStalled means a transient reason has held longer than its
	// expected window. Treat it as suspect and escalate.
	//
	// Deliberately not ActionabilityPlatform: platform means a cause was
	// reported and it is Datum's, while stalled means nothing reported a cause
	// at all and only elapsed time falsified the classification — the fault may
	// still be the customer's, or the registrar's. Report the duration; do not
	// assert a platform fault.
	//
	// Never written in the catalog: derived at read time from
	// LastTransitionTime, so it cannot go stale in storage.
	ActionabilityStalled Actionability = "stalled"
)

// ReasonInfo explains one condition reason.
type ReasonInfo struct {
	// Reason is the condition reason as written by the operator.
	Reason string `json:"reason"`
	// ConditionTypes are the condition types this reason is observed on.
	ConditionTypes []string `json:"conditionTypes"`
	// Actionability says who must act. For Conflict and PDNSError this is a
	// default only — the diagnosis walk can refine it once it has more
	// evidence than the reason alone carries (see classifyConflict and
	// classifyPDNSError in diagnose.go).
	Actionability Actionability `json:"actionability"`
	// Explanation states, in the customer's language, what happened. They
	// know records, zones, nameservers, and registrars; they have never heard
	// of a reconciler or a downstream shadow. copy_test.go enforces the
	// vocabulary.
	Explanation string `json:"explanation"`
	// Remediation says what to do next. Empty for healthy reasons.
	Remediation string `json:"remediation,omitempty"`
	// Skill names the runbook covering this class of failure, if any.
	Skill string `json:"skill,omitempty"`
	// ExpectedDuration bounds how long a transient reason should plausibly
	// last; past it, callers report ActionabilityStalled. Zero means no
	// window: the reason is not transient, or it is a resting state that
	// legitimately persists forever (Programmed, Accepted, Discovered).
	//
	// Omitted from JSON: time.Duration marshals as a nanosecond count, which
	// is noise in a tool result; ExpectedWithin carries it instead.
	ExpectedDuration time.Duration `json:"-"`
	// ExpectedWithin renders ExpectedDuration for a reader ("30m").
	ExpectedWithin string `json:"expectedWithin,omitempty"`
}

// Expected windows for the transient reasons. A transient classification is a
// claim about duration, so every reason that says "wait" also says how long
// is reasonable and the claim becomes falsifiable.
//
// They are generous on purpose: a window that is too short turns healthy work
// into a false alarm, which costs more trust than a stall noticed an hour
// late.
const (
	// windowBackendProgramming: the operator hands a record to the DNS
	// backend and waits for it to report the result. No external party is
	// involved.
	windowBackendProgramming = 10 * time.Minute

	// windowDomainVerification: verification is a separate service and can
	// legitimately wait on the customer to act, but the check itself should
	// not sit idle much longer than this once they have.
	windowDomainVerification = 30 * time.Minute
)

// Remediation strings shared by several reasons.
const (
	// remediationWait is the advice for transient states. The phrase "clears
	// on its own" is load-bearing: it is the claim the stalled path has to
	// retract.
	remediationWait = "Nothing to do here — this is normal and clears on its own."
	// remediationEscalate is the advice for faults on Datum's side that the
	// customer has no lever on at all.
	remediationEscalate = "Raise this with Datum. There is no change to your zone or records that will clear it."
)

// Skill names. These correspond to the runbooks published alongside this
// package; a binding advertises them and the assistant loads one on demand.
const (
	SkillZoneNotResolving    = "zone-not-resolving"
	SkillRecordNotProgrammed = "record-not-programmed"
	SkillConflictingRecord   = "conflicting-record"
	SkillRecordNotOwner      = "record-not-owner"
	SkillDelegationCheck     = "delegation-check"
	SkillDomainVerification  = "domain-verification"
	SkillManagedRecord       = "managed-record-refused"
)

// catalog is the full reason vocabulary. Entries key off
// go.miloapis.com/dns-operator/internal/dns/util's exported reason constants
// rather than string literals so the two cannot drift silently;
// TestCatalogCoversEveryKnownReason enforces completeness.
var catalog = []ReasonInfo{
	// --------------------------------------------------------------- healthy
	{
		Reason:         sharedutil.ReasonAccepted,
		ConditionTypes: []string{sharedutil.CondAccepted},
		Actionability:  ActionabilityTransient,
		Explanation:    "Datum admitted this zone or record set: it is well-formed and was not rejected outright.",
	},
	{
		Reason:         sharedutil.ReasonProgrammed,
		ConditionTypes: []string{sharedutil.CondProgrammed},
		Actionability:  ActionabilityTransient,
		Explanation:    "This is live in the DNS backend.",
	},
	{
		Reason:         sharedutil.ReasonDiscovered,
		ConditionTypes: []string{sharedutil.CondDiscovered},
		Actionability:  ActionabilityTransient,
		Explanation:    "Datum read the records this domain currently serves and captured a snapshot of them.",
	},

	// -------------------------------------------------------------- pending
	{
		Reason:           sharedutil.ReasonPending,
		ConditionTypes:   []string{sharedutil.CondProgrammed},
		Actionability:    ActionabilityTransient,
		Explanation:      "Datum has not finished sending this to the DNS backend yet.",
		Remediation:      remediationWait,
		Skill:            SkillRecordNotProgrammed,
		ExpectedDuration: windowBackendProgramming,
	},
	{
		Reason:         sharedutil.ReasonPendingDomainVerification,
		ConditionTypes: []string{sharedutil.CondAccepted, sharedutil.CondProgrammed},
		Actionability:  ActionabilityUser,
		Explanation: "This domain has not been verified as yours yet. Nothing programs into the DNS " +
			"backend until verification finishes, and verification is a separate step the customer " +
			"completes — Datum cannot finish it for them.",
		Remediation:      "Complete domain verification: follow the instructions on the Domain object.",
		Skill:            SkillDomainVerification,
		ExpectedDuration: windowDomainVerification,
	},

	// ------------------------------------------------------------- rejected
	{
		Reason:         sharedutil.ReasonInvalidDNSRecordSet,
		ConditionTypes: []string{sharedutil.CondAccepted},
		Actionability:  ActionabilityUser,
		Explanation:    "This record set was rejected before it ever reached the DNS backend because it is invalid as written.",
		Remediation:    "Read the rejection message; it names what is wrong with the record's shape or content.",
	},

	// ----------------------------------------------------------- ownership
	{
		Reason:         sharedutil.ReasonNotOwner,
		ConditionTypes: []string{sharedutil.CondProgrammed},
		Actionability:  ActionabilityUser,
		Explanation: "Another record set already holds this name in the DNS backend, so this one lost " +
			"the race to own it. This never clears on its own.",
		Remediation: "Find the older record set holding this name and decide which one should survive — " +
			"delete or rename the loser.",
		Skill: SkillRecordNotOwner,
	},

	// ------------------------------------------------------------- conflict
	// Actionability here is the default only. classifyConflict in diagnose.go
	// distinguishes a genuine coexistence conflict (user) from an orphaned
	// downstream record holding the name hostage (platform) — the reason
	// alone cannot tell the two apart, only the record graph can.
	{
		Reason:         sharedutil.ReasonConflict,
		ConditionTypes: []string{sharedutil.CondProgrammed},
		Actionability:  ActionabilityUser,
		Explanation: "The DNS backend already has a record at this name that this record set cannot " +
			"coexist with — most often a CNAME or ALIAS next to another record type at the same name.",
		Remediation: "Remove or rename whichever record should not be there. Conflicts are re-checked " +
			"only every 10 minutes, so nothing changing right away is expected.",
		Skill: SkillConflictingRecord,
	},

	// ------------------------------------------------------------- backend
	// Actionability here is the default only. classifyPDNSError in
	// diagnose.go reclassifies by the message's content, since one reason
	// covers messages that are the customer's fault, transient backend
	// trouble, and a zone still provisioning.
	{
		Reason:         sharedutil.ReasonPDNSError,
		ConditionTypes: []string{sharedutil.CondProgrammed},
		Actionability:  ActionabilityUser,
		Explanation:    "The DNS backend rejected this record. The status message states why.",
		Remediation:    "Read the status message — it names the specific problem with this record.",
	},

	// --------------------------------------------------------------- platform
	{
		Reason:         sharedutil.ReasonDNSZoneInUse,
		ConditionTypes: []string{sharedutil.CondAccepted},
		Actionability:  ActionabilityPlatform,
		Explanation: "Another zone already owns this domain in Datum's records, so this one cannot be " +
			"provisioned. This is not something a customer can see or cause from their own project.",
		Remediation: remediationEscalate,
	},
}

// byReason indexes the catalog, rendering each entry's window in the same
// pass so the duration is written in one place and every reader sees one
// string.
var byReason = func() map[string]ReasonInfo {
	m := make(map[string]ReasonInfo, len(catalog))
	for i := range catalog {
		catalog[i].ExpectedWithin = humanDuration(catalog[i].ExpectedDuration)
		m[catalog[i].Reason] = catalog[i]
	}
	return m
}()

// ActionabilityAt reports how a reason should be treated given how long its
// condition has held, escalating a transient reason to ActionabilityStalled
// once it outlives the catalog's window. Nothing is persisted: lastTransition
// (RFC 3339) is compared against now on every read.
//
// A missing, unparseable, or implausible timestamp returns the static
// classification — absence of evidence that a state is old is not evidence it
// has stalled.
func ActionabilityAt(info ReasonInfo, lastTransition string, now time.Time) Actionability {
	if info.Actionability != ActionabilityTransient || info.ExpectedDuration <= 0 {
		return info.Actionability
	}
	elapsed, ok := age(lastTransition, now)
	if !ok || elapsed <= info.ExpectedDuration {
		return info.Actionability
	}
	return ActionabilityStalled
}

// ExplainReason returns the catalog entry for a condition reason.
func ExplainReason(reason string) (ReasonInfo, bool) {
	info, ok := byReason[reason]
	return info, ok
}

// AllReasons returns the whole catalog, in declaration order.
func AllReasons() []ReasonInfo {
	out := make([]ReasonInfo, len(catalog))
	copy(out, catalog)
	return out
}
