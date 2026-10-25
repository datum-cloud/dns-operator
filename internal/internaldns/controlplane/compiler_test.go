package controlplane

import (
	"encoding/json"
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
	if record.ContributionUID != "contribution-a" || record.Generation != 18 || record.Sequence != 18 || !record.Eligible {
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
		if got.Reasons["contribution-a"] != "NameOrTypeOutsideRegistration" || len(got.Plan.RRsets) != 0 {
			t.Fatalf("partial invalid bundle published: %#v", got.Plan.RRsets)
		}
		if len(got.Plan.Contributions) != 1 || got.Plan.Contributions[0].Sequence != 18 {
			t.Fatalf("rejected observation did not advance fence: %#v", got.Plan.Contributions)
		}
	})
}

func TestCompileWithdrawalAdvancesHighWaterAndRejectsRollback(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	in := fixture(now)
	previous := in.Contributions[0]
	in.Previous = &PublicationPlan{Contributions: []ContributionFence{{UID: previous.UID, Generation: previous.Generation, Sequence: 18}}}
	in.Contributions[0].Generation = 19
	in.Contributions[0].Spec.Eligible = false
	got, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reasons[previous.UID] != "Ineligible" || len(got.Plan.RRsets) != 0 || got.Plan.Contributions[0].Sequence != 19 {
		t.Fatalf("withdrawal fence incorrect: %#v", got)
	}
	in.Previous = &got.Plan
	in.Contributions[0].Generation = 18
	in.Contributions[0].Spec.Eligible = true
	rolled, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	if rolled.Reasons[previous.UID] != "StaleContributionGeneration" || len(rolled.Plan.RRsets) != 0 {
		t.Fatalf("generation rollback resurrected records: %#v", rolled)
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
	rollbackInput.Contributions[0].Generation = 17
	rollback, err := Compile(rollbackInput)
	if err != nil {
		t.Fatal(err)
	}
	if rollback.Reasons["contribution-a"] != "StaleContributionGeneration" || len(rollback.Plan.RRsets) != 0 {
		t.Fatalf("rollback was not rejected: %#v", rollback)
	}
	if len(rollback.Plan.Contributions) != 1 || rollback.Plan.Contributions[0].Sequence != 18 {
		t.Fatalf("rejected publication erased high-water: %#v", rollback.Plan.Contributions)
	}

	retryInput := fixture(now)
	retryInput.Previous = &rollback.Plan
	retryInput.Contributions[0].Generation = 17
	retry, err := Compile(retryInput)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Reasons["contribution-a"] != "StaleContributionGeneration" || len(retry.Plan.RRsets) != 0 || retry.Plan.Contributions[0].Sequence != 18 {
		t.Fatalf("second reconcile resurrected rolled-back data: %#v", retry)
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
	if result, err := Compile(in); err != nil || result.Reasons[in.Contributions[0].UID] != "MissingSRVTargetAddress" {
		t.Fatalf("missing SRV target address was not rejected: %v", err)
	}
}

func fixture(now time.Time) CompileInput {
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "private", UID: "zone-a", Generation: 2}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "corp.internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate}}
	assoc := func(name string) dnsv1alpha1.DNSZoneAssociation {
		return dnsv1alpha1.DNSZoneAssociation{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name)}, Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: zone.Name}, ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: name}}, Status: dnsv1alpha1.DNSZoneAssociationStatus{ResolvedDNSZoneRef: dnsv1alpha1.DNSNameReference{Name: zone.Name}, ResolvedResolverContextRef: dnsv1alpha1.DNSNameReference{Name: name}, Conditions: []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue}}}}
	}
	reg := dnsv1alpha1.DNSRegistration{ObjectMeta: metav1.ObjectMeta{Name: "api", UID: "registration-a", Generation: 3}, Spec: dnsv1alpha1.DNSRegistrationSpec{DNSZoneRef: dnsv1alpha1.DNSNameReference{Name: zone.Name}, Name: "api", RecordTypes: []dnsv1alpha1.RRType{dnsv1alpha1.RRTypeAAAA}, TTLSeconds: 30}}
	c := dnsv1alpha1.DNSRecordContribution{ObjectMeta: metav1.ObjectMeta{Name: "endpoint-a", UID: "contribution-a", Generation: 18}, Spec: dnsv1alpha1.DNSRecordContributionSpec{RegistrationRef: dnsv1alpha1.DNSObjectReference{Name: reg.Name, UID: reg.UID, Generation: reg.Generation}, RecordSets: []dnsv1alpha1.DNSContributionRecordSet{{RecordType: dnsv1alpha1.RRTypeAAAA, Records: []dnsv1alpha1.RecordEntry{{Name: "api.corp.internal", AAAA: &dnsv1alpha1.AAAARecordSpec{Content: "fd00::1"}}}}}, Eligible: true}, Status: dnsv1alpha1.DNSRecordContributionStatus{ObservedGeneration: 4}}
	zoneRef := dnsv1alpha1.DNSObjectReference{Name: zone.Name, UID: zone.UID, Generation: zone.Generation}
	return CompileInput{Zone: zone, Associations: []dnsv1alpha1.DNSZoneAssociation{assoc("b"), assoc("a")}, AssociationReferences: map[types.UID]associationReferences{
		"a": {Zone: zoneRef, Context: dnsv1alpha1.DNSObjectReference{Name: "a", UID: "vpc-a"}},
		"b": {Zone: zoneRef, Context: dnsv1alpha1.DNSObjectReference{Name: "b", UID: "vpc-b"}},
	}, RegistrationZoneReferences: map[types.UID]dnsv1alpha1.DNSObjectReference{reg.UID: zoneRef}, Registrations: []dnsv1alpha1.DNSRegistration{reg}, Contributions: []dnsv1alpha1.DNSRecordContribution{c}, Now: now}
}

