// SPDX-License-Identifier: AGPL-3.0-only

package plugin_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// shellCompNoFileComp is spelled out so the completion assertion reads without
// a cobra import at the call site.
const shellCompNoFileComp = cobra.ShellCompDirectiveNoFileComp

// newRootForCompletion builds a root command wired to the harness's project, so
// helpers that read --project off the root see the right value.
func newRootForCompletion(t *testing.T) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "dns"}
	root.PersistentFlags().String("project", testProject, "")
	root.PersistentFlags().String("org", testOrg, "")
	root.SetContext(t.Context())
	return root
}

func TestEntitlementPreflightAgainstTheRealAPI(t *testing.T) {
	t.Run("an Active entitlement lets the command proceed", func(t *testing.T) {
		var out bytes.Buffer
		if err := util.EnsureDNSEntitlement(t.Context(), testProject, nonTTY(t), &out); err != nil {
			t.Fatalf("EnsureDNSEntitlement returned %v, want nil", err)
		}
		if out.Len() != 0 {
			t.Errorf("the happy path wrote to stderr: %q", out.String())
		}
	})

	t.Run("no entitlement refuses non-interactively", func(t *testing.T) {
		withoutEntitlement(t)

		var out bytes.Buffer
		err := util.EnsureDNSEntitlement(t.Context(), testProject, nonTTY(t), &out)
		if err == nil {
			t.Fatal("EnsureDNSEntitlement succeeded with no entitlement")
		}

		var cliErr *util.CLIError
		if !errors.As(err, &cliErr) {
			t.Fatalf("error is %T, want *util.CLIError", err)
		}
		if cliErr.Code() != util.ExitForbidden {
			t.Errorf("code = %d (%s), want %d (DNS_FORBIDDEN)",
				cliErr.Code(), util.ExitCodeName(cliErr.Code()), util.ExitForbidden)
		}
		if !strings.Contains(cliErr.Error(), testProject) {
			t.Errorf("message %q does not name the project", cliErr.Error())
		}
		if !strings.Contains(cliErr.Fix(), "datumctl services enable dns.networking.miloapis.com") {
			t.Errorf("fix %q does not carry the enable command", cliErr.Fix())
		}
		// It must never hang waiting for an answer nobody can give.
		if strings.Contains(out.String(), "[y/N]") {
			t.Errorf("a prompt was written non-interactively: %q", out.String())
		}
	})

	t.Run("a Rejected entitlement stops with the resubmit hint", func(t *testing.T) {
		withEntitlementPhase(t, "Rejected")

		var out bytes.Buffer
		err := util.EnsureDNSEntitlement(t.Context(), testProject, nonTTY(t), &out)
		if err == nil {
			t.Fatal("EnsureDNSEntitlement succeeded with a rejected entitlement")
		}
		var cliErr *util.CLIError
		if !errors.As(err, &cliErr) {
			t.Fatalf("error is %T, want *util.CLIError", err)
		}
		if cliErr.Code() != util.ExitForbidden {
			t.Errorf("code = %d, want %d", cliErr.Code(), util.ExitForbidden)
		}
		if !strings.Contains(cliErr.Error(), "rejected") {
			t.Errorf("message %q does not say the request was rejected", cliErr.Error())
		}
	})

	t.Run("a PendingApproval entitlement stops with the status hint", func(t *testing.T) {
		withEntitlementPhase(t, "PendingApproval")

		var out bytes.Buffer
		err := util.EnsureDNSEntitlement(t.Context(), testProject, nonTTY(t), &out)
		if err == nil {
			t.Fatal("EnsureDNSEntitlement succeeded with a pending entitlement")
		}
		var cliErr *util.CLIError
		if !errors.As(err, &cliErr) {
			t.Fatalf("error is %T, want *util.CLIError", err)
		}
		if !strings.Contains(cliErr.Fix(), "datumctl services list") {
			t.Errorf("fix %q does not point at `datumctl services list`", cliErr.Fix())
		}
	})

	t.Run("an empty project is a no-op", func(t *testing.T) {
		var out bytes.Buffer
		if err := util.EnsureDNSEntitlement(t.Context(), "", nonTTY(t), &out); err != nil {
			t.Errorf("EnsureDNSEntitlement(\"\") = %v, want nil", err)
		}
	})
}

// The refusal must render as the documented three-line error, since that is
// what a user actually sees when the pre-flight blocks a command.
func TestEntitlementRefusalRendersTheContract(t *testing.T) {
	withoutEntitlement(t)

	err := util.EnsureDNSEntitlement(t.Context(), testProject, nonTTY(t), &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected a refusal")
	}

	var rendered bytes.Buffer
	code := util.RenderExit(&rendered, err, false)

	if code != util.ExitForbidden {
		t.Errorf("code = %d, want %d", code, util.ExitForbidden)
	}
	for _, want := range []string{
		`Error: DNS is not enabled for project "acme-prod"`,
		"Fix:   enable it with:",
		"datumctl services enable dns.networking.miloapis.com --wait",
		"exit status 3   # DNS_FORBIDDEN",
	} {
		if !strings.Contains(rendered.String(), want) {
			t.Errorf("rendered output does not contain %q\ngot:\n%s", want, rendered.String())
		}
	}
}

// The legacy-alias fold, against a real API server holding two objects.
// Recognition accepts both spellings, so both can exist at once; a stale
// rejected one must not lock the user out of a service they actually hold.
func TestStaleRejectedEntitlementDoesNotMaskALiveOne(t *testing.T) {
	withEntitlementPhase(t, "Active") // the conventional object, restored on cleanup

	legacy := &unstructured.Unstructured{}
	legacy.SetGroupVersionKind(serviceEntitlementGVK)
	legacy.SetName("dns")
	if err := unstructured.SetNestedField(legacy.Object, "dns", "spec", "serviceRef", "name"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(legacy.Object, "Rejected", "status", "phase"); err != nil {
		t.Fatal(err)
	}
	if err := h.k8s.Create(t.Context(), legacy); err != nil {
		t.Fatalf("creating the legacy entitlement: %v", err)
	}
	t.Cleanup(func() {
		if err := h.k8s.Delete(context.Background(), legacy); err != nil {
			t.Errorf("deleting the legacy entitlement: %v", err)
		}
	})

	if err := util.EnsureDNSEntitlement(t.Context(), testProject, nonTTY(t), &bytes.Buffer{}); err != nil {
		t.Errorf("EnsureDNSEntitlement = %v, want nil; a stale rejected object masked a live grant", err)
	}
}
