// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	internaldnsadmission "go.miloapis.com/dns-operator/internal/internaldns/admission"
)

// Both the same-cluster and the control-plane registrations must send every
// private DNS reference write to the handler and fail closed.
func TestInternalDNSWebhookIsRegisteredFailClosed(t *testing.T) {
	for _, path := range []string{
		filepath.Join("..", "..", "config", "webhook", "manifests.yaml"),
		filepath.Join("..", "..", "config", "components", "admission-webhooks", "validating-webhook-configuration.yaml"),
	} {
		t.Run(path, func(t *testing.T) {
			hook := findValidatingWebhook(t, path, internaldnsadmission.Path)
			if hook.FailurePolicy == nil || *hook.FailurePolicy != admissionregistrationv1.Fail {
				t.Errorf("failurePolicy = %v, want Fail", hook.FailurePolicy)
			}
			if len(hook.Rules) != 1 {
				t.Fatalf("rules = %d, want 1", len(hook.Rules))
			}
			rule := hook.Rules[0]
			if !slices.Equal(rule.APIGroups, []string{dnsv1alpha1.GroupVersion.Group}) {
				t.Errorf("apiGroups = %v", rule.APIGroups)
			}
			ops := []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update}
			if !slices.Equal(rule.Operations, ops) {
				t.Errorf("operations = %v, want %v", rule.Operations, ops)
			}
			got := slices.Sorted(slices.Values(rule.Resources))
			want := slices.Sorted(slices.Values(internaldnsadmission.Resources))
			if !slices.Equal(got, want) {
				t.Errorf("resources = %v, want the handled resources %v", got, want)
			}
		})
	}
}

func findValidatingWebhook(t *testing.T, path, servicePath string) admissionregistrationv1.ValidatingWebhook {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var config admissionregistrationv1.ValidatingWebhookConfiguration
		if err := decoder.Decode(&config); err != nil {
			break
		}
		if config.Kind != "ValidatingWebhookConfiguration" {
			continue
		}
		for _, hook := range config.Webhooks {
			if hook.ClientConfig.Service != nil && hook.ClientConfig.Service.Path != nil && *hook.ClientConfig.Service.Path == servicePath {
				return hook
			}
		}
	}
	t.Fatalf("%s registers no validating webhook for %s", path, servicePath)
	return admissionregistrationv1.ValidatingWebhook{}
}
