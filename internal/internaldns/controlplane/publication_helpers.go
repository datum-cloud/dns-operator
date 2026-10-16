// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func boolCondition(v bool) metav1.ConditionStatus {
	if v {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func regionToken(r ServingRegion) string {
	return model.SafeToken(r.Region) + "-" + model.SafeToken(r.Shard)
}

func boundedName(s string) string {
	if len(s) <= 63 {
		return strings.Trim(s, "-")
	}
	sum := sha256.Sum256([]byte(s))
	return strings.Trim(s[:50], "-") + fmt.Sprintf("-%x", sum[:6])
}
