// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	sharedutil "go.miloapis.com/dns-operator/internal/dns/util"
)

// The tools this package publishes to an assistant. All read-only.
//
// There is deliberately no mutating tool. Creating or editing a record goes
// through the assistant's own plan and apply tools, which hold the
// confirmation step every service shares — DNS has the strongest case of any
// service for that boundary, since a bad record takes effect globally in
// seconds and stays cached well past the fix. dns_record_render, added in a
// later phase, only renders a proposed manifest for review; it never applies
// one.
const (
	ToolZonesList       = "dns_zones_list"
	ToolZonesGet        = "dns_zones_get"
	ToolZoneDiagnose    = "dns_zone_diagnose"
	ToolDelegationCheck = "dns_delegation_check"
	ToolRecordsList     = "dns_records_list"
	ToolRecordsGet      = "dns_records_get"
	ToolRecordDiagnose  = "dns_record_diagnose"
)

// ToolDeps is what one request's tool calls operate over: where to read
// from, and which project's namespace they are confined to.
type ToolDeps struct {
	Reader    Reader
	Namespace string
}

// DepsFor resolves the dependencies for a tool call. A function rather than
// a value so the caller decides how identity and project are established:
// the server derives both from the HTTP request, tests supply them directly.
type DepsFor func(context.Context) (ToolDeps, error)

// ---------------------------------------------------------------- I/O types

// ConditionView is the part of a condition worth spending tokens on.
type ConditionView struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

// ZoneSummary is one row of the zone listing.
type ZoneSummary struct {
	Domain      string `json:"domain"`
	Status      string `json:"status"`
	Delegation  string `json:"delegation"`
	RecordCount int    `json:"recordCount"`
	// RootCauseReason and Actionability are empty for a healthy zone.
	RootCauseReason string        `json:"rootCauseReason,omitempty"`
	Actionability   Actionability `json:"actionability,omitempty"`
	// RootCauseFor is how long the root-cause condition has held ("5d",
	// "12m"). The consumer is a language model: this is what makes a
	// five-day "pending" impossible to read as normal in-flight work.
	RootCauseFor string `json:"rootCauseFor,omitempty"`
}

// ZonesListInput takes no arguments: the project is fixed by the request,
// never chosen by the model.
type ZonesListInput struct{}

// ZonesListOutput is the fleet view, worst first.
type ZonesListOutput struct {
	Zones []ZoneSummary `json:"zones"`
}

// ZonesGetInput names one zone.
type ZonesGetInput struct {
	Name string `json:"name" jsonschema:"DNSZone object name, e.g. \"example-com\""`
}

// ZoneView is a DNSZone's identity and status, without its spec.
type ZoneView struct {
	Domain      string          `json:"domain"`
	Conditions  []ConditionView `json:"conditions"`
	RecordCount int             `json:"recordCount"`
	// Nameservers is what Datum assigned. Observed is what the registrar
	// actually publishes, or empty if that has never been checked.
	Nameservers  []string `json:"nameservers,omitempty"`
	Observed     []string `json:"observedNameservers,omitempty"`
	Delegation   string   `json:"delegation"`
	DomainLinked bool     `json:"domainLinked"`
}

// ZonesGetOutput is the raw condition and delegation view for one zone.
type ZonesGetOutput struct {
	Zone ZoneView `json:"zone"`
}

// ZoneDiagnoseInput names the zone to diagnose.
type ZoneDiagnoseInput struct {
	Name string `json:"name" jsonschema:"DNSZone object name, e.g. \"example-com\""`
}

// DelegationCheckInput names the zone whose delegation to check.
type DelegationCheckInput struct {
	Name string `json:"name" jsonschema:"DNSZone object name, e.g. \"example-com\""`
}

// NameserverStatus is one assigned nameserver and whether the registrar
// actually publishes it.
type NameserverStatus struct {
	Hostname string `json:"hostname"`
	Set      bool   `json:"set"`
}

// DelegationCheckOutput is the expected-versus-observed nameserver
// comparison for one zone.
type DelegationCheckOutput struct {
	// State is Complete, Partial, Incomplete, or Unknown. Unknown means
	// the registrar has never been observed, not that delegation is
	// broken — see DomainLinked and Nameservers below for why.
	State string `json:"state"`
	// Nameservers is every nameserver Datum assigned, each marked with
	// whether the registrar was observed to publish it.
	Nameservers []NameserverStatus `json:"nameservers"`
	// DomainLinked reports whether the zone has a Domain object to check
	// delegation against at all. False here is the other reason State can
	// be Unknown, distinct from "linked but not observed yet".
	DomainLinked bool `json:"domainLinked"`
}

