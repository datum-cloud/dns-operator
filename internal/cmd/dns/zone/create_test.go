// SPDX-License-Identifier: AGPL-3.0-only

package zone

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// fixedSuffix makes generated object names deterministic for the run.
func fixedSuffix(t *testing.T) {
	t.Helper()
	prev := randomSuffix
	randomSuffix = func() string { return "abc123" }
	t.Cleanup(func() { randomSuffix = prev })
}

// fastPolling shortens the create --wait loop so tests do not sleep.
func fastPolling(t *testing.T) {
	t.Helper()
	prev := waitInterval
	waitInterval = time.Millisecond
	t.Cleanup(func() { waitInterval = prev })
}

// zoneClass builds a cluster-scoped DNSZoneClass.
func zoneClass(name string) *dnsv1alpha1.DNSZoneClass {
	return &dnsv1alpha1.DNSZoneClass{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       dnsv1alpha1.DNSZoneClassSpec{ControllerName: "powerdns"},
	}
}

// created fetches the single zone the fake API holds.
func created(t *testing.T, c client.Client) *dnsv1alpha1.DNSZone {
	t.Helper()
	var list dnsv1alpha1.DNSZoneList
	if err := c.List(t.Context(), &list); err != nil {
		t.Fatalf("listing zones: %v", err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("zones = %d, want exactly 1", len(list.Items))
	}
	return &list.Items[0]
}

func TestCreateDefaults(t *testing.T) {
	fixedSuffix(t)
	c := newFakeClient(t, zoneClass(DefaultZoneClass))
	h := newHarness(t, c)

	if err := h.run("zone", "create", "example.com", "--no-wait"); err != nil {
		t.Fatalf("zone create: %v", err)
	}

	z := created(t, c)
	if z.Name != "example-com-abc123" {
		t.Errorf("object name = %q, want a name generated from the domain", z.Name)
	}
	if z.Namespace != util.ResourceNamespace {
		t.Errorf("namespace = %q, want %q", z.Namespace, util.ResourceNamespace)
	}
	if z.Spec.DomainName != "example.com" {
		t.Errorf("spec.domainName = %q, want example.com", z.Spec.DomainName)
	}
	if z.Spec.DNSZoneClassName != DefaultZoneClass {
		t.Errorf("spec.dnsZoneClassName = %q, want %q", z.Spec.DNSZoneClassName, DefaultZoneClass)
	}
	if _, hasDesc := z.Annotations[descriptionAnnotation]; hasDesc {
		t.Error("no --description was given, so no description annotation should be written")
	}
	if !strings.Contains(h.out.String(), "zone/example.com created") {
		t.Errorf("output does not confirm the create:\n%s", h.out.String())
	}
}

func TestCreateLowercasesTheDomain(t *testing.T) {
	fixedSuffix(t)
	c := newFakeClient(t, zoneClass(DefaultZoneClass))
	h := newHarness(t, c)

	// spec.domainName is lowercase-only at admission, and a registrar page is
	// as likely to show "Example.COM." as anything else.
	if err := h.run("zone", "create", "Example.COM.", "--no-wait"); err != nil {
		t.Fatalf("zone create: %v", err)
	}
	if got := created(t, c).Spec.DomainName; got != "example.com" {
		t.Errorf("spec.domainName = %q, want example.com", got)
	}
}

func TestCreateDescriptionIsAnAnnotation(t *testing.T) {
	fixedSuffix(t)
	c := newFakeClient(t, zoneClass(DefaultZoneClass))
	h := newHarness(t, c)

	if err := h.run("zone", "create", "example.com", "--no-wait",
		"--description", "production apex"); err != nil {
		t.Fatalf("zone create: %v", err)
	}

	z := created(t, c)
	if got := z.Annotations[descriptionAnnotation]; got != "production apex" {
		t.Errorf("%s = %q, want %q", descriptionAnnotation, got, "production apex")
	}
}

func TestCreateCustomClass(t *testing.T) {
	fixedSuffix(t)
	c := newFakeClient(t, zoneClass(DefaultZoneClass), zoneClass("datum-internal-dns"))
	h := newHarness(t, c)

	if err := h.run("zone", "create", "example.com", "--no-wait",
		"--class", "datum-internal-dns"); err != nil {
		t.Fatalf("zone create: %v", err)
	}
	if got := created(t, c).Spec.DNSZoneClassName; got != "datum-internal-dns" {
		t.Errorf("spec.dnsZoneClassName = %q, want datum-internal-dns", got)
	}
}

func TestCreateUnknownClass(t *testing.T) {
	fixedSuffix(t)
	c := newFakeClient(t, zoneClass(DefaultZoneClass))
	h := newHarness(t, c)

	err := h.run("zone", "create", "example.com", "--no-wait", "--class", "typo")
	if err == nil {
		t.Fatal("expected an error for a class that does not exist")
	}
	assertExitCode(t, err, util.ExitNotFound)

	var ce *util.CLIError
	if !asCLIError(err, &ce) {
		t.Fatalf("error is not a CLIError: %v", err)
	}
	if !strings.Contains(ce.Fix(), DefaultZoneClass) {
		t.Errorf("fix = %q, want it to list the classes that do exist", ce.Fix())
	}

	var list dnsv1alpha1.DNSZoneList
	if err := c.List(t.Context(), &list); err != nil {
		t.Fatalf("listing zones: %v", err)
	}
	if len(list.Items) != 0 {
		t.Error("a zone was created despite the unknown class")
	}
}

func TestCreateDryRunCreatesNothing(t *testing.T) {
	fixedSuffix(t)

	var sawDryRun bool
	c := newFakeClientWith(t, interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			for _, o := range opts {
				co := &client.CreateOptions{}
				o.ApplyToCreate(co)
				for _, dr := range co.DryRun {
					if dr == metav1.DryRunAll {
						sawDryRun = true
					}
				}
			}
			// Server-side dry run validates but does not persist.
			if sawDryRun {
				return nil
			}
			return cl.Create(ctx, obj, opts...)
		},
	}, zoneClass(DefaultZoneClass))
	h := newHarness(t, c)

	if err := h.run("zone", "create", "example.com", "--dry-run"); err != nil {
		t.Fatalf("zone create --dry-run: %v", err)
	}

	if !sawDryRun {
		t.Error("--dry-run did not send the server-side dry-run option")
	}
	var list dnsv1alpha1.DNSZoneList
	if err := c.List(t.Context(), &list); err != nil {
		t.Fatalf("listing zones: %v", err)
	}
	if len(list.Items) != 0 {
		t.Error("--dry-run persisted a zone")
	}
	if !strings.Contains(h.out.String(), "dry run, nothing was created") {
		t.Errorf("output does not say the run was a dry run:\n%s", h.out.String())
	}
}

