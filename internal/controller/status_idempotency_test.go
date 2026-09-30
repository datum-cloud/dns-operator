// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"testing"
	"time"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestRecordSetReplicator_UpdateStatus_NoPatch(t *testing.T) {
	t.Parallel()

	accepted := testCondition(CondAccepted, ReasonAccepted, "Accepted")
	programmed := testCondition(CondProgrammed, ReasonProgrammed, "Programmed")
	recordProgrammed := testCondition(CondProgrammed, ReasonProgrammed, "RecordProgrammed")
	converged := dnsv1alpha1.DNSRecordSetStatus{
		Conditions: []metav1.Condition{accepted, programmed},
		RecordSets: []dnsv1alpha1.RecordSetStatus{{Name: "www", Conditions: []metav1.Condition{recordProgrammed}}},
	}

	tests := []struct {
		name       string
		upstream   dnsv1alpha1.DNSRecordSetStatus
		downstream dnsv1alpha1.DNSRecordSetStatus
		failMsg    string
	}{
		{
			name:       "equal status",
			upstream:   *converged.DeepCopy(),
			downstream: *converged.DeepCopy(),
			failMsg:    "expected 0 status patches when upstream already matches downstream",
		},
		{
			name:       "nil vs empty record sets",
			upstream:   dnsv1alpha1.DNSRecordSetStatus{Conditions: []metav1.Condition{accepted, programmed}},
			downstream: dnsv1alpha1.DNSRecordSetStatus{Conditions: []metav1.Condition{accepted, programmed}, RecordSets: []dnsv1alpha1.RecordSetStatus{}},
			failMsg:    "nil vs empty RecordSets caused a spurious status patch",
		},
		{
			name:       "nil vs empty conditions",
			upstream:   dnsv1alpha1.DNSRecordSetStatus{},
			downstream: dnsv1alpha1.DNSRecordSetStatus{Conditions: []metav1.Condition{}},
			failMsg:    "nil vs empty Conditions caused a spurious status patch",
		},
		{
			name: "nil vs empty per-record conditions",
			upstream: dnsv1alpha1.DNSRecordSetStatus{
				Conditions: []metav1.Condition{accepted},
				RecordSets: []dnsv1alpha1.RecordSetStatus{{Name: "www"}},
			},
			downstream: dnsv1alpha1.DNSRecordSetStatus{
				Conditions: []metav1.Condition{accepted},
				RecordSets: []dnsv1alpha1.RecordSetStatus{{Name: "www", Conditions: []metav1.Condition{}}},
			},
			failMsg: "nil vs empty per-record Conditions caused a spurious status patch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			upstream := testRecordSet(func(rs *dnsv1alpha1.DNSRecordSet) { rs.Status = tt.upstream })
			upstreamClient := &countingClient{Client: fake.NewClientBuilder().
				WithScheme(newFullTestScheme(t)).
				WithStatusSubresource(&dnsv1alpha1.DNSRecordSet{}).
				WithObjects(upstream).
				Build()}

			r := &DNSRecordSetReplicator{}
			if err := r.updateStatus(context.Background(), upstreamClient, upstream, &tt.downstream); err != nil {
				t.Fatalf("updateStatus: %v", err)
			}
			if upstreamClient.statusPatchCount != 0 {
				t.Fatalf("%s (got %d patches); this triggers a re-reconcile loop", tt.failMsg, upstreamClient.statusPatchCount)
			}
		})
	}
}

