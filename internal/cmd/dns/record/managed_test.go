// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

func gatewaySet(t dnsv1alpha1.RRType, entries ...dnsv1alpha1.RecordEntry) *dnsv1alpha1.DNSRecordSet {
	return withLabels(recordSet(t, entries...), map[string]string{
		util.LabelSourceKind: util.ValueSourceKindGateway,
		util.LabelSourceName: "edge-gw",
	})
}

func apexNSSet() *dnsv1alpha1.DNSRecordSet {
	return recordSet(dnsv1alpha1.RRTypeNS,
		dnsv1alpha1.RecordEntry{Name: "@", TTL: ttl(3600), NS: &dnsv1alpha1.NSRecordSpec{Content: "ns1.datum.net."}},
		dnsv1alpha1.RecordEntry{Name: "@", TTL: ttl(3600), NS: &dnsv1alpha1.NSRecordSpec{Content: "ns2.datum.net."}},
	)
}

func soaSet() *dnsv1alpha1.DNSRecordSet {
	return recordSet(dnsv1alpha1.RRTypeSOA, dnsv1alpha1.RecordEntry{
		Name: "@", TTL: ttl(3600),
		SOA: &dnsv1alpha1.SOARecordSpec{MName: "ns1.datum.net.", RName: "hostmaster.example.com.", Serial: 1},
	})
}

// TestGatewayRecordsAreReadOnly — editing them fights a controller that reverts
// the change, so a success here would be a lie.
func TestGatewayRecordsAreReadOnly(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "create", args: []string{"record", "create", testDomain, "_acme", "TXT", "other"}},
		{name: "set", args: []string{"record", "set", testDomain, "_acme", "TXT", "other"}},
		{name: "delete", args: []string{"record", "delete", testDomain, "_acme", "TXT", "--yes"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			interactive(t)
			h := newHarness(t, testZone(), gatewaySet(dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RecordEntry{
				Name: "_acme", TXT: &dnsv1alpha1.TXTRecordSpec{Content: `"token"`},
			}))
			ce := requireExit(t, h.run(tc.args...), util.ExitConflict)
			mustContain(t, ce.Error(), "managed by AI Edge and are read-only")
			mustContain(t, ce.Fix(), `edit Gateway "edge-gw"`)
		})
	}
}

// TestGatewayRecordsAreNotUnlockedByForce — --force is the SOA/NS escape hatch,
// not a way to fight a controller.
func TestGatewayRecordsAreNotUnlockedByForce(t *testing.T) {
	h := newHarness(t, testZone(), gatewaySet(dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RecordEntry{
		Name: "_acme", TXT: &dnsv1alpha1.TXTRecordSpec{Content: `"token"`},
	}))
	_ = requireExit(t, h.run("record", "set", testDomain, "_acme", "TXT", "other", "--force"), util.ExitConflict)
}

// TestPlatformRecordsWarnRatherThanBlock — the API permits the edit and the
// operator never reconciles the content back, so the user is allowed to
// proceed once the risk has been named.
func TestPlatformRecordsWarnRatherThanBlock(t *testing.T) {
	tests := []struct {
		name     string
		seed     *dnsv1alpha1.DNSRecordSet
		args     []string
		wantRisk string
	}{
		{
			name:     "apex NS names delegation",
			seed:     apexNSSet(),
			args:     []string{"record", "create", testDomain, "@", "NS", "ns3.datum.net."},
			wantRisk: "editing apex NS records can break delegation",
		},
		{
			name:     "SOA names zone transfers",
			seed:     soaSet(),
			args:     []string{"record", "set", testDomain, "@", "SOA", "--mname", "ns9.datum.net.", "--rname", "hostmaster.example.com."},
			wantRisk: "editing the SOA record can break zone transfers and negative caching",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("blocked without --force", func(t *testing.T) {
				h := newHarness(t, testZone(), tc.seed.DeepCopy())
				ce := requireExit(t, h.run(tc.args...), util.ExitUsage)
				mustContain(t, ce.Error(), "is a platform-managed record")
				mustContain(t, ce.Fix(), tc.wantRisk)
				mustContain(t, ce.Fix(), "--force")
			})

			t.Run("permitted with --force", func(t *testing.T) {
				h := newHarness(t, testZone(), tc.seed.DeepCopy())
				requireNoError(t, h.run(append(append([]string{}, tc.args...), "--force")...))
				mustContain(t, h.stderr(), "Warning:")
				mustContain(t, h.stderr(), tc.wantRisk)
			})
		})
	}
}

