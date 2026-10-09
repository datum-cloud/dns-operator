// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// PublicationPlan is serialized into DNSPublicationChunk payloads. It carries
// original observation deadlines so a disconnected serving agent can expire an
// endpoint without receiving a central deletion.
type PublicationPlan struct {
	ZoneUID       types.UID           `json:"zoneUID"`
	ZoneApex      string              `json:"zoneApex"`
	ContextUIDs   []types.UID         `json:"contextUIDs"`
	Owners        []PublicationOwner  `json:"owners"`
	RRsets        []PublicationRRSet  `json:"rrsets"`
	Contributions []ContributionFence `json:"contributions,omitempty"`
	Registrations []RegistrationFence `json:"registrations,omitempty"`
}

type PublicationOwner struct {
	Name            string               `json:"name"`
	RecordTypes     []dnsv1alpha1.RRType `json:"recordTypes"`
	RegistrationUID types.UID            `json:"registrationUID,omitempty"`
	StaticRecordUID types.UID            `json:"staticRecordUID,omitempty"`
}

type PublicationRRSet struct {
	Name             string                    `json:"name"`
	RecordType       dnsv1alpha1.RRType        `json:"recordType"`
	TTLSeconds       int32                     `json:"ttlSeconds"`
	Records          []dnsv1alpha1.RecordEntry `json:"records"`
	ContributionUIDs []types.UID               `json:"contributionUIDs,omitempty"`
	ValidUntil       *time.Time                `json:"validUntil,omitempty"`
}

type ContributionFence struct {
	UID        types.UID `json:"uid"`
	GrantUID   types.UID `json:"grantUID"`
	Epoch      int64     `json:"epoch"`
	Sequence   int64     `json:"sequence"`
	ValidUntil time.Time `json:"validUntil"`
}

type RegistrationFence struct {
	UID        types.UID `json:"uid"`
	Generation int64     `json:"generation"`
}

type CompileInput struct {
	Zone          *dnsv1alpha1.DNSZone
	Associations  []dnsv1alpha1.DNSZoneAssociation
	Registrations []dnsv1alpha1.DNSRegistration
	Grants        []dnsv1alpha1.DNSContributionGrant
	Contributions []dnsv1alpha1.DNSRecordContribution
	StaticRecords []dnsv1alpha1.DNSRecordSet
	Previous      *PublicationPlan
	Now           time.Time
}

type CompileResult struct {
	Plan      PublicationPlan
	Accepted  map[types.UID]bool
	Reasons   map[types.UID]string
	Sequences map[types.UID]int64
}

type ownershipKey struct {
	name   string
	rrtype dnsv1alpha1.RRType
}

