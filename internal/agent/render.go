// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	sigsyaml "sigs.k8s.io/yaml"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	sharedutil "go.miloapis.com/dns-operator/internal/dns/util"
)

// ToolRecordRender turns a short description of a record into a complete
// DNSRecordSet manifest for the customer to review.
//
// This repo publishes no tool that writes. The assistant's own base tools
// validate, plan and apply manifests of any kind as the caller, behind a
// plan token and an explicit confirmation — DNS has the strongest case of
// any service for keeping that boundary, since a bad record takes effect
// globally in seconds and stays cached well past the fix. Rendering is the
// DNS-specific half a model cannot safely guess at on its own: which record
// types this operator accepts, that one DNSRecordSet holds every owner name
// of one type in a zone, and that a CNAME cannot share a name with anything
// else. Render is read-only in the sense that matters — it writes nothing —
// but it does read the target zone and its existing record sets, because a
// render that cannot say whether it is creating a new object or one that
// should instead update an existing one is not a preview a customer can
// trust.
const ToolRecordRender = "dns_record_render"

// renderableTypes are the record types render accepts. Not every type
// api/v1alpha1 defines is here: ALIAS, PTR, TLSA, HTTPS, SVCB and SOA are
// either provider-specific, rare enough in customer requests to not be
// worth the schema surface yet, or — SOA — never customer-authored at all.
// A type outside this list is refused with a clear message rather than
// guessed at.
var renderableTypes = []dnsv1alpha1.RRType{
	dnsv1alpha1.RRTypeA,
	dnsv1alpha1.RRTypeAAAA,
	dnsv1alpha1.RRTypeCNAME,
	dnsv1alpha1.RRTypeTXT,
	dnsv1alpha1.RRTypeMX,
	dnsv1alpha1.RRTypeSRV,
	dnsv1alpha1.RRTypeCAA,
	dnsv1alpha1.RRTypeNS,
}

func isRenderable(t dnsv1alpha1.RRType) bool {
	for _, rt := range renderableTypes {
		if rt == t {
			return true
		}
	}
	return false
}

func renderableTypeNames() []string {
	out := make([]string, len(renderableTypes))
	for i, t := range renderableTypes {
		out[i] = string(t)
	}
	return out
}

// ---------------------------------------------------------------- I/O types

// RecordRenderMX is an MX record's value.
type RecordRenderMX struct {
	Preference uint16 `json:"preference" jsonschema:"Lower values are preferred by mail senders."`
	Exchange   string `json:"exchange" jsonschema:"Mail server hostname."`
}

// RecordRenderSRV is an SRV record's value.
type RecordRenderSRV struct {
	Priority uint16 `json:"priority" jsonschema:"Lower values are preferred."`
	Weight   uint16 `json:"weight" jsonschema:"Relative weight among records of the same priority."`
	Port     uint16 `json:"port"`
	Target   string `json:"target" jsonschema:"Hostname of the service."`
}

// RecordRenderCAA is a CAA record's value.
type RecordRenderCAA struct {
	Flag  uint8  `json:"flag" jsonschema:"0 for a non-critical record, 128 to mark it critical."`
	Tag   string `json:"tag" jsonschema:"issue, issuewild, or iodef."`
	Value string `json:"value"`
}

// RecordRenderInput is the flat description a manifest is rendered from.
type RecordRenderInput struct {
	Zone      string `json:"zone" jsonschema:"DNSZone object name, e.g. \"example-com\""`
	OwnerName string `json:"ownerName" jsonschema:"Owner name, e.g. \"www\" or \"@\" for the apex."`
	Type      string `json:"type" jsonschema:"One of A, AAAA, CNAME, TXT, MX, SRV, CAA, NS."`
	TTL       *int64 `json:"ttl,omitempty" jsonschema:"Seconds. Omit to take the operator's default of 300."`
	// Content is the record's value for the single-value types: A, AAAA,
	// CNAME, NS, and TXT. Ignored for MX, SRV, and CAA, which carry their
	// own structured value below.
	Content string           `json:"content,omitempty" jsonschema:"Record value for A, AAAA, CNAME, NS, or TXT. Required for those types; ignored otherwise."`
	MX      *RecordRenderMX  `json:"mx,omitempty" jsonschema:"Required, and only meaningful, when type is MX."`
	SRV     *RecordRenderSRV `json:"srv,omitempty" jsonschema:"Required, and only meaningful, when type is SRV."`
	CAA     *RecordRenderCAA `json:"caa,omitempty" jsonschema:"Required, and only meaningful, when type is CAA."`
}

// RecordRenderOutput is the manifest and what rendering it found.
type RecordRenderOutput struct {
	// Manifest is the complete DNSRecordSet, as YAML.
	Manifest string `json:"manifest"`
	// ObjectName is the manifest's metadata.name, named separately since
	// it is what Warnings and Notes refer back to.
	ObjectName string `json:"objectName"`
	// Warnings are reasons the API is likely to reject this once applied,
	// discovered by reading the zone's other records — never invented
	// without checking.
	Warnings []string `json:"warnings,omitempty"`
	// Notes explain what rendering settled: the TTL that will apply, and
	// whether this creates a new object or should instead update one that
	// already exists.
	Notes []string `json:"notes,omitempty"`
}

