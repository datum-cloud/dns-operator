// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"strings"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	sharedutil "go.miloapis.com/dns-operator/internal/dns/util"
)

func TestDiagnoseZone(t *testing.T) {
	tests := map[string]struct {
		zone              *dnsv1alpha1.DNSZone
		wantHealthy       bool
		wantReason        string
		wantActionability Actionability
	}{
		"healthy": {
			zone: zone("example.com",
				cond(sharedutil.CondAccepted, "True", sharedutil.ReasonAccepted, "ok"),
				cond(sharedutil.CondProgrammed, "True", sharedutil.ReasonProgrammed, "ok")),
			wantHealthy: true,
		},
		"rejected outranks pending programmed": {
			zone: zone("example.com",
				cond(sharedutil.CondAccepted, "False", sharedutil.ReasonInvalidDNSRecordSet, "bad zone"),
				cond(sharedutil.CondProgrammed, "False", sharedutil.ReasonPending, "waiting")),
			wantHealthy:       false,
			wantReason:        sharedutil.ReasonInvalidDNSRecordSet,
			wantActionability: ActionabilityUser,
		},
		"pending is transient": {
			zone: zone("example.com",
				cond(sharedutil.CondAccepted, "True", sharedutil.ReasonAccepted, "ok"),
				cond(sharedutil.CondProgrammed, "False", sharedutil.ReasonPending, "waiting")),
			wantHealthy:       false,
			wantReason:        sharedutil.ReasonPending,
			wantActionability: ActionabilityTransient,
		},
		"platform fault": {
			zone: zone("example.com",
				cond(sharedutil.CondAccepted, "False", sharedutil.ReasonDNSZoneInUse, "already claimed")),
			wantHealthy:       false,
			wantReason:        sharedutil.ReasonDNSZoneInUse,
			wantActionability: ActionabilityPlatform,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			d := DiagnoseZoneAt(stagingNow, tt.zone)
			if d.Healthy != tt.wantHealthy {
				t.Errorf("Healthy = %v, want %v", d.Healthy, tt.wantHealthy)
			}
			if tt.wantReason == "" {
				return
			}
			if d.RootCause == nil {
				t.Fatal("RootCause is nil, want a cause")
			}
			if d.RootCause.Reason != tt.wantReason {
				t.Errorf("RootCause.Reason = %q, want %q", d.RootCause.Reason, tt.wantReason)
			}
			if d.RootCause.Actionability != tt.wantActionability {
				t.Errorf("RootCause.Actionability = %s, want %s", d.RootCause.Actionability, tt.wantActionability)
			}
		})
	}
}

func TestDiagnoseZoneStalledPending(t *testing.T) {
	z := zone("example.com",
		cond(sharedutil.CondAccepted, "True", sharedutil.ReasonAccepted, "ok"),
		condAt(sharedutil.CondProgrammed, "False", sharedutil.ReasonPending, "waiting",
			stagingNow.Add(-time.Hour)))
	d := DiagnoseZoneAt(stagingNow, z)
	if d.RootCause == nil || d.RootCause.Actionability != ActionabilityStalled {
		t.Fatalf("RootCause = %+v, want ActionabilityStalled", d.RootCause)
	}
}

func TestDiagnoseRecordRejected(t *testing.T) {
	z := zone("example.com", cond(sharedutil.CondAccepted, "True", sharedutil.ReasonAccepted, "ok"),
		cond(sharedutil.CondProgrammed, "True", sharedutil.ReasonProgrammed, "ok"))
	rs := rejected("www", cond(sharedutil.CondAccepted, "False", sharedutil.ReasonInvalidDNSRecordSet, "bad shape"))

	d := DiagnoseRecordAt(stagingNow, z, []dnsv1alpha1.DNSRecordSet{*rs}, rs, "www")
	if d.Healthy {
		t.Fatal("Healthy = true, want false for a rejected record set")
	}
	if d.RootCause == nil || d.RootCause.Reason != sharedutil.ReasonInvalidDNSRecordSet {
		t.Fatalf("RootCause = %+v, want InvalidDNSRecordSet", d.RootCause)
	}
}

