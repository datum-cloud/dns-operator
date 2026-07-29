// SPDX-License-Identifier: AGPL-3.0-only

package activitypolicy_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"gopkg.in/yaml.v3"
)

// activityPolicy is a minimal subset of ActivityPolicy for loading fixtures.
type activityPolicy struct {
	Spec struct {
		AuditRules []struct {
			Name    string `yaml:"name"`
			Match   string `yaml:"match"`
			Summary string `yaml:"summary"`
		} `yaml:"auditRules"`
		EventRules []struct {
			Name    string `yaml:"name"`
			Match   string `yaml:"match"`
			Summary string `yaml:"summary"`
		} `yaml:"eventRules"`
	} `yaml:"spec"`
}

func loadDNSRecordSetPolicy(t *testing.T) activityPolicy {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/activitypolicy -> repo root
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	path := filepath.Join(root, "config/milo/activity/policies/dnsrecordset-policy.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read policy: %v", err)
	}
	var pol activityPolicy
	if err := yaml.Unmarshal(data, &pol); err != nil {
		t.Fatalf("unmarshal policy: %v", err)
	}
	return pol
}

func TestDNSRecordSetPolicy_Structure(t *testing.T) {
	t.Parallel()
	pol := loadDNSRecordSetPolicy(t)

	byName := map[string]string{}
	summaries := map[string]string{}
	for _, r := range pol.Spec.AuditRules {
		byName[r.Name] = r.Match
		summaries[r.Name] = r.Summary
	}

	// #36: typed update rules must read recordType from responseObject
	for _, name := range []string{"update-a-aaaa", "update-cname"} {
		m, ok := byName[name]
		if !ok {
			t.Fatalf("missing rule %q", name)
		}
		if !strings.Contains(m, "responseObject.spec.recordType") {
			t.Errorf("%s match must use responseObject.spec.recordType, got: %s", name, m)
		}
		if strings.Contains(m, "requestObject.spec.recordType") {
			t.Errorf("%s must not require requestObject.spec.recordType (portal PATCH omits it)", name)
		}
		if !strings.Contains(m, "has(audit.requestObject.spec)") {
			t.Errorf("%s must require requestObject.spec to ignore metadata-only patches", name)
		}
	}
	otherUpdate, ok := byName["update-other-annotated"]
	if !ok {
		t.Fatal("missing rule update-other-annotated")
	}
	if strings.Contains(otherUpdate, "requestObject.spec.recordType") {
		t.Error("update-other-annotated must not require requestObject.spec.recordType")
	}
	if !strings.Contains(summaries["update-other-annotated"], "responseObject.spec.recordType") {
		t.Error("update-other-annotated summary must use responseObject.spec.recordType")
	}
	if !strings.Contains(otherUpdate, "has(audit.requestObject.spec)") {
		t.Error("update-other-annotated must require requestObject.spec")
	}

	if _, ok := byName["update-fallback"]; ok {
		t.Error("update-fallback must be removed so metadata-only patches are not summarized as updates")
	}

	for _, name := range []string{"delete-annotated", "delete-from-response", "delete-fallback"} {
		s, ok := summaries[name]
		if !ok {
			t.Fatalf("missing delete rule %q", name)
		}
		if !strings.Contains(s, "deleted") {
			t.Errorf("%s summary must use 'deleted', got: %s", name, s)
		}
		if strings.Contains(s, "removed") {
			t.Errorf("%s summary must not use 'removed', got: %s", name, s)
		}
	}

	createTXT := summaries["create-txt"]
	if !strings.Contains(createTXT, "display-name") {
		t.Errorf("create-txt must include display-name hostname, got: %s", createTXT)
	}
	if !strings.Contains(createTXT, "TXT record") {
		t.Errorf("create-txt must name the record type, got: %s", createTXT)
	}
	if !strings.Contains(createTXT, "display-value") {
		t.Errorf("create-txt must include display-value for searchability, got: %s", createTXT)
	}

	createA := summaries["create-a-aaaa"]
	if !strings.Contains(createA, "recordType") {
		t.Errorf("create-a-aaaa must include A/AAAA recordType in summary, got: %s", createA)
	}
	if !strings.Contains(createA, "pointing to") {
		t.Errorf("create-a-aaaa must include address phrasing, got: %s", createA)
	}

	updateA := summaries["update-a-aaaa"]
	if !strings.Contains(updateA, "responseObject.spec.recordType") {
		t.Errorf("update-a-aaaa must include A/AAAA recordType in summary, got: %s", updateA)
	}

	createCNAME := summaries["create-cname"]
	if !strings.Contains(createCNAME, "CNAME record") {
		t.Errorf("create-cname must name the record type, got: %s", createCNAME)
	}
	createALIAS := summaries["create-alias"]
	if !strings.Contains(createALIAS, "ALIAS record") {
		t.Errorf("create-alias must name the record type (distinct from CNAME), got: %s", createALIAS)
	}
	createMX := summaries["create-mx"]
	if !strings.Contains(createMX, "MX record") {
		t.Errorf("create-mx must name the record type, got: %s", createMX)
	}
	createNS := summaries["create-ns"]
	if !strings.Contains(createNS, "NS record") {
		t.Errorf("create-ns must name the record type, got: %s", createNS)
	}

	for _, name := range []string{"update-cname", "update-alias", "update-mx", "update-txt", "update-ns"} {
		s, ok := summaries[name]
		if !ok {
			t.Fatalf("missing typed update rule %q", name)
		}
		if !strings.Contains(s, "display-value") {
			t.Errorf("%s must include display-value, got: %s", name, s)
		}
	}

	createFromReq := summaries["create-from-request"]
	if !strings.Contains(createFromReq, "records[0].name") {
		t.Errorf("create-from-request must include relative owner name, got: %s", createFromReq)
	}

	for _, er := range pol.Spec.EventRules {
		if strings.Contains(er.Match, "RecordSetProgrammed") {
			t.Errorf("programmed event rule %q must be removed to reduce TXT/search noise", er.Name)
		}
	}
	if len(pol.Spec.EventRules) == 0 {
		t.Error("expected at least programming-failed event rule")
	}
}