// ------------------------------------------------------------ registration

func registerRecordRenderTool(s *mcp.Server, deps DepsFor) {
	mcp.AddTool(s, &mcp.Tool{
		Name:  ToolRecordRender,
		Title: "Render a DNS record manifest",
		Description: "Turn a short description of a record — zone, owner name, type, value — into a " +
			"complete DNSRecordSet manifest, and report what rendering found. Supports A, AAAA, CNAME, " +
			"TXT, MX, SRV, CAA, and NS; any other type is refused. Writes nothing, but does read the " +
			"target zone and its existing record sets: one DNSRecordSet holds every owner name of one " +
			"record type in a zone, so the notes say whether this manifest creates a new object or " +
			"should instead be merged into one that already exists — read that before handing the " +
			"manifest to resources_plan, since planning a second object of the same type does not merge " +
			"with the first, it conflicts with it. For CNAME, the warnings say when the name is the " +
			"zone apex or already used by something else, either of which the API will reject. Load the " +
			"record-create skill before using this. Read-only in that it writes nothing itself; the " +
			"manifest is then passed to resources_plan and, once the person agrees, resources_apply.",
	}, recordRender(deps))
}

// ---------------------------------------------------------------- handlers

func recordRender(deps DepsFor) mcp.ToolHandlerFor[RecordRenderInput, RecordRenderOutput] {
	return func(
		ctx context.Context, _ *mcp.CallToolRequest, in RecordRenderInput,
	) (*mcp.CallToolResult, RecordRenderOutput, error) {
		d, err := deps(ctx)
		if err != nil {
			return nil, RecordRenderOutput{}, err
		}
		if in.Zone == "" || in.OwnerName == "" || in.Type == "" {
			return nil, RecordRenderOutput{}, fmt.Errorf("zone, ownerName, and type are all required")
		}

		t := dnsv1alpha1.RRType(strings.ToUpper(in.Type))
		if !isRenderable(t) {
			return nil, RecordRenderOutput{}, fmt.Errorf(
				"type %q is not supported by render; supported types are %s",
				in.Type, strings.Join(renderableTypeNames(), ", "))
		}

		entry, err := toRecordEntry(t, in)
		if err != nil {
			return nil, RecordRenderOutput{}, err
		}

		zone, err := d.Reader.GetZone(ctx, d.Namespace, in.Zone)
		if err != nil {
			return nil, RecordRenderOutput{}, err
		}
		siblings, err := d.Reader.ListRecordSets(ctx, d.Namespace, in.Zone)
		if err != nil {
			return nil, RecordRenderOutput{}, err
		}

		objectName := fmt.Sprintf("%s-%s", zone.Name, strings.ToLower(string(t)))
		obj := &dnsv1alpha1.DNSRecordSet{
			TypeMeta: metav1.TypeMeta{
				APIVersion: dnsv1alpha1.GroupVersion.String(),
				Kind:       "DNSRecordSet",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      objectName,
				Namespace: zone.Namespace,
			},
			Spec: dnsv1alpha1.DNSRecordSetSpec{
				DNSZoneRef: corev1.LocalObjectReference{Name: zone.Name},
				RecordType: t,
				Records:    []dnsv1alpha1.RecordEntry{entry},
			},
		}

		manifest, err := marshalRecordSetYAML(obj)
		if err != nil {
			return nil, RecordRenderOutput{}, err
		}

		out := RecordRenderOutput{
			Manifest:   manifest,
			ObjectName: objectName,
			Warnings:   renderWarnings(t, in.OwnerName, zone.Spec.DomainName),
			Notes:      renderNotesForRecord(t, in.TTL, objectName, siblings),
		}
		return nil, out, nil
	}
}

