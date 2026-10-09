package runtime

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestDeploymentExampleConfigsValidate(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..", "..", "config", "internal-dns")
	cases := []struct {
		file       string
		namePrefix string
		roles      []string
	}{
		{file: "control-plane.example.yaml", namePrefix: "internal-dns-control-plane-config", roles: []string{"control-plane"}},
		{file: "fleet.example.yaml", namePrefix: "internal-dns-", roles: []string{"agent", "watchdog"}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			contents, err := os.ReadFile(filepath.Join(root, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(contents), 4096)
			found := 0
			for {
				var object map[string]any
				if err := decoder.Decode(&object); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				if object["kind"] != "ConfigMap" {
					continue
				}
				metadata, _ := object["metadata"].(map[string]any)
				name, _ := metadata["name"].(string)
				if !strings.HasPrefix(name, tc.namePrefix) {
					continue
				}
				data, _ := object["data"].(map[string]any)
				raw, _ := data["config.json"].(string)
				if raw == "" {
					continue
				}
				found++
				for _, role := range tc.roles {
					configPath := filepath.Join(t.TempDir(), "config.json")
					if err := os.WriteFile(configPath, []byte(raw), 0600); err != nil {
						t.Fatal(err)
					}
					config, err := LoadConfig(configPath)
					if err != nil {
						t.Fatalf("%s config JSON: %v", name, err)
					}
					if err := config.Validate(role); err != nil {
						t.Errorf("%s role %s: %v", name, role, err)
					}
				}
			}
			if found == 0 {
				t.Fatal("no example runtime ConfigMaps found")
			}
		})
	}
}

func TestNATSExampleUsesExactMemberAckSubjects(t *testing.T) {
	t.Parallel()
	path := filepath.Join("..", "..", "..", "config", "internal-dns", "nats-server.conf.example")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, memberID := range []string{"node-us-central1-0", "regional-front-us-central1-0", "regional-bind-us-central1-0", "regional-bind-us-central1-1"} {
		subject := model.AckSubject("us-central1", "shared-0", memberID)
		if !bytes.Contains(contents, []byte(`"`+subject+`"`)) {
			t.Errorf("NATS policy is missing exact ACK subject %q", subject)
		}
	}
}
