// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

const VPCFlag = "vpc"

func AddVPCFlag(flags *pflag.FlagSet) {
	flags.String(VPCFlag, "", "Use the zone this VPC resolves, when several zones in the project share the domain")
}

func VPCFromCmd(cmd *cobra.Command) string {
	f := cmd.Flags().Lookup(VPCFlag)
	if f == nil {
		return ""
	}
	return strings.TrimSpace(f.Value.String())
}

var sharedDomainZones sync.Map

// ZoneArg is how to name a zone on a later command line: by domain only when
// no other zone could answer to the same argument.
func ZoneArg(z *dnsv1alpha1.DNSZone) string {
	if _, shared := sharedDomainZones.Load(z); shared || z.Spec.Visibility == dnsv1alpha1.DNSZoneVisibilityPrivate {
		return z.Name
	}
	return z.Spec.DomainName
}

// ZoneLabel names a zone for prompts and receipts.
func ZoneLabel(z *dnsv1alpha1.DNSZone) string {
	if z.Spec.Visibility != dnsv1alpha1.DNSZoneVisibilityPrivate {
		return z.Spec.DomainName
	}
	return fmt.Sprintf("%s (zone %s)", z.Spec.DomainName, z.Name)
}

// MarkSharedDomain records that other zones in the project carry z's domain,
// so ZoneArg names it by object.
func MarkSharedDomain(z *dnsv1alpha1.DNSZone) {
	sharedDomainZones.Store(z, true)
}

// PickZone chooses among every zone whose domain or object name equals typed.
// Without --vpc it refuses whenever more than one could be meant, including a
// private zone that is being deleted: skipping it could hand the write to
// another VPC's zone.
func PickZone(ctx context.Context, c client.Client, candidates []dnsv1alpha1.DNSZone, typed, vpc string) (*dnsv1alpha1.DNSZone, error) {
	if vpc == "" {
		if !anyPrivate(candidates) {
			candidates = preferLive(candidates)
		}
		if len(candidates) == 1 {
			return &candidates[0], nil
		}
		return nil, ambiguousZoneError(ctx, c, candidates, typed, "")
	}

	if !anyPrivate(candidates) {
		return nil, UsageErrorf("%q is a public zone, and --vpc applies only to private zones", typed).
			WithFix("drop --vpc.")
	}

	used, err := zonesUsedByVPC(ctx, c, vpc)
	if err != nil {
		return nil, err
	}
	var matched []dnsv1alpha1.DNSZone
	for i := range candidates {
		if used[candidates[i].Name] {
			matched = append(matched, candidates[i])
		}
	}
	matched = preferLive(matched)

	switch len(matched) {
	case 1:
		return &matched[0], nil
	case 0:
		return nil, NewCLIError(ExitNotFound,
			fmt.Sprintf("VPC %q does not use a zone for %q", vpc, typed)).
			WithFix("run the command without --vpc to see which VPCs use each zone.")
	default:
		return nil, ambiguousZoneError(ctx, c, matched, typed, vpc)
	}
}

func anyPrivate(zones []dnsv1alpha1.DNSZone) bool {
	for i := range zones {
		if zones[i].Spec.Visibility == dnsv1alpha1.DNSZoneVisibilityPrivate {
			return true
		}
	}
	return false
}

func preferLive(zones []dnsv1alpha1.DNSZone) []dnsv1alpha1.DNSZone {
	var live []dnsv1alpha1.DNSZone
	for i := range zones {
		if zones[i].DeletionTimestamp.IsZero() {
			live = append(live, zones[i])
		}
	}
	if len(live) == 0 {
		return zones
	}
	return live
}

func zonesUsedByVPC(ctx context.Context, c client.Client, vpc string) (map[string]bool, error) {
	var network networkingv1alpha.Network
	err := c.Get(ctx, types.NamespacedName{Namespace: ResourceNamespace, Name: vpc}, &network)
	switch {
	case apierrors.IsNotFound(err):
		return nil, NewCLIError(ExitNotFound, fmt.Sprintf("no VPC named %q in this project", vpc)).
			WithFix("check the VPC name:\n       datumctl network list")
	case err != nil:
		return nil, ClassifyError(fmt.Errorf("getting VPC %q: %w", vpc, err))
	}

	byZone, err := zoneVPCs(ctx, c, map[string]string{string(network.UID): vpc})
	if err != nil {
		return nil, ClassifyError(err)
	}
	used := map[string]bool{}
	for zone, vpcs := range byZone {
		if contains(vpcs, vpc) {
			used[zone] = true
		}
	}
	return used, nil
}