// TestNonApexNSIsAnOrdinaryRecord — delegating a subdomain is a normal thing to
// do and must not need --force.
func TestNonApexNSIsAnOrdinaryRecord(t *testing.T) {
	h := newHarness(t, testZone())
	requireNoError(t, h.run("record", "create", testDomain, "sub", "NS", "ns1.other.net."))
	mustNotContain(t, h.stderr(), "Warning:")
}

// TestPlatformMarkingIsShapeBased documents the heuristic: the operator stamps
// no provenance label, so the guess is type plus object name plus apex.
func TestPlatformMarkingIsShapeBased(t *testing.T) {
	tests := []struct {
		name       string
		set        *dnsv1alpha1.DNSRecordSet
		entry      dnsv1alpha1.RecordEntry
		wantMarker string
	}{
		{
			name:       "the operator's SOA",
			set:        soaSet(),
			entry:      dnsv1alpha1.RecordEntry{Name: "@"},
			wantMarker: markerPlatform,
		},
		{
			name:       "the operator's apex NS",
			set:        apexNSSet(),
			entry:      dnsv1alpha1.RecordEntry{Name: "@"},
			wantMarker: markerPlatform,
		},
		{
			name:       "a subdomain delegation in the same bucket",
			set:        apexNSSet(),
			entry:      dnsv1alpha1.RecordEntry{Name: "sub"},
			wantMarker: "",
		},
		{
			// The object name is deliberately not consulted. It used to be, and
			// an apex NS set under any other name was then unmarked here while
			// `delete` refused it — one zone state, two answers. `delete` was
			// the right one.
			name: "an apex NS set the user named themselves",
			set: func() *dnsv1alpha1.DNSRecordSet {
				rs := apexNSSet()
				rs.Name = "my-ns-records"
				return rs
			}(),
			entry:      dnsv1alpha1.RecordEntry{Name: "@"},
			wantMarker: markerPlatform,
		},
		{
			name: "an SOA set the user named themselves",
			set: func() *dnsv1alpha1.DNSRecordSet {
				rs := soaSet()
				rs.Name = "my-soa"
				return rs
			}(),
			entry:      dnsv1alpha1.RecordEntry{Name: "@"},
			wantMarker: markerPlatform,
		},
		{
			// The spelling is not consulted either: the backend qualifies both
			// onto one RRset, so both are the delegation.
			name:       "an apex NS entry stored fully qualified",
			set:        apexNSSet(),
			entry:      dnsv1alpha1.RecordEntry{Name: testDomain + "."},
			wantMarker: markerPlatform,
		},
		{
			name:       "a Gateway set",
			set:        gatewaySet(dnsv1alpha1.RRTypeTXT),
			entry:      dnsv1alpha1.RecordEntry{Name: "_acme"},
			wantMarker: markerGateway,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.set, tc.entry, testDomain).marker()
			if got != tc.wantMarker {
				t.Errorf("marker = %q, want %q", got, tc.wantMarker)
			}
		})
	}
}

