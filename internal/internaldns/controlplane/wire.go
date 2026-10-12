// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"fmt"
	"sort"
	"strings"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

// WirePlan converts the compiler's API-rich records to the deliberately small,
// stable regional wire contract.
func WirePlan(plan PublicationPlan) (model.PublicationPlan, error) {
	out := model.PublicationPlan{ZoneUID: string(plan.ZoneUID), Apex: plan.ZoneApex}
	for _, registration := range plan.Registrations {
		out.Registrations = append(out.Registrations, model.RegistrationFence{UID: string(registration.UID), Generation: uint64(registration.Generation)})
	}
	for _, contribution := range plan.Contributions {
		out.ObservationFences = append(out.ObservationFences, model.ObservationFence{
			ContributionUID: string(contribution.UID), GrantUID: string(contribution.GrantUID),
			WriterEpoch: uint64(contribution.Epoch), Sequence: uint64(contribution.Sequence), ValidUntil: contribution.ValidUntil,
		})
	}
	sort.Slice(out.ObservationFences, func(i, j int) bool {
		return out.ObservationFences[i].ContributionUID < out.ObservationFences[j].ContributionUID
	})
	seenOwners := map[string]bool{}
	for _, owner := range plan.Owners {
		name := absoluteOwner(owner.Name, plan.ZoneApex)
		if !seenOwners[name] {
			out.Owners = append(out.Owners, name)
			seenOwners[name] = true
		}
	}
	sort.Strings(out.Owners)
	for _, set := range plan.RRsets {
		ttl := set.TTLSeconds
		if ttl < 0 {
			return model.PublicationPlan{}, fmt.Errorf("negative RRset TTL")
		}
		rr := model.RRSet{Name: absoluteOwner(set.Name, plan.ZoneApex), Type: string(set.RecordType), TTL: uint32(ttl)}
		for i, entry := range set.Records {
			content, err := recordContent(set.RecordType, entry)
			if err != nil {
				return model.PublicationPlan{}, err
			}
			record := model.RRRecord{Content: content, Eligible: true}
			if i < len(set.ContributionUIDs) {
				uid := set.ContributionUIDs[i]
				record.ContributionUID = string(uid)
				for _, f := range plan.Contributions {
					if f.UID == uid {
						record.WriterEpoch = uint64(f.Epoch)
						record.Sequence = uint64(f.Sequence)
						record.ValidUntil = f.ValidUntil
						break
					}
				}
			}
			rr.Records = append(rr.Records, record)
		}
		out.RRSets = append(out.RRSets, rr)
	}
	return out, out.Validate()
}

func absoluteOwner(name, apex string) string {
	name = canonicalName(name)
	apex = canonicalName(apex)
	if name == "@" {
		return apex
	}
	if name == apex || strings.HasSuffix(name, "."+apex) {
		return name
	}
	return name + "." + apex
}

func recordContent(rt dnsv1alpha1.RRType, e dnsv1alpha1.RecordEntry) (string, error) {
	content := rdata.Render(rt, e)
	if content == "" {
		return "", fmt.Errorf("record %q has no value matching type %s", e.Name, rt)
	}
	return content, nil
}