func TestCreateWaitsForNameservers(t *testing.T) {
	fixedSuffix(t)
	fastPolling(t)

	// The operator assigns nameservers asynchronously; the fake API cannot, so
	// the Get is intercepted to answer the way a reconciled zone would.
	c := newFakeClientWith(t, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := cl.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if z, isZone := obj.(*dnsv1alpha1.DNSZone); isZone {
				z.Status.Nameservers = []string{"ns1.datum.net.", "ns2.datum.net."}
			}
			return nil
		},
	}, zoneClass(DefaultZoneClass))
	h := newHarness(t, c)

	if err := h.run("zone", "create", "example.com"); err != nil {
		t.Fatalf("zone create: %v", err)
	}

	out := h.out.String()
	wantBlock := strings.Join([]string{
		"zone/example.com created",
		"",
		"Set these nameservers at your domain registrar:",
		"  ns1.datum.net.",
		"  ns2.datum.net.",
		"",
		"The zone will not resolve until the registrar publishes them.",
	}, "\n")
	if !strings.Contains(out, wantBlock) {
		t.Errorf("output =\n%s\nwant it to contain\n%s", out, wantBlock)
	}
	if !strings.Contains(h.err.String(), "Waiting for nameservers") {
		t.Errorf("progress was not written to stderr: %q", h.err.String())
	}
}

