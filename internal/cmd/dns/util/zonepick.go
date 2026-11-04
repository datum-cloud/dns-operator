// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

const VPCFlag = "vpc"

// NetworkLabel is set on a VPC's resolver context by network services.
const NetworkLabel = "networking.datumapis.com/network"

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

// PickZone never guesses between zones that share a domain: private zones
// can, and the first match may belong to another VPC.
func PickZone(ctx context.Context, c client.Client, candidates []dnsv1alpha1.DNSZone, typed, vpc string) (*dnsv1alpha1.DNSZone, error) {
	if vpc == "" {
		if len(candidates) == 1 {
			return &candidates[0], nil
		}
		return nil, ambiguousZoneError(ctx, c, candidates, typed, "")
	}

	contexts, err := vpcContexts(ctx, c, vpc)
	if err != nil {
		return nil, err
	}
	if len(contexts) == 0 {
		return nil, NewCLIError(ExitNotFound,
			fmt.Sprintf("no VPC named %q uses DNS in this project", vpc)).
			WithFix("check the VPC name:\n       datumctl network list")
	}

	used, err := zonesUsedBy(ctx, c, contexts)
	if err != nil {
		return nil, err
	}
	var matched []dnsv1alpha1.DNSZone
	for i := range candidates {
		if used[candidates[i].Name] {
			matched = append(matched, candidates[i])
		}
	}

	switch len(matched) {
	case 1:
		return &matched[0], nil
	case 0:
		return nil, NewCLIError(ExitNotFound,
			fmt.Sprintf("VPC %q does not use a zone for %q", vpc, typed)).
			WithFix("run `datumctl dns zone list` to see the zones in this project.")
	default:
		return nil, ambiguousZoneError(ctx, c, matched, typed, vpc)
	}
}

func vpcContexts(ctx context.Context, c client.Client, vpc string) (map[string]bool, error) {
	var list dnsv1alpha1.DNSResolverContextList
	if err := c.List(ctx, &list,
		client.InNamespace(ResourceNamespace),
		client.MatchingLabels{NetworkLabel: vpc},
	); err != nil {
		return nil, ClassifyError(fmt.Errorf("listing DNS contexts for VPC %q: %w", vpc, err))
	}
	names := make(map[string]bool, len(list.Items))
	for i := range list.Items {
		names[list.Items[i].Name] = true
	}
	return names, nil
}

func zonesUsedBy(ctx context.Context, c client.Client, contexts map[string]bool) (map[string]bool, error) {
	var rcs dnsv1alpha1.DNSResolverContextList
	if err := c.List(ctx, &rcs, client.InNamespace(ResourceNamespace)); err != nil {
		return nil, ClassifyError(fmt.Errorf("listing DNS contexts: %w", err))
	}
	used := map[string]bool{}
	for i := range rcs.Items {
		rc := &rcs.Items[i]
		if contexts[rc.Name] && rc.Status.ManagedNamespace.DNSZoneRef.Name != "" {
			used[rc.Status.ManagedNamespace.DNSZoneRef.Name] = true
		}
	}

	var assocs dnsv1alpha1.DNSZoneAssociationList
	if err := c.List(ctx, &assocs, client.InNamespace(ResourceNamespace)); err != nil {
		return nil, ClassifyError(fmt.Errorf("listing zone associations: %w", err))
	}
	for i := range assocs.Items {
		a := &assocs.Items[i]
		if a.DeletionTimestamp.IsZero() && contexts[a.Spec.ResolverContextRef.Name] {
			used[a.Spec.DNSZoneRef.Name] = true
		}
	}
	return used, nil
}

func ambiguousZoneError(ctx context.Context, c client.Client, zones []dnsv1alpha1.DNSZone, typed, vpc string) *CLIError {
	vpcs := zoneVPCs(ctx, c)

	sorted := append([]dnsv1alpha1.DNSZone(nil), zones...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var b strings.Builder
	for i := range sorted {
		z := &sorted[i]
		names := vpcs[z.Name]
		if v := z.Labels[NetworkLabel]; v != "" && !contains(names, v) {
			names = append(names, v)
		}
		sort.Strings(names)
		where := "used by no VPC"
		if len(names) > 0 {
			where = "VPC " + strings.Join(names, ", ")
		}
		fmt.Fprintf(&b, "\n       %s  (%s)", z.Name, where)
	}

	msg := fmt.Sprintf("%d zones in this project serve %q", len(sorted), typed)
	fix := "pass --vpc <name> to choose the VPC's zone, or the zone's object name instead of the domain:" + b.String()
	if vpc != "" {
		msg = fmt.Sprintf("VPC %q uses %d zones for %q", vpc, len(sorted), typed)
		fix = "pass the zone's object name instead of the domain:" + b.String()
	}
	return NewCLIError(ExitUsage, msg).WithFix(fix)
}

func zoneVPCs(ctx context.Context, c client.Client) map[string][]string {
	out := map[string][]string{}

	var rcs dnsv1alpha1.DNSResolverContextList
	if err := c.List(ctx, &rcs, client.InNamespace(ResourceNamespace)); err != nil {
		return out
	}
	vpcOf := map[string]string{}
	for i := range rcs.Items {
		rc := &rcs.Items[i]
		vpc := rc.Labels[NetworkLabel]
		if vpc == "" {
			continue
		}
		vpcOf[rc.Name] = vpc
		if z := rc.Status.ManagedNamespace.DNSZoneRef.Name; z != "" && !contains(out[z], vpc) {
			out[z] = append(out[z], vpc)
		}
	}

	var assocs dnsv1alpha1.DNSZoneAssociationList
	if err := c.List(ctx, &assocs, client.InNamespace(ResourceNamespace)); err != nil {
		return out
	}
	for i := range assocs.Items {
		a := &assocs.Items[i]
		vpc := vpcOf[a.Spec.ResolverContextRef.Name]
		if vpc == "" || !a.DeletionTimestamp.IsZero() {
			continue
		}
		if z := a.Spec.DNSZoneRef.Name; !contains(out[z], vpc) {
			out[z] = append(out[z], vpc)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
