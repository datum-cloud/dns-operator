package controlplane

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestCompileDeterministicMultiVPCAggregation(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	in := fixture(now)
	first, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	in.Associations[0], in.Associations[1] = in.Associations[1], in.Associations[0]
	in.Contributions = append([]dnsv1alpha1.DNSRecordContribution(nil), in.Contributions...)
	second, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(first.Plan)
	b, _ := json.Marshal(second.Plan)
	if string(a) != string(b) {
		t.Fatalf("plan depends on input order:\n%s\n%s", a, b)
	}
	if len(first.Plan.ContextUIDs) != 2 || first.Plan.ContextUIDs[0] != "vpc-a" || first.Plan.ContextUIDs[1] != "vpc-b" {
		t.Fatalf("unexpected VPCs: %v", first.Plan.ContextUIDs)
	}
	wire, err := WirePlan(first.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire.RRSets) != 1 || len(wire.RRSets[0].Records) != 1 {
		t.Fatalf("unexpected wire plan: %#v", wire)
	}
	record := wire.RRSets[0].Records[0]
	if record.ContributionUID != "contribution-a" || record.WriterEpoch != 7 || record.Sequence != 18 || !record.Eligible {
		t.Fatalf("missing contribution fence: %#v", record)
	}
}

func TestCompileRejectsStaleGenerationAndWholeInvalidBundle(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	t.Run("stale registration", func(t *testing.T) {
		in := fixture(now)
		in.Contributions[0].Spec.RegistrationRef.Generation--
		got, err := Compile(in)
		if err != nil {
			t.Fatal(err)
		}
		if got.Reasons["contribution-a"] != "StaleRegistrationGeneration" || len(got.Plan.RRsets) != 0 {
			t.Fatalf("stale contribution published: %#v", got)
		}
	})
	t.Run("bundle atomic", func(t *testing.T) {
		in := fixture(now)
		in.Registrations[0].Spec.ReservedDescendants = []string{"targets"}
		in.Contributions[0].Spec.RecordSets[0].Records = append(in.Contributions[0].Spec.RecordSets[0].Records, dnsv1alpha1.RecordEntry{Name: "other", AAAA: &dnsv1alpha1.AAAARecordSpec{Content: "fd00::2"}})
		got, err := Compile(in)
		if err != nil {
			t.Fatal(err)
		}
		if got.Reasons["contribution-a"] != "NameOrTypeOutsideGrant" || len(got.Plan.RRsets) != 0 {
			t.Fatalf("partial invalid bundle published: %#v", got.Plan.RRsets)
		}
		if len(got.Plan.Contributions) != 1 || got.Plan.Contributions[0].Sequence != 18 {
			t.Fatalf("authenticated rejected observation did not advance fence: %#v", got.Plan.Contributions)
		}
	})
}

func TestCompileExpiryStillAdvancesHighWaterAndRejectsRollback(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	in := fixture(now)
	previous := in.Contributions[0]
	in.Previous = &PublicationPlan{Contributions: []ContributionFence{{UID: previous.UID, GrantUID: previous.Spec.GrantRef.UID, Epoch: 7, Sequence: 18, ValidUntil: now}}}
	in.Contributions[0].Status.Sequence = 19
	expired := metav1.NewTime(now.Add(-time.Second))
	in.Contributions[0].Status.ValidUntil = &expired
	got, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reasons[previous.UID] != "Expired" || len(got.Plan.RRsets) != 0 || got.Plan.Contributions[0].Sequence != 19 {
		t.Fatalf("expiry fence incorrect: %#v", got)
	}
	in.Previous = &got.Plan
	in.Contributions[0].Status.Sequence = 18
	fresh := metav1.NewTime(now.Add(time.Minute))
	in.Contributions[0].Status.ValidUntil = &fresh
	rolled, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	if rolled.Reasons[previous.UID] != "NonIncreasingSequence" || len(rolled.Plan.RRsets) != 0 {
		t.Fatalf("sequence rollback resurrected records: %#v", rolled)
	}
}

func TestCompileRetainsHighWaterAcrossRejectedPublications(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	firstInput := fixture(now)
	first, err := Compile(firstInput)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Plan.Contributions) != 1 || first.Plan.Contributions[0].Sequence != 18 {
		t.Fatalf("initial high-water missing: %#v", first.Plan.Contributions)
	}

	rollbackInput := fixture(now)
	rollbackInput.Previous = &first.Plan
	rollbackInput.Contributions[0].Status.Sequence = 17
	rollback, err := Compile(rollbackInput)
	if err != nil {
		t.Fatal(err)
	}
	if rollback.Reasons["contribution-a"] != "NonIncreasingSequence" || len(rollback.Plan.RRsets) != 0 {
		t.Fatalf("rollback was not rejected: %#v", rollback)
	}
	if len(rollback.Plan.Contributions) != 1 || rollback.Plan.Contributions[0].Sequence != 18 {
		t.Fatalf("rejected publication erased high-water: %#v", rollback.Plan.Contributions)
	}

	retryInput := fixture(now)
	retryInput.Previous = &rollback.Plan
	retryInput.Contributions[0].Status.Sequence = 17
	retry, err := Compile(retryInput)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Reasons["contribution-a"] != "NonIncreasingSequence" || len(retry.Plan.RRsets) != 0 || retry.Plan.Contributions[0].Sequence != 18 {
		t.Fatalf("second reconcile resurrected rolled-back data: %#v", retry)
	}
}

