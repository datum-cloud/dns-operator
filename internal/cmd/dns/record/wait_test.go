// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// fastPolling keeps --wait tests to milliseconds.
func fastPolling(t *testing.T) {
	t.Helper()
	originalInterval, originalTimeout := pollInterval, defaultWaitTimeout
	pollInterval, defaultWaitTimeout = time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { pollInterval, defaultWaitTimeout = originalInterval, originalTimeout })
}

// programmeAfter stamps the per-owner Programmed condition once the set has
// been listed n times, standing in for the controller.
func programmeAfter(n int, ownerName string, status metav1.ConditionStatus, reason, message string) interceptor.Funcs {
	var reads int
	return interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			sets, isRecordSets := list.(*dnsv1alpha1.DNSRecordSetList)
			if !isRecordSets {
				return nil
			}
			reads++
			if reads < n {
				return nil
			}
			for i := range sets.Items {
				withOwnerStatus(&sets.Items[i], ownerName, status, reason, message)
			}
			return nil
		},
	}
}

func TestWaitReportsProgrammed(t *testing.T) {
	fastPolling(t)
	ic := programmeAfter(2, "www", metav1.ConditionTrue, "Programmed", "")

	h := newHarnessWithInterceptor(t, &ic, testZone())
	requireNoError(t, h.run("record", "create", testDomain, "www", "A", "203.0.113.10", "--wait"))

	out := h.stdout()
	mustContain(t, out, "waiting for www.example.com A to be programmed...")
	mustContain(t, collapse(out), "www.example.com A Programmed")
}

// TestWaitSurfacesTheBackendsRefusal — a wait that ends in Conflict is a
// failure, and the exit code has to say so.
func TestWaitSurfacesTheBackendsRefusal(t *testing.T) {
	fastPolling(t)
	ic := programmeAfter(2, "www", metav1.ConditionFalse, "Conflict", "another record set owns this name")

	h := newHarnessWithInterceptor(t, &ic, testZone())
	ce := requireExit(t, h.run("record", "create", testDomain, "www", "A", "203.0.113.10", "--wait"), util.ExitConflict)

	mustContain(t, ce.Error(), "another record set owns this name")
	mustContain(t, ce.Fix(), "datumctl dns record describe example.com www A")
}

// TestWaitIsBounded — the write succeeded; the wait timing out must say so
// rather than implying the record was lost.
func TestWaitIsBounded(t *testing.T) {
	fastPolling(t)

	h := newHarness(t, testZone())
	ce := requireExit(t, h.run("record", "create", testDomain, "www", "A", "203.0.113.10",
		"--wait", "--timeout", "20ms"), util.ExitError)

	mustContain(t, ce.Error(), "timed out after 20ms waiting for www.example.com A to be programmed")
	mustContain(t, ce.Error(), "last status was Pending")
	mustContain(t, ce.Fix(), "the record was written")

	if got := len(h.getSet(t, "example-com-a").Spec.Records); got != 1 {
		t.Errorf("records = %d, want 1 — the write itself succeeded", got)
	}
}

func TestNoWaitReturnsImmediately(t *testing.T) {
	h := newHarness(t, testZone())
	requireNoError(t, h.run("record", "create", testDomain, "www", "A", "203.0.113.10"))
	mustNotContain(t, h.stdout(), "waiting for")
}

// spelledTwice is one logical owner name written two ways in one bucket.
// pdns.QualifyOwner collapses them onto a single RRset, but the controller keys
// status.recordSets[] off spec.records[].Name verbatim, so the set carries two
// status entries for what is really one record.
func spelledTwice() *dnsv1alpha1.DNSRecordSet {
	return recordSet(dnsv1alpha1.RRTypeA,
		aEntry("www", "203.0.113.10", ttl(300)),
		aEntry("www.example.com.", "203.0.113.11", ttl(300)),
	)
}

