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
		var broker struct {
			Authorization struct {
				Users []json.RawMessage `json:"users"`
			} `json:"authorization"`
		}
		if err := json.Unmarshal([]byte(cfg), &broker); err != nil {
			t.Fatal(err)
		}
		if len(broker.Authorization.Users) != 6 {
			t.Fatal("broker requires credentials for both controllers and all serving members")
		}
		for _, raw := range broker.Authorization.Users {
			var user struct {
				User        string                     `json:"user"`
				Permissions map[string]json.RawMessage `json:"permissions"`
			}
			if err := json.Unmarshal(raw, &user); err != nil {
				t.Fatal(err)
			}
			if user.User == "" || user.Permissions["publish"] == nil || user.Permissions["subscribe"] == nil {
				t.Fatal("broker users require scoped publish and subscribe permissions")
			}
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal([]byte(cfg), &top); err != nil {
			t.Fatal(err)
		}
		if top["version"] != nil {
			t.Fatal("broker configuration contains an unsupported version option")
		}
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
	scoped := false
	for _, item := range sourceObjects("project-a") {
		metadata := item["metadata"].(map[string]any)
		switch {
		case item["kind"] == "ClusterRoleBinding" && metadata["name"] == "internal-dns-control-plane":
			t.Fatal("source worker inherited platform cluster-wide binding")
		case item["kind"] == "RoleBinding" && metadata["name"] == "internal-dns-source-worker":
			scoped = metadata["namespace"] == projectNS

		}
	}
	if !scoped {
		t.Fatal("source worker requires project-scoped permissions")
	}
}

func TestFixtureSourcesUseIndependentProjectAPIs(t *testing.T) {
	root, err := filepath.Abs("../../../..")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	cfg := exampleConfigs("control-plane.example.yaml")["internal-dns-control-plane-config"]
	if cfg.ProjectDiscovery == nil {
		t.Fatal("deployment example must exercise project discovery")
	}
	configureProjectSources(&cfg)
	if cfg.ProjectDiscovery != nil || len(cfg.Projects) != 2 {
		t.Fatal("fixture source selection mixes discovery and direct APIs")
	}
	for _, project := range cfg.Projects {
		if project.Kubeconfig == "" || project.Namespace != projectNS {
			t.Fatalf("fixture source scope missing: %#v", project)
		}
	}
	if err := cfg.Validate("control-plane"); err != nil {
		t.Fatalf("fixture source config rejected: %v", err)
	}
}