func TestCreateWaitStopsOnRejection(t *testing.T) {
	fixedSuffix(t)
	fastPolling(t)

	// A rejected zone never gets nameservers. Waiting out the timeout would
	// bury the reason.
	c := newFakeClientWith(t, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := cl.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if z, isZone := obj.(*dnsv1alpha1.DNSZone); isZone {
				z.Status.Conditions = []metav1.Condition{{
					Type:               "Accepted",
					Status:             metav1.ConditionFalse,
					Reason:             "DNSZoneInUse",
					Message:            "DNSZone claimed by another resource",
					LastTransitionTime: metav1.Now(),
				}}
			}
			return nil
		},
	}, zoneClass(DefaultZoneClass))
	h := newHarness(t, c)

	err := h.run("zone", "create", "example.com", "--timeout", "5s")
	if err == nil {
		t.Fatal("expected an error when the zone is rejected")
	}
	assertExitCode(t, err, util.ExitInvalid)
	if !strings.Contains(err.Error(), "DNSZone claimed by another resource") {
		t.Errorf("error = %q, want the server's message", err.Error())
	}
}

func TestCreateWaitTimeout(t *testing.T) {
	fixedSuffix(t)
	fastPolling(t)

	c := newFakeClient(t, zoneClass(DefaultZoneClass))
	h := newHarness(t, c)

	err := h.run("zone", "create", "example.com", "--timeout", "10ms")
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	var ce *util.CLIError
	if !asCLIError(err, &ce) {
		t.Fatalf("error is not a CLIError: %v", err)
	}
	if !strings.Contains(ce.Error(), "timed out") {
		t.Errorf("error = %q, want it to report the timeout", ce.Error())
	}
	// The zone exists even though the wait gave up; the fix must say so.
	if !strings.Contains(ce.Fix(), "datumctl dns zone describe example.com") {
		t.Errorf("fix = %q, want it to point at the created zone", ce.Fix())
	}
}

func TestCreateRefusesADomainThatAlreadyHasAZone(t *testing.T) {
	fixedSuffix(t)
	c := newFakeClient(t,
		zoneClass(DefaultZoneClass),
		newZone("example-com-old111", "example.com"),
	)
	h := newHarness(t, c)

	err := h.run("zone", "create", "example.com", "--no-wait")
	if err == nil {
		t.Fatal("expected a conflict for a domain that already has a zone")
	}
	assertExitCode(t, err, util.ExitConflict)

	var list dnsv1alpha1.DNSZoneList
	if err := c.List(t.Context(), &list); err != nil {
		t.Fatalf("listing zones: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("zones = %d, want the second create to have been refused", len(list.Items))
	}
}

func TestCreateInvalidDomain(t *testing.T) {
	tests := []struct {
		name   string
		domain string
	}{
		{name: "single label", domain: "localhost"},
		{name: "underscore", domain: "my_zone.com"},
		{name: "trailing hyphen in a label", domain: "bad-.com"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixedSuffix(t)
			c := newFakeClient(t, zoneClass(DefaultZoneClass))
			h := newHarness(t, c)

			err := h.run("zone", "create", tc.domain, "--no-wait")
			if err == nil {
				t.Fatalf("expected an error for %q", tc.domain)
			}
			assertExitCode(t, err, util.ExitUsage)
			if !strings.Contains(err.Error(), tc.domain) {
				t.Errorf("error = %q, want it to quote the input", err.Error())
			}
		})
	}
}