// RecordsListInput names the zone whose records to list.
type RecordsListInput struct {
	Zone string `json:"zone" jsonschema:"DNSZone object name, e.g. \"example-com\""`
}

// RecordRow is one owner name's status within a record set.
type RecordRow struct {
	RecordSet string `json:"recordSet"`
	Type      string `json:"type"`
	OwnerName string `json:"ownerName"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	// Provenance is who created this owner name: user, platform (the
	// operator's own SOA/apex NS), gateway (AI Edge), iroh, or
	// external-dns. Anything other than user is reverted or put at risk
	// by a hand edit here — see the managed-record-refused skill.
	Provenance string `json:"provenance"`
	// ProvenanceSource names the owning object, when Provenance carries
	// one (empty for user and platform).
	ProvenanceSource string `json:"provenanceSource,omitempty"`
}

// RecordsListOutput is every owner name in the zone, worst first.
type RecordsListOutput struct {
	Records []RecordRow `json:"records"`
}

// RecordsGetInput names one record set.
type RecordsGetInput struct {
	Name string `json:"name" jsonschema:"DNSRecordSet object name, e.g. \"www-a\""`
}

// RecordEntryView is one owner name, its per-name status, and its
// provenance.
type RecordEntryView struct {
	Name             string          `json:"name"`
	Conditions       []ConditionView `json:"conditions,omitempty"`
	Provenance       string          `json:"provenance"`
	ProvenanceSource string          `json:"provenanceSource,omitempty"`
}

// RecordSetView is a DNSRecordSet's identity and status, without its values.
type RecordSetView struct {
	Name       string            `json:"name"`
	Zone       string            `json:"zone"`
	Type       string            `json:"type"`
	Conditions []ConditionView   `json:"conditions"`
	OwnerNames []RecordEntryView `json:"ownerNames"`
}

// RecordsGetOutput is the raw per-name condition view for one record set.
type RecordsGetOutput struct {
	RecordSet RecordSetView `json:"recordSet"`
}

// RecordDiagnoseInput names the record set and owner name to diagnose.
type RecordDiagnoseInput struct {
	Zone      string `json:"zone" jsonschema:"DNSZone object name, e.g. \"example-com\""`
	RecordSet string `json:"recordSet" jsonschema:"DNSRecordSet object name, e.g. \"www-a\""`
	OwnerName string `json:"ownerName" jsonschema:"Owner name within the record set, e.g. \"www\" or \"@\" for the apex"`
}

// ------------------------------------------------------------ registration

// RegisterTools adds every tool this package publishes to s. deps is
// consulted per call rather than captured once, so no caller can inherit
// another's identity or project.
func RegisterTools(s *mcp.Server, deps DepsFor) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  ToolZonesList,
		Title: "List DNS zones",
		Description: "List every DNSZone in the project with its derived status (OK, Pending, Error, " +
			"Rejected, Not owner, Conflict), delegation state (Complete, Partial, Incomplete, Unknown — " +
			"Unknown means the registrar has not been checked yet, not that it is broken), and record " +
			"count. When a zone is not OK, this includes the root-cause reason, whether it is " +
			"user-actionable, a platform fault, transient, or stalled, and how long it has held that " +
			"state. Start here. Read-only.",
	}, zonesList(deps))

	mcp.AddTool(s, &mcp.Tool{
		Name:  ToolZonesGet,
		Title: "Get zone detail",
		Description: "Get one DNSZone by name with its full condition tree, the nameservers Datum " +
			"assigned, and what the registrar actually publishes when that has been observed. A zone " +
			"can be fully Programmed and still resolve nothing if delegation is incomplete — check both " +
			"fields, do not infer one from the other. Read-only.",
	}, zonesGet(deps))

	mcp.AddTool(s, &mcp.Tool{
		Name:  ToolZoneDiagnose,
		Title: "Diagnose zone",
		Description: "Diagnose why a DNSZone is not Accepted or not Programmed. Returns the root cause " +
			"with an explanation, how long it has held that state, whether it is user-actionable, a " +
			"platform fault, transient, or stalled, concrete next steps, and which skill covers the " +
			"full procedure. This does not check delegation — a zone can be diagnosed healthy here and " +
			"still not resolve because the registrar was never pointed at Datum's nameservers; use " +
			"dns_zones_get for that. Read-only.",
	}, zoneDiagnose(deps))

	mcp.AddTool(s, &mcp.Tool{
		Name:  ToolDelegationCheck,
		Title: "Check zone delegation",
		Description: "Compare the nameservers Datum assigned to a zone against what the registrar " +
			"actually publishes, one nameserver at a time. State is Complete when every nameserver is " +
			"set, Partial when some are, Incomplete when none are, and Unknown when nothing can be " +
			"compared yet — either no Domain is linked to this zone at all, or one is linked but the " +
			"registrar has never been observed. Unknown means \"not yet checked\", never \"broken\": " +
			"reporting it as a delegation failure sends a customer to fix a registrar setting that was " +
			"simply never looked at. Read-only.",
	}, delegationCheck(deps))

	mcp.AddTool(s, &mcp.Tool{
		Name:  ToolRecordsList,
		Title: "List records in a zone",
		Description: "List every owner name across every DNSRecordSet in one zone, with each name's " +
			"status and provenance. The status comes from status.recordSets[] per owner name, never " +
			"from a record set's top-level Programmed condition, which only names the first blocked " +
			"name and rolls the rest into \"and N more\". Provenance is user, platform (the operator's " +
			"own SOA/apex NS), gateway (AI Edge), iroh, or external-dns — anything other than user is " +
			"reverted or put at risk by editing it here, so check this before suggesting any change. " +
			"Worst first. Read-only.",
	}, recordsList(deps))

	mcp.AddTool(s, &mcp.Tool{
		Name:  ToolRecordsGet,
		Title: "Get record set detail",
		Description: "Get one DNSRecordSet by name with the condition and provenance for every owner " +
			"name it holds. Use when you need the raw per-name status rather than a diagnosis. " +
			"Read-only.",
	}, recordsGet(deps))

	mcp.AddTool(s, &mcp.Tool{
		Name:  ToolRecordDiagnose,
		Title: "Diagnose a record",
		Description: "Diagnose why one owner name within a DNSRecordSet is not programmed. Matches the " +
			"name qualified and case-folded, since the backend treats \"www\", \"WWW\", and " +
			"\"www.example.com.\" as one name and status can disagree across spellings. For a Conflict, " +
			"checks whether anything else in the zone actually claims the name before blaming the " +
			"customer's record — if nothing does, this is a leftover entry on the backend that needs " +
			"an operator, not a change the customer can make. For a backend rejection (PDNSError), " +
			"classifies by the message: an invalid value or a name outside the zone is the customer's " +
			"to fix; a missing zone or an internal error is transient. Returns the root cause, whether " +
			"it is user-actionable, a platform fault, transient, or stalled, next steps, and which " +
			"skill covers the full procedure. Read-only.",
	}, recordDiagnose(deps))

	registerRecordRenderTool(s, deps)
	registerZoneDiscoveryTool(s, deps)
}

// ---------------------------------------------------------------- handlers

func zonesList(deps DepsFor) mcp.ToolHandlerFor[ZonesListInput, ZonesListOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, _ ZonesListInput,
	) (*mcp.CallToolResult, ZonesListOutput, error) {
		d, err := deps(ctx)
		if err != nil {
			return nil, ZonesListOutput{}, err
		}

		zones, err := d.Reader.ListZones(ctx, d.Namespace)
		if err != nil {
			return nil, ZonesListOutput{}, err
		}

		// One clock for the whole listing, so every row's age is measured
		// from the same instant and the rows are comparable to each other.
		now := time.Now()

		out := ZonesListOutput{Zones: make([]ZoneSummary, 0, len(zones))}
		for i := range zones {
			z := &zones[i]
			status, _ := sharedutil.ZoneStatus(z)
			delegation := sharedutil.DelegationState(z)
			summary := ZoneSummary{
				Domain:      z.Spec.DomainName,
				Status:      status,
				Delegation:  delegation.State,
				RecordCount: z.Status.RecordCount,
			}
			diagnosis := DiagnoseZoneAt(now, z)
			if diagnosis.RootCause != nil {
				summary.RootCauseReason = diagnosis.RootCause.Reason
				summary.Actionability = diagnosis.RootCause.Actionability
				summary.RootCauseFor = diagnosis.RootCause.InStateFor
			}
			out.Zones = append(out.Zones, summary)
		}

		sortNotOKFirst(out.Zones)
		return nil, out, nil
	}
}

func zonesGet(deps DepsFor) mcp.ToolHandlerFor[ZonesGetInput, ZonesGetOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, in ZonesGetInput,
	) (*mcp.CallToolResult, ZonesGetOutput, error) {
		d, err := deps(ctx)
		if err != nil {
			return nil, ZonesGetOutput{}, err
		}
		if in.Name == "" {
			return nil, ZonesGetOutput{}, fmt.Errorf("name is required")
		}

		zone, err := d.Reader.GetZone(ctx, d.Namespace, in.Name)
		if err != nil {
			return nil, ZonesGetOutput{}, err
		}
		return nil, ZonesGetOutput{Zone: toZoneView(zone)}, nil
	}
}

func zoneDiagnose(deps DepsFor) mcp.ToolHandlerFor[ZoneDiagnoseInput, Diagnosis] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, in ZoneDiagnoseInput,
	) (*mcp.CallToolResult, Diagnosis, error) {
		d, err := deps(ctx)
		if err != nil {
			return nil, Diagnosis{}, err
		}
		if in.Name == "" {
			return nil, Diagnosis{}, fmt.Errorf("name is required")
		}

		zone, err := d.Reader.GetZone(ctx, d.Namespace, in.Name)
		if err != nil {
			return nil, Diagnosis{}, err
		}
		return nil, DiagnoseZone(zone), nil
	}
}

func delegationCheck(deps DepsFor) mcp.ToolHandlerFor[DelegationCheckInput, DelegationCheckOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, in DelegationCheckInput,
	) (*mcp.CallToolResult, DelegationCheckOutput, error) {
		d, err := deps(ctx)
		if err != nil {
			return nil, DelegationCheckOutput{}, err
		}
		if in.Name == "" {
			return nil, DelegationCheckOutput{}, fmt.Errorf("name is required")
		}

		zone, err := d.Reader.GetZone(ctx, d.Namespace, in.Name)
		if err != nil {
			return nil, DelegationCheckOutput{}, err
		}

		delegation := sharedutil.DelegationState(zone)
		out := DelegationCheckOutput{
			State:        delegation.State,
			DomainLinked: delegation.Linked,
			Nameservers:  make([]NameserverStatus, 0, len(delegation.Expected)),
		}
		for _, ns := range delegation.Expected {
			out.Nameservers = append(out.Nameservers, NameserverStatus{Hostname: ns, Set: delegation.IsSet(ns)})
		}
		return nil, out, nil
	}
}

func recordsList(deps DepsFor) mcp.ToolHandlerFor[RecordsListInput, RecordsListOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, in RecordsListInput,
	) (*mcp.CallToolResult, RecordsListOutput, error) {
		d, err := deps(ctx)
		if err != nil {
			return nil, RecordsListOutput{}, err
		}
		if in.Zone == "" {
			return nil, RecordsListOutput{}, fmt.Errorf("zone is required")
		}

		zone, err := d.Reader.GetZone(ctx, d.Namespace, in.Zone)
		if err != nil {
			return nil, RecordsListOutput{}, err
		}
		sets, err := d.Reader.ListRecordSets(ctx, d.Namespace, in.Zone)
		if err != nil {
			return nil, RecordsListOutput{}, err
		}

		out := RecordsListOutput{}
		for i := range sets {
			rs := &sets[i]
			for _, entry := range rs.Spec.Records {
				word, detail := sharedutil.RecordStatusInZone(rs, entry.Name, zone.Spec.DomainName)
				ownership := sharedutil.ClassifyOwnership(rs.Labels, rs.Spec.RecordType, entry.Name, zone.Spec.DomainName)
				out.Records = append(out.Records, RecordRow{
					RecordSet:        rs.Name,
					Type:             string(rs.Spec.RecordType),
					OwnerName:        entry.Name,
					Status:           word,
					Detail:           detail,
					Provenance:       string(ownership.Provenance),
					ProvenanceSource: ownership.Source,
				})
			}
		}

		sortWorstFirst(out.Records)
		return nil, out, nil
	}
}

func recordsGet(deps DepsFor) mcp.ToolHandlerFor[RecordsGetInput, RecordsGetOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, in RecordsGetInput,
	) (*mcp.CallToolResult, RecordsGetOutput, error) {
		d, err := deps(ctx)
		if err != nil {
			return nil, RecordsGetOutput{}, err
		}
		if in.Name == "" {
			return nil, RecordsGetOutput{}, fmt.Errorf("name is required")
		}

		rs, err := d.Reader.GetRecordSet(ctx, d.Namespace, in.Name)
		if err != nil {
			return nil, RecordsGetOutput{}, err
		}

		var zoneDomain string
		if rs.Spec.DNSZoneRef.Name != "" {
			zone, err := d.Reader.GetZone(ctx, d.Namespace, rs.Spec.DNSZoneRef.Name)
			if err != nil {
				return nil, RecordsGetOutput{}, err
			}
			zoneDomain = zone.Spec.DomainName
		}
		return nil, RecordsGetOutput{RecordSet: toRecordSetView(rs, zoneDomain)}, nil
	}
}

func recordDiagnose(deps DepsFor) mcp.ToolHandlerFor[RecordDiagnoseInput, Diagnosis] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, in RecordDiagnoseInput,
	) (*mcp.CallToolResult, Diagnosis, error) {
		d, err := deps(ctx)
		if err != nil {
			return nil, Diagnosis{}, err
		}
		if in.Zone == "" || in.RecordSet == "" || in.OwnerName == "" {
			return nil, Diagnosis{}, fmt.Errorf("zone, recordSet, and ownerName are all required")
		}

		zone, err := d.Reader.GetZone(ctx, d.Namespace, in.Zone)
		if err != nil {
			return nil, Diagnosis{}, err
		}
		siblings, err := d.Reader.ListRecordSets(ctx, d.Namespace, in.Zone)
		if err != nil {
			return nil, Diagnosis{}, err
		}
		target, err := d.Reader.GetRecordSet(ctx, d.Namespace, in.RecordSet)
		if err != nil {
			return nil, Diagnosis{}, err
		}
		return nil, DiagnoseRecord(zone, siblings, target, in.OwnerName), nil
	}
}

// ----------------------------------------------------------------- helpers

// sortNotOKFirst puts unhealthy zones at the top. Stable, so rows in the
// same state keep the reader's order and output stays reproducible.
func sortNotOKFirst(rows []ZoneSummary) {
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].Status != sharedutil.StatusOK && rows[j].Status == sharedutil.StatusOK
	})
}

// sortWorstFirst orders record rows by the same status severity RecordStatus
// itself uses, so the worst row for the whole zone leads without needing its
// own scale.
func sortWorstFirst(rows []RecordRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		return recordRowSeverity(rows[i].Status) > recordRowSeverity(rows[j].Status)
	})
}

func recordRowSeverity(status string) int {
	switch status {
	case sharedutil.StatusRejected:
		return 70
	case sharedutil.StatusError:
		return 60
	case sharedutil.StatusConflict:
		return 50
	case sharedutil.StatusNotOwner:
		return 40
	case sharedutil.StatusPending:
		return 20
	case sharedutil.StatusProgrammed:
		return 10
	case sharedutil.StatusUnknown:
		return 5
	default:
		return 30
	}
}

func toConditionViews(conditions []metav1.Condition) []ConditionView {
	out := make([]ConditionView, 0, len(conditions))
	for _, c := range conditions {
		out = append(out, ConditionView{
			Type:    c.Type,
			Status:  string(c.Status),
			Reason:  c.Reason,
			Message: c.Message,
		})
	}
	return out
}

func toZoneView(z *dnsv1alpha1.DNSZone) ZoneView {
	delegation := sharedutil.DelegationState(z)
	view := ZoneView{
		Domain:       z.Spec.DomainName,
		Conditions:   toConditionViews(z.Status.Conditions),
		RecordCount:  z.Status.RecordCount,
		Nameservers:  delegation.Expected,
		Observed:     delegation.Observed,
		Delegation:   delegation.State,
		DomainLinked: delegation.Linked,
	}
	return view
}

func toRecordSetView(rs *dnsv1alpha1.DNSRecordSet, zoneDomain string) RecordSetView {
	view := RecordSetView{
		Name:       rs.Name,
		Zone:       rs.Spec.DNSZoneRef.Name,
		Type:       string(rs.Spec.RecordType),
		Conditions: toConditionViews(rs.Status.Conditions),
		OwnerNames: make([]RecordEntryView, 0, len(rs.Status.RecordSets)),
	}
	for _, st := range rs.Status.RecordSets {
		ownership := sharedutil.ClassifyOwnership(rs.Labels, rs.Spec.RecordType, st.Name, zoneDomain)
		view.OwnerNames = append(view.OwnerNames, RecordEntryView{
			Name:             st.Name,
			Conditions:       toConditionViews(st.Conditions),
			Provenance:       string(ownership.Provenance),
			ProvenanceSource: ownership.Source,
		})
	}
	return view
}
