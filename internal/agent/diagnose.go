// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"fmt"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	sharedutil "go.miloapis.com/dns-operator/internal/dns/util"
)

// Cause is one failing condition, joined to its catalog entry.
type Cause struct {
	Object        string `json:"object"`
	ConditionType string `json:"conditionType"`
	Status        string `json:"status"`
	Reason        string `json:"reason"`
	Message       string `json:"message"`
	// LastTransitionTime is RFC 3339. It is a string rather than a metav1.Time
	// because this struct is a tool's output schema: metav1.Time marshals as a
	// string but is a struct, which JSON Schema inference rejects.
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
	// InStateFor is how long the condition has held this status ("5d", "12m"),
	// derived at read time and never stored. Empty when the API set no
	// timestamp, or set one that cannot be believed.
	InStateFor string `json:"inStateFor,omitempty"`
	// Reported separates False — something looked and reported a failure —
	// from Unknown, where nothing reported either way and the reason states
	// an intent, not an observation.
	Reported bool `json:"reported"`
	// ObjectAge is how long the object carrying this condition has existed.
	ObjectAge string `json:"objectAge,omitempty"`
	// FailingFor is a floor on how long this object has been failing: the
	// greater of InStateFor and ObjectAge, set only while it is not ready. Not
	// a claim that Reason held that whole span — a rewrite of the condition
	// resets InStateFor without the underlying problem changing.
	FailingFor string `json:"failingFor,omitempty"`
	// Pattern names a failure shape that no controller reported and only the
	// evidence available to this diagnosis reveals. Empty unless one was
	// recognised.
	Pattern       string        `json:"pattern,omitempty"`
	Actionability Actionability `json:"actionability,omitempty"`
	Explanation   string        `json:"explanation"`
	Remediation   string        `json:"remediation,omitempty"`
	Skill         string        `json:"skill,omitempty"`
}

func (c *Cause) note(s string) {
	if s == "" {
		return
	}
	c.Explanation = strings.TrimSpace(c.Explanation + " " + s)
}

// PatternOrphanedBackendRecord: the backend refuses a record as a conflict,
// but nothing else in the customer's project claims that name. The record
// that is actually holding the name is not visible from here — it needs an
// operator to find and remove it. See
// docs/troubleshooting/dnsrecordset-downstream-orphan.md.
const PatternOrphanedBackendRecord = "OrphanedBackendRecord"

// Diagnosis is the answer: one root cause, the evidence behind it, and what
// to do next.
type Diagnosis struct {
	Zone string `json:"zone"`
	// OwnerName is set only for a record diagnosis.
	OwnerName              string   `json:"ownerName,omitempty"`
	Healthy                bool     `json:"healthy"`
	Summary                string   `json:"summary"`
	RootCause              *Cause   `json:"rootCause"`
	ContributingConditions []Cause  `json:"contributingConditions"`
	NextSteps              []string `json:"nextSteps"`
	SuggestedSkill         string   `json:"suggestedSkill,omitempty"`
}

// object is the context a condition is read in.
type object struct {
	name     string
	created  metav1.Time
	notReady bool
}

// DiagnoseZone explains why a DNSZone is not Accepted or not Programmed.
//
// It does not look at delegation — whether the registrar actually points at
// the assigned nameservers — because a zone can be fully Programmed and
// still resolve nothing for a reason this diagnosis has no evidence for.
// dns_delegation_check answers that question separately.
func DiagnoseZone(zone *dnsv1alpha1.DNSZone) Diagnosis {
	return DiagnoseZoneAt(time.Now(), zone)
}