// TestCreateSurfacesAdmissionRejection covers the immutability rule too: the
// CEL validation on spec.domainName reaches the CLI as an Invalid status, and
// there is no `zone update` for a user to hit it any other way.
func TestCreateSurfacesAdmissionRejection(t *testing.T) {
	fixedSuffix(t)

	const celMessage = "A domain name is immutable and cannot be changed after creation"
	c := newFakeClientWith(t, interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			return apierrors.NewInvalid(
				schema.GroupKind{Group: dnsv1alpha1.GroupVersion.Group, Kind: "DNSZone"},
				obj.GetName(),
				field.ErrorList{field.Invalid(field.NewPath("spec", "domainName"), "example.com", celMessage)},
			)
		},
	}, zoneClass(DefaultZoneClass))
	h := newHarness(t, c)

	err := h.run("zone", "create", "example.com", "--no-wait")
	if err == nil {
		t.Fatal("expected the admission rejection to surface")
	}
	assertExitCode(t, err, util.ExitInvalid)
	if !strings.Contains(err.Error(), celMessage) {
		t.Errorf("error = %q, want the server's own message verbatim", err.Error())
	}
}

func TestCreateConflictExplainsTheClaim(t *testing.T) {
	fixedSuffix(t)

	c := newFakeClientWith(t, interceptor.Funcs{
		Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
			return apierrors.NewConflict(
				schema.GroupResource{Group: dnsv1alpha1.GroupVersion.Group, Resource: "dnszones"},
				obj.GetName(), errStub{})
		},
	}, zoneClass(DefaultZoneClass))
	h := newHarness(t, c)

	err := h.run("zone", "create", "example.com", "--no-wait")
	if err == nil {
		t.Fatal("expected a conflict error")
	}
	assertExitCode(t, err, util.ExitConflict)

	var ce *util.CLIError
	if !asCLIError(err, &ce) {
		t.Fatalf("error is not a CLIError: %v", err)
	}
	// The claim is not self-healing, so the fix has to say what to do.
	if !strings.Contains(ce.Fix(), "delete the zone that holds it") {
		t.Errorf("fix = %q, want it to explain the claim does not clear itself", ce.Fix())
	}
}

// errStub is a placeholder cause for a synthesised API conflict.
type errStub struct{}

func (errStub) Error() string { return "the domain is claimed by another zone" }

// flakyGet fails the first n Get calls with a 500, then behaves.
func flakyGet(n int, nameservers ...string) interceptor.Funcs {
	calls := 0
	return interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			calls++
			if calls <= n {
				return apierrors.NewInternalError(errStub{})
			}
			if err := cl.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if z, isZone := obj.(*dnsv1alpha1.DNSZone); isZone {
				z.Status.Nameservers = nameservers
			}
			return nil
		},
	}
}

func TestCreateWaitToleratesATransientReadFailure(t *testing.T) {
	fixedSuffix(t)
	fastPolling(t)

	// One blip must not kill a two-minute poll.
	c := newFakeClientWith(t, flakyGet(2, "ns1.datum.net.", "ns2.datum.net."), zoneClass(DefaultZoneClass))
	h := newHarness(t, c)

	if err := h.run("zone", "create", "example.com", "--timeout", "5s"); err != nil {
		t.Fatalf("zone create: %v", err)
	}
	if !strings.Contains(h.out.String(), "  ns1.datum.net.") {
		t.Errorf("the wait did not recover from a transient failure:\n%s", h.out.String())
	}
}

func TestCreateWaitGivesUpWithAdviceThatWorks(t *testing.T) {
	fixedSuffix(t)
	fastPolling(t)

	// Persistent failure. The zone exists — Create already succeeded — so no
	// message here may suggest re-running the command, which would fail with
	// "already claimed".
	c := newFakeClientWith(t, flakyGet(100), zoneClass(DefaultZoneClass))
	h := newHarness(t, c)

	err := h.run("zone", "create", "example.com", "--timeout", "5s")
	if err == nil {
		t.Fatal("expected the wait to fail")
	}

	var ce *util.CLIError
	if !asCLIError(err, &ce) {
		t.Fatalf("error is not a CLIError: %v", err)
	}
	if !strings.Contains(ce.Fix(), "datumctl dns zone describe example.com") {
		t.Errorf("fix = %q, want it to point at the zone that was created", ce.Fix())
	}
	for _, forbidden := range []string{"retry", "try again", "re-run"} {
		if strings.Contains(strings.ToLower(ce.Fix()), forbidden) {
			t.Errorf("fix = %q, want no suggestion to re-run — the zone already exists", ce.Fix())
		}
	}
	if !strings.Contains(h.out.String(), "zone/example.com created") {
		t.Errorf("stdout should still report the create that succeeded:\n%s", h.out.String())
	}
}