// toRecordEntry builds the type-specific RecordEntry, or an error naming
// what is missing for the type given.
func toRecordEntry(t dnsv1alpha1.RRType, in RecordRenderInput) (dnsv1alpha1.RecordEntry, error) {
	entry := dnsv1alpha1.RecordEntry{Name: in.OwnerName, TTL: in.TTL}

	switch t {
	case dnsv1alpha1.RRTypeA:
		if in.Content == "" {
			return entry, fmt.Errorf("content is required for type A")
		}
		entry.A = &dnsv1alpha1.ARecordSpec{Content: in.Content}
	case dnsv1alpha1.RRTypeAAAA:
		if in.Content == "" {
			return entry, fmt.Errorf("content is required for type AAAA")
		}
		entry.AAAA = &dnsv1alpha1.AAAARecordSpec{Content: in.Content}
	case dnsv1alpha1.RRTypeCNAME:
		if in.Content == "" {
			return entry, fmt.Errorf("content is required for type CNAME")
		}
		entry.CNAME = &dnsv1alpha1.CNAMERecordSpec{Content: in.Content}
	case dnsv1alpha1.RRTypeNS:
		if in.Content == "" {
			return entry, fmt.Errorf("content is required for type NS")
		}
		entry.NS = &dnsv1alpha1.NSRecordSpec{Content: in.Content}
	case dnsv1alpha1.RRTypeTXT:
		if in.Content == "" {
			return entry, fmt.Errorf("content is required for type TXT")
		}
		entry.TXT = &dnsv1alpha1.TXTRecordSpec{Content: in.Content}
	case dnsv1alpha1.RRTypeMX:
		if in.MX == nil || in.MX.Exchange == "" {
			return entry, fmt.Errorf("mx.exchange is required for type MX")
		}
		entry.MX = &dnsv1alpha1.MXRecordSpec{Preference: in.MX.Preference, Exchange: in.MX.Exchange}
	case dnsv1alpha1.RRTypeSRV:
		if in.SRV == nil || in.SRV.Target == "" {
			return entry, fmt.Errorf("srv.target is required for type SRV")
		}
		entry.SRV = &dnsv1alpha1.SRVRecordSpec{
			Priority: in.SRV.Priority, Weight: in.SRV.Weight, Port: in.SRV.Port, Target: in.SRV.Target,
		}
	case dnsv1alpha1.RRTypeCAA:
		if in.CAA == nil || in.CAA.Tag == "" || in.CAA.Value == "" {
			return entry, fmt.Errorf("caa.tag and caa.value are required for type CAA")
		}
		entry.CAA = &dnsv1alpha1.CAARecordSpec{Flag: in.CAA.Flag, Tag: in.CAA.Tag, Value: in.CAA.Value}
	default:
		// Unreachable: isRenderable already refused anything not handled
		// above.
		return entry, fmt.Errorf("type %q is not supported by render", t)
	}
	return entry, nil
}

// renderWarnings names reasons the API is likely to reject this record,
// discovered from the zone's own domain and shape rather than guessed at.
func renderWarnings(t dnsv1alpha1.RRType, ownerName, zoneDomain string) []string {
	if t != dnsv1alpha1.RRTypeCNAME {
		return nil
	}
	var warnings []string
	if sharedutil.QualifyOwner(ownerName, zoneDomain) == sharedutil.QualifyOwner("@", zoneDomain) {
		warnings = append(warnings, "This name is the zone apex. A CNAME cannot exist at the apex — "+
			"the apex must carry the zone's SOA and NS records, and a CNAME next to them is rejected. "+
			"Use A or AAAA at the apex instead.")
	}
	warnings = append(warnings, "A CNAME cannot coexist with any other record at this name. Check "+
		"dns_records_list for this zone before applying, and remove or rename anything else already "+
		"using this name.")
	return warnings
}

// renderNotesForRecord explains what rendering settled: the TTL that will
// apply, and whether this manifest creates a new object or should instead
// be merged into one that already exists.
func renderNotesForRecord(t dnsv1alpha1.RRType, ttl *int64, objectName string, siblings []dnsv1alpha1.DNSRecordSet) []string {
	var notes []string
	if ttl == nil {
		notes = append(notes, "No TTL was given, so this record defaults to 300 seconds once applied.")
	} else {
		notes = append(notes, fmt.Sprintf("TTL is set to %d seconds.", *ttl))
	}

	for i := range siblings {
		if siblings[i].Spec.RecordType == t {
			notes = append(notes, fmt.Sprintf(
				"A %s record set already exists in this zone (%s). One DNSRecordSet holds every owner "+
					"name of one record type in a zone, so applying this manifest as a new object "+
					"conflicts with that one — read it with dns_records_get and add this owner name to "+
					"its records instead.", t, siblings[i].Name))
			return notes
		}
	}
	notes = append(notes, fmt.Sprintf(
		"No %s record set exists yet in this zone, so applying this manifest creates a new "+
			"DNSRecordSet named %q.", t, objectName))
	return notes
}

// marshalRecordSetYAML renders obj as YAML the way a person would author it:
// no status, and no explicit nulls, which metav1.ObjectMeta emits for
// creationTimestamp and similar fields left unset.
func marshalRecordSetYAML(obj *dnsv1alpha1.DNSRecordSet) (string, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return "", fmt.Errorf("marshalling record set: %w", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("normalizing record set: %w", err)
	}
	delete(doc, "status")
	pruneNulls(doc)

	data, err := sigsyaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshalling record set: %w", err)
	}
	return string(data), nil
}

// pruneNulls removes explicit nulls, which the Kubernetes object meta emits
// for creationTimestamp at every level of a manifest.
func pruneNulls(node any) {
	switch n := node.(type) {
	case map[string]any:
		for k, v := range n {
			if v == nil {
				delete(n, k)
				continue
			}
			pruneNulls(v)
		}
	case []any:
		for _, v := range n {
			pruneNulls(v)
		}
	}
}
