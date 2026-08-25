// SPDX-License-Identifier: AGPL-3.0-only

package dns

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// runVersion executes `version` through a real root, so the -o flag resolves
// exactly as it does for a user.
func runVersion(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := Command()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"version"}, args...))
	err := root.Execute()
	return out.String(), err
}

func TestVersionDefaultOutput(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })
	Version = "v1.2.3"

	got, err := runVersion(t)
	if err != nil {
		t.Fatalf("version returned %v", err)
	}
	if want := "datumctl-dns v1.2.3 (DNS API dns.networking.miloapis.com/v1alpha1)\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestVersionJSON(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })
	Version = "v1.2.3"

	got, err := runVersion(t, "-o", "json")
	if err != nil {
		t.Fatalf("version -o json returned %v", err)
	}

	var info versionInfo
	if err := json.Unmarshal([]byte(got), &info); err != nil {
		t.Fatalf("output is not valid JSON: %v\ngot:\n%s", err, got)
	}
	if info.Version != "v1.2.3" {
		t.Errorf("version = %q, want %q", info.Version, "v1.2.3")
	}
	if info.APIGroup != "dns.networking.miloapis.com" {
		t.Errorf("apiGroup = %q", info.APIGroup)
	}
	if info.APIVersion != "v1alpha1" {
		t.Errorf("apiVersion = %q", info.APIVersion)
	}
	// The manifest and the version command must not drift apart.
	if info.PluginAPI != pluginAPIVersion {
		t.Errorf("pluginApiVersion = %d, want %d", info.PluginAPI, pluginAPIVersion)
	}
	if info.GoVersion == "" || info.Platform == "" {
		t.Errorf("goVersion/platform are empty: %+v", info)
	}
}

func TestVersionYAMLAndWide(t *testing.T) {
	got, err := runVersion(t, "-o", "yaml")
	if err != nil {
		t.Fatalf("version -o yaml returned %v", err)
	}
	if !strings.Contains(got, "apiGroup: dns.networking.miloapis.com") {
		t.Errorf("yaml output = %q", got)
	}

	got, err = runVersion(t, "-o", "wide")
	if err != nil {
		t.Fatalf("version -o wide returned %v", err)
	}
	for _, want := range []string{"datumctl-dns", "plugin API 1", "go"} {
		if !strings.Contains(got, want) {
			t.Errorf("wide output does not contain %q\ngot:\n%s", want, got)
		}
	}
}

func TestVersionRejectsNameFormat(t *testing.T) {
	// -o name has no meaning for a version, and silently falling back to a
	// table would hide the typo from a script.
	_, err := runVersion(t, "-o", "name")
	if err == nil {
		t.Fatal("version -o name succeeded")
	}
	var cliErr *util.CLIError
	if !errors.As(err, &cliErr) || cliErr.Code() != util.ExitUsage {
		t.Errorf("error = %v, want a CLIError with ExitUsage", err)
	}
}

func TestVersionNeedsNoEnvironment(t *testing.T) {
	// The whole point: no credentials helper, no API host, no project. A
	// version command that needs a working control plane is useless exactly
	// when you reach for it.
	for _, key := range []string{
		"DATUM_API_HOST", "DATUM_PROJECT", "DATUM_ORG",
		"DATUM_CREDENTIALS_HELPER", "DATUM_PLUGIN_API_VERSION", "DATUM_SESSION",
	} {
		t.Setenv(key, "")
	}

	got, err := runVersion(t)
	if err != nil {
		t.Fatalf("version returned %v with no environment", err)
	}
	if !strings.Contains(got, "datumctl-dns") {
		t.Errorf("output = %q", got)
	}
}

func TestVersionSkipsTheEntitlementPreflight(t *testing.T) {
	// Guards the pre-existing entry in the skip set, which was dead code until
	// this command existed.
	root := Command()
	var found *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == cmdVersion {
			found = c
		}
	}
	if found == nil {
		t.Fatal("the version command is not registered")
	}
	if !skipsEntitlement(found) {
		t.Errorf("skipsEntitlement(version) = false; it must never need a project")
	}
}

func TestVersionRejectsExtraArguments(t *testing.T) {
	// cobra.NoArgs returns a plain error, which would exit 1. enforceUsageExit
	// re-labels it, so a typo'd argument is a usage failure everywhere.
	_, err := runVersion(t, "extra")
	if err == nil {
		t.Fatal("version accepted an extra argument")
	}
	var cliErr *util.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error is %T, want *util.CLIError", err)
	}
	if cliErr.Code() != util.ExitUsage {
		t.Errorf("code = %d (%s), want %d (DNS_USAGE)",
			cliErr.Code(), util.ExitCodeName(cliErr.Code()), util.ExitUsage)
	}
}

func TestEnforceUsageExitLeavesRicherErrorsAlone(t *testing.T) {
	// A command that built its own CLIError with a Fix must keep both.
	cmd := &cobra.Command{
		Use: "custom",
		Args: func(*cobra.Command, []string) error {
			return util.NewCLIError(util.ExitInvalid, "bespoke").WithFix("do the other thing")
		},
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	enforceUsageExit(cmd)

	err := cmd.Args(cmd, []string{"x"})
	var cliErr *util.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error is %T, want *util.CLIError", err)
	}
	if cliErr.Code() != util.ExitInvalid {
		t.Errorf("code = %d, want %d — a richer error must not be flattened to usage", cliErr.Code(), util.ExitInvalid)
	}
	if cliErr.Fix() != "do the other thing" {
		t.Errorf("fix = %q, want it preserved", cliErr.Fix())
	}
}

func TestEnforceUsageExitConvertsStockValidators(t *testing.T) {
	cmd := &cobra.Command{Use: "leaf", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error { return nil }}
	enforceUsageExit(cmd)

	err := cmd.Args(cmd, []string{"unexpected"})
	var cliErr *util.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error is %T, want *util.CLIError", err)
	}
	if cliErr.Code() != util.ExitUsage {
		t.Errorf("code = %d, want %d", cliErr.Code(), util.ExitUsage)
	}

	// A valid invocation is still valid.
	if err := cmd.Args(cmd, nil); err != nil {
		t.Errorf("Args(nil) = %v, want nil", err)
	}
}
