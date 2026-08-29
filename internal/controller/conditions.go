package controller

import (
	"fmt"
	"sort"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

const (
	CondAccepted   = "Accepted"
	CondProgrammed = "Programmed"
	CondDiscovered = "Discovered"

	ReasonAccepted                  = "Accepted"
	ReasonPending                   = "Pending"
	ReasonInvalidDNSRecordSet       = "InvalidDNSRecordSet"
	ReasonProgrammed                = "Programmed"
	ReasonDiscovered                = "Discovered"
	ReasonDNSZoneInUse              = "DNSZoneInUse"
	ReasonNotOwner                  = "NotOwner"
	ReasonPDNSError                 = "PDNSError"
	ReasonConflict                  = "Conflict"
	ReasonPendingDomainVerification = "PendingDomainVerification"
)

const (
	messageRecordsPending = "One or more records not yet programmed"

	maxBlockedRecordsInMessage = 3
)

func aggregateProgrammedStatus(statuses []dnsv1alpha1.RecordSetStatus) (bool, string, string) {
	ordered := make([]dnsv1alpha1.RecordSetStatus, len(statuses))
	copy(ordered, statuses)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })

	allProgrammed := true
	blockedReason := ""
	blockedDetails := make([]string, 0, len(ordered))

	for _, st := range ordered {
		cond := apimeta.FindStatusCondition(st.Conditions, CondProgrammed)
		if cond != nil && cond.Status == metav1.ConditionTrue {
			continue
		}
		allProgrammed = false
		if cond == nil || !namesBlockingCause(cond.Reason) {
			continue
		}
		if blockedReason == "" {
			blockedReason = cond.Reason
		}
		blockedDetails = append(blockedDetails, blockedRecordDetail(st.Name, cond))
	}

	if allProgrammed {
		return true, "", ""
	}
	if blockedReason == "" {
		return false, ReasonPending, messageRecordsPending
	}
	return false, blockedReason, joinBlockedDetails(blockedDetails)
}

func namesBlockingCause(reason string) bool {
	switch reason {
	case "", ReasonPending, ReasonProgrammed:
		return false
	default:
		return true
	}
}

func blockedRecordDetail(name string, cond *metav1.Condition) string {
	if cond.Message == "" {
		return fmt.Sprintf("%s: %s", name, cond.Reason)
	}
	return fmt.Sprintf("%s: %s", name, cond.Message)
}

func joinBlockedDetails(details []string) string {
	if len(details) <= maxBlockedRecordsInMessage {
		return strings.Join(details, "; ")
	}
	shown := strings.Join(details[:maxBlockedRecordsInMessage], "; ")
	return fmt.Sprintf("%s; and %d more", shown, len(details)-maxBlockedRecordsInMessage)
}