func TestCompileRejectsEqualSequenceDeadlineMutation(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	in := fixture(now)
	first, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	in.Previous = &first.Plan
	changed := metav1.NewTime(in.Contributions[0].Status.ValidUntil.Add(time.Minute))
	in.Contributions[0].Status.ValidUntil = &changed
	got, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reasons["contribution-a"] != "FenceDeadlineMismatch" || len(got.Plan.RRsets) != 0 {
		t.Fatalf("equal sequence changed its immutable deadline: %#v", got)
	}
	if len(got.Plan.Contributions) != 1 || !got.Plan.Contributions[0].ValidUntil.Equal(first.Plan.Contributions[0].ValidUntil) {
		t.Fatalf("deadline mutation replaced durable fence: %#v", got.Plan.Contributions)
	}
}

func TestCompileNormalizesFQDNAndEnforcesCNAMEExclusivity(t *testing.T) {
	in := fixture(time.Now().UTC())
	in.StaticRecords = []dnsv1alpha1.DNSRecordSet{{
		ObjectMeta: metav1.ObjectMeta{Name: "static", UID: "static-a"},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: structLocal("private"), RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{{Name: "api.corp.internal", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.1"}}},
		},
	}}
	in.Registrations[0].Spec.Name = "api"
	in.Registrations[0].Spec.RecordTypes = []dnsv1alpha1.RRType{dnsv1alpha1.RRTypeCNAME}
	got, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reasons["registration-a"] != "NameConflict" {
		t.Fatalf("CNAME coexistence accepted: %#v", got.Reasons)
	}
}

func TestCompileSRVTargetUsesAbsoluteZoneIdentity(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	in := fixture(now)
	in.Registrations[0].Spec.Name = "_grpc._tcp.svc"
	in.Registrations[0].Spec.ReservedDescendants = []string{"vm1.instances"}
	in.Registrations[0].Spec.RecordTypes = []dnsv1alpha1.RRType{dnsv1alpha1.RRTypeAAAA, dnsv1alpha1.RRTypeSRV}
	in.Grants[0].Spec.RecordTypes = []dnsv1alpha1.RRType{dnsv1alpha1.RRTypeAAAA, dnsv1alpha1.RRTypeSRV}
	in.Contributions[0].Spec.RecordSets = []dnsv1alpha1.DNSContributionRecordSet{
		{RecordType: dnsv1alpha1.RRTypeAAAA, Records: []dnsv1alpha1.RecordEntry{{Name: "vm1.instances", AAAA: &dnsv1alpha1.AAAARecordSpec{Content: "fd00::1"}}}},
		{RecordType: dnsv1alpha1.RRTypeSRV, Records: []dnsv1alpha1.RecordEntry{{Name: "_grpc._tcp.svc", SRV: &dnsv1alpha1.SRVRecordSpec{Priority: 10, Weight: 20, Port: 8443, Target: "vm1.instances.corp.internal."}}}},
	}
	got, err := Compile(in)
	if err != nil {
		t.Fatalf("absolute in-zone SRV target rejected: %v", err)
	}
	if len(got.Plan.RRsets) != 2 {
		t.Fatalf("SRV bundle was not published: %#v", got)
	}

	in.Contributions[0].Spec.RecordSets = in.Contributions[0].Spec.RecordSets[1:]
	if _, err := Compile(in); err == nil || !strings.Contains(err.Error(), "has no address") {
		t.Fatalf("missing SRV target address was not rejected: %v", err)
	}
}

func fixture(now time.Time) CompileInput {
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "private", UID: "zone-a", Generation: 2}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "corp.internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	assoc := func(name string, vpc types.UID) dnsv1alpha1.DNSZoneAssociation {
		return dnsv1alpha1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name)}, Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: zone.Name, UID: zone.UID}, ResolverContextRef: dnsv1alpha1.DNSObjectReference{Name: name, UID: vpc}}, Status: dnsv1alpha1.DNSZoneAssociationStatus{ResolvedDNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: zone.Name, UID: zone.UID, Generation: zone.Generation}, ResolvedResolverContextRef: dnsv1alpha1.DNSObjectReference{Name: name, UID: vpc}, Conditions: []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue}}}}
	}
	reg := dnsv1alpha1.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: "api", UID: "registration-a", Generation: 3}, Spec: dnsv1alpha1.DNSRegistrationSpec{DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: zone.Name, UID: zone.UID, Generation: zone.Generation}, Name: "api", RecordTypes: []dnsv1alpha1.RRType{dnsv1alpha1.RRTypeAAAA}, PublicationPolicy: dnsv1alpha1.DNSPublicationPolicyEligibleContributions, TTLSeconds: 30}}
	grant := dnsv1alpha1.DNSContributionGrant{ObjectMeta: metav1.ObjectMeta{Name: "compute", UID: "grant-a", Generation: 2}, Spec: dnsv1alpha1.DNSContributionGrantSpec{RegistrationRef: dnsv1alpha1.DNSObjectReference{Name: reg.Name, UID: reg.UID, Generation: reg.Generation}, ProducerID: "compute-east", RecordTypes: []dnsv1alpha1.RRType{dnsv1alpha1.RRTypeAAAA}}, Status: dnsv1alpha1.DNSContributionGrantStatus{ActiveWriterEpoch: 7, ObservedGrantGeneration: 2, ObservedRegistrationGeneration: 3}}
	until := metav1.NewTime(now.Add(time.Minute))
	c := dnsv1alpha1.DNSRecordContribution{ObjectMeta: metav1.ObjectMeta{Name: "endpoint-a", UID: "contribution-a", Generation: 4}, Spec: dnsv1alpha1.DNSRecordContributionSpec{RegistrationRef: dnsv1alpha1.DNSObjectReference{Name: reg.Name, UID: reg.UID, Generation: reg.Generation}, GrantRef: dnsv1alpha1.DNSObjectReference{Name: grant.Name, UID: grant.UID}, RecordSets: []dnsv1alpha1.DNSContributionRecordSet{{RecordType: dnsv1alpha1.RRTypeAAAA, Records: []dnsv1alpha1.RecordEntry{{Name: "api.corp.internal", AAAA: &dnsv1alpha1.AAAARecordSpec{Content: "fd00::1"}}}}}}, Status: dnsv1alpha1.DNSRecordContributionStatus{ObservedGeneration: 4, WriterEpoch: 7, Sequence: 18, Eligible: true, ValidUntil: &until}}
	return CompileInput{Zone: zone, Associations: []dnsv1alpha1.DNSZoneAssociation{assoc("b", "vpc-b"), assoc("a", "vpc-a")}, Registrations: []dnsv1alpha1.DNSRegistration{reg}, Grants: []dnsv1alpha1.DNSContributionGrant{grant}, Contributions: []dnsv1alpha1.DNSRecordContribution{c}, Now: now}
}