// Compile builds a canonical plan and applies the same ownership and fencing
// checks even when an admission webhook was unavailable.
func Compile(in CompileInput) (CompileResult, error) {
	if in.Zone == nil || in.Zone.UID == "" {
		return CompileResult{}, fmt.Errorf("zone and API-assigned UID are required")
	}
	if in.Zone.Spec.Visibility != dnsv1alpha1.DNSZoneVisibilityPrivate {
		return CompileResult{}, fmt.Errorf("zone %s is not private", in.Zone.Name)
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result := CompileResult{Accepted: map[types.UID]bool{}, Reasons: map[types.UID]string{}, Sequences: map[types.UID]int64{}}
	result.Plan.ZoneUID = in.Zone.UID
	result.Plan.ZoneApex = canonicalName(in.Zone.Spec.DomainName)

	vpcs := map[types.UID]struct{}{}
	for i := range in.Associations {
		a := &in.Associations[i]
		accepted := apimeta.FindStatusCondition(a.Status.Conditions, "Accepted")
		consumerUID := a.Status.ResolvedResolverContextRef.UID
		if a.DeletionTimestamp.IsZero() && accepted != nil && accepted.Status == metav1.ConditionTrue && refMatches(a.Status.ResolvedDNSZoneRef, in.Zone.Name, in.Zone.UID, in.Zone.Generation) && consumerUID != "" {
			vpcs[consumerUID] = struct{}{}
		}
	}
	for uid := range vpcs {
		result.Plan.ContextUIDs = append(result.Plan.ContextUIDs, uid)
	}
	sort.Slice(result.Plan.ContextUIDs, func(i, j int) bool { return result.Plan.ContextUIDs[i] < result.Plan.ContextUIDs[j] })

	claims := map[ownershipKey]types.UID{}
	staticClaim := map[ownershipKey]types.UID{}
	validStatic := map[types.UID]bool{}
	for i := range in.StaticRecords {
		rs := &in.StaticRecords[i]
		if rs.Spec.DNSZoneRef.Name != in.Zone.Name {
			continue
		}
		valid := true
		for _, record := range rs.Spec.Records {
			if model.ValidatePrivateRecordOwner(zoneOwner(record.Name, result.Plan.ZoneApex), result.Plan.ZoneApex, string(rs.Spec.RecordType)) != nil {
				valid = false
				break
			}
		}
		if !valid {
			result.Reasons[rs.UID] = "UnsupportedPrivateRecord"
			continue
		}
		validStatic[rs.UID] = true
		for _, record := range rs.Spec.Records {
			key := ownershipKey{zoneOwner(record.Name, result.Plan.ZoneApex), rs.Spec.RecordType}
			if old, ok := claims[key]; ok && old != rs.UID {
				return result, fmt.Errorf("static ownership conflict for %s %s", key.name, key.rrtype)
			}
			claims[key], staticClaim[key] = rs.UID, rs.UID
		}
	}

	regs := map[types.UID]*dnsv1alpha1.DNSRegistration{}
	for i := range in.Registrations {
		reg := &in.Registrations[i]
		if !refMatches(reg.Spec.DNSZoneRef, in.Zone.Name, in.Zone.UID, in.Zone.Generation) {
			continue
		}
		if reg.UID == "" {
			result.Reasons[reg.UID] = "MissingUID"
			continue
		}
		name := zoneOwner(reg.Spec.Name, result.Plan.ZoneApex)
		valid := true
		for _, rt := range reg.Spec.RecordTypes {
			if model.ValidatePrivateRecordOwner(name, result.Plan.ZoneApex, string(rt)) != nil {
				valid = false
				break
			}
		}
		if !valid {
			result.Reasons[reg.UID] = "UnsupportedPrivateRecord"
			continue
		}

		for _, rt := range uniqueTypes(reg.Spec.RecordTypes) {
			key := ownershipKey{name, rt}
			if old, ok := conflictingClaim(claims, key); ok && old != reg.UID {
				result.Reasons[reg.UID] = "NameConflict"
				continue
			}
			claims[key] = reg.UID
		}
		if result.Reasons[reg.UID] == "" {
			regs[reg.UID] = reg
			result.Accepted[reg.UID] = true
			result.Plan.Registrations = append(result.Plan.Registrations, RegistrationFence{UID: reg.UID, Generation: reg.Generation})
		}
	}
	sort.Slice(result.Plan.Registrations, func(i, j int) bool { return result.Plan.Registrations[i].UID < result.Plan.Registrations[j].UID })

	keys := make([]ownershipKey, 0, len(claims))
	for key := range claims {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].name == keys[j].name {
			return keys[i].rrtype < keys[j].rrtype
		}
		return keys[i].name < keys[j].name
	})
	for _, key := range keys {
		owner := PublicationOwner{Name: key.name, RecordTypes: []dnsv1alpha1.RRType{key.rrtype}}
		if uid, ok := staticClaim[key]; ok {
			owner.StaticRecordUID = uid
		} else {
			owner.RegistrationUID = claims[key]
		}
		result.Plan.Owners = append(result.Plan.Owners, owner)
	}

	grants := map[types.UID]*dnsv1alpha1.DNSContributionGrant{}
	for i := range in.Grants {
		grant := &in.Grants[i]
		reg := regs[grant.Spec.RegistrationRef.UID]
		if reg == nil || grant.Spec.RegistrationRef.Name != reg.Name || grant.Spec.RegistrationRef.Generation != reg.Generation {
			result.Reasons[grant.UID] = "StaleRegistrationGeneration"
			continue
		}
		if grant.Status.ActiveWriterEpoch <= 0 || grant.Status.ObservedGrantGeneration != grant.Generation || grant.Status.ObservedRegistrationGeneration != reg.Generation {
			result.Reasons[grant.UID] = "WriterAuthorityPending"
			continue
		}
		grants[grant.UID] = grant
		result.Accepted[grant.UID] = true
	}

	previousSeq := map[types.UID]ContributionFence{}
	if in.Previous != nil {
		for _, f := range in.Previous.Contributions {
			f.ValidUntil = f.ValidUntil.UTC()
			previousSeq[f.UID] = f
		}
	}
	// Publication fences are lifetime high-water marks. Keep fences for absent,
	// retired, invalid, and rolled-back observations so a later reconcile or
	// controller takeover cannot resurrect an older contribution.
	publicationFences := make(map[types.UID]ContributionFence, len(previousSeq))
	for uid, fence := range previousSeq {
		publicationFences[uid] = fence
	}
	type aggregate struct {
		ttl     int32
		records []dnsv1alpha1.RecordEntry
		uids    []types.UID
		until   *time.Time
	}
	aggregates := map[ownershipKey]*aggregate{}
	for i := range in.Contributions {
		c := &in.Contributions[i]
		reg := regs[c.Spec.RegistrationRef.UID]
		grant := grants[c.Spec.GrantRef.UID]
		reason := validateContribution(c, reg, grant, previousSeq[c.UID], now)
		if reason != "" {
			result.Reasons[c.UID] = reason
			continue
		}
		until := time.Time{}
		if c.Status.ValidUntil != nil {
			until = c.Status.ValidUntil.Time.UTC()
		}
		currentFence := ContributionFence{UID: c.UID, GrantUID: grant.UID, Epoch: c.Status.WriterEpoch, Sequence: c.Status.Sequence, ValidUntil: until}
		previousFence := previousSeq[c.UID]
		if previousFence.UID == "" || currentFence.Epoch > previousFence.Epoch || currentFence.Sequence > previousFence.Sequence {
			publicationFences[c.UID] = currentFence
		}
		result.Sequences[c.UID] = c.Status.Sequence
		if !c.Status.Eligible {
			result.Reasons[c.UID] = "Ineligible"
			continue
		}
		if c.Status.ValidUntil == nil || !c.Status.ValidUntil.Time.After(now) {
			result.Reasons[c.UID] = "Expired"
			continue
		}
		validBundle := true
		for _, set := range c.Spec.RecordSets {
			if len(set.Records) == 0 || !containsType(reg.Spec.RecordTypes, set.RecordType) || !containsType(grant.Spec.RecordTypes, set.RecordType) {
				validBundle = false
				break
			}
			for _, record := range set.Records {
				key := ownershipKey{zoneOwner(record.Name, result.Plan.ZoneApex), set.RecordType}
				if model.ValidatePrivateRecordOwner(key.name, result.Plan.ZoneApex, string(set.RecordType)) != nil || !registrationOwns(reg, key, result.Plan.ZoneApex, claims) || !recordAllowed(reg, grant, key, result.Plan.ZoneApex) {
					validBundle = false
					break
				}
				if _, err := recordContent(set.RecordType, record); err != nil {
					validBundle = false
					break
				}
			}
			if !validBundle {
				break
			}
		}
		if !validBundle {
			result.Reasons[c.UID] = "NameOrTypeOutsideGrant"
			continue
		}
		result.Accepted[c.UID] = true
		for _, set := range c.Spec.RecordSets {
			for _, record := range set.Records {
				key := ownershipKey{zoneOwner(record.Name, result.Plan.ZoneApex), set.RecordType}
				a := aggregates[key]
				if a == nil {
					a = &aggregate{ttl: reg.Spec.TTLSeconds}
					aggregates[key] = a
				}
				record.Name = key.name
				a.records = append(a.records, record)
				a.uids = append(a.uids, c.UID)
				if a.until == nil || until.Before(*a.until) {
					u := until
					a.until = &u
				}
			}
		}
	}
	result.Plan.Contributions = make([]ContributionFence, 0, len(publicationFences))
	for _, fence := range publicationFences {
		result.Plan.Contributions = append(result.Plan.Contributions, fence)
	}

	for i := range in.StaticRecords {
		rs := &in.StaticRecords[i]
		if rs.Spec.DNSZoneRef.Name != in.Zone.Name || !validStatic[rs.UID] {
			continue
		}
		for _, record := range rs.Spec.Records {
			key := ownershipKey{zoneOwner(record.Name, result.Plan.ZoneApex), rs.Spec.RecordType}
			a := aggregates[key]
			if a == nil {
				a = &aggregate{}
				aggregates[key] = a
			}
			record.Name = key.name
			a.records = append(a.records, record)
		}
	}
	for key, a := range aggregates {
		sortAggregate(a.records, a.uids)
		result.Plan.RRsets = append(result.Plan.RRsets, PublicationRRSet{Name: key.name, RecordType: key.rrtype, TTLSeconds: a.ttl, Records: a.records, ContributionUIDs: a.uids, ValidUntil: a.until})
	}
	sort.Slice(result.Plan.RRsets, func(i, j int) bool {
		if result.Plan.RRsets[i].Name == result.Plan.RRsets[j].Name {
			return result.Plan.RRsets[i].RecordType < result.Plan.RRsets[j].RecordType
		}
		return result.Plan.RRsets[i].Name < result.Plan.RRsets[j].Name
	})
	sort.Slice(result.Plan.Contributions, func(i, j int) bool { return result.Plan.Contributions[i].UID < result.Plan.Contributions[j].UID })
	if err := validateSRVTargets(result.Plan.RRsets, result.Plan.ZoneApex); err != nil {
		return result, err
	}
	return result, nil
}

