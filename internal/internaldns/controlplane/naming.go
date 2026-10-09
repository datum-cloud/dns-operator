// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"context"
	"fmt"
	"sort"
	"strings"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ResolvedName struct {
	DNSZoneRef dnsv1alpha1.DNSObjectReference
	Name       string
	Canonical  bool
}

// ResolveNaming returns the stable platform namespace first and authorized
// additive policy names after it. It never selects an arbitrary associated
// customer zone.
func ResolveNaming(ctx context.Context, c client.Client, namespace string, projectUID, vpcUID types.UID, class dnsv1alpha1.DNSRegistrationClass, allocatedName string) ([]ResolvedName, error) {
	label := dnsLabel(allocatedName)
	if label == "" {
		return nil, fmt.Errorf("allocated name %q has no DNS-safe label", allocatedName)
	}
	var managed dnsv1alpha1.DNSManagedNamespaceList
	if err := c.List(ctx, &managed, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var out []ResolvedName
	for i := range managed.Items {
		m := &managed.Items[i]
		accepted := apimeta.FindStatusCondition(m.Status.Conditions, "Accepted")
		if m.Spec.ProjectUID == projectUID && m.Spec.VPCRef.UID == vpcUID && accepted != nil && accepted.Status == metav1.ConditionTrue {
			prefix := classPrefix(class)
			out = append(out, ResolvedName{DNSZoneRef: m.Status.DNSZoneRef, Name: label + "." + prefix, Canonical: true})
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("managed DNS namespace for VPC %s is not ready", vpcUID)
	}
	var policies dnsv1alpha1.DNSNamingPolicyList
	if err := c.List(ctx, &policies, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	sort.Slice(policies.Items, func(i, j int) bool {
		if policies.Items[i].Spec.Priority == policies.Items[j].Spec.Priority {
			return policies.Items[i].Name < policies.Items[j].Name
		}
		return policies.Items[i].Spec.Priority > policies.Items[j].Spec.Priority
	})
	for i := range policies.Items {
		p := &policies.Items[i]
		accepted := apimeta.FindStatusCondition(p.Status.Conditions, "Accepted")
		if p.Status.ResolvedVPCRef.UID != vpcUID || accepted == nil || accepted.Status != metav1.ConditionTrue {
			continue
		}
		for _, rule := range p.Status.ResolvedAdditionalNames {
			if rule.RegistrationClass == class {
				out = append(out, ResolvedName{DNSZoneRef: rule.DNSZoneRef, Name: label + "." + canonicalName(rule.NamePrefix)})
			}
		}
	}
	return out, nil
}

func classPrefix(class dnsv1alpha1.DNSRegistrationClass) string {
	switch class {
	case dnsv1alpha1.DNSRegistrationClassInstanceIdentity:
		return "instances"
	default:
		return "services"
	}
}
func dnsLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	dash := false
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(b.String(), "-")
}
