// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"fmt"
	"io"
	"strings"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// Labels the Gateway controller stamps on the record sets it owns. They are the
// only provenance marker anywhere in the system; the operator's own SOA and NS
// sets carry nothing, which is why those are recognised by shape instead.
//
// Nothing in THIS repository writes them, and nothing in the pinned dependency
// does either — network-services-operator is pinned at v0.9.0 here and the file
// that sets them does not exist until v0.17.0, so following a line number into
// the module cache finds nothing and invites the conclusion that they are
// invented. They are not. The producer is network-services-operator's Gateway
// DNS controller: ensureDNSRecordSets writes all five together, and
// garbageCollectDNSRecordSets reads four of them back. Named by function rather
// than by line so the reference survives the next release.
//
// The keys and values themselves live in util, exported precisely so no caller
// keeps its own copy of the strings the shared matcher decides on. Only the
// source kind and namespace are read here at all, and only to word a refusal;
// the ownership decision itself is util.MachineOwned.

// provenance is who created a record and therefore who may change it.
type provenance int

const (
	// provUser is an ordinary record: free to edit.
	provUser provenance = iota
	// provPlatform is the operator's own SOA or apex NS. Editing is permitted
	// with --force, because the API allows it and the operator never reconciles
	// the content back — but apex NS is delegation, so it is not silent.
	provPlatform
	// provGateway is a set the Gateway controller owns. Editing it fights a
	// controller that reverts the change, so it is refused outright.
	provGateway
)

// Markers rendered in the STATUS column of `record list`.
const (
	markerPlatform = "(platform)"
	markerGateway  = "(managed by AI Edge)"
)

// classify decides who owns one entry within a record set.
//
// The Gateway case is a fact read off a label. The platform case is a heuristic
// — the operator stamps nothing, it creates `<zone>-soa` and `<zone>-ns` and
// relies on their existence — but the heuristic is on SHAPE, not on the object
// name. A provenance label on those two would replace the guess entirely and is
// worth adding to the operator.
func classify(rs *dnsv1alpha1.DNSRecordSet, entry dnsv1alpha1.RecordEntry, zoneDomain string) provenance {
	if rs == nil {
		return provUser
	}
	if isMachineOwned(rs) {
		return provGateway
	}
	if isPlatformShape(rs.Spec.RecordType, entry.Name, zoneDomain) {
		return provPlatform
	}
	return provUser
}

// isPlatformShape reports whether a (type, owner name) pair is one the platform
// creates and depends on. It is the single definition of the platform tier, and
// both the display path (classify) and the guard path (platformRisk) go through
// it so the two cannot answer differently for the same record.
//
// Protection rests on the two facts about a record that a third party cannot
// spell differently: which zone it belongs to, and its shape. Membership is
// spec.dnsZoneRef, a reference the OPERATOR sets, and it is established before
// this is ever called — listSets is the only way a DNSRecordSet enters this
// package and it always selects on that field, so every set reaching here is
// already known to be this zone's. Shape is the type plus the qualified owner
// name, which is what the backend itself keys an RRset on.
//
// Two things it deliberately does NOT look at, because both were tried and both
// were violated in practice:
//
// The object's name. Gating SOA on `<zone>-soa` left an SOA set created under
// any other name unprotected, and ensureSOARecordSet skips creating its own
// whenever ANY SOA set already exists — so a zone that got its SOA from
// somewhere else keeps a permanently unprotected one, and `apply --prune`
// deleted it at exit 0. An object name is a spelling; dnsZoneRef is a fact.
//
// The literal spelling of the owner name. rdata.IsApex tests the string, so an
// apex NS entry stored as "example.com." rather than "@" was not recognised and
// its delegation was pruned away. IsApexIn puts both sides through FQDN, the
// same rule pdns.QualifyOwner applies.
func isPlatformShape(t dnsv1alpha1.RRType, ownerName, zoneDomain string) bool {
	switch t {
	case dnsv1alpha1.RRTypeSOA:
		// A zone has exactly one SOA and the platform depends on it whatever
		// object happens to hold it.
		return true
	case dnsv1alpha1.RRTypeNS:
		return rdata.IsApexIn(ownerName, zoneDomain)
	default:
		return false
	}
}

// isMachineOwned reports whether a controller owns this record set.
//
// The three-label rule this package established now lives in util.MachineOwned,
// so it is one definition shared with zone import rather than the better of two
// implementations. The reasoning is recorded there.
func isMachineOwned(rs *dnsv1alpha1.DNSRecordSet) bool {
	if rs == nil {
		return false
	}
	owned, _ := util.MachineOwned(rs.Labels)
	return owned
}

// marker is the parenthetical `record list` appends to a managed row.
func (p provenance) marker() string {
	switch p {
	case provGateway:
		return markerGateway
	case provPlatform:
		return markerPlatform
	default:
		return ""
	}
}

