// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	sharedutil "go.miloapis.com/dns-operator/internal/dns/util"
)

const (
	testNamespace   = "project-test"
	testImplVersion = "0.0.1"
	testServerName  = "test"
	testClientName  = "test-client"
)

// fakeReader serves canned objects so the tools can be exercised without a
// cluster.
type fakeReader struct {
	zones      []dnsv1alpha1.DNSZone
	recordSets map[string][]dnsv1alpha1.DNSRecordSet // keyed by zone object name
	byName     map[string]*dnsv1alpha1.DNSRecordSet  // keyed by record set object name
	err        error
}

var _ Reader = (*fakeReader)(nil)

func (f *fakeReader) ListZones(_ context.Context, _ string) ([]dnsv1alpha1.DNSZone, error) {
	return f.zones, f.err
}

func (f *fakeReader) GetZone(_ context.Context, _, name string) (*dnsv1alpha1.DNSZone, error) {
	if f.err != nil {
		return nil, f.err
	}
	for i := range f.zones {
		if f.zones[i].Name == name {
			return &f.zones[i], nil
		}
	}
	return nil, errors.New("zone not found")
}

func (f *fakeReader) ListRecordSets(_ context.Context, _, zoneName string) ([]dnsv1alpha1.DNSRecordSet, error) {
	return f.recordSets[zoneName], f.err
}

func (f *fakeReader) GetRecordSet(_ context.Context, _, name string) (*dnsv1alpha1.DNSRecordSet, error) {
	if f.err != nil {
		return nil, f.err
	}
	if rs, ok := f.byName[name]; ok {
		return rs, nil
	}
	return nil, errors.New("record set not found")
}

func fixtureDeps(r Reader) DepsFor {
	return func(context.Context) (ToolDeps, error) {
		return ToolDeps{Reader: r, Namespace: testNamespace}, nil
	}
}

// fixtureReader builds one healthy zone and one pending zone, each with one
// record set.
func fixtureReader() *fakeReader {
	healthyZone := zone("healthy.com",
		cond(sharedutil.CondAccepted, "True", sharedutil.ReasonAccepted, "ok"),
		cond(sharedutil.CondProgrammed, "True", sharedutil.ReasonProgrammed, "ok"))
	healthyZone.Name = "healthy-com"
	healthyZone.Status.RecordCount = 1

	pendingZone := zone("pending.com",
		cond(sharedutil.CondAccepted, "True", sharedutil.ReasonAccepted, "ok"),
		cond(sharedutil.CondProgrammed, "False", sharedutil.ReasonPending, "waiting"))
	pendingZone.Name = "pending-com"

	healthyRS := recordSet("www-healthy", []dnsv1alpha1.RecordEntry{{Name: "www"}},
		ownerStatus("www.healthy.com.", cond(sharedutil.CondProgrammed, "True", sharedutil.ReasonProgrammed, "live")))
	healthyRS.Spec.DNSZoneRef.Name = healthyZone.Name

	pendingRS := recordSet("www-pending", []dnsv1alpha1.RecordEntry{{Name: "www"}})
	pendingRS.Spec.DNSZoneRef.Name = pendingZone.Name

	return &fakeReader{
		zones: []dnsv1alpha1.DNSZone{*healthyZone, *pendingZone},
		recordSets: map[string][]dnsv1alpha1.DNSRecordSet{
			healthyZone.Name: {*healthyRS},
			pendingZone.Name: {*pendingRS},
		},
		byName: map[string]*dnsv1alpha1.DNSRecordSet{
			healthyRS.Name: healthyRS,
			pendingRS.Name: pendingRS,
		},
	}
}

func TestZonesListOrdersNotOKFirst(t *testing.T) {
	deps := fixtureDeps(fixtureReader())

	_, out, err := zonesList(deps)(context.Background(), nil, ZonesListInput{})
	if err != nil {
		t.Fatalf("dns_zones_list: %v", err)
	}
	if len(out.Zones) != 2 {
		t.Fatalf("got %d zones, want 2", len(out.Zones))
	}
	if last := out.Zones[len(out.Zones)-1]; last.Domain != "healthy.com" {
		t.Errorf("last row = %q, want healthy.com (OK zones sort last)", last.Domain)
	}

	byDomain := make(map[string]ZoneSummary, len(out.Zones))
	for _, z := range out.Zones {
		byDomain[z.Domain] = z
	}
	if got := byDomain["pending.com"]; got.RootCauseReason != sharedutil.ReasonPending {
		t.Errorf("pending.com RootCauseReason = %q, want Pending", got.RootCauseReason)
	}
	if got := byDomain["healthy.com"]; got.RootCauseReason != "" {
		t.Errorf("healthy.com RootCauseReason = %q, want empty", got.RootCauseReason)
	}
}

func TestZonesGetRequiresName(t *testing.T) {
	deps := fixtureDeps(fixtureReader())
	if _, _, err := zonesGet(deps)(context.Background(), nil, ZonesGetInput{}); err == nil {
		t.Fatal("want an error when name is empty")
	}
}

func TestZonesGetReturnsDelegation(t *testing.T) {
	deps := fixtureDeps(fixtureReader())
	_, out, err := zonesGet(deps)(context.Background(), nil, ZonesGetInput{Name: "healthy-com"})
	if err != nil {
		t.Fatalf("dns_zones_get: %v", err)
	}
	if out.Zone.Domain != "healthy.com" {
		t.Errorf("Domain = %q, want healthy.com", out.Zone.Domain)
	}
	if out.Zone.Delegation != sharedutil.DelegationUnknown {
		t.Errorf("Delegation = %q, want Unknown (no Domain linked in the fixture)", out.Zone.Delegation)
	}
}

