// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Wildcard subjects must stay literal: NATS rejects JSON's HTML Unicode
// escapes even though a JSON decoder accepts them.
func TestBrokerConfigurationKeepsNATSWildcards(t *testing.T) {
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	previous := out
	out = t.TempDir()
	t.Cleanup(func() { out = previous })
	bootstrap()
	data, err := os.ReadFile(filepath.Join(out, "platform-bootstrap.json"))
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Items []object `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	for _, item := range list.Items {
		if item["kind"] != "Secret" || item["metadata"].(map[string]any)["name"] != "internal-dns-nats-config" {
			continue
		}
		cfg := item["stringData"].(map[string]any)["nats.conf"].(string)
		if strings.Contains(cfg, `\u003e`) || !strings.Contains(cfg, `dns.private.serving.>`) {
			t.Fatal("broker configuration escaped NATS wildcard subjects")
		}
		if !strings.Contains(cfg, "$JS.ACK.DNS_PRIVATE.control-plane-us-central1-shared-0-acks.>") {
			t.Fatal("controller cannot acknowledge its member-ACK durable")
		}
		return
	}
	t.Fatal("broker configuration missing")
}

// Project workers must not inherit the platform's cluster-wide artifact
// privileges when both APIs share the shipped ClusterRole.
func TestSourceWorkerBindingIsProjectScoped(t *testing.T) {
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	scoped, sar := false, false
	for _, item := range sourceObjects("project-a") {
		metadata := item["metadata"].(map[string]any)
		switch {
		case item["kind"] == "ClusterRoleBinding" && metadata["name"] == "internal-dns-control-plane":
			t.Fatal("source worker inherited platform cluster-wide binding")
		case item["kind"] == "RoleBinding" && metadata["name"] == "internal-dns-source-worker":
			scoped = metadata["namespace"] == projectNS
		case item["kind"] == "ClusterRole" && metadata["name"] == "internal-dns-source-authorization":
			rules := item["rules"].([]object)
			sar = len(rules) == 1 && rules[0]["resources"].([]string)[0] == "subjectaccessreviews"
		}
	}
	if !scoped || !sar {
		t.Fatal("source worker requires project scope and separate authorization access")
	}
}
