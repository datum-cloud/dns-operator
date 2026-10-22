// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	internaldnsadmission "go.miloapis.com/dns-operator/internal/internaldns/admission"
)

// The kind and e2e registration must send every private DNS reference write to
// the handler and fail closed.
func TestInternalDNSWebhookIsRegisteredFailClosed(t *testing.T) {
	path := filepath.Join("..", "..", "config", "webhook", "manifests.yaml")
	hook, ok := findValidatingWebhook(t, path, internaldnsadmission.Path)
	if !ok {
		t.Fatalf("%s registers no validating webhook for %s", path, internaldnsadmission.Path)
	}
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
}

// In deployed environments the internal-dns control plane registers the
// private DNS reference checks. A second fail-closed registration from the
// control-plane component would block those writes whenever either server is
// unavailable.
func TestDeployedComponentLeavesInternalDNSWebhookToControlPlane(t *testing.T) {
	path := filepath.Join("..", "..", "config", "components", "admission-webhooks", "validating-webhook-configuration.yaml")
	if _, ok := findValidatingWebhook(t, path, internaldnsadmission.Path); ok {
		t.Fatalf("%s registers %s; the internal-dns control plane owns that registration", path, internaldnsadmission.Path)
	}
}

func findValidatingWebhook(t *testing.T, path, servicePath string) (admissionregistrationv1.ValidatingWebhook, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	found := false
	for {
		var config admissionregistrationv1.ValidatingWebhookConfiguration
		if err := decoder.Decode(&config); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
		if config.Kind != "ValidatingWebhookConfiguration" {
			continue
		}
		found = true
		for _, hook := range config.Webhooks {
			if hook.ClientConfig.Service != nil && hook.ClientConfig.Service.Path != nil && *hook.ClientConfig.Service.Path == servicePath {
				return hook, true
			}
		}
	}
	if !found {
		t.Fatalf("%s contains no ValidatingWebhookConfiguration", path)
	}
	return admissionregistrationv1.ValidatingWebhook{}, false
}
