// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"strings"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"k8s.io/apimachinery/pkg/types"
)

func refMatches(ref dnsv1alpha1.DNSObjectReference, name string, uid types.UID, generation int64) bool {
	return ref.Name == name && (ref.UID == "" || ref.UID == uid) && (ref.Generation == 0 || ref.Generation == generation)
}

func canonicalName(s string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "."))
}
