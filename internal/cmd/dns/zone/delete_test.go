// SPDX-License-Identifier: AGPL-3.0-only

package zone

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// interactive makes the confirmation prompt answerable: CI is what
// util.NonInteractive checks first, and it is set in the environment the tests
// themselves run in.
func interactive(t *testing.T) {
	t.Helper()
	t.Setenv("CI", "")
	if err := os.Unsetenv("CI"); err != nil {
		t.Fatalf("unsetting CI: %v", err)
	}
}

// zoneWithRecords is a zone holding 12 record entries across three sets, which
// is the count the cascade warning must report.
func zoneWithRecords(t *testing.T) client.Client {
	t.Helper()
	return newFakeClient(t,
		newZone("example-com-abc123", "example.com", withRecordCount(12)),
		newRecordSet("example-com-a", "example-com-abc123", dnsv1alpha1.RRTypeA, 8),
		newRecordSet("example-com-mx", "example-com-abc123", dnsv1alpha1.RRTypeMX, 2),
		newRecordSet("example-com-txt", "example-com-abc123", dnsv1alpha1.RRTypeTXT, 2),
		// A record set in another zone must not be counted.
		newRecordSet("other-a", "other-com-zzz999", dnsv1alpha1.RRTypeA, 5),
	)
}

func TestDeletePromptStatesTheCascade(t *testing.T) {
	interactive(t)
	c := zoneWithRecords(t)
	h := newHarness(t, c)
	h.in.WriteString("example.com\n")

	if err := h.run("zone", "delete", "example.com"); err != nil {
		t.Fatalf("zone delete: %v", err)
	}

	// The prompt goes to stderr so it never pollutes stdout.
	prompt := h.err.String()
	wantPrompt := strings.Join([]string{
		"Deleting zone example.com will also delete all 12 DNS records it contains.",
		"This cannot be undone, and the domain will stop resolving.",
	}, "\n")
	if !strings.Contains(prompt, wantPrompt) {
		t.Errorf("prompt =\n%s\nwant it to contain\n%s", prompt, wantPrompt)
	}
	if !strings.Contains(prompt, `Type "example.com" to confirm:`) {
		t.Errorf("prompt does not ask for the zone name: %q", prompt)
	}

	if err := c.Get(t.Context(), client.ObjectKey{
		Namespace: util.ResourceNamespace, Name: "example-com-abc123",
	}, &dnsv1alpha1.DNSZone{}); err == nil {
		t.Error("the zone still exists after a confirmed delete")
	}
	if !strings.Contains(h.out.String(), "zone/example.com deleted — 12 DNS records were deleted with it") {
		t.Errorf("output does not report the cascade:\n%s", h.out.String())
	}
}

func TestDeleteAbortsOnAMismatchedAnswer(t *testing.T) {
	interactive(t)
	c := zoneWithRecords(t)
	h := newHarness(t, c)
	h.in.WriteString("yes\n")

	err := h.run("zone", "delete", "example.com")
	if err == nil {
		t.Fatal("expected the delete to abort")
	}
	assertExitCode(t, err, util.ExitAborted)

	if getErr := c.Get(t.Context(), client.ObjectKey{
		Namespace: util.ResourceNamespace, Name: "example-com-abc123",
	}, &dnsv1alpha1.DNSZone{}); getErr != nil {
		t.Errorf("the zone was deleted despite a mismatched confirmation: %v", getErr)
	}
}

func TestDeleteRefusesNonInteractivelyWithoutYes(t *testing.T) {
	// The high-blast-radius gate refuses rather than proceeding: nobody can
	// type the zone name in CI, and the action destroys every record.
	t.Setenv("CI", "1")

	c := zoneWithRecords(t)
	h := newHarness(t, c)

	err := h.run("zone", "delete", "example.com")
	if err == nil {
		t.Fatal("expected the delete to be refused")
	}
	assertExitCode(t, err, util.ExitAborted)

	var ce *util.CLIError
	if !asCLIError(err, &ce) {
		t.Fatalf("error is not a CLIError: %v", err)
	}
	if !strings.Contains(ce.Fix(), "--yes") {
		t.Errorf("fix = %q, want it to name --yes", ce.Fix())
	}

	if getErr := c.Get(t.Context(), client.ObjectKey{
		Namespace: util.ResourceNamespace, Name: "example-com-abc123",
	}, &dnsv1alpha1.DNSZone{}); getErr != nil {
		t.Errorf("the zone was deleted despite the refusal: %v", getErr)
	}
}