func TestZoneReplicator_UpdateStatus_NoPatch(t *testing.T) {
	t.Parallel()

	twoNameservers := []networkingv1alpha.Nameserver{
		testNameserver("ns1.example.com", "192.0.2.5"),
		testNameserver("ns2.example.com", "192.0.2.10", "192.0.2.20"),
	}
	tests := []struct {
		name       string
		hostnames  []string
		zoneRefNS  []networkingv1alpha.Nameserver
		domainNS   []networkingv1alpha.Nameserver
		calls      int
		failPrefix string
	}{
		{
			name:       "steady state",
			hostnames:  []string{"ns1.example.com", "ns2.example.com"},
			zoneRefNS:  twoNameservers,
			domainNS:   twoNameservers,
			calls:      1,
			failPrefix: "steady state",
		},
		{
			name:      "called twice as Reconcile does",
			hostnames: []string{"ns1.example.com", "ns2.example.com"},
			zoneRefNS: []networkingv1alpha.Nameserver{
				testNameserver("ns1.example.com", "192.0.2.5"),
				testNameserver("ns2.example.com", "192.0.2.10"),
			},
			domainNS: []networkingv1alpha.Nameserver{
				testNameserver("ns1.example.com", "192.0.2.5"),
				testNameserver("ns2.example.com", "192.0.2.10"),
			},
			calls:      2,
			failPrefix: "steady state across 2 calls",
		},
		{
			name:       "domain ref nil vs empty nameservers",
			hostnames:  []string{"ns1.example.com"},
			zoneRefNS:  nil,
			domainNS:   []networkingv1alpha.Nameserver{},
			calls:      1,
			failPrefix: "DomainRef nil-vs-empty nameservers",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			scheme := newFullTestScheme(t)
			zone := testZone(withConvergedZoneStatus(tt.hostnames, tt.zoneRefNS))
			objs := append([]client.Object{zone, testDomain(tt.domainNS)}, testZoneDefaultRecordSets(zone)...)
			upstreamClient := &countingClient{Client: newZoneUpstreamClient(scheme, objs...)}

			downstreamZone := &dnsv1alpha1.DNSZone{
				ObjectMeta: metav1.ObjectMeta{Name: "shadow-zone-a", Namespace: "downstream"},
				Status:     dnsv1alpha1.DNSZoneStatus{Nameservers: tt.hostnames},
			}
			downstreamClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(downstreamZone).Build()
			r := &DNSZoneReplicator{DownstreamClient: downstreamClient}
			strategy := fakeStrategy{namespace: downstreamZone.Namespace, name: downstreamZone.Name, client: downstreamClient}

			for i := 1; i <= tt.calls; i++ {
				if err := r.updateStatus(context.Background(), upstreamClient, strategy, zone); err != nil {
					t.Fatalf("updateStatus call %d: %v", i, err)
				}
			}
			if upstreamClient.statusPatchCount != 0 {
				t.Fatalf("%s caused a spurious status patch (got %d patches); this triggers a re-reconcile loop",
					tt.failPrefix, upstreamClient.statusPatchCount)
			}
		})
	}
}

func TestReplicator_EnsureDownstream_NoPatchOnSecondCall(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		ensure     func(ctx context.Context, c *trackingClient) (controllerutil.OperationResult, error)
		shadowName string
		shadow     client.Object
	}{
		{
			name: "record set",
			ensure: func(ctx context.Context, c *trackingClient) (controllerutil.OperationResult, error) {
				r := &DNSRecordSetReplicator{DownstreamClient: c}
				strategy := annotatingStrategy{fakeStrategy{namespace: "ns-downstream", client: c}}
				return r.ensureDownstreamRecordSet(ctx, strategy, testRecordSet(func(rs *dnsv1alpha1.DNSRecordSet) {
					rs.Generation = 0
					rs.UID = "upstream-uid-1"
				}))
			},
			shadowName: "rs-a",
			shadow:     &dnsv1alpha1.DNSRecordSet{},
		},
		{
			name: "zone",
			ensure: func(ctx context.Context, c *trackingClient) (controllerutil.OperationResult, error) {
				r := &DNSZoneReplicator{DownstreamClient: c}
				strategy := annotatingZoneStrategy{fakeStrategy{namespace: "ns-downstream", client: c}}
				return r.ensureDownstreamZone(ctx, strategy, testZone(func(z *dnsv1alpha1.DNSZone) {
					z.Generation = 0
					z.UID = "upstream-uid-1"
				}))
			},
			shadowName: "zone-a",
			shadow:     &dnsv1alpha1.DNSZone{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			downstreamClient := &trackingClient{Client: fake.NewClientBuilder().WithScheme(newFullTestScheme(t)).Build()}

			if _, err := tt.ensure(ctx, downstreamClient); err != nil {
				t.Fatalf("ensure %s (call 1): %v", tt.name, err)
			}
			downstreamClient.createCount = 0
			downstreamClient.patchCount = 0

			res, err := tt.ensure(ctx, downstreamClient)
			if err != nil {
				t.Fatalf("ensure %s (call 2): %v", tt.name, err)
			}
			if downstreamClient.patchCount != 0 {
				_ = downstreamClient.Get(ctx, client.ObjectKey{Namespace: "ns-downstream", Name: tt.shadowName}, tt.shadow)
				t.Logf("annotations after call 2: %v", tt.shadow.GetAnnotations())
				t.Fatalf("ensure %s patched downstream on second call (result=%s, patches=%d); "+
					"this triggers the downstream watch and re-enqueues upstream", tt.name, res, downstreamClient.patchCount)
			}
		})
	}
}