func TestZoneDiagnose(t *testing.T) {
	deps := fixtureDeps(fixtureReader())
	_, d, err := zoneDiagnose(deps)(context.Background(), nil, ZoneDiagnoseInput{Name: "pending-com"})
	if err != nil {
		t.Fatalf("dns_zone_diagnose: %v", err)
	}
	if d.RootCause == nil || d.RootCause.Reason != sharedutil.ReasonPending {
		t.Fatalf("RootCause = %+v, want Pending", d.RootCause)
	}
}

func TestRecordsListRequiresZone(t *testing.T) {
	deps := fixtureDeps(fixtureReader())
	if _, _, err := recordsList(deps)(context.Background(), nil, RecordsListInput{}); err == nil {
		t.Fatal("want an error when zone is empty")
	}
}

func TestRecordsListReportsPerOwnerStatus(t *testing.T) {
	deps := fixtureDeps(fixtureReader())
	_, out, err := recordsList(deps)(context.Background(), nil, RecordsListInput{Zone: "pending-com"})
	if err != nil {
		t.Fatalf("dns_records_list: %v", err)
	}
	if len(out.Records) != 1 {
		t.Fatalf("got %d records, want 1", len(out.Records))
	}
	if out.Records[0].Status != sharedutil.StatusPending {
		t.Errorf("Status = %q, want Pending", out.Records[0].Status)
	}
}

func TestRecordsGetRequiresName(t *testing.T) {
	deps := fixtureDeps(fixtureReader())
	if _, _, err := recordsGet(deps)(context.Background(), nil, RecordsGetInput{}); err == nil {
		t.Fatal("want an error when name is empty")
	}
}

func TestRecordsGetReturnsOwnerNames(t *testing.T) {
	deps := fixtureDeps(fixtureReader())
	_, out, err := recordsGet(deps)(context.Background(), nil, RecordsGetInput{Name: "www-healthy"})
	if err != nil {
		t.Fatalf("dns_records_get: %v", err)
	}
	if len(out.RecordSet.OwnerNames) != 1 || out.RecordSet.OwnerNames[0].Name != "www.healthy.com." {
		t.Fatalf("OwnerNames = %+v, want one entry for www.healthy.com.", out.RecordSet.OwnerNames)
	}
}

func TestRecordDiagnoseRequiresAllFields(t *testing.T) {
	deps := fixtureDeps(fixtureReader())
	if _, _, err := recordDiagnose(deps)(context.Background(), nil, RecordDiagnoseInput{}); err == nil {
		t.Fatal("want an error when zone, recordSet, and ownerName are empty")
	}
}

func TestRecordDiagnoseSurfacesRootCause(t *testing.T) {
	deps := fixtureDeps(fixtureReader())
	_, d, err := recordDiagnose(deps)(context.Background(), nil, RecordDiagnoseInput{
		Zone: "pending-com", RecordSet: "www-pending", OwnerName: "www",
	})
	if err != nil {
		t.Fatalf("dns_record_diagnose: %v", err)
	}
	if d.RootCause == nil || d.RootCause.Reason != sharedutil.ReasonPending {
		t.Fatalf("RootCause = %+v, want Pending", d.RootCause)
	}
}

func TestToolsFailWhenDepsUnavailable(t *testing.T) {
	failing := func(context.Context) (ToolDeps, error) { return ToolDeps{}, errors.New("no project") }

	if _, _, err := zonesList(failing)(context.Background(), nil, ZonesListInput{}); err == nil {
		t.Error("dns_zones_list: want an error when deps fail")
	}
	if _, _, err := zonesGet(failing)(context.Background(), nil, ZonesGetInput{Name: "x"}); err == nil {
		t.Error("dns_zones_get: want an error when deps fail")
	}
	if _, _, err := zoneDiagnose(failing)(context.Background(), nil, ZoneDiagnoseInput{Name: "x"}); err == nil {
		t.Error("dns_zone_diagnose: want an error when deps fail")
	}
	if _, _, err := recordsList(failing)(context.Background(), nil, RecordsListInput{Zone: "x"}); err == nil {
		t.Error("dns_records_list: want an error when deps fail")
	}
	if _, _, err := recordsGet(failing)(context.Background(), nil, RecordsGetInput{Name: "x"}); err == nil {
		t.Error("dns_records_get: want an error when deps fail")
	}
	if _, _, err := recordDiagnose(failing)(context.Background(), nil, RecordDiagnoseInput{
		Zone: "x", RecordSet: "y", OwnerName: "z",
	}); err == nil {
		t.Error("dns_record_diagnose: want an error when deps fail")
	}
}

func TestRegisterToolsPublishesExactlyTheDocumentedSet(t *testing.T) {
	ctx := context.Background()

	server := mcp.NewServer(&mcp.Implementation{Name: testServerName, Version: testImplVersion}, nil)
	RegisterTools(server, fixtureDeps(fixtureReader()))

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connecting server: %v", err)
	}
	defer func() { _ = serverSession.Close() }()

	c := mcp.NewClient(&mcp.Implementation{Name: testClientName, Version: testImplVersion}, nil)
	clientSession, err := c.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connecting client: %v", err)
	}
	defer func() { _ = clientSession.Close() }()

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	want := map[string]bool{
		ToolZonesList: false, ToolZonesGet: false, ToolZoneDiagnose: false,
		ToolRecordsList: false, ToolRecordsGet: false, ToolRecordDiagnose: false,
	}
	for _, tool := range res.Tools {
		if _, ok := want[tool.Name]; !ok {
			t.Errorf("published undocumented tool %q", tool.Name)
			continue
		}
		want[tool.Name] = true
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("tool %q was not published", name)
		}
	}
}
