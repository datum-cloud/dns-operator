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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type ResolvedName struct {
	DNSZoneRef dnsv1alpha1.DNSObjectReference
	Name       string
	Canonical  bool
}

// ResolveNaming returns the stable platform namespace first and configured
// additive policy names after it. It never selects an arbitrary associated
// customer zone.
// Selectors match DNSRegistration metadata labels supplied by the publisher.
func ResolveNaming(ctx context.Context, c client.Client, namespace string, _ types.UID, contextUID types.UID, registrationLabels map[string]string, allocatedName string) ([]ResolvedName, error) {
	label := dnsLabel(allocatedName)
	if label == "" {
		return nil, fmt.Errorf("allocated name %q has no DNS-safe label", allocatedName)
	}
	var managed dnsv1alpha1.DNSResolverContextList
	if err := c.List(ctx, &managed, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var out []ResolvedName
	for i := range managed.Items {
		m := &managed.Items[i]
		accepted := apimeta.FindStatusCondition(m.Status.Conditions, "Ready")
		// A Ready context without a managed namespace, or whose Ready
		// condition predates its spec, has no current canonical zone.
		if m.UID == contextUID && m.DeletionTimestamp.IsZero() && accepted != nil && accepted.Status == metav1.ConditionTrue && accepted.ObservedGeneration == m.Generation && m.Status.ManagedNamespace.DNSZoneRef.UID != "" {
			out = append(out, ResolvedName{DNSZoneRef: m.Status.ManagedNamespace.DNSZoneRef, Name: label, Canonical: true})
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("managed DNS namespace for resolver context %s is not ready", contextUID)
	}
	seen := map[string]bool{string(out[0].DNSZoneRef.UID) + "\x00" + out[0].Name: true}
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
		accepted := apimeta.FindStatusCondition(p.Status.Conditions, dnsValueAccepted)
		if p.Status.ResolvedResolverContextRef.UID != contextUID || !p.DeletionTimestamp.IsZero() || accepted == nil || accepted.Status != metav1.ConditionTrue || accepted.ObservedGeneration != p.Generation {
			continue
		}
		for _, rule := range p.Status.ResolvedAdditionalNames {
			selector, err := namingSelector(rule.Selector)
			if err != nil {
				return nil, fmt.Errorf("naming policy %s has an invalid selector: %w", p.Name, err)
			}
			name := label + "." + canonicalName(rule.NamePrefix)
			key := string(rule.DNSZoneRef.UID) + "\x00" + name
			if selector.Matches(labels.Set(registrationLabels)) && !seen[key] {
				out = append(out, ResolvedName{DNSZoneRef: rule.DNSZoneRef, Name: name})
				seen[key] = true
			}
		}
	}
	return out, nil
}

func namingSelector(selector *metav1.LabelSelector) (labels.Selector, error) {
	if selector == nil {
		return labels.Everything(), nil
	}
	return metav1.LabelSelectorAsSelector(selector)
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
