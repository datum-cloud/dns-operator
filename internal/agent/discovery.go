// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
)

// ToolZoneDiscoveryGet reads a DNSZoneDiscovery snapshot: what a domain
// currently serves, captured for an import or migration conversation. It is
// not a live query — see the zone-import skill for the caveats that
// deserve stating alongside it.
const ToolZoneDiscoveryGet = "dns_zone_discovery_get"

// DiscoveryEntryView is one discovered owner name, with its value rendered
// the way a person would write it — the same field labels
// dns_record_render's input schema uses for the corresponding type.
type DiscoveryEntryView struct {
	Name   string            `json:"name"`
	Fields map[string]string `json:"fields"`
}

// DiscoveredRecordSetView groups discovered entries by record type.
type DiscoveredRecordSetView struct {
	Type    string               `json:"type"`
	Records []DiscoveryEntryView `json:"records"`
}

// ZoneDiscoveryGetInput names the snapshot to read.
type ZoneDiscoveryGetInput struct {
	Name string `json:"name" jsonschema:"DNSZoneDiscovery object name."`
}

// ZoneDiscoveryGetOutput is the snapshot's discovered content.
type ZoneDiscoveryGetOutput struct {
	// Conditions includes Accepted and Discovered. Discovered=True means
	// the snapshot finished; read it before treating RecordSets as
	// complete.
	Conditions []ConditionView           `json:"conditions"`
	RecordSets []DiscoveredRecordSetView `json:"recordSets"`
}

func registerZoneDiscoveryTool(s *mcp.Server, deps DepsFor) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  ToolZoneDiscoveryGet,
		Title: "Get a zone discovery snapshot",
		Description: "Get one DNSZoneDiscovery by name: the records a domain was observed serving at " +
			"the time the snapshot was taken, grouped by type with each value's fields spelled out the " +
			"same way dns_record_render's input does. This is a one-time snapshot, not a live query — " +
			"say so if a customer asks whether it reflects the domain right now. Check the Discovered " +
			"condition before treating the record sets as complete; a snapshot that hasn't finished may " +
			"list only some of what the domain actually serves. Load the zone-import skill before using " +
			"this. Read-only.",
	}, zoneDiscoveryGet(deps))
}

func zoneDiscoveryGet(deps DepsFor) mcp.ToolHandlerFor[ZoneDiscoveryGetInput, ZoneDiscoveryGetOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, in ZoneDiscoveryGetInput,
	) (*mcp.CallToolResult, ZoneDiscoveryGetOutput, error) {
		d, err := deps(ctx)
		if err != nil {
			return nil, ZoneDiscoveryGetOutput{}, err
		}
		if in.Name == "" {
			return nil, ZoneDiscoveryGetOutput{}, fmt.Errorf("name is required")
		}

		disc, err := d.Reader.GetZoneDiscovery(ctx, d.Namespace, in.Name)
		if err != nil {
			return nil, ZoneDiscoveryGetOutput{}, err
		}

		out := ZoneDiscoveryGetOutput{
			Conditions: toConditionViews(disc.Status.Conditions),
			RecordSets: make([]DiscoveredRecordSetView, 0, len(disc.Status.RecordSets)),
		}
		for _, rs := range disc.Status.RecordSets {
			view := DiscoveredRecordSetView{Type: string(rs.RecordType)}
			for _, entry := range rs.Records {
				view.Records = append(view.Records, DiscoveryEntryView{
					Name:   entry.Name,
					Fields: fieldsToMap(rdata.Fields(rs.RecordType, entry)),
				})
			}
			out.RecordSets = append(out.RecordSets, view)
		}
		return nil, out, nil
	}
}

func fieldsToMap(fields [][2]string) map[string]string {
	if len(fields) == 0 {
		return nil
	}
	out := make(map[string]string, len(fields))
	for _, f := range fields {
		out[strings.ToLower(strings.ReplaceAll(f[0], " ", "_"))] = f[1]
	}
	return out
}
