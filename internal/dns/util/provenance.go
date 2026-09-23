// SPDX-License-Identifier: AGPL-3.0-only

package util

import dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"

// Labels the iroh DNS controller (network-services-operator's
// iroh_dns_controller.go) stamps on every DNSRecordSet it creates for a
// connector endpoint. Verified against that controller's source rather than
// assumed: it also sets app.kubernetes.io/managed-by to the same
// ValueManagedByNetworking value the Gateway DNS controller uses, which is
// why iroh must be checked for BEFORE falling back to the three-label
// Gateway rule below — otherwise every iroh-owned set would be misreported
// as Gateway-owned, with no source name to show for it.
const (
	LabelIrohConnectorName      = "networking.datumapis.com/iroh-dns-connector-name"
	LabelIrohConnectorNamespace = "networking.datumapis.com/iroh-dns-connector-namespace"
)

// Labels the ExternalDNS webhook (datum-cloud/external-dns-webhook,
// internal/provider/recordset.go) stamps on every DNSRecordSet it manages.
// Verified against that repository's source.
const (
	LabelExternalDNSManagedBy = "external-dns.io/managed-by"
	ValueExternalDNSManagedBy = "datum-cloud-webhook"
	LabelExternalDNSOwner     = "external-dns.io/owner"
)

// Provenance is who created a record set and therefore who may change it.
type Provenance string

const (
	// ProvenanceUser is an ordinary record: free to edit.
	ProvenanceUser Provenance = "user"
	// ProvenancePlatform is the operator's own SOA or apex NS. The API
	// allows editing it and the operator never reconciles the content
	// back, but it is still relied on — see IsPlatformShape.
	ProvenancePlatform Provenance = "platform"
	// ProvenanceGateway is a set the Gateway DNS controller owns, surfaced
	// to customers as AI Edge. Hand edits are reverted.
	ProvenanceGateway Provenance = "gateway"
	// ProvenanceIroh is a set the iroh connector owns. Hand edits are
	// reverted.
	ProvenanceIroh Provenance = "iroh"
	// ProvenanceExternalDNS is a set the ExternalDNS webhook owns. Hand
	// edits are reverted.
	ProvenanceExternalDNS Provenance = "external-dns"
)

// Managed reports whether p is any producer other than the customer's own
// project — the tier that a direct edit either gets reverted or put at risk.
func (p Provenance) Managed() bool { return p != ProvenanceUser }

// Ownership is the result of classifying one owner name within a
// DNSRecordSet: who owns it, and the identity to name when refusing an edit.
type Ownership struct {
	Provenance Provenance
	// Source names the owning object, "namespace/name" when both are
	// known, "name" when only one is, or empty when Provenance carries no
	// identity of its own (ProvenanceUser, ProvenancePlatform).
	Source string
}

// ClassifyOwnership decides who owns one entry within a record set.
//
// The Gateway, iroh, and ExternalDNS cases are read off labels the
// producing controller stamps; the platform case is IsPlatformShape's
// heuristic. Checked in this order because iroh and Gateway share one label
// value (see the iroh label comment above) and because a set can be shape
// eligible for the platform tier while genuinely belonging to a producer —
// labels are always the stronger signal.
func ClassifyOwnership(labels map[string]string, t dnsv1alpha1.RRType, ownerName, zoneDomain string) Ownership {
	if len(labels) > 0 {
		if labels[LabelExternalDNSManagedBy] == ValueExternalDNSManagedBy {
			return Ownership{Provenance: ProvenanceExternalDNS, Source: labels[LabelExternalDNSOwner]}
		}
		if name := labels[LabelIrohConnectorName]; name != "" {
			if ns := labels[LabelIrohConnectorNamespace]; ns != "" {
				return Ownership{Provenance: ProvenanceIroh, Source: ns + "/" + name}
			}
			return Ownership{Provenance: ProvenanceIroh, Source: name}
		}
		if owned, source := MachineOwned(labels); owned {
			return Ownership{Provenance: ProvenanceGateway, Source: source}
		}
	}
	if IsPlatformShape(t, ownerName, zoneDomain) {
		return Ownership{Provenance: ProvenancePlatform}
	}
	return Ownership{Provenance: ProvenanceUser}
}

// IsPlatformShape reports whether a (type, owner name) pair is one the
// platform creates and depends on: a zone's SOA, or its apex NS records.
//
// It is shape-based rather than object-based deliberately: the operator
// stamps no label on these two record sets, so membership can only be
// tested by what a set IS, never by what it is named or who is asking. See
// the equivalent reasoning historically kept alongside the CLI's copy of
// this function (internal/cmd/dns/record/managed.go, before it moved here).
func IsPlatformShape(t dnsv1alpha1.RRType, ownerName, zoneDomain string) bool {
	switch t {
	case dnsv1alpha1.RRTypeSOA:
		// A zone has exactly one SOA and the platform depends on it
		// whatever object happens to hold it.
		return true
	case dnsv1alpha1.RRTypeNS:
		return qualifyOwner(ownerName, zoneDomain) == qualifyOwner("@", zoneDomain)
	default:
		return false
	}
}
