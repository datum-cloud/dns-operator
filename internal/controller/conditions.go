package controller

import (
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	CondAccepted       = "Accepted"
	CondProgrammed     = "Programmed"
	ReasonAccepted     = "Accepted"
	ReasonPending      = "Pending"
	ReasonProgrammed   = "Programmed"
	ReasonDNSZoneInUse = "DNSZoneInUse"
)

// set/update one condition; returns true if it changed
func setCond(conds *[]metav1.Condition, t, reason, msg string, status metav1.ConditionStatus, gen int64) bool {
	c := metav1.Condition{
		Type:               t,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: gen,
		LastTransitionTime: metav1.NewTime(time.Now()),
	}
	old := apimeta.FindStatusCondition(*conds, t)
	if old != nil &&
		old.Status == c.Status &&
		old.Reason == c.Reason &&
		old.Message == c.Message &&
		old.ObservedGeneration == c.ObservedGeneration {
		return false
	}
	apimeta.SetStatusCondition(conds, c)
	return true
}

// getCondStatus returns the condition status for type t, or Unknown when not present.
func getCondStatus(conds []metav1.Condition, t string) metav1.ConditionStatus {
	c := apimeta.FindStatusCondition(conds, t)
	if c == nil {
		return metav1.ConditionUnknown
	}
	return c.Status
}