func validateContribution(c *dnsv1alpha1.DNSRecordContribution, reg *dnsv1alpha1.DNSRegistration, grant *dnsv1alpha1.DNSContributionGrant, previous ContributionFence, now time.Time) string {
	if reg == nil || c.Spec.RegistrationRef.Name != reg.Name || c.Spec.RegistrationRef.Generation != reg.Generation {
		return "StaleRegistrationGeneration"
	}
	if grant == nil || c.Spec.GrantRef.Name != grant.Name {
		return "GrantNotActive"
	}
	if c.Status.ObservedGeneration != c.Generation {
		return "ObservationGenerationMismatch"
	}
	if c.Status.WriterEpoch != grant.Status.ActiveWriterEpoch {
		return "StaleWriterEpoch"
	}
	if c.Status.Sequence <= 0 {
		return "InvalidSequence"
	}
	if c.Status.ValidUntil == nil || c.Status.ValidUntil.IsZero() {
		return "MissingValidityDeadline"
	}
	if previous.UID != "" {
		if previous.GrantUID != "" && previous.GrantUID != grant.UID {
			return "FenceIdentityMismatch"
		}
		if c.Status.WriterEpoch < previous.Epoch {
			return "StaleWriterEpoch"
		}
		if previous.Epoch == c.Status.WriterEpoch {
			if c.Status.Sequence < previous.Sequence {
				return "NonIncreasingSequence"
			}
			if c.Status.Sequence == previous.Sequence && !c.Status.ValidUntil.Time.Equal(previous.ValidUntil) {
				return "FenceDeadlineMismatch"
			}
		}
	}
	return ""
}

