// SPDX-License-Identifier: AGPL-3.0-only

package activitypolicy_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"gopkg.in/yaml.v3"
)

// Exercise the actual policy in first-match order and evaluate its summary,
// including link's string argument contract. Evaluation errors must fail tests:
// treating them as non-matches would conceal the production DLQ failure.
func TestDNSZonePolicy_AuditFixtures(t *testing.T) {
	t.Parallel()
	_, file, _, _ := runtime.Caller(0)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(file), "../../config/milo/activity/policies/dnszone-policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var policy activityPolicy
	if err := yaml.Unmarshal(data, &policy); err != nil {
		t.Fatal(err)
	}
	env, err := cel.NewEnv(
		cel.Variable("audit", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("actor", cel.StringType),
		cel.Function("link", cel.Overload("link_string_dyn", []*cel.Type{cel.StringType, cel.DynType}, cel.StringType,
			cel.BinaryBinding(func(label, resource ref.Val) ref.Val { return label }))),
	)
	if err != nil {
		t.Fatal(err)
	}
	evaluate := func(t *testing.T, expression string, audit map[string]any) ref.Val {
		t.Helper()
		ast, issues := env.Compile(expression)
		if issues != nil && issues.Err() != nil {
			t.Fatalf("compile %q: %v", expression, issues.Err())
		}
		program, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		result, _, err := program.Eval(map[string]any{"audit": withDefaults(audit), "actor": "Alice"})
		if err != nil {
			t.Fatalf("evaluate %q: %v", expression, err)
		}
		return result
	}
	spec := func(domain any) map[string]any { return map[string]any{"spec": map[string]any{"domainName": domain}} }
	patch := func(op, path string) []any { return []any{map[string]any{"op": op, "path": path}} }
	type fixture struct {
		name, verb          string
		request, response   any
		want                string
		system, subresource bool
		code                int
		omitStatus          bool
		dryRun              bool
	}
	fixtures := make([]fixture, 0, 127)
	fixtures = append(fixtures, []fixture{
		{name: "create domain", verb: "create", request: spec("example.com"), want: "Alice created zone example.com"},
		{name: "delete domain from response", verb: "delete", request: map[string]any{"kind": "DeleteOptions"}, response: spec("example.com"), want: "Alice deleted zone example.com"},
		{name: "partial patch omitting domain", request: map[string]any{"spec": map[string]any{"zoneClassName": "public"}}, response: spec("example.com"), want: "Alice updated zone example.com"},
		{name: "prefer response domain", request: spec("old.example.com"), response: spec("new.example.com"), want: "Alice updated zone new.example.com"},
		{name: "request domain fallback", request: spec("example.com"), want: "Alice updated zone example.com"},
		{name: "absent domain resource fallback", request: map[string]any{"spec": map[string]any{}}, want: "Alice updated zone example-zone"},
		{name: "null domain production failure", request: spec(nil), response: spec(nil), want: "Alice updated zone example-zone"},
		{name: "null spec", request: map[string]any{"spec": nil}, want: "Alice updated zone example-zone"},
		{name: "update full object", verb: "update", request: spec("example.com"), want: "Alice updated zone example.com"},
		{name: "metadata merge patch", request: map[string]any{"metadata": map[string]any{"labels": map[string]any{"foo": "bar"}}}, response: spec("example.com")},
		{name: "metadata JSON patch", request: patch("add", "/metadata/labels/foo"), response: spec("example.com")},
		{name: "JSON patch test", request: patch("test", "/spec/domainName"), response: spec("example.com")},
		{name: "JSON patch prefix collision", request: patch("replace", "/specification/domainName")},
		{name: "JSON patch escaped prefix", request: patch("replace", "/spec~1domainName")},
		{name: "JSON patch moves spec field to metadata", request: []any{map[string]any{"op": "move", "from": "/spec/domainName", "path": "/metadata/annotations/domain"}}, want: "Alice updated zone example-zone"},
		{name: "JSON patch copy from spec to metadata", request: []any{map[string]any{"op": "copy", "from": "/spec/domainName", "path": "/metadata/annotations/domain"}}},
		{name: "mixed JSON patch", request: []any{map[string]any{"op": "test", "path": "/metadata/name"}, map[string]any{"op": "replace", "path": "/spec/domainName"}}, response: spec("example.com"), want: "Alice updated zone example.com"},
		{name: "missing request"},
		{name: "scalar request", request: "invalid"},
		{name: "empty JSON patch", request: []any{}},
		{name: "malformed JSON operations", request: []any{nil, "invalid", map[string]any{"op": nil}, map[string]any{"op": "add", "path": nil}}},
		{name: "system update", request: spec("example.com"), system: true},
		{name: "status update", request: spec("example.com"), subresource: true},
	}...)
	for _, value := range []any{nil, "", 42, false, []any{}, map[string]any{}} {
		fixtures = append(fixtures,
			fixture{name: fmt.Sprintf("create invalid domain %T/%v", value, value), verb: "create", request: spec(value), want: "Alice created a DNS zone"},
			fixture{name: fmt.Sprintf("delete invalid domain %T/%v", value, value), verb: "delete", response: spec(value), want: "Alice deleted a DNS zone"},
			fixture{name: fmt.Sprintf("update invalid domain %T/%v", value, value), request: spec(value), response: spec(value), want: "Alice updated zone example-zone"},
		)
	}
	for _, value := range []any{nil, "invalid", []any{}, map[string]any{}, map[string]any{"spec": nil}, map[string]any{"spec": []any{}}} {
		fixtures = append(fixtures,
			fixture{name: fmt.Sprintf("create invalid resource %T/%v", value, value), verb: "create", request: value, want: "Alice created a DNS zone"},
			fixture{name: fmt.Sprintf("delete invalid response %T/%v", value, value), verb: "delete", response: value, want: "Alice deleted a DNS zone"},
			fixture{name: fmt.Sprintf("update invalid response %T/%v", value, value), request: spec("example.com"), response: value, want: "Alice updated zone example.com"},
		)
	}
	for _, op := range []string{"add", "replace", "remove", "move", "copy"} {
		for _, path := range []string{"", "/spec", "/spec/domainName"} {
			fixtures = append(fixtures, fixture{name: op + " " + path, request: patch(op, path), response: spec("example.com"), want: "Alice updated zone example.com"})
		}
	}
	for _, verb := range []string{"create", "update", "patch", "delete"} {
		for _, code := range []int{401, 403, 404, 409, 422, 500} {
			fixtures = append(fixtures,
				fixture{name: fmt.Sprintf("rejected %s %d with domain", verb, code), verb: verb, code: code, request: spec("example.com"), response: spec("example.com")},
				fixture{name: fmt.Sprintf("rejected %s %d null domain", verb, code), verb: verb, code: code, request: spec(nil), response: map[string]any{"kind": "Status", "status": "Failure"}},
			)
		}
		fixtures = append(fixtures, fixture{name: verb + " without response status", verb: verb, omitStatus: true, request: spec("example.com")})
		fixtures = append(fixtures, fixture{name: verb + " dry run", verb: verb, dryRun: true, request: spec("example.com"), response: spec("example.com")})
	}
	fixtures = append(fixtures, fixture{name: "accepted deletion", verb: "delete", code: 202, want: "Alice deleted a DNS zone"})
	expressionRE := regexp.MustCompile(`\{\{\s*(.+?)\s*\}\}`)
	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			verb := fx.verb
			if verb == "" {
				verb = "patch"
			}
			username := "alice@example.com"
			if fx.system {
				username = "system:serviceaccount:dns:controller"
			}
			objectRef := map[string]any{"name": "example-zone"}
			if fx.subresource {
				objectRef["subresource"] = "status"
			}
			audit := map[string]any{"verb": verb, "user": map[string]any{"username": username}, "objectRef": objectRef}
			code := fx.code
			if code == 0 {
				code = 200
			}
			if !fx.omitStatus {
				audit["responseStatus"] = map[string]any{"code": code}
			}
			if fx.dryRun {
				audit["requestURI"] = "/apis/dns.networking.miloapis.com/v1alpha1/namespaces/default/dnszones/example-zone?dryRun=All"
			}
			if fx.request != nil {
				audit["requestObject"] = fx.request
			}
			if fx.response != nil {
				audit["responseObject"] = fx.response
			}
			want := fx.want
			if want == "" && !fx.system && !fx.subresource && !fx.omitStatus && !fx.dryRun && code >= 200 && code < 300 {
				want = "Alice updated a DNS zone"
			}
			got := ""
			for _, rule := range policy.Spec.AuditRules {
				matched := evaluate(t, rule.Match, audit)
				if matched.Type() != types.BoolType {
					t.Fatalf("%s match returned %v", rule.Name, matched)
				}
				if !isCELTrue(matched) {
					continue
				}
				got = expressionRE.ReplaceAllStringFunc(rule.Summary, func(template string) string {
					expr := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(template, "{{"), "}}"))
					value := evaluate(t, expr, audit)
					if value.Type() != types.StringType {
						t.Fatalf("summary expression returned %v", value)
					}
					return value.Value().(string)
				})
				break
			}
			if strings.Contains(got, "{{") || strings.Contains(got, "}}") {
				t.Fatalf("summary has unevaluated template: %q", got)
			}
			if got != want {
				t.Fatalf("summary = %q, want %q", got, want)
			}
		})
	}
}
