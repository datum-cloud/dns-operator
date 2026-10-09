package serving

import (
	"github.com/miekg/dns"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	"strings"
	"testing"
	"time"
)

func parseRenderedZone(t *testing.T, zone []byte) []dns.RR {
	t.Helper()
	parser := dns.NewZoneParser(strings.NewReader(string(zone)), "", "test-zone")
	var records []dns.RR
	for rr, ok := parser.Next(); ok; rr, ok = parser.Next() {
		records = append(records, rr)
	}
	if err := parser.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

func TestNODATAOwnershipDoesNotGrowNamesOrCollideWithTenantCNAME(t *testing.T) {
	now := time.Now()
	owner := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 45) + ".x.internal."
	plan := model.PublicationPlan{ZoneUID: "zone", Apex: "x.internal", Owners: []string{owner, "api.x.internal"}, RRSets: []model.RRSet{{Name: "_dns-ownership.api.x.internal", Type: "CNAME", TTL: 5, Records: []model.RRRecord{{Content: "target.example.", Eligible: true}}}}}
	data, err := renderZone(publicationState{Plan: plan}, now)
	if err != nil {
		t.Fatal(err)
	}
	records := parseRenderedZone(t, data)
	markers := map[string]bool{}
	for _, rr := range records {
		if rr.Header().Rrtype == 65280 {
			markers[rr.Header().Name] = true
		}
	}
	if !markers[owner] || !markers["api.x.internal."] {
		t.Fatalf("missing ownership metadata: %v", markers)
	}
	plan.RRSets = append(plan.RRSets, model.RRSet{Name: "api.x.internal", Type: "CNAME", TTL: 5, Records: []model.RRRecord{{Content: "healthy.example.", Eligible: true}}})
	data, err = renderZone(publicationState{Plan: plan}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, rr := range parseRenderedZone(t, data) {
		if rr.Header().Name == "api.x.internal." && rr.Header().Rrtype == 65280 {
			t.Fatal("metadata coexists with recovered CNAME")
		}
	}
}

func TestPublicationRejectsInvalidGeneratedAuthorityBeforeServing(t *testing.T) {
	for _, sets := range [][]model.RRSet{
		{{Name: "api.x.internal", Type: "CNAME", TTL: 5, Records: []model.RRRecord{{Content: "target.example.", Eligible: true}}}, {Name: "api.x.internal", Type: "TXT", TTL: 5, Records: []model.RRRecord{{Content: `"conflict"`, Eligible: true}}}},
		{{Name: "api.x.internal", Type: model.OwnershipMarkerType, TTL: 5, Records: []model.RRRecord{{Content: `\# 1 00`, Eligible: true}}}},
	} {
		plan := model.PublicationPlan{ZoneUID: "zone", Apex: "x.internal", RRSets: sets}
		if err := plan.Validate(); err == nil {
			t.Fatalf("invalid generated authority accepted: %#v", sets)
		}
	}
}
