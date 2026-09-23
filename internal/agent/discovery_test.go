// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	sharedutil "go.miloapis.com/dns-operator/internal/dns/util"
)

func TestZoneDiscoveryGetRequiresName(t *testing.T) {
	deps := fixtureDeps(fixtureReader())
	if _, _, err := zoneDiscoveryGet(deps)(context.Background(), nil, ZoneDiscoveryGetInput{}); err == nil {
		t.Fatal("want an error when name is empty")
	}
}

func TestZoneDiscoveryGetNotFound(t *testing.T) {
	deps := fixtureDeps(fixtureReader())
	if _, _, err := zoneDiscoveryGet(deps)(context.Background(), nil, ZoneDiscoveryGetInput{Name: "missing"}); err == nil {
		t.Fatal("want an error for a discovery that does not exist")
	}
}

func TestZoneDiscoveryGetRendersFields(t *testing.T) {
	disc := &dnsv1alpha1.DNSZoneDiscovery{
		ObjectMeta: metav1.ObjectMeta{Name: "example-com-discovery"},
		Status: dnsv1alpha1.DNSZoneDiscoveryStatus{
			Conditions: []metav1.Condition{
				cond(sharedutil.CondDiscovered, "True", sharedutil.ReasonDiscovered, "snapshot complete"),
			},
			RecordSets: []dnsv1alpha1.DiscoveredRecordSet{
				{
					RecordType: dnsv1alpha1.RRTypeA,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.10"}}},
				},
				{
					RecordType: dnsv1alpha1.RRTypeMX,
					Records: []dnsv1alpha1.RecordEntry{{
						Name: "@",
						MX:   &dnsv1alpha1.MXRecordSpec{Preference: 10, Exchange: "mail.example.com."},
					}},
				},
			},
		},
	}

	reader := fixtureReader()
	reader.discoveries = map[string]*dnsv1alpha1.DNSZoneDiscovery{disc.Name: disc}
	deps := fixtureDeps(reader)

	_, out, err := zoneDiscoveryGet(deps)(context.Background(), nil, ZoneDiscoveryGetInput{Name: disc.Name})
	if err != nil {
		t.Fatalf("dns_zone_discovery_get: %v", err)
	}
	if len(out.Conditions) != 1 || out.Conditions[0].Reason != sharedutil.ReasonDiscovered {
		t.Fatalf("Conditions = %+v, want one Discovered condition", out.Conditions)
	}
	if len(out.RecordSets) != 2 {
		t.Fatalf("got %d record sets, want 2", len(out.RecordSets))
	}

	byType := make(map[string]DiscoveredRecordSetView, len(out.RecordSets))
	for _, rs := range out.RecordSets {
		byType[rs.Type] = rs
	}

	a, ok := byType["A"]
	if !ok || len(a.Records) != 1 {
		t.Fatalf("A record set = %+v, want one entry", a)
	}
	if a.Records[0].Name != "www" || a.Records[0].Fields["address"] != "203.0.113.10" {
		t.Errorf("A entry = %+v, want www / address=203.0.113.10", a.Records[0])
	}

	mx, ok := byType["MX"]
	if !ok || len(mx.Records) != 1 {
		t.Fatalf("MX record set = %+v, want one entry", mx)
	}
	if mx.Records[0].Fields["exchange"] != "mail.example.com." || mx.Records[0].Fields["preference"] != "10" {
		t.Errorf("MX entry = %+v, want exchange/preference set", mx.Records[0])
	}
}

func TestZoneDiscoveryGetFailsWhenDepsUnavailable(t *testing.T) {
	failing := func(context.Context) (ToolDeps, error) { return ToolDeps{}, errors.New("no project") }
	if _, _, err := zoneDiscoveryGet(failing)(context.Background(), nil, ZoneDiscoveryGetInput{Name: "x"}); err == nil {
		t.Error("dns_zone_discovery_get: want an error when deps fail")
	}
}
