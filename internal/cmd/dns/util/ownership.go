// SPDX-License-Identifier: AGPL-3.0-only

package util

import "strings"

// Labels the Gateway DNS controller stamps on every DNSRecordSet it creates.
//
// Grepping this repository for them finds only this file, which makes them look
// invented — a conclusion that has genuinely been reached once already. They
// come from a different repository: datum-cloud/network-services-operator,
// internal/controller/gateway_dns_controller.go. There is no Gateway controller
// here and there never will be.
//
// Note that the version this module pins, network-services-operator v0.9.0,
// does not contain that file at all; it first appears around v0.17.0. So the
// labels cannot be verified against the dependency in go.mod, only against the
// producer's own repository. Citing the functions rather than line numbers is
// deliberate for the same reason.
// These are exported because MachineOwned takes a raw label map, so a caller
// has to name the keys to build one. Left unexported, every caller would keep
// its own copy of exactly the strings the shared matcher decides on — which is
// the duplication this file exists to remove, moved from the logic to the keys.
// A caller's test building a map from its own constants would keep passing
// after a rename here, against keys MachineOwned no longer recognises.
const (
	LabelManagedBy       = "app.kubernetes.io/managed-by"
	LabelDNSManaged      = "dns.datumapis.com/managed"
	LabelSourceKind      = "dns.datumapis.com/source-kind"
	LabelSourceName      = "dns.datumapis.com/source-name"
	LabelSourceNamespace = "dns.datumapis.com/source-namespace"

	ValueManagedByNetworking = "networking.datumapis.com"
	ValueDNSManaged          = "true"
	ValueSourceKindGateway   = "Gateway"
)

// MachineOwned reports whether a DNSRecordSet's labels mark it as owned by the
// Gateway DNS controller, and returns the owning Gateway's identity.
//
// The label constants above and this function are one unit. The matcher means
// nothing without the keys it matches on, and a key changed without looking at
// the matcher — or the reverse — is the failure this consolidation exists to
// prevent. Keep them adjacent and change them together.
//
// source is "namespace/name", or just "name" when no namespace label is set,
// or empty when the set is unlabelled. Callers phrase their own sentence around
// it; this function reports the fact, not the wording.
//
// # Why three labels, not one
//
// Any one of source-kind, dns.datumapis.com/managed, or
// app.kubernetes.io/managed-by is enough. The producer writes five labels
// together (ensureDNSRecordSets), but its own garbage collector
// (garbageCollectDNSRecordSets) lists by managed, managed-by, source-name and
// source-namespace — pointedly NOT by source-kind — and a separate conflict
// check lists on managed alone. So source-kind is not a key the producer treats
// as load-bearing, and a rule resting on it alone would fail OPEN the day it
// stopped being written. Failing open here means silently permitting an edit
// that a controller will revert, handing the user a success report for a change
// that quietly disappears.
//
// That reasoning was verified against the producer's source rather than assumed
// — the GC really does select on four labels and omit source-kind. It is still
// a statement about a repository this one does not control, so it can change
// without warning; the three-label OR is what makes that survivable, because
// any single label going away leaves the other two.
//
// # Why a label map rather than an object
//
// So it is callable from anywhere and testable without constructing a
// DNSRecordSet. A nil map is not owned, which is the safe answer for an object
// that carries no labels at all.
//
// This is the plugin's only ownership test built on a fact the producer asserts
// rather than on an inference from a record's shape, and it is consequently the
// only one immune to the owner-name spelling class of bugs — it never compares
// a name. Prefer it over re-deriving the rule; it existed at two different
// strengths in two packages before it lived here, and the weaker copy guarded
// the bulk path.
func MachineOwned(labels map[string]string) (owned bool, source string) {
	if len(labels) == 0 {
		return false, ""
	}

	owned = strings.EqualFold(labels[LabelSourceKind], ValueSourceKindGateway) ||
		strings.EqualFold(labels[LabelDNSManaged], ValueDNSManaged) ||
		strings.EqualFold(labels[LabelManagedBy], ValueManagedByNetworking)
	if !owned {
		return false, ""
	}

	// The namespace is included when set because the producer's GC pairs name
	// with namespace: two Gateways in different namespaces can share a name, so
	// a bare name would identify more than one object.
	name := labels[LabelSourceName]
	if name == "" {
		return true, ""
	}
	if ns := labels[LabelSourceNamespace]; ns != "" {
		return true, ns + "/" + name
	}
	return true, name
}
