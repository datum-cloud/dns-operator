// SPDX-License-Identifier: AGPL-3.0-only

// Package projectstatus defines the single project-facing DNS publication
// projection used by both the source reconciler and the acknowledgement sink.
package projectstatus

import (
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Published(conditions *[]metav1.Condition, complete bool, generation int64, now time.Time) bool {
	reason, message := "PendingReplicaVerification", "publication awaits required replica verification"
	if complete {
		reason, message = "Published", "required serving replicas verified the publication"
	}
	return set(conditions, "Published", complete, reason, message, generation, now)
}

func Programmed(conditions *[]metav1.Condition, applied bool, generation int64, now time.Time) bool {
	reason, message := "PendingReplicaApplication", "publication awaits serving replica application"
	if applied {
		reason, message = "Programmed", "at least one serving replica applied the publication"
	}
	return set(conditions, "Programmed", applied, reason, message, generation, now)
}

func Available(conditions *[]metav1.Condition, available bool, generation int64, now time.Time) bool {
	reason, message := "NoEligibleEndpoints", "publication contains no eligible endpoint"
	if available {
		reason, message = "Available", "publication contains an eligible endpoint"
	}
	return set(conditions, "Available", available, reason, message, generation, now)
}

func set(conditions *[]metav1.Condition, conditionType string, value bool, reason, message string, generation int64, now time.Time) bool {
	status := metav1.ConditionFalse
	if value {
		status = metav1.ConditionTrue
	}
	return apimeta.SetStatusCondition(conditions, metav1.Condition{Type: conditionType, Status: status, Reason: reason, Message: message, ObservedGeneration: generation, LastTransitionTime: metav1.NewTime(now)})
}