func structLocal(name string) corev1.LocalObjectReference {
	return corev1.LocalObjectReference{Name: name}
}

func TestCompileRejectsReplacementBehindANameOnlyReference(t *testing.T) {
	in := fixture(time.Now().UTC())
	in.Zone.UID = "replacement-zone"
	result, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted[in.Registrations[0].UID] || result.Reasons[in.Registrations[0].UID] != "InvalidReference" || len(result.Plan.RRsets) != 0 {
		t.Fatalf("replacement reused a registration's private reference: %#v", result)
	}
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
			in.RegistrationZoneReferences[bad.UID] = in.RegistrationZoneReferences[in.Registrations[0].UID]
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

func TestStaticTTLPreservedAndConflictsRejected(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	in := fixture(now)
	in.Registrations = nil
	in.Contributions = nil
	ttl := int64(30)
	in.StaticRecords = []dnsv1alpha1.DNSRecordSet{{ObjectMeta: metav1.ObjectMeta{Name: "static", UID: "static"}, Spec: dnsv1alpha1.DNSRecordSetSpec{DNSZoneRef: corev1.LocalObjectReference{Name: in.Zone.Name}, RecordType: dnsv1alpha1.RRTypeA, Records: []dnsv1alpha1.RecordEntry{{Name: "api", TTL: &ttl, A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.1"}}}}}}
	result, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := WirePlan(result.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if wire.RRSets[0].TTL != 30 {
		t.Fatalf("TTL=%d", wire.RRSets[0].TTL)
	}
	ttl = 0
	result, err = Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	if result.Plan.RRsets[0].TTLSeconds != 0 {
		t.Fatal("explicit zero TTL was discarded")
	}
	ttl = 30
	stamp := metav1.NewTime(now)
	in.StaticRecords[0].DeletionTimestamp = &stamp
	result, err = Compile(in)
	if err != nil || len(result.Plan.RRsets) != 0 {
		t.Fatalf("terminating static records retained: %#v %v", result, err)
	}
	in.StaticRecords[0].DeletionTimestamp = nil
	otherTTL := int64(31)
	in.StaticRecords[0].Spec.Records = append(in.StaticRecords[0].Spec.Records, dnsv1alpha1.RecordEntry{Name: "api", TTL: &otherTTL, A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.2"}})
	if result, err := Compile(in); err != nil || result.Reasons["static"] != "ConflictingTTL" {
		t.Fatal("conflicting per-RRset TTL accepted")
	}
}

func TestCompileRejectsTenantRecordsInProviderScope(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	in := fixture(now)
	in.Registrations = nil
	in.Contributions = nil
	static := func(uid types.UID, owner string) dnsv1alpha1.DNSRecordSet {
		return dnsv1alpha1.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: string(uid), UID: uid}, Spec: dnsv1alpha1.DNSRecordSetSpec{DNSZoneRef: corev1.LocalObjectReference{Name: in.Zone.Name}, RecordType: dnsv1alpha1.RRTypeA, Records: []dnsv1alpha1.RecordEntry{{Name: owner, A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.1"}}}}}
	}
	in.StaticRecords = []dnsv1alpha1.DNSRecordSet{
		static("scoped", "web.instances"),
		static("label", "instances"),
		static("absolute", "web.instances."+in.Zone.Spec.DomainName+"."),
		static("outside", "api"),
	}
	result, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, uid := range []types.UID{"scoped", "label", "absolute"} {
		if result.Accepted[uid] || result.Reasons[uid] != "ReservedScope" {
			t.Fatalf("%s accepted=%v reason=%q, want ReservedScope", uid, result.Accepted[uid], result.Reasons[uid])
		}
	}
	if !result.Accepted["outside"] || len(result.Plan.RRsets) != 1 || result.Plan.RRsets[0].Name != "api" {
		t.Fatalf("record outside the scope was not published: %#v", result.Plan.RRsets)
	}
}

func TestCompileRejectsUnboundedDynamicTTL(t *testing.T) {
	in := fixture(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	in.Registrations[0].Spec.TTLSeconds = 2147483647
	result, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted[in.Registrations[0].UID] || len(result.Plan.RRsets) != 0 {
		t.Fatal("unbounded TTL published")
	}
}

func TestCompileRetainsHealthyRecordsUntilExplicitWithdrawal(t *testing.T) {
	now := time.Now().UTC()
	input := fixture(now)
	first, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	input.Now = now.Add(365 * 24 * time.Hour)
	input.Previous = &first.Plan
	after, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	beforeJSON, _ := json.Marshal(first.Plan)
	afterJSON, _ := json.Marshal(after.Plan)
	if string(beforeJSON) != string(afterJSON) {
		t.Fatal("elapsed time changed the last reported endpoint state")
	}
	input.Contributions[0].Spec.Eligible = false
	input.Contributions[0].Generation++
	withdrawn, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	if len(withdrawn.Plan.RRsets) != 0 {
		t.Fatal("explicit unhealthy observation did not withdraw the endpoint")
	}
}

func TestCompileRejectedRegistrationDoesNotClaimOtherTypes(t *testing.T) {
	in := fixture(time.Now())
	original := in.Registrations[0]
	original.CreationTimestamp = metav1.NewTime(time.Unix(1, 0))
	rejected := *original.DeepCopy()
	rejected.Name, rejected.UID = "aaa-rejected", "registration-b"
	rejected.CreationTimestamp = metav1.NewTime(time.Unix(2, 0))
	rejected.Spec.RecordTypes = []dnsv1alpha1.RRType{dnsv1alpha1.RRTypeA, dnsv1alpha1.RRTypeAAAA}
	valid := *original.DeepCopy()
	valid.Name, valid.UID = "other-a", "registration-c"
	valid.CreationTimestamp = metav1.NewTime(time.Unix(3, 0))
	valid.Spec.RecordTypes = []dnsv1alpha1.RRType{dnsv1alpha1.RRTypeA}
	in.Registrations = []dnsv1alpha1.DNSRegistration{rejected, valid, original}
	in.RegistrationZoneReferences[rejected.UID] = in.RegistrationZoneReferences[original.UID]
	in.RegistrationZoneReferences[valid.UID] = in.RegistrationZoneReferences[original.UID]
	got, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Accepted[original.UID] || !got.Accepted[valid.UID] || got.Accepted[rejected.UID] {
		t.Fatalf("non-atomic or unstable claims: %#v", got.Reasons)
	}
}

func TestCompileInvalidSRVDoesNotBlockWithdrawal(t *testing.T) {
	in := fixture(time.Now())
	previous, err := Compile(in)
	if err != nil {
		t.Fatal(err)
	}
	in.Previous = &previous.Plan
	in.Contributions[0].Spec.Eligible = false
	in.Contributions[0].Generation++
	reg := *in.Registrations[0].DeepCopy()
	reg.Name, reg.UID, reg.Spec.Name = "srv", "srv", "_grpc._tcp.svc"
	reg.Spec.RecordTypes = []dnsv1alpha1.RRType{dnsv1alpha1.RRTypeSRV}
	in.Registrations = append(in.Registrations, reg)
	in.RegistrationZoneReferences[reg.UID] = in.RegistrationZoneReferences[in.Registrations[0].UID]
	c := *in.Contributions[0].DeepCopy()
	c.Name, c.UID, c.Spec.Eligible = "srv", "srv-contribution", true
	c.Spec.RegistrationRef = dnsv1alpha1.DNSObjectReference{Name: reg.Name, UID: reg.UID, Generation: reg.Generation}
	c.Spec.RecordSets = []dnsv1alpha1.DNSContributionRecordSet{{RecordType: dnsv1alpha1.RRTypeSRV, Records: []dnsv1alpha1.RecordEntry{{Name: reg.Spec.Name, SRV: &dnsv1alpha1.SRVRecordSpec{Port: 443, Target: "missing.corp.internal."}}}}}
	in.Contributions = append(in.Contributions, c)
	got, err := Compile(in)
	if err != nil {
		t.Fatalf("bad SRV blocks withdrawal: %v", err)
	}
	if len(got.Plan.RRsets) != 0 || got.Accepted[c.UID] || got.Reasons[c.UID] != "MissingSRVTargetAddress" {
		t.Fatalf("invalid publication: %#v", got)
	}
	if len(got.Plan.Contributions) != 2 {
		t.Fatal("rejected contribution lost its generation fence")
	}
}