func TestDiagnoseRecordNoStatusYet(t *testing.T) {
	z := zone("example.com")
	rs := recordSet("www", []dnsv1alpha1.RecordEntry{{Name: "www"}})

	d := DiagnoseRecordAt(stagingNow, z, []dnsv1alpha1.DNSRecordSet{*rs}, rs, "www")
	if d.Healthy {
		t.Fatal("Healthy = true, want false when nothing has reported yet")
	}
	if d.RootCause == nil || d.RootCause.Reason != sharedutil.ReasonPending {
		t.Fatalf("RootCause = %+v, want Pending", d.RootCause)
	}
}

func TestDiagnoseRecordNotOwner(t *testing.T) {
	z := zone("example.com")
	rs := recordSet("www", []dnsv1alpha1.RecordEntry{{Name: "www"}},
		ownerStatus("www.example.com.", cond(sharedutil.CondProgrammed, "False", sharedutil.ReasonNotOwner, "taken")))

	d := DiagnoseRecordAt(stagingNow, z, []dnsv1alpha1.DNSRecordSet{*rs}, rs, "www")
	if d.RootCause == nil || d.RootCause.Reason != sharedutil.ReasonNotOwner {
		t.Fatalf("RootCause = %+v, want NotOwner", d.RootCause)
	}
	if d.RootCause.Actionability != ActionabilityUser {
		t.Errorf("Actionability = %s, want user; NotOwner never self-clears", d.RootCause.Actionability)
	}
}

func TestDiagnoseRecordConflictGenuine(t *testing.T) {
	z := zone("example.com")
	target := recordSet("www-cname", []dnsv1alpha1.RecordEntry{{Name: "www"}},
		ownerStatus("www.example.com.", cond(sharedutil.CondProgrammed, "False", sharedutil.ReasonConflict,
			"A conflicting record already exists for this name. Remove the existing record and try again.")))
	sibling := recordSet("www-a", []dnsv1alpha1.RecordEntry{{Name: "www"}})

	d := DiagnoseRecordAt(stagingNow, z, []dnsv1alpha1.DNSRecordSet{*target, *sibling}, target, "www")
	if d.RootCause == nil || d.RootCause.Reason != sharedutil.ReasonConflict {
		t.Fatalf("RootCause = %+v, want Conflict", d.RootCause)
	}
	if d.RootCause.Actionability != ActionabilityUser {
		t.Errorf("Actionability = %s, want user: a sibling record set genuinely claims this name", d.RootCause.Actionability)
	}
	if d.RootCause.Pattern != "" {
		t.Errorf("Pattern = %q, want empty for a genuine conflict", d.RootCause.Pattern)
	}
}

func TestDiagnoseRecordConflictOrphaned(t *testing.T) {
	z := zone("example.com")
	target := recordSet("www-cname", []dnsv1alpha1.RecordEntry{{Name: "www"}},
		ownerStatus("www.example.com.", cond(sharedutil.CondProgrammed, "False", sharedutil.ReasonConflict,
			"A conflicting record already exists for this name. Remove the existing record and try again.")))

	// No sibling in the project claims "www" at all.
	d := DiagnoseRecordAt(stagingNow, z, []dnsv1alpha1.DNSRecordSet{*target}, target, "www")
	if d.RootCause == nil || d.RootCause.Reason != sharedutil.ReasonConflict {
		t.Fatalf("RootCause = %+v, want Conflict", d.RootCause)
	}
	if d.RootCause.Actionability != ActionabilityPlatform {
		t.Errorf("Actionability = %s, want platform: nothing upstream claims this name", d.RootCause.Actionability)
	}
	if d.RootCause.Pattern != PatternOrphanedBackendRecord {
		t.Errorf("Pattern = %q, want %q", d.RootCause.Pattern, PatternOrphanedBackendRecord)
	}
}