// DiagnoseZoneAt is DiagnoseZone with the clock supplied, so a test can pin
// an age instead of racing wall time.
func DiagnoseZoneAt(now time.Time, zone *dnsv1alpha1.DNSZone) Diagnosis {
	obj := object{
		name:    zone.Spec.DomainName,
		created: zone.CreationTimestamp,
	}

	accepted := apimeta.FindStatusCondition(zone.Status.Conditions, sharedutil.CondAccepted)
	programmed := apimeta.FindStatusCondition(zone.Status.Conditions, sharedutil.CondProgrammed)
	obj.notReady = accepted == nil || accepted.Status != metav1.ConditionTrue ||
		programmed == nil || programmed.Status != metav1.ConditionTrue

	// Admission rejection is terminal and outranks whatever Programmed says,
	// the same precedence sharedutil.ZoneStatus uses.
	var causes []Cause
	if accepted != nil && accepted.Status != metav1.ConditionTrue {
		causes = append(causes, toCause(obj, *accepted, now))
	} else if programmed != nil && programmed.Status != metav1.ConditionTrue {
		causes = append(causes, toCause(obj, *programmed, now))
	}

	var root *Cause
	if len(causes) > 0 {
		root = &causes[0]
	}

	healthy := !obj.notReady
	d := Diagnosis{
		Zone:                   zone.Spec.DomainName,
		Healthy:                healthy,
		RootCause:              root,
		ContributingConditions: causes,
		NextSteps:              nextSteps(root),
	}
	d.Summary = summarizeZone(zone, healthy, root)
	if root != nil {
		d.SuggestedSkill = root.Skill
	}
	return d
}

// DiagnoseRecord explains why one owner name within a DNSRecordSet is not
// programmed.
//
// siblings is every DNSRecordSet in the zone, target's own set included. It
// exists so a Conflict can be told apart from an orphaned backend record: if
// no owner name anywhere in the zone resolves to the same identity as
// ownerName, nothing in the customer's project is holding this name, and the
// backend's rejection points at a record this diagnosis cannot see.
func DiagnoseRecord(zone *dnsv1alpha1.DNSZone, siblings []dnsv1alpha1.DNSRecordSet, target *dnsv1alpha1.DNSRecordSet, ownerName string) Diagnosis {
	return DiagnoseRecordAt(time.Now(), zone, siblings, target, ownerName)
}

// DiagnoseRecordAt is DiagnoseRecord with the clock supplied.
func DiagnoseRecordAt(now time.Time, zone *dnsv1alpha1.DNSZone, siblings []dnsv1alpha1.DNSRecordSet, target *dnsv1alpha1.DNSRecordSet, ownerName string) Diagnosis {
	var zoneDomain string
	if zone != nil {
		zoneDomain = zone.Spec.DomainName
	}

	obj := object{name: target.Name, created: target.CreationTimestamp}

	// A set the API server rejected never reaches the backend, so no per-name
	// condition will ever appear for it — the same precedence
	// sharedutil.RecordStatus uses.
	if accepted := apimeta.FindStatusCondition(target.Status.Conditions, sharedutil.CondAccepted); accepted != nil &&
		accepted.Status != metav1.ConditionTrue {
		obj.notReady = true
		cause := toCause(obj, *accepted, now)
		return recordDiagnosis(zoneDomain, ownerName, obj.notReady, []Cause{cause})
	}

	matches := sharedutil.OwnerConditions(target, ownerName, zoneDomain, sharedutil.CondProgrammed)
	if len(matches) == 0 {
		// No per-name status yet at all: pending, same as sharedutil.RecordStatus.
		obj.notReady = true
		cause := Cause{
			Object:        target.Name,
			ConditionType: sharedutil.CondProgrammed,
			Reason:        sharedutil.ReasonPending,
			Actionability: ActionabilityTransient,
			Explanation:   "Datum has not reported any status for this name yet.",
			Remediation:   remediationWait,
			Skill:         SkillRecordNotProgrammed,
		}
		return recordDiagnosis(zoneDomain, ownerName, obj.notReady, []Cause{cause})
	}

	var causes []Cause
	spellingsDisagree := false
	var firstReason string
	hasHealthySpelling := false
	for _, c := range matches {
		if c == nil {
			continue
		}
		if c.Status == metav1.ConditionTrue {
			hasHealthySpelling = true
			continue
		}
		obj.notReady = true
		cause := toCause(obj, *c, now)
		if cause.Reason == sharedutil.ReasonConflict {
			refineConflict(&cause, zoneDomain, ownerName, siblings, target)
		}
		if cause.Reason == sharedutil.ReasonPDNSError {
			refinePDNSError(&cause)
		}
		if firstReason == "" {
			firstReason = cause.Reason
		} else if firstReason != cause.Reason {
			spellingsDisagree = true
		}
		causes = append(causes, cause)
	}

	if len(causes) == 0 {
		// Every spelling matched is healthy.
		return recordDiagnosis(zoneDomain, ownerName, false, nil)
	}

	if hasHealthySpelling {
		spellingsDisagree = true
	}
	if spellingsDisagree {
		causes[0].note(
			"This name has more than one spelling in status with different results — check for a " +
				"duplicate entry under a different case or trailing dot; whichever was written last is " +
				"the one actually live.")
	}

	return recordDiagnosis(zoneDomain, ownerName, true, causes)
}

