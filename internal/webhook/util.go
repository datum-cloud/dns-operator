// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import authv1 "k8s.io/api/authentication/v1"

const (
	// ParentNameExtraKey is set by Milo on project-scoped admission requests.
	ParentNameExtraKey = "iam.miloapis.com/parent-name"
	// ParentTypeExtraKey is set by Milo to identify the parent resource kind.
	ParentTypeExtraKey = "iam.miloapis.com/parent-type"
)

// clusterNameFromExtra extracts the project control plane cluster name from
// UserInfo.Extra. Only Project parents are treated as cluster context.
func clusterNameFromExtra(extra map[string]authv1.ExtraValue) string {
	if parentTypes, ok := extra[ParentTypeExtraKey]; !ok || len(parentTypes) == 0 || parentTypes[0] != "Project" {
		return ""
	}
	if parentNames, ok := extra[ParentNameExtraKey]; ok && len(parentNames) > 0 && parentNames[0] != "" {
		return parentNames[0]
	}
	return ""
}