func TestDeleteWithYesSkipsThePrompt(t *testing.T) {
	t.Setenv("CI", "1")

	c := zoneWithRecords(t)
	h := newHarness(t, c)

	if err := h.run("zone", "delete", "example.com", "--yes"); err != nil {
		t.Fatalf("zone delete --yes: %v", err)
	}
	if h.err.Len() != 0 {
		t.Errorf("--yes still prompted: %q", h.err.String())
	}
	if getErr := c.Get(t.Context(), client.ObjectKey{
		Namespace: util.ResourceNamespace, Name: "example-com-abc123",
	}, &dnsv1alpha1.DNSZone{}); getErr == nil {
		t.Error("the zone still exists after --yes")
	}
}

func TestDeleteEmptyZoneWording(t *testing.T) {
	interactive(t)
	c := newFakeClient(t, newZone("example-com-abc123", "example.com"))
	h := newHarness(t, c)
	h.in.WriteString("example.com\n")

	if err := h.run("zone", "delete", "example.com"); err != nil {
		t.Fatalf("zone delete: %v", err)
	}
	if strings.Contains(h.err.String(), "all 0 DNS records") {
		t.Errorf("an empty zone should not be described by a zero count:\n%s", h.err.String())
	}
	if !strings.Contains(h.err.String(), "Deleting zone example.com removes it permanently.") {
		t.Errorf("prompt =\n%s\nwant the empty-zone wording", h.err.String())
	}
	if got := h.out.String(); got != "zone/example.com deleted\n" {
		t.Errorf("output = %q, want a bare confirmation", got)
	}
}

func TestDeleteDryRun(t *testing.T) {
	t.Setenv("CI", "1")

	var sawDryRun bool
	c := newFakeClientWith(t, interceptor.Funcs{
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			for _, o := range opts {
				do := &client.DeleteOptions{}
				o.ApplyToDelete(do)
				for _, dr := range do.DryRun {
					if dr == metav1.DryRunAll {
						sawDryRun = true
					}
				}
			}
			if sawDryRun {
				return nil
			}
			return cl.Delete(ctx, obj, opts...)
		},
	},
		newZone("example-com-abc123", "example.com", withRecordCount(12)),
		newRecordSet("example-com-a", "example-com-abc123", dnsv1alpha1.RRTypeA, 12),
	)
	h := newHarness(t, c)

	// --dry-run does not prompt: it changes nothing, so there is nothing to
	// confirm, and it must work non-interactively.
	if err := h.run("zone", "delete", "example.com", "--dry-run"); err != nil {
		t.Fatalf("zone delete --dry-run: %v", err)
	}
	if !sawDryRun {
		t.Error("--dry-run did not send the server-side dry-run option")
	}
	if !strings.Contains(h.out.String(),
		"zone/example.com would be deleted, along with 12 DNS records — dry run, nothing was deleted") {
		t.Errorf("output =\n%s", h.out.String())
	}
	if getErr := c.Get(t.Context(), client.ObjectKey{
		Namespace: util.ResourceNamespace, Name: "example-com-abc123",
	}, &dnsv1alpha1.DNSZone{}); getErr != nil {
		t.Errorf("--dry-run deleted the zone: %v", getErr)
	}
}

func TestDeleteNotFound(t *testing.T) {
	t.Setenv("CI", "1")
	h := newHarness(t, newFakeClient(t, newZone("example-com-abc123", "example.com")))

	err := h.run("zone", "delete", "missing.com", "--yes")
	if err == nil {
		t.Fatal("expected an error for a zone that does not exist")
	}
	assertExitCode(t, err, util.ExitNotFound)
}

// denyRecordSetList fails only the record-set listing, the way RBAC granting
// get and delete on DNSZone without list on DNSRecordSet does. The zone lookup
// itself must keep working, or the test would prove nothing about the count.
func denyRecordSetList() interceptor.Funcs {
	return interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, isRecordSets := list.(*dnsv1alpha1.DNSRecordSetList); isRecordSets {
				return apierrors.NewForbidden(
					schema.GroupResource{Group: dnsv1alpha1.GroupVersion.Group, Resource: "dnsrecordsets"},
					"", errors.New("not authorized"))
			}
			return cl.List(ctx, list, opts...)
		},
	}
}

