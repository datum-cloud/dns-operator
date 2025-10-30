package webhook

import authv1 "k8s.io/api/authentication/v1"

const (
	ParentNameExtraKey = "iam.miloapis.com/parent-name"
)

func clusterFromExtra(extra map[string]authv1.ExtraValue) string {
	if v, ok := extra[ParentNameExtraKey]; ok && len(v) > 0 && v[0] != "" {
		return v[0]
	}
	return ""
}
