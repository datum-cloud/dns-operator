// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// The field selectors both CRDs declare as +kubebuilder:selectablefield. Every
// fetch in this package goes through them so the API server does the filtering:
// a zone with a thousand records must not be pulled down whole to show one.
const (
	fieldZoneRef    = "spec.dnsZoneRef.name"
	fieldRecordType = "spec.recordType"
	fieldDomainName = "spec.domainName"
)

// clientFactory builds the API client for a project. It is a variable so tests
// can inject a fake client without a control plane behind it.
var clientFactory = util.NewClient

// resolveZone finds the DNSZone serving a domain. Users type the domain name,
// which is a selectable field; the object name is accepted as a fallback so a
// name copied out of `kubectl get dnszones` also works.
func resolveZone(ctx context.Context, c client.Client, domain string) (*dnsv1alpha1.DNSZone, error) {
	name := normalizeDomain(domain)
	if name == "" {
		return nil, util.UsageErrorf("a zone domain is required").
			WithFix("pass the zone as the first argument:\n       datumctl dns record list example.com")
	}

	var list dnsv1alpha1.DNSZoneList
	if err := c.List(ctx, &list,
		client.InNamespace(util.ResourceNamespace),
		client.MatchingFields{fieldDomainName: name},
	); err != nil {
		return nil, util.ClassifyError(fmt.Errorf("listing zones: %w", err))
	}

	if len(list.Items) > 0 {
		sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
		return &list.Items[0], nil
	}

	var byName dnsv1alpha1.DNSZone
	err := c.Get(ctx, types.NamespacedName{Namespace: util.ResourceNamespace, Name: name}, &byName)
	switch {
	case err == nil:
		return &byName, nil
	case apierrors.IsNotFound(err):
		return nil, util.NewCLIError(util.ExitNotFound, fmt.Sprintf("zone %q not found", name)).
			WithFix("run `datumctl dns zone list` to see the zones in this project.").
			WithCause(err)
	default:
		return nil, util.ClassifyError(fmt.Errorf("getting zone: %w", err))
	}
}

// normalizeDomain folds a domain to the spelling spec.domainName uses: lowercase
// and no trailing dot.
func normalizeDomain(domain string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}

// listSets fetches the record sets of a zone, one server-side query per
// requested type. An empty types slice fetches every type.
func listSets(ctx context.Context, c client.Client, zone *dnsv1alpha1.DNSZone, rrTypes []dnsv1alpha1.RRType) ([]dnsv1alpha1.DNSRecordSet, error) {
	queries := []client.MatchingFields{{fieldZoneRef: zone.Name}}
	if len(rrTypes) > 0 {
		queries = queries[:0]
		for _, t := range rrTypes {
			queries = append(queries, client.MatchingFields{
				fieldZoneRef:    zone.Name,
				fieldRecordType: string(t),
			})
		}
	}

	var out []dnsv1alpha1.DNSRecordSet
	for _, q := range queries {
		var page dnsv1alpha1.DNSRecordSetList
		if err := c.List(ctx, &page, client.InNamespace(zone.Namespace), q); err != nil {
			return nil, util.ClassifyError(fmt.Errorf("listing record sets: %w", err))
		}
		out = append(out, page.Items...)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// findSet returns the (zone, type) bucket a write should land in, or nil when
// none exists yet.
//
// More than one set per (zone, type) is possible — nothing in the API forbids
// it — so when several come back the one that already holds the owner name
// wins, and otherwise the first by object name. Picking deterministically
// matters more than picking cleverly: the same command must hit the same
// object every time.
func findSet(ctx context.Context, c client.Client, zone *dnsv1alpha1.DNSZone, t dnsv1alpha1.RRType, ownerName string) (*dnsv1alpha1.DNSRecordSet, error) {
	sets, err := listSets(ctx, c, zone, []dnsv1alpha1.RRType{t})
	if err != nil {
		return nil, err
	}
	if len(sets) == 0 {
		return nil, nil
	}
	if ownerName != "" {
		// A machine-owned set wins when several hold the name. The pick decides
		// which set the read-only guard inspects, so preferring the user's set
		// because it sorted first would fail OPEN and permit an edit the
		// controller reverts. Losing the tie the other way only over-protects.
		var first *dnsv1alpha1.DNSRecordSet
		for i := range sets {
			if !setHasOwner(&sets[i], ownerName, zone.Spec.DomainName) {
				continue
			}
			if isMachineOwned(&sets[i]) {
				return &sets[i], nil
			}
			if first == nil {
				first = &sets[i]
			}
		}
		if first != nil {
			return first, nil
		}
	}
	return &sets[0], nil
}

// setHasOwner reports whether a set carries any entry for an owner name.
func setHasOwner(rs *dnsv1alpha1.DNSRecordSet, ownerName, zoneDomain string) bool {
	for _, e := range rs.Spec.Records {
		if sameOwner(e.Name, ownerName, zoneDomain) {
			return true
		}
	}
	return false
}

// sameOwner compares two owner-name spellings the way the backend will: "@",
// "", and the fully qualified form all name the apex.
func sameOwner(a, b, zoneDomain string) bool {
	return rdata.FQDN(a, zoneDomain) == rdata.FQDN(b, zoneDomain)
}

// setObjectName is the name a new bucket is created under. It matches the
// convention the portal and the operator already use — `<zone>-soa`, `<zone>-ns`
// — so a zone's objects stay recognisable however they were created.
func setObjectName(zone *dnsv1alpha1.DNSZone, t dnsv1alpha1.RRType) string {
	return fmt.Sprintf("%s-%s", zone.Name, strings.ToLower(string(t)))
}