func recordAllowed(reg *dnsv1alpha1.DNSRegistration, grant *dnsv1alpha1.DNSContributionGrant, key ownershipKey, apex string) bool {
	if !containsType(reg.Spec.RecordTypes, key.rrtype) || !containsType(grant.Spec.RecordTypes, key.rrtype) {
		return false
	}
	name := zoneOwner(reg.Spec.Name, apex)
	allowedName := key.name == name
	for _, scope := range reg.Spec.ReservedDescendants {
		s := zoneOwner(scope, apex)
		if key.name == s || strings.HasSuffix(key.name, "."+s) {
			allowedName = true
		}
	}
	if !allowedName {
		return false
	}
	if len(grant.Spec.NameScopes) == 0 {
		return true
	}
	for _, scope := range grant.Spec.NameScopes {
		s := zoneOwner(scope, apex)
		if key.name == s || strings.HasSuffix(key.name, "."+s) {
			return true
		}
	}
	return false
}

func validateSRVTargets(rrsets []PublicationRRSet, apex string) error {
	addresses := map[string]bool{}
	for _, r := range rrsets {
		if r.RecordType == dnsv1alpha1.RRTypeA || r.RecordType == dnsv1alpha1.RRTypeAAAA {
			addresses[absoluteOwner(r.Name, apex)] = true
		}
	}
	for _, r := range rrsets {
		if r.RecordType != dnsv1alpha1.RRTypeSRV {
			continue
		}
		for _, entry := range r.Records {
			if entry.SRV != nil && !addresses[srvTargetOwner(entry.SRV.Target, apex)] {
				return fmt.Errorf("SRV target %s has no address in publication", entry.SRV.Target)
			}
		}
	}
	return nil
}