func recordDiagnosis(zoneDomain, ownerName string, notReady bool, causes []Cause) Diagnosis {
	var root *Cause
	if len(causes) > 0 {
		root = &causes[0]
	}
	d := Diagnosis{
		Zone:                   zoneDomain,
		OwnerName:              ownerName,
		Healthy:                !notReady,
		RootCause:              root,
		ContributingConditions: causes,
		NextSteps:              nextSteps(root),
	}
	d.Summary = summarizeRecord(ownerName, !notReady, root)
	if root != nil {
		d.SuggestedSkill = root.Skill
	}
	return d
}

// refineConflict distinguishes a genuine coexistence conflict from an
// orphaned backend record. The reason alone cannot tell the two apart — only
// checking whether anything else in the zone actually claims this name can.
func refineConflict(cause *Cause, zoneDomain, ownerName string, siblings []dnsv1alpha1.DNSRecordSet, target *dnsv1alpha1.DNSRecordSet) {
	want := sharedutil.QualifyOwner(ownerName, zoneDomain)
	for i := range siblings {
		rs := &siblings[i]
		if rs.Name == target.Name && rs.Namespace == target.Namespace {
			continue
		}
		for _, entry := range rs.Spec.Records {
			if sharedutil.QualifyOwner(entry.Name, zoneDomain) == want {
				// A genuine conflict: something else really does use this name.
				return
			}
		}
	}

	// Nothing in this project claims the name, yet the backend says it is
	// taken. The record actually holding it is not visible from here.
	cause.Pattern = PatternOrphanedBackendRecord
	cause.Actionability = ActionabilityPlatform
	cause.Explanation = "The DNS backend says this name is already taken, but nothing in your project " +
		"references it. The record actually holding it was left behind by something else and is not " +
		"visible from your project."
	cause.Remediation = "Raise this with Datum with the record's name and type — an operator needs to " +
		"find and remove the leftover entry on the backend. There is nothing you can change in your " +
		"project to clear it yourself."
	cause.Skill = SkillConflictingRecord
}

// refinePDNSError reclassifies a PDNSError by its message: one reason covers
// messages that are the customer's fault, transient backend trouble, and a
// zone still provisioning, and the reason alone cannot distinguish them.
func refinePDNSError(cause *Cause) {
	msg := strings.ToLower(cause.Message)
	switch {
	case strings.Contains(msg, "invalid character"):
		cause.Actionability = ActionabilityUser
		cause.Remediation = "Quote or escape the special characters the message names."
	case strings.Contains(msg, "outside the zone"):
		cause.Actionability = ActionabilityUser
		cause.Remediation = "Use a name that belongs to this zone, or create the record in the correct zone."
	case strings.Contains(msg, "rejected as invalid"):
		cause.Actionability = ActionabilityUser
		cause.Remediation = "Fix the record's type or value as the message describes, then try again."
	case strings.Contains(msg, "could not be found"):
		cause.Actionability = ActionabilityTransient
		cause.Remediation = remediationWait
	case strings.Contains(msg, "internal error"):
		cause.Actionability = ActionabilityTransient
		cause.Remediation = remediationWait
	default:
		cause.Actionability = ActionabilityTransient
		cause.Remediation = remediationWait
	}
}

