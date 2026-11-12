// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestDNSAdminCannotWriteServiceOwnedResources(t *testing.T) {
	granted := map[string]bool{}
	for _, file := range []string{"dns-admin.yaml", "dns-viewer.yaml"} {
		data, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "iam", "roles", file))
		if err != nil {
			t.Fatal(err)
		}
		var role struct {
			Spec struct {
				InheritedRoles []struct {
					Name string `json:"name"`
				} `json:"inheritedRoles"`
				IncludedPermissions []string `json:"includedPermissions"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(data, &role); err != nil {
			t.Fatal(err)
		}
		for _, inherited := range role.Spec.InheritedRoles {
			if inherited.Name != "dns.networking.miloapis.com-dns-viewer" {
				t.Fatalf("%s inherits %s, which this test does not check", file, inherited.Name)
			}
		}
		for _, permission := range role.Spec.IncludedPermissions {
			granted[permission] = true
		}
	}
	if len(granted) == 0 {
		t.Fatal("dns-admin role grants no permissions")
	}
	for _, resource := range []string{"dnszones", "dnsrecordsets", "dnszoneassociations"} {
		if !granted["dns.networking.miloapis.com/"+resource+".create"] {
			t.Errorf("dns-admin must create %s", resource)
		}
	}
	for permission := range granted {
		for _, resource := range []string{"dnsregistrations", "dnsnamingpolicies", "dnsrecordcontributions", "dnsresolvercontexts", "dnsresolveraccessbindings"} {
			if strings.HasPrefix(permission, "dns.networking.miloapis.com/"+resource+".") && !strings.HasSuffix(permission, ".get") && !strings.HasSuffix(permission, ".list") && !strings.HasSuffix(permission, ".watch") {
				t.Errorf("dns-admin must not grant %s", permission)
			}
		}
	}
}
