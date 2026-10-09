// SPDX-License-Identifier: AGPL-3.0-only

package projectstatus

import (
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestProjectionIsQuiescentAfterConvergence(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var conditions []metav1.Condition
	if !Published(&conditions, true, 7, now) || !Programmed(&conditions, true, 7, now) || !Available(&conditions, false, 7, now) {
		t.Fatal("initial projection did not report changes")
	}
	converged := append([]metav1.Condition(nil), conditions...)
	if Published(&conditions, true, 7, now.Add(time.Minute)) || Programmed(&conditions, true, 7, now.Add(time.Minute)) || Available(&conditions, false, 7, now.Add(time.Minute)) {
		t.Fatal("unchanged projection reported a status mutation")
	}
	if !reflect.DeepEqual(conditions, converged) {
		t.Fatalf("unchanged projection churned conditions:\n%#v\n%#v", converged, conditions)
	}
}