// managed reports whether --managed should keep this row.
func (p provenance) managed() bool { return p != provUser }

// gatewayOwner names the source object a set belongs to, for the error that
// refuses to edit it. It is "namespace/name" when both are known, because the
// producer's own GC pairs them: two Gateways in different namespaces can share
// a name, and `edit Gateway "web"` would otherwise name several objects at once.
//
// Every caller is already inside an ownership check, so the empty string
// MachineOwned returns for an unowned set is never rendered.
func gatewayOwner(rs *dnsv1alpha1.DNSRecordSet) string {
	if rs == nil {
		return ""
	}
	_, source := util.MachineOwned(rs.Labels)
	return source
}

// sourceKind names what kind of object owns a set, for the wording of the
// refusal. util.MachineOwned reports the fact and the source's identity but not
// its kind, so the label is read directly here.
//
// The fallback to the generic word matters: a set owned via managed-by alone
// genuinely has no kind to name, and asserting "Gateway" without evidence is
// precisely what the three-label rule exists to avoid doing in the other
// direction.
func sourceKind(rs *dnsv1alpha1.DNSRecordSet) string {
	if rs == nil {
		return "controller"
	}
	if kind := rs.Labels[util.LabelSourceKind]; kind != "" {
		return kind
	}
	return "controller"
}

// guardMutation enforces the two managed-record tiers before a write.
//
// Gateway-owned sets are read-only: the controller reverts anything written
// here, so a "success" would be a lie. SOA and apex NS are warned instead of
// blocked — the API permits the edit and the operator will not undo it, so the
// user is allowed to proceed with --force once the risk has been named.
func guardMutation(warnTo io.Writer, rs *dnsv1alpha1.DNSRecordSet, zone *dnsv1alpha1.DNSZone, t dnsv1alpha1.RRType, ownerName string, force bool) error {
	if isMachineOwned(rs) {
		msg := fmt.Sprintf("the %s records for %s are managed by AI Edge and are read-only", t, ownerDisplay(ownerName, zone.Spec.DomainName))
		fix := "edit the object that owns them — a change made here is reverted by the controller."
		if owner := gatewayOwner(rs); owner != "" {
			fix = fmt.Sprintf("edit %s %q — a change made here is reverted by the controller.", sourceKind(rs), owner)
		}
		return util.NewCLIError(util.ExitConflict, msg).WithFix(fix)
	}

	risk := platformRisk(rs, zone, t, ownerName)
	if risk == "" {
		return nil
	}
	if !force {
		return util.UsageErrorf("%s is a platform-managed record", recordDisplay(ownerName, t, zone.Spec.DomainName)).
			WithFix(risk + " — re-run with --force if that is what you want.")
	}
	fmt.Fprintf(warnTo, "Warning: %s is platform-managed; %s\n", recordDisplay(ownerName, t, zone.Spec.DomainName), risk)
	return nil
}

// platformRisk returns the sentence naming what breaks, or "" when the write
// touches nothing platform-managed. It is deliberately shape-based rather than
// set-based: creating the first apex NS record in a zone is as risky as editing
// the operator's, and the object may not exist yet to be classified.
func platformRisk(rs *dnsv1alpha1.DNSRecordSet, zone *dnsv1alpha1.DNSZone, t dnsv1alpha1.RRType, ownerName string) string {
	if !isPlatformShape(t, ownerName, zone.Spec.DomainName) {
		// A set the display path calls platform-managed for some other reason
		// still warns, so the two can never disagree in the direction that
		// permits a write.
		if rs != nil && classify(rs, dnsv1alpha1.RecordEntry{Name: ownerName}, zone.Spec.DomainName) == provPlatform {
			return "this record is created and relied on by the platform"
		}
		return ""
	}

	switch t {
	case dnsv1alpha1.RRTypeSOA:
		return "editing the SOA record can break zone transfers and negative caching"
	case dnsv1alpha1.RRTypeNS:
		return "editing apex NS records can break delegation"
	default:
		return "this record is created and relied on by the platform"
	}
}

// ownerDisplay renders an owner name the way a user reads it back: apex as the
// bare domain, everything else fully qualified.
func ownerDisplay(ownerName, zoneDomain string) string {
	// IsApexIn rather than IsApex even though this is display and the two agree
	// here: the zone is in hand, and managed.go is the file where a reader looks
	// to see how the apex is tested. Leaving a literal test in it invites the
	// next guard to be written the same way, which is how four of them ended up
	// failing open.
	if rdata.IsApexIn(ownerName, zoneDomain) {
		return zoneDomain
	}
	return strings.TrimSuffix(rdata.FQDN(ownerName, zoneDomain), ".")
}

// recordDisplay names a (name, type) pair in an error or a prompt.
func recordDisplay(ownerName string, t dnsv1alpha1.RRType, zoneDomain string) string {
	return fmt.Sprintf("the %s record for %s", t, ownerDisplay(ownerName, zoneDomain))
}