// TestStatusOfANameSpelledTwoWaysTakesTheWorst — reporting Programmed while the
// other spelling sits in Conflict is the exact failure --wait exists to prevent,
// and reading only the first spelling would do it half the time.
//
// The reduction lives in util now; these cases stay here because they are what
// this package depends on, and a fold that regressed to first-match would pass
// util's own tests for a single-spelling record while breaking --wait.
func TestStatusOfANameSpelledTwoWaysTakesTheWorst(t *testing.T) {
	tests := []struct {
		name        string
		firstReason string
		firstStatus metav1.ConditionStatus
		wantWord    string
	}{
		{
			name:        "the failing spelling is second",
			firstStatus: metav1.ConditionTrue,
			firstReason: "Programmed",
			wantWord:    util.StatusConflict,
		},
		{
			name:        "the failing spelling is first",
			firstStatus: metav1.ConditionFalse,
			firstReason: "Conflict",
			wantWord:    util.StatusConflict,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rs := spelledTwice()
			if tc.firstReason == "Programmed" {
				withOwnerStatus(rs, "www", metav1.ConditionTrue, "Programmed", "")
				withOwnerStatus(rs, "www.example.com.", metav1.ConditionFalse, "Conflict", "the backend reported a conflict")
			} else {
				withOwnerStatus(rs, "www", metav1.ConditionFalse, "Conflict", "the backend reported a conflict")
				withOwnerStatus(rs, "www.example.com.", metav1.ConditionTrue, "Programmed", "")
			}

			got, _ := util.RecordStatusInZone(rs, "www", testDomain)
			if got != tc.wantWord {
				t.Errorf("status = %q, want %q", got, tc.wantWord)
			}
		})
	}
}

// TestBothSpellingsProgrammedIsProgrammed.
func TestBothSpellingsProgrammedIsProgrammed(t *testing.T) {
	rs := spelledTwice()
	withOwnerStatus(rs, "www", metav1.ConditionTrue, "Programmed", "")
	withOwnerStatus(rs, "www.example.com.", metav1.ConditionTrue, "Programmed", "")

	if got, _ := util.RecordStatusInZone(rs, "www", testDomain); got != util.StatusProgrammed {
		t.Errorf("status = %q, want Programmed", got)
	}
}

// TestWaitDoesNotSucceedOnAHalfProgrammedName.
func TestWaitDoesNotSucceedOnAHalfProgrammedName(t *testing.T) {
	fastPolling(t)

	rs := spelledTwice()
	withOwnerStatus(rs, "www", metav1.ConditionTrue, "Programmed", "")
	withOwnerStatus(rs, "www.example.com.", metav1.ConditionFalse, "Conflict", "the backend reported a conflict")

	h := newHarness(t, testZone(), rs)
	ce := requireExit(t, h.run("record", "create", testDomain, "www", "A", "203.0.113.12", "--wait"), util.ExitConflict)
	mustContain(t, ce.Error(), "the backend reported a conflict")
}

// TestStatusResolvesSpellingsThroughUtil — the apex written as "@" in one
// place and "example.com." in another is one record, and util's zone-aware
// lookup is what makes them meet.
func TestStatusResolvesSpellingsThroughUtil(t *testing.T) {
	rs := recordSet(dnsv1alpha1.RRTypeMX, dnsv1alpha1.RecordEntry{
		Name: "@", TTL: ttl(300),
		MX: &dnsv1alpha1.MXRecordSpec{Preference: 10, Exchange: "mail.example.com."},
	})
	withOwnerStatus(rs, "example.com.", metav1.ConditionTrue, "Programmed", "")

	if got, _ := util.RecordStatusInZone(rs, "@", testDomain); got != util.StatusProgrammed {
		t.Errorf("status = %q, want Programmed — \"@\" and \"example.com.\" are one owner", got)
	}
}

// TestStatusWithNoStatusAtAllIsPending.
func TestStatusWithNoStatusAtAllIsPending(t *testing.T) {
	rs := recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", nil))
	if got, _ := util.RecordStatusInZone(rs, "www", testDomain); got != util.StatusPending {
		t.Errorf("status = %q, want Pending", got)
	}
}

// TestStatusHonoursAcceptedFalse — a rejected set outranks its per-name
// entries, however many spellings those entries use.
func TestStatusHonoursAcceptedFalse(t *testing.T) {
	rs := spelledTwice()
	withOwnerStatus(rs, "www", metav1.ConditionTrue, "Programmed", "")
	withOwnerStatus(rs, "www.example.com.", metav1.ConditionTrue, "Programmed", "")
	withAcceptedFalse(rs, "spec.records[0] is invalid")

	got, detail := util.RecordStatusInZone(rs, "www", testDomain)
	if got != util.StatusRejected {
		t.Errorf("status = %q, want Rejected", got)
	}
	if detail != "spec.records[0] is invalid" {
		t.Errorf("detail = %q, want the server's message verbatim", detail)
	}
}
