// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"testing"
	"time"

	sharedutil "go.miloapis.com/dns-operator/internal/dns/util"
)

// TestCatalogCoversEveryKnownReason pins the catalog against the reason
// vocabulary internal/dns/util duplicates from the controller, so a new
// reason cannot go unclassified without a test failing here.
func TestCatalogCoversEveryKnownReason(t *testing.T) {
	want := []string{
		sharedutil.ReasonAccepted,
		sharedutil.ReasonPending,
		sharedutil.ReasonInvalidDNSRecordSet,
		sharedutil.ReasonProgrammed,
		sharedutil.ReasonDiscovered,
		sharedutil.ReasonDNSZoneInUse,
		sharedutil.ReasonNotOwner,
		sharedutil.ReasonPDNSError,
		sharedutil.ReasonConflict,
		sharedutil.ReasonPendingDomainVerification,
	}
	for _, reason := range want {
		if _, ok := ExplainReason(reason); !ok {
			t.Errorf("catalog has no entry for reason %q", reason)
		}
	}
}

// TestTransientReasonsThatSayWaitDeclareAWindow pins that "wait" is always a
// falsifiable claim: any entry telling the customer this clears on its own
// declares how long that should take, so ActionabilityAt has something to
// escalate against.
func TestTransientReasonsThatSayWaitDeclareAWindow(t *testing.T) {
	for _, info := range AllReasons() {
		if info.Remediation != remediationWait {
			continue
		}
		if info.ExpectedDuration <= 0 {
			t.Errorf("%s: remediation says %q but declares no ExpectedDuration", info.Reason, remediationWait)
		}
	}
}

func TestActionabilityAtEscalatesPastTheWindow(t *testing.T) {
	info, ok := ExplainReason(sharedutil.ReasonPending)
	if !ok {
		t.Fatal("catalog has no entry for Pending")
	}

	fresh := stagingNow.Add(-1 * time.Minute).Format(time.RFC3339)
	if got := ActionabilityAt(info, fresh, stagingNow); got != ActionabilityTransient {
		t.Errorf("fresh Pending: got %s, want transient", got)
	}

	stale := stagingNow.Add(-1 * time.Hour).Format(time.RFC3339)
	if got := ActionabilityAt(info, stale, stagingNow); got != ActionabilityStalled {
		t.Errorf("stale Pending: got %s, want stalled", got)
	}

	if got := ActionabilityAt(info, "", stagingNow); got != ActionabilityTransient {
		t.Errorf("missing timestamp: got %s, want the static classification (transient), not an escalation", got)
	}
}