// TestDeleteUncountableCascadeIsNotReportedAsEmpty is the regression guard for
// the worst failure this command can have: asking for informed consent to
// destroy records while reporting that there are none.
func TestDeleteUncountableCascadeIsNotReportedAsEmpty(t *testing.T) {
	interactive(t)

	// status.recordCount is 0 and the listing is denied, so neither input can
	// see the twelve entries that are really there.
	c := newFakeClientWith(t, denyRecordSetList(),
		newZone("example-com-abc123", "example.com"),
		newRecordSet("example-com-a", "example-com-abc123", dnsv1alpha1.RRTypeA, 12),
	)
	h := newHarness(t, c)
	h.in.WriteString("example.com\n")

	if err := h.run("zone", "delete", "example.com"); err != nil {
		t.Fatalf("zone delete: %v", err)
	}

	prompt := h.err.String()
	want := strings.Join([]string{
		"Deleting zone example.com will also delete every DNS record it contains.",
		"The record count is unavailable — you are not authorized to list record sets in this project — " +
			"so this zone may hold records that are not listed here.",
		"This cannot be undone, and the domain will stop resolving.",
	}, "\n")
	if !strings.Contains(prompt, want) {
		t.Errorf("prompt =\n%s\nwant it to contain\n%s", prompt, want)
	}
	// The old wording claimed the zone held nothing.
	if strings.Contains(prompt, "removes it permanently") {
		t.Errorf("an uncounted zone must not be described as holding nothing:\n%s", prompt)
	}

	if got, want := h.out.String(),
		"zone/example.com deleted — any DNS records it contained were deleted with it\n"; got != want {
		t.Errorf("receipt = %q, want %q", got, want)
	}
}

func TestDeleteDryRunWithUncountableCascade(t *testing.T) {
	t.Setenv("CI", "1")

	c := newFakeClientWith(t, denyRecordSetList(), newZone("example-com-abc123", "example.com"))
	h := newHarness(t, c)

	if err := h.run("zone", "delete", "example.com", "--dry-run"); err != nil {
		t.Fatalf("zone delete --dry-run: %v", err)
	}
	want := "zone/example.com would be deleted, along with every DNS record it contains — " +
		"dry run, nothing was deleted\n"
	if got := h.out.String(); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

// TestDeleteRefusesThroughARealPipe exercises the production non-interactive
// path rather than the CI environment variable: a real *os.File that is not a
// terminal, which is what a script redirecting stdin actually hands us.
func TestDeleteRefusesThroughARealPipe(t *testing.T) {
	interactive(t)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("creating pipe: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	// Something is on the other end, and it is still not a terminal: the typed
	// confirmation must refuse rather than read whatever arrives.
	if _, err := w.WriteString("example.com\n"); err != nil {
		t.Fatalf("writing to pipe: %v", err)
	}
	_ = w.Close()

	c := zoneWithRecords(t)
	h := newHarness(t, c)
	h.root.SetIn(r)

	runErr := h.run("zone", "delete", "example.com")
	if runErr == nil {
		t.Fatal("expected the delete to be refused through a pipe")
	}
	assertExitCode(t, runErr, util.ExitAborted)

	if getErr := c.Get(t.Context(), client.ObjectKey{
		Namespace: util.ResourceNamespace, Name: "example-com-abc123",
	}, &dnsv1alpha1.DNSZone{}); getErr != nil {
		t.Errorf("the zone was deleted through a non-interactive pipe: %v", getErr)
	}
}

func TestDeleteByObjectNameSaysSoInThePrompt(t *testing.T) {
	interactive(t)

	c := zoneWithRecords(t)
	h := newHarness(t, c)
	h.in.WriteString("example.com\n")

	if err := h.run("zone", "delete", "example-com-abc123"); err != nil {
		t.Fatalf("zone delete by object name: %v", err)
	}
	if !strings.Contains(h.err.String(),
		`You named the zone by its object name "example-com-abc123"; confirm with its domain.`) {
		t.Errorf("prompt does not explain the name mismatch:\n%s", h.err.String())
	}
}
