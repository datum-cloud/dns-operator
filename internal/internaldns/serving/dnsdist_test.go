// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDNSDistKeyPersistsAcrossAgentRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime", "dnsdist.key")
	if err := prepareDNSDistKey(path); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareDNSDistKey(path); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil || string(first) != string(second) {
		t.Fatal("control identity changed", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(first)))
	if err != nil || len(decoded) != 32 {
		t.Fatal("invalid control identity", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatal("control key must be readable only by its owner and serving group", err)
	}
	if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareDNSDistKey(path); err == nil {
		t.Fatal("invalid retained key was accepted")
	}
}

func TestDNSDistBootstrapPublishesOnlyCompleteConfiguration(t *testing.T) {
	config, _ := renderFixture(t)
	router := config.NodeDNSDist
	router.Path = filepath.Join(t.TempDir(), "node-dnsdist.conf")
	if err := os.Mkdir(router.Path+".bootstrap", 0700); err != nil {
		t.Fatal(err)
	}
	if err := prepareDNSDistRouter(router); err == nil {
		t.Fatal("failed staging was accepted")
	}
	if _, err := os.Stat(router.Path); !os.IsNotExist(err) {
		t.Fatal("failed staging exposed a startup file", err)
	}
	if err := os.Remove(router.Path + ".bootstrap"); err != nil {
		t.Fatal(err)
	}
	if err := prepareDNSDistRouter(router); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(router.Path)
	if err != nil || !strings.Contains(string(data), "DNSAction.Refused") || !strings.Contains(string(data), "setRules(rules)") {
		t.Fatal("bootstrap must refuse unknown contexts", err)
	}
	if err := os.WriteFile(router.Path, []byte("accepted snapshot"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := prepareDNSDistRouter(router); err != nil {
		t.Fatal(err)
	}
	retained, err := os.ReadFile(router.Path)
	if err != nil || string(retained) != "accepted snapshot" {
		t.Fatal("bootstrap replaced accepted routing", err)
	}
}