func TestDNSRecordSetPolicy_CELMatchFixtures(t *testing.T) {
	t.Parallel()
	pol := loadDNSRecordSetPolicy(t)

	env, err := cel.NewEnv(
		cel.Variable("audit", cel.MapType(cel.StringType, cel.DynType)),
	)
	if err != nil {
		t.Fatalf("cel env: %v", err)
	}

	type fixture struct {
		name     string
		wantRule string // empty => no audit rule should match
		audit    map[string]any
	}

	displayAnns := map[string]any{
		"dns.networking.miloapis.com/display-name":  "_dmarc.datum.net",
		"dns.networking.miloapis.com/display-value": "\"v=DMARC1; p=none\"",
	}

	fixtures := []fixture{
		{
			name:     "human create TXT with annotations",
			wantRule: "create-txt",
			audit: map[string]any{
				"user": map[string]any{"username": "jsmith@datum.net"},
				"verb": "create",
				"requestObject": map[string]any{
					"spec": map[string]any{
						"recordType": "TXT",
						"records":    []any{map[string]any{"name": "_dmarc"}},
					},
				},
				"responseObject": map[string]any{
					"metadata": map[string]any{"annotations": displayAnns},
					"spec": map[string]any{
						"recordType": "TXT",
						"records":    []any{map[string]any{"name": "_dmarc"}},
					},
				},
			},
		},
		{
			name:     "portal PATCH without recordType on request (#36)",
			wantRule: "update-a-aaaa",
			audit: map[string]any{
				"user":      map[string]any{"username": "jsmith@datum.net"},
				"verb":      "patch",
				"objectRef": map[string]any{
					// no subresource
				},
				"requestObject": map[string]any{
					"spec": map[string]any{
						"records": []any{map[string]any{
							"name": "www",
							"a":    map[string]any{"content": "192.0.2.10"},
						}},
					},
				},
				"responseObject": map[string]any{
					"metadata": map[string]any{"annotations": map[string]any{
						"dns.networking.miloapis.com/display-name":  "www.example.com",
						"dns.networking.miloapis.com/display-value": "192.0.2.10",
					}},
					"spec": map[string]any{
						"recordType": "A",
						"records": []any{map[string]any{
							"name": "www",
							"a":    map[string]any{"content": "192.0.2.10"},
						}},
					},
				},
			},
		},
		{
			name:     "human delete with annotations",
			wantRule: "delete-annotated",
			audit: map[string]any{
				"user": map[string]any{"username": "jsmith@datum.net"},
				"verb": "delete",
				"responseObject": map[string]any{
					"metadata": map[string]any{"annotations": displayAnns},
					"spec": map[string]any{
						"recordType": "TXT",
						"records":    []any{map[string]any{"name": "_dmarc"}},
					},
				},
			},
		},
		{
			name:     "metadata-only patch does not match any update rule",
			wantRule: "",
			audit: map[string]any{
				"user": map[string]any{"username": "jsmith@datum.net"},
				"verb": "patch",
				"objectRef": map[string]any{
					"name": "www",
				},
				"requestObject": map[string]any{
					"metadata": map[string]any{
						"annotations": map[string]any{"foo": "bar"},
					},
				},
				"responseObject": map[string]any{
					"metadata": map[string]any{"annotations": map[string]any{
						"dns.networking.miloapis.com/display-name": "www.example.com",
					}},
					"spec": map[string]any{"recordType": "A"},
				},
			},
		},
	}

	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			t.Parallel()
			matched := ""
			for _, rule := range pol.Spec.AuditRules {
				ast, issues := env.Compile(rule.Match)
				if issues != nil && issues.Err() != nil {
					t.Fatalf("compile %s: %v", rule.Name, issues.Err())
				}
				prg, err := env.Program(ast)
				if err != nil {
					t.Fatalf("program %s: %v", rule.Name, err)
				}
				out, _, err := prg.Eval(map[string]any{"audit": fx.audit})
				if err != nil {
					// CEL may error on missing keys depending on expression; treat as non-match
					continue
				}
				if isCELTrue(out) {
					matched = rule.Name
					break
				}
			}
			if matched != fx.wantRule {
				t.Fatalf("matched rule %q, want %q", matched, fx.wantRule)
			}
		})
	}
}

func isCELTrue(v ref.Val) bool {
	b, ok := v.(types.Bool)
	return ok && bool(b)
}