func srvTargetOwner(name, apex string) string {
	if strings.HasSuffix(strings.TrimSpace(name), ".") {
		return canonicalName(name)
	}
	return absoluteOwner(name, apex)
}

func zoneOwner(s, apex string) string {
	n := canonicalName(s)
	a := canonicalName(apex)
	if n == a {
		return "@"
	}
	if strings.HasSuffix(n, "."+a) {
		return strings.TrimSuffix(n, "."+a)
	}
	return n
}

func conflictingClaim(claims map[ownershipKey]types.UID, key ownershipKey) (types.UID, bool) {
	if uid, ok := claims[key]; ok {
		return uid, true
	}
	if key.rrtype == dnsv1alpha1.RRTypeCNAME {
		for k, uid := range claims {
			if k.name == key.name {
				return uid, true
			}
		}
	} else if uid, ok := claims[ownershipKey{key.name, dnsv1alpha1.RRTypeCNAME}]; ok {
		return uid, true
	}
	return "", false
}

func registrationOwns(reg *dnsv1alpha1.DNSRegistration, key ownershipKey, apex string, claims map[ownershipKey]types.UID) bool {
	if uid, ok := conflictingClaim(claims, key); ok && uid != reg.UID {
		return false
	}
	if key.name == zoneOwner(reg.Spec.Name, apex) {
		return true
	}
	for _, scope := range reg.Spec.ReservedDescendants {
		s := zoneOwner(scope, apex)
		if key.name == s || strings.HasSuffix(key.name, "."+s) {
			return true
		}
	}
	return false
}

func containsType(xs []dnsv1alpha1.RRType, x dnsv1alpha1.RRType) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func uniqueTypes(xs []dnsv1alpha1.RRType) []dnsv1alpha1.RRType {
	m := map[dnsv1alpha1.RRType]bool{}
	var out []dnsv1alpha1.RRType
	for _, x := range xs {
		if !m[x] {
			m[x] = true
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sortRecords(records []dnsv1alpha1.RecordEntry) {
	sort.Slice(records, func(i, j int) bool {
		a, _ := json.Marshal(records[i])
		b, _ := json.Marshal(records[j])
		return string(a) < string(b)
	})
}

func sortAggregate(records []dnsv1alpha1.RecordEntry, uids []types.UID) {
	if len(uids) != len(records) {
		sortRecords(records)
		return
	}
	type pair struct {
		record dnsv1alpha1.RecordEntry
		uid    types.UID
		key    string
	}
	pairs := make([]pair, len(records))
	for i := range records {
		b, _ := json.Marshal(records[i])
		pairs[i] = pair{records[i], uids[i], string(b) + "\x00" + string(uids[i])}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
	for i := range pairs {
		records[i] = pairs[i].record
		uids[i] = pairs[i].uid
	}
}
