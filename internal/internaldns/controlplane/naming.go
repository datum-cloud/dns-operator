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
func ResolveNaming(ctx context.Context, c client.Client, namespace string, projectUID types.UID, contextName string, registrationLabels map[string]string, allocatedName string) ([]ResolvedName, error) {
	label := dnsLabel(allocatedName)
	if label == "" {
		return nil, fmt.Errorf("allocated name %q has no DNS-safe label", allocatedName)
	}
	var resolver dnsv1alpha1.DNSResolverContext
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: contextName}, &resolver); err != nil {
		return nil, err
	}
	ready := apimeta.FindStatusCondition(resolver.Status.Conditions, "Ready")
	// A Ready context without a managed namespace, or whose Ready
	// condition predates its spec, has no current canonical zone.
	if !resolver.DeletionTimestamp.IsZero() || ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != resolver.Generation || resolver.Status.ManagedNamespace.DNSZoneRef.Name == "" {
		return nil, fmt.Errorf("managed DNS namespace for context %s is not ready", contextName)
	}
	var canonical dnsv1alpha1.DNSZone
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: resolver.Status.ManagedNamespace.DNSZoneRef.Name}, &canonical); err != nil {
		return nil, err
	}
	out := []ResolvedName{{DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: canonical.Name, UID: canonical.UID}, Name: label, Canonical: true}}
	r := &Reconciler{Client: c, Options: Options{ProjectUID: projectUID}}
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
		if p.Status.ResolvedResolverContextRef.Name != contextName || !p.DeletionTimestamp.IsZero() || accepted == nil || accepted.Status != metav1.ConditionTrue || accepted.ObservedGeneration != p.Generation {
			continue
		}
		pins, err := r.readReferences(ctx, p)
		if err != nil {
			return nil, err
		}
		if pins["context"].UID != resolver.UID {
			continue
		}
		for _, rule := range p.Status.ResolvedAdditionalNames {
			selector, err := namingSelector(rule.Selector)
			if err != nil {
				return nil, fmt.Errorf("naming policy %s has an invalid selector: %w", p.Name, err)
			}
			name := label + "." + canonicalName(rule.NamePrefix)
			zoneRef := pins["zone/"+rule.DNSZoneRef.Name]
			var zone dnsv1alpha1.DNSZone
			if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: zoneRef.Name}, &zone); err != nil {
				return nil, err
			}
			if !refMatches(zoneRef, zone.Name, zone.UID, zone.Generation) {
				continue
			}
			key := string(zoneRef.UID) + "\x00" + name
			if selector.Matches(labels.Set(registrationLabels)) && !seen[key] {
				out = append(out, ResolvedName{DNSZoneRef: zoneRef, Name: name})
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