// TestMachineOwnershipDoesNotHangOnOneLabel.
//
// The producer (network-services-operator, gateway_dns_controller.go:309-316)
// sets five labels together, and its own garbage collector lists by managed-by,
// managed, source-name and source-namespace — pointedly not by source-kind. So
// source-kind alone must not be the key the read-only rule depends on: hanging
// on it fails OPEN, silently permitting an edit a controller will revert.
func TestMachineOwnershipDoesNotHangOnOneLabel(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{
			name: "the full set the producer writes",
			labels: map[string]string{
				util.LabelManagedBy:       util.ValueManagedByNetworking,
				util.LabelDNSManaged:      util.ValueDNSManaged,
				util.LabelSourceKind:      util.ValueSourceKindGateway,
				util.LabelSourceName:      "web",
				util.LabelSourceNamespace: "team-a",
			},
			want: true,
		},
		{name: "source-kind alone", labels: map[string]string{util.LabelSourceKind: util.ValueSourceKindGateway}, want: true},
		{name: "managed alone", labels: map[string]string{util.LabelDNSManaged: util.ValueDNSManaged}, want: true},
		{name: "managed-by alone", labels: map[string]string{util.LabelManagedBy: util.ValueManagedByNetworking}, want: true},
		{
			name:   "the GC label set, with source-kind dropped",
			labels: map[string]string{util.LabelManagedBy: util.ValueManagedByNetworking, util.LabelDNSManaged: util.ValueDNSManaged, util.LabelSourceName: "web"},
			want:   true,
		},
		{name: "no labels", labels: nil, want: false},
		{name: "an unrelated label", labels: map[string]string{"team": "web"}, want: false},
		{name: "managed explicitly false", labels: map[string]string{util.LabelDNSManaged: "false"}, want: false},
		{name: "managed-by something else", labels: map[string]string{util.LabelManagedBy: "helm"}, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rs := withLabels(recordSet(dnsv1alpha1.RRTypeTXT), tc.labels)
			if got := isMachineOwned(rs); got != tc.want {
				t.Errorf("isMachineOwned = %v, want %v", got, tc.want)
			}
			if got := classify(rs, dnsv1alpha1.RecordEntry{Name: "_acme"}, testDomain) == provGateway; got != tc.want {
				t.Errorf("classify says gateway = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGatewayRefusalNamesTheSourceWithItsNamespace — two Gateways in different
// namespaces can share a name, which is why the producer's GC pairs the two.
func TestGatewayRefusalNamesTheSourceWithItsNamespace(t *testing.T) {
	rs := withLabels(recordSet(dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RecordEntry{
		Name: "_acme", TXT: &dnsv1alpha1.TXTRecordSpec{Content: `"token"`},
	}), map[string]string{
		util.LabelSourceKind:      util.ValueSourceKindGateway,
		util.LabelSourceName:      "web",
		util.LabelSourceNamespace: "team-a",
	})

	h := newHarness(t, testZone(), rs)
	ce := requireExit(t, h.run("record", "set", testDomain, "_acme", "TXT", "other"), util.ExitConflict)
	mustContain(t, ce.Fix(), `edit Gateway "team-a/web"`)
}

// TestRefusalStaysGenericWithoutASourceKind — the weaker ownership labels still
// block the edit; only the wording softens.
func TestRefusalStaysGenericWithoutASourceKind(t *testing.T) {
	rs := withLabels(recordSet(dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RecordEntry{
		Name: "_acme", TXT: &dnsv1alpha1.TXTRecordSpec{Content: `"token"`},
	}), map[string]string{util.LabelDNSManaged: util.ValueDNSManaged, util.LabelSourceName: "web"})

	h := newHarness(t, testZone(), rs)
	ce := requireExit(t, h.run("record", "set", testDomain, "_acme", "TXT", "other"), util.ExitConflict)
	mustContain(t, ce.Error(), "managed by AI Edge and are read-only")
	mustContain(t, ce.Fix(), `edit controller "web"`)
}

// TestMachineOwnedSetWinsThePickWhenTwoHoldTheName.
//
// findSet decides which set the read-only guard inspects. When a user set and a
// Gateway set both carry the owner, picking the user's because it sorted first
// failed OPEN — the guard never saw the Gateway labels and permitted an edit the
// controller reverts.
func TestMachineOwnedSetWinsThePickWhenTwoHoldTheName(t *testing.T) {
	// "aaa-txt" sorts before the Gateway set, so first-by-name picks the wrong one.
	userSet := recordSet(dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RecordEntry{
		Name: "_acme", TXT: &dnsv1alpha1.TXTRecordSpec{Content: `"mine"`},
	})
	userSet.Name = "aaa-txt"

	gw := gatewaySet(dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RecordEntry{
		Name: "_acme", TXT: &dnsv1alpha1.TXTRecordSpec{Content: `"theirs"`},
	})
	gw.Name = "zzz-txt"

	h := newHarness(t, testZone(), userSet, gw)
	ce := requireExit(t, h.run("record", "set", testDomain, "_acme", "TXT", "other"), util.ExitConflict)
	mustContain(t, ce.Error(), "managed by AI Edge and are read-only")
}