func TestCreateRejectionExplainsTheDeadObject(t *testing.T) {
	fixedSuffix(t)
	fastPolling(t)

	c := newFakeClientWith(t, interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := cl.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if z, isZone := obj.(*dnsv1alpha1.DNSZone); isZone {
				z.Status.Conditions = []metav1.Condition{{
					Type:               util.CondAccepted,
					Status:             metav1.ConditionFalse,
					Reason:             "DNSZoneInUse",
					Message:            "DNSZone claimed by another resource",
					LastTransitionTime: metav1.Now(),
				}}
			}
			return nil
		},
	}, zoneClass(DefaultZoneClass))
	h := newHarness(t, c)

	err := h.run("zone", "create", "example.com", "--timeout", "5s")
	if err == nil {
		t.Fatal("expected the rejection to surface")
	}
	assertExitCode(t, err, util.ExitInvalid)

	var ce *util.CLIError
	if !asCLIError(err, &ce) {
		t.Fatalf("error is not a CLIError: %v", err)
	}
	// The object was created and is parked on Accepted=False forever. Saying
	// only "rejected" leaves the user owning a dead object they do not know is
	// there.
	if !strings.Contains(ce.Fix(), "datumctl dns zone delete example.com") {
		t.Errorf("fix = %q, want it to say how to remove the parked zone", ce.Fix())
	}
}

func TestCreateRejectsAZeroTimeout(t *testing.T) {
	fixedSuffix(t)
	c := newFakeClient(t, zoneClass(DefaultZoneClass))
	h := newHarness(t, c)

	err := h.run("zone", "create", "example.com", "--timeout", "0")
	if err == nil {
		t.Fatal("expected --timeout 0 to be rejected rather than silently meaning the default")
	}
	assertExitCode(t, err, util.ExitUsage)

	var list dnsv1alpha1.DNSZoneList
	if listErr := c.List(t.Context(), &list); listErr != nil {
		t.Fatalf("listing zones: %v", listErr)
	}
	if len(list.Items) != 0 {
		t.Error("a usage error should be caught before anything is created")
	}
}

func TestCreateQuotesWhatTheUserTyped(t *testing.T) {
	fixedSuffix(t)
	h := newHarness(t, newFakeClient(t, zoneClass(DefaultZoneClass)))

	// Normalization strips the trailing dots; the message must still show the
	// string the user actually wrote, or they go looking in the wrong place.
	err := h.run("zone", "create", "example.com..", "--no-wait")
	if err == nil {
		t.Fatal("expected a validation error")
	}
	if !strings.Contains(err.Error(), `"example.com.."`) {
		t.Errorf("error = %q, want it to quote the input as typed", err.Error())
	}
}

func TestCreateRejectsAnOversizedLabel(t *testing.T) {
	fixedSuffix(t)
	h := newHarness(t, newFakeClient(t, zoneClass(DefaultZoneClass)))

	// The CRD pattern accepts this, so client and server agree on a zone that
	// DNS itself can never serve.
	long := strings.Repeat("a", 64) + ".com"
	err := h.run("zone", "create", long, "--no-wait")
	if err == nil {
		t.Fatal("expected a label longer than 63 characters to be rejected")
	}
	assertExitCode(t, err, util.ExitUsage)
	if !strings.Contains(err.Error(), "label longer than 63 characters") {
		t.Errorf("error = %q, want it to name the label limit", err.Error())
	}
}