func TestDiagnoseRecordPDNSErrorClassification(t *testing.T) {
	tests := map[string]struct {
		message string
		want    Actionability
	}{
		"invalid character":       {"The record content contains an invalid character. TXT records containing semicolons or special characters must be properly quoted.", ActionabilityUser},
		"outside the zone":        {"The record name is outside the zone. Check that the name belongs to this DNS zone.", ActionabilityUser},
		"rejected as invalid":     {"The DNS record was rejected as invalid: bad value.", ActionabilityUser},
		"zone still provisioning": {"The DNS zone could not be found. It may still be provisioning.", ActionabilityTransient},
		"internal error":          {"An internal error occurred while applying the record. It will be retried automatically.", ActionabilityTransient},
		"generic fallback":        {"Failed to apply DNS record. It will be retried automatically.", ActionabilityTransient},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			z := zone("example.com")
			rs := recordSet("www", []dnsv1alpha1.RecordEntry{{Name: "www"}},
				ownerStatus("www.example.com.", cond(sharedutil.CondProgrammed, "False", sharedutil.ReasonPDNSError, tt.message)))

			d := DiagnoseRecordAt(stagingNow, z, []dnsv1alpha1.DNSRecordSet{*rs}, rs, "www")
			if d.RootCause == nil {
				t.Fatal("RootCause is nil")
			}
			if d.RootCause.Actionability != tt.want {
				t.Errorf("message %q: Actionability = %s, want %s", tt.message, d.RootCause.Actionability, tt.want)
			}
		})
	}
}

func TestDiagnoseRecordSpellingsDisagree(t *testing.T) {
	z := zone("example.com")
	rs := recordSet("www", []dnsv1alpha1.RecordEntry{{Name: "www"}},
		ownerStatus("www.example.com.", cond(sharedutil.CondProgrammed, "True", sharedutil.ReasonProgrammed, "live")),
		ownerStatus("WWW.example.com.", cond(sharedutil.CondProgrammed, "False", sharedutil.ReasonConflict, "conflict")))

	d := DiagnoseRecordAt(stagingNow, z, []dnsv1alpha1.DNSRecordSet{*rs}, rs, "www")
	if d.RootCause == nil {
		t.Fatal("RootCause is nil, want the failing spelling reported")
	}
	if !strings.Contains(d.RootCause.Explanation, "more than one spelling") {
		t.Errorf("Explanation = %q, want it to flag the disagreeing spellings", d.RootCause.Explanation)
	}
}

func TestDiagnoseRecordUncatalogued(t *testing.T) {
	z := zone("example.com")
	rs := recordSet("www", []dnsv1alpha1.RecordEntry{{Name: "www"}},
		ownerStatus("www.example.com.", cond(sharedutil.CondProgrammed, "False", "SomeBrandNewReason", "Something new.")))

	d := DiagnoseRecordAt(stagingNow, z, []dnsv1alpha1.DNSRecordSet{*rs}, rs, "www")
	if d.RootCause == nil {
		t.Fatal("RootCause is nil")
	}
	if !strings.Contains(d.RootCause.Explanation, "SomeBrandNewReason") {
		t.Errorf("Explanation = %q, want it to name the uncatalogued reason", d.RootCause.Explanation)
	}
	if d.RootCause.Actionability != "" {
		t.Errorf("Actionability = %s, want empty for an uncatalogued reason", d.RootCause.Actionability)
	}
}

func TestDiagnoseRecordEpochSentinelDiscarded(t *testing.T) {
	z := zoneCreated("example.com", stagingNow.Add(-24*time.Hour))
	rs := recordSetCreated("www", stagingNow.Add(-24*time.Hour), []dnsv1alpha1.RecordEntry{{Name: "www"}},
		ownerStatus("www.example.com.", epochCond(sharedutil.CondProgrammed, "False", sharedutil.ReasonPending, "waiting")))

	d := DiagnoseRecordAt(stagingNow, z, []dnsv1alpha1.DNSRecordSet{*rs}, rs, "www")
	if d.RootCause == nil {
		t.Fatal("RootCause is nil")
	}
	if d.RootCause.LastTransitionTime != "" {
		t.Errorf("LastTransitionTime = %q, want empty: the epoch sentinel must not be believed", d.RootCause.LastTransitionTime)
	}
	if d.RootCause.InStateFor != "" {
		t.Errorf("InStateFor = %q, want empty when the timestamp was discarded", d.RootCause.InStateFor)
	}
}