// contextNetworkUID reads the network UID out of a resolver context's
// immutable consumerID, which network services writes as
// "<project UID>/<network UID>".
func contextNetworkUID(rc *dnsv1alpha1.DNSResolverContext) string {
	i := strings.LastIndex(rc.Spec.ConsumerID, "/")
	if i < 0 {
		return ""
	}
	return rc.Spec.ConsumerID[i+1:]
}

// zoneVPCs maps zone names to the VPCs that resolve them, counting a VPC's
// managed zone and its accepted associations. vpcByUID maps network UIDs to
// VPC names.
func zoneVPCs(ctx context.Context, c client.Client, vpcByUID map[string]string) (map[string][]string, error) {
	out := map[string][]string{}

	var rcs dnsv1alpha1.DNSResolverContextList
	if err := c.List(ctx, &rcs, client.InNamespace(ResourceNamespace)); err != nil {
		return nil, fmt.Errorf("listing DNS contexts: %w", err)
	}
	vpcOf := map[string]string{}
	for i := range rcs.Items {
		rc := &rcs.Items[i]
		vpc := vpcByUID[contextNetworkUID(rc)]
		if vpc == "" || !rc.DeletionTimestamp.IsZero() {
			continue
		}
		vpcOf[rc.Name] = vpc
		if z := rc.Status.ManagedNamespace.DNSZoneRef.Name; z != "" && !contains(out[z], vpc) {
			out[z] = append(out[z], vpc)
		}
	}

	var assocs dnsv1alpha1.DNSZoneAssociationList
	if err := c.List(ctx, &assocs, client.InNamespace(ResourceNamespace)); err != nil {
		return nil, fmt.Errorf("listing zone associations: %w", err)
	}
	for i := range assocs.Items {
		a := &assocs.Items[i]
		vpc := vpcOf[a.Spec.ResolverContextRef.Name]
		if vpc == "" || !a.DeletionTimestamp.IsZero() ||
			a.Status.ResolvedDNSZoneRef.Name != a.Spec.DNSZoneRef.Name ||
			a.Status.ResolvedResolverContextRef.Name != a.Spec.ResolverContextRef.Name {
			continue
		}
		if z := a.Spec.DNSZoneRef.Name; !contains(out[z], vpc) {
			out[z] = append(out[z], vpc)
		}
	}
	return out, nil
}

func allVPCs(ctx context.Context, c client.Client) (map[string]string, error) {
	var networks networkingv1alpha.NetworkList
	if err := c.List(ctx, &networks, client.InNamespace(ResourceNamespace)); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i := range networks.Items {
		out[string(networks.Items[i].UID)] = networks.Items[i].Name
	}
	return out, nil
}

func ambiguousZoneError(ctx context.Context, c client.Client, zones []dnsv1alpha1.DNSZone, typed, vpc string) *CLIError {
	var vpcs map[string][]string
	known := false
	if byUID, err := allVPCs(ctx, c); err == nil {
		if m, err := zoneVPCs(ctx, c, byUID); err == nil {
			vpcs, known = m, true
		}
	}

	sorted := append([]dnsv1alpha1.DNSZone(nil), zones...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var b strings.Builder
	for i := range sorted {
		z := &sorted[i]
		names := append([]string(nil), vpcs[z.Name]...)
		sort.Strings(names)
		var where string
		switch {
		case z.Spec.Visibility != dnsv1alpha1.DNSZoneVisibilityPrivate:
			where = "public"
		case len(names) > 0:
			where = "VPC " + strings.Join(names, ", ")
		case known:
			where = "used by no VPC"
		default:
			where = "VPC unknown"
		}
		if !z.DeletionTimestamp.IsZero() {
			where += ", being deleted"
		}
		fmt.Fprintf(&b, "\n       %s  %s (%s)", z.Name, z.Spec.DomainName, where)
	}

	msg := fmt.Sprintf("%d zones in this project match %q", len(sorted), typed)
	fix := "pass --vpc <name> to choose the VPC's zone, or the zone's object name instead of the domain:" + b.String()
	if vpc != "" {
		msg = fmt.Sprintf("VPC %q uses %d zones for %q", vpc, len(sorted), typed)
		fix = "pass the zone's object name instead of the domain:" + b.String()
	}
	return NewCLIError(ExitUsage, msg).WithFix(fix)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