func TestZoneReplicator_SteadyState_NoWrites(t *testing.T) {
	t.Parallel()
	scheme := newFullTestScheme(t)

	nameservers := []networkingv1alpha.Nameserver{
		testNameserver("ns1.example.com", "192.0.2.5"),
		testNameserver("ns2.example.com", "192.0.2.10"),
	}
	hostnames := []string{"ns1.example.com", "ns2.example.com"}
	zone := testZone(withConvergedZoneStatus(hostnames, nameservers), func(z *dnsv1alpha1.DNSZone) {
		z.Finalizers = []string{"dns.networking.miloapis.com/finalize-dnszone"}
	})
	zoneClass := &dnsv1alpha1.DNSZoneClass{
		ObjectMeta: metav1.ObjectMeta{Name: "pdns"},
		Spec:       dnsv1alpha1.DNSZoneClassSpec{ControllerName: "powerdns"},
	}
	domain := testDomain(nameservers, metav1.Condition{
		Type: networkingv1alpha.DomainConditionVerified, Status: metav1.ConditionTrue, Reason: "Verified", LastTransitionTime: testConditionTime,
	})
	objs := append([]client.Object{zone, zoneClass, domain}, testZoneDefaultRecordSets(zone)...)
	upstreamClient := &allTrackingClient{Client: newZoneUpstreamClient(scheme, objs...)}

	downstreamZone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "zone-a", Namespace: "ns-downstream", Annotations: upstreamZoneAnnotations("default", "zone-a")},
		Spec:       zone.Spec,
		Status:     dnsv1alpha1.DNSZoneStatus{Nameservers: hostnames},
	}
	accountingCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "example.com", Namespace: "dns-zone-accounting"},
		Data:       map[string]string{"owner": "single/default/zone-a"},
	}
	accountingNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "dns-zone-accounting"}}
	downstreamClient := &allTrackingClient{Client: fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(downstreamZone, accountingCM, accountingNS).
		Build()}

	strategy := annotatingZoneStrategy{fakeStrategy{namespace: "ns-downstream", client: downstreamClient}}
	r := &DNSZoneReplicator{DownstreamClient: downstreamClient, AccountingNamespace: "dns-zone-accounting"}
	ctx := context.Background()

	owned, err := r.ensureZoneAccounting(ctx, zone, "single/default/zone-a")
	if err != nil {
		t.Fatalf("ensureZoneAccounting: %v", err)
	}
	if !owned {
		t.Fatalf("expected zone to be owned")
	}
	if err := r.ensureDomain(ctx, upstreamClient, zone); err != nil {
		t.Fatalf("ensureDomain: %v", err)
	}
	if err := r.ensureDomainRef(ctx, upstreamClient, zone); err != nil {
		t.Fatalf("ensureDomainRef: %v", err)
	}
	verified, err := r.isDomainVerified(ctx, upstreamClient, zone.Namespace, zone.Spec.DomainName)
	if err != nil {
		t.Fatalf("isDomainVerified: %v", err)
	}
	if !verified {
		t.Fatalf("expected domain to be verified")
	}
	if _, err := r.ensureDownstreamZone(ctx, strategy, zone); err != nil {
		t.Fatalf("ensureDownstreamZone: %v", err)
	}
	if err := r.updateStatus(ctx, upstreamClient, strategy, zone); err != nil {
		t.Fatalf("updateStatus call 1: %v", err)
	}
	if err := r.ensureSOARecordSet(ctx, upstreamClient, zone); err != nil {
		t.Fatalf("ensureSOARecordSet: %v", err)
	}
	if err := r.ensureNSRecordSet(ctx, upstreamClient, zone); err != nil {
		t.Fatalf("ensureNSRecordSet: %v", err)
	}
	if err := r.updateStatus(ctx, upstreamClient, strategy, zone); err != nil {
		t.Fatalf("updateStatus call 2: %v", err)
	}

	for side, c := range map[string]*allTrackingClient{"upstream": upstreamClient, "downstream": downstreamClient} {
		if c.totalWrites() != 0 {
			t.Errorf("steady-state zone reconcile produced %d %s writes (creates=%d, patches=%d, updates=%d, statusPatches=%d); "+
				"each write triggers re-enqueue via watch",
				c.totalWrites(), side, c.creates, c.patches, c.updates, c.statusPatch)
		}
	}
}

func TestZoneReplicator_UpdateStatus_ConditionsAlreadySet(t *testing.T) {
	t.Parallel()

	conditions := []metav1.Condition{testCondition(CondAccepted, ReasonAccepted, testZoneMsgAccepted)}
	reapplied := testCondition(CondAccepted, ReasonAccepted, testZoneMsgAccepted)
	reapplied.LastTransitionTime = metav1.NewTime(time.Now())

	if apimeta.SetStatusCondition(&conditions, reapplied) {
		t.Fatal("SetStatusCondition reported changed=true when re-applying identical condition with different LastTransitionTime; " +
			"this means every reconcile loop will patch status and re-enqueue")
	}
	cond := apimeta.FindStatusCondition(conditions, CondAccepted)
	if !cond.LastTransitionTime.Equal(&testConditionTime) {
		t.Fatalf("LastTransitionTime was mutated from %v to %v", testConditionTime, cond.LastTransitionTime)
	}
}