func structLocal(name string) corev1.LocalObjectReference {
	return corev1.LocalObjectReference{Name: name}
}

func TestCompileRejectsPrivateAuthorityOverridesWithoutBlockingHealthyRecords(t *testing.T) {
	for _, tt := range []struct {
		name       string
		recordType dnsv1alpha1.RRType
		entry      dnsv1alpha1.RecordEntry
	}{
		{"SOA", dnsv1alpha1.RRTypeSOA, dnsv1alpha1.RecordEntry{Name: "@", SOA: &dnsv1alpha1.SOARecordSpec{MName: "ns.corp.internal.", RName: "hostmaster.corp.internal.", Serial: 1}}},
		{"apex CNAME", dnsv1alpha1.RRTypeCNAME, dnsv1alpha1.RecordEntry{Name: "CORP.INTERNAL.", CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "target.example."}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in := fixture(time.Now().UTC())
			in.StaticRecords = []dnsv1alpha1.DNSRecordSet{{ObjectMeta: metav1.ObjectMeta{Name: "invalid", UID: "static-invalid"}, Spec: dnsv1alpha1.DNSRecordSetSpec{DNSZoneRef: structLocal(in.Zone.Name), RecordType: tt.recordType, Records: []dnsv1alpha1.RecordEntry{tt.entry}}}}
			bad := in.Registrations[0].DeepCopy()
			bad.Name = "invalid-registration"
			bad.UID = "invalid-registration-uid"
			bad.Spec.Name = tt.entry.Name
			bad.Spec.RecordTypes = []dnsv1alpha1.RRType{tt.recordType}
			in.Registrations = append(in.Registrations, *bad)
			got, err := Compile(in)
			if err != nil {
				t.Fatal(err)
			}
			if got.Reasons["static-invalid"] != "UnsupportedPrivateRecord" || got.Reasons[bad.UID] != "UnsupportedPrivateRecord" {
				t.Fatalf("authority override not rejected: %#v", got.Reasons)
			}
			if len(got.Plan.RRsets) != 1 || got.Plan.RRsets[0].RecordType != dnsv1alpha1.RRTypeAAAA || !got.Accepted["contribution-a"] {
				t.Fatalf("healthy publication was blocked: %#v", got)
			}
			wire, err := WirePlan(got.Plan)
			if err != nil {
				t.Fatal(err)
			}
			if err = wire.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