func toCause(obj object, c metav1.Condition, now time.Time) Cause {
	cause := Cause{
		Object:             obj.name,
		ConditionType:      c.Type,
		Status:             string(c.Status),
		Reported:           c.Status == metav1.ConditionFalse,
		Reason:             c.Reason,
		Message:            c.Message,
		LastTransitionTime: transitionTime(c, obj.created),
	}

	var inState time.Duration
	if elapsed, ok := age(cause.LastTransitionTime, now); ok {
		inState = elapsed
		cause.InStateFor = humanDuration(elapsed)
	}

	var objectAge time.Duration
	if elapsed, ok := age(formatTime(obj.created), now); ok {
		objectAge = elapsed
		cause.ObjectAge = humanDuration(elapsed)
	}
	if obj.notReady {
		cause.FailingFor = humanDuration(maxDuration(inState, objectAge))
	}

	info, ok := ExplainReason(c.Reason)
	if !ok {
		cause.Explanation = fmt.Sprintf(
			"Datum reported %q, which this diagnosis has no explanation written for. Go by the status "+
				"message: it is the best account of what happened.", c.Reason)
		return cause
	}

	cause.Actionability = ActionabilityAt(info, cause.LastTransitionTime, now)
	cause.Explanation = info.Explanation
	if !cause.Reported {
		cause.Explanation = "Nobody has confirmed this — it is what the status claims, not what was " +
			"seen: " + info.Explanation
	}
	cause.Remediation = info.Remediation
	cause.Skill = info.Skill
	if cause.Actionability == ActionabilityStalled {
		cause.Remediation = fmt.Sprintf(
			"Stop waiting on this one. A step like this normally finishes within %s, and this one has "+
				"been sitting for %s. Take it to Datum, with the name (%s), the status code (%s), and "+
				"how long it has been stuck.",
			info.ExpectedWithin, cause.InStateFor, cause.Object, c.Reason)
	}
	if !cause.Reported {
		cause.note(fmt.Sprintf(
			"Datum has not reported back on this either way — not success, not failure (%s is still "+
				"unknown) — so %q says what is meant to be happening, not what anyone saw.",
			c.Type, c.Reason))
	}
	return cause
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func summarizeZone(z *dnsv1alpha1.DNSZone, healthy bool, root *Cause) string {
	if healthy {
		return fmt.Sprintf("Zone %s is accepted and programmed.", z.Spec.DomainName)
	}
	if root == nil {
		return fmt.Sprintf("Zone %s reports nothing wrong, but is not yet accepted and programmed.", z.Spec.DomainName)
	}
	return fmt.Sprintf("Zone %s is not ready: %s (%s on %s). %s",
		z.Spec.DomainName, actionabilitySentence(root), root.Reason, root.ConditionType, root.Explanation)
}

func summarizeRecord(ownerName string, healthy bool, root *Cause) string {
	if healthy {
		return fmt.Sprintf("%s is live in the DNS backend.", ownerName)
	}
	if root == nil {
		return fmt.Sprintf("%s reports nothing wrong, but is not yet live.", ownerName)
	}
	return fmt.Sprintf("%s is not programmed: %s (%s on %s). %s",
		ownerName, actionabilitySentence(root), root.Reason, root.ConditionType, root.Explanation)
}

func actionabilitySentence(root *Cause) string {
	switch root.Actionability {
	case ActionabilityUser:
		return "this is one you can fix yourself"
	case ActionabilityPlatform:
		return "this one is Datum's to fix, not yours — take it to them"
	case ActionabilityTransient:
		return "this is a normal step along the way and should clear on its own"
	case ActionabilityStalled:
		return "a step like this normally finishes quickly and this one has not, so treat it as stuck rather than in progress"
	default:
		return "who has to act on this one is not something Datum has classified"
	}
}

func nextSteps(root *Cause) []string {
	if root == nil {
		return nil
	}
	var steps []string
	if root.Remediation != "" {
		steps = append(steps, root.Remediation)
	}
	if root.Actionability == ActionabilityPlatform {
		steps = append(steps,
			"Do not change your zone or records to work around this one: the fault is on Datum's side, "+
				"and no edit on your side clears it.")
	}
	if root.Actionability == ActionabilityStalled {
		step := "Do not describe this as normal progress. The status still says the work is under way, " +
			"but it has taken far longer than that work should take."
		if root.Reported {
			step += " Datum did report this state, so quote its status message as what was reported."
		} else {
			step += " Nothing has reported a cause at all, so gather the details rather than naming a culprit."
		}
		steps = append(steps, step)
	}
	if root.Skill != "" {
		steps = append(steps, fmt.Sprintf("The %q runbook has the full procedure.", root.Skill))
	}
	return steps
}
