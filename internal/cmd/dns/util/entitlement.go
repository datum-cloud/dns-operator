// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"context"
	"fmt"
	"io"
	"time"

	"go.datum.net/datumctl/plugin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The service catalog names DNS two different ways, and the difference matters.
//
// dnsServiceIdentifier is what a human types: `datumctl services enable
// dns.networking.miloapis.com`. Every user-facing hint must use this form.
//
// dnsServiceRef is what a ServiceEntitlement carries in both metadata.name and
// spec.serviceRef.name. The convention is the identifier with dots replaced by
// hyphens — ipam.miloapis.com entitles as "ipam-miloapis-com",
// networking.datumapis.com as "networking-datumapis-com". Compute is a legacy
// outlier that entitles as the bare "compute".
//
// Both values are confirmed against a live entitlement: enabling DNS on a real
// project produced an object named "dns-networking-miloapis-com" whose
// spec.serviceRef.name is the same string, reaching phase Active.
const (
	dnsServiceIdentifier = "dns.networking.miloapis.com"
	dnsServiceRef        = "dns-networking-miloapis-com"

	// dnsServiceRefLegacy is the short form compute uses. Recognised, never
	// created — see dnsServiceRefAliases.
	dnsServiceRefLegacy = "dns"
)

// dnsServiceRefAliases are the spec.serviceRef.name values that count as "this
// is the DNS entitlement" when scanning.
//
// dnsServiceRef is the observed value, so the legacy bare "dns" is belt and
// braces rather than a hedge against a guess: a project entitled under an older
// convention still gets recognised instead of prompting to create a duplicate
// on every invocation. Creation always uses dnsServiceRef; only recognition is
// lenient.
var dnsServiceRefAliases = map[string]bool{
	dnsServiceRef:       true,
	dnsServiceRefLegacy: true,
}

// bestEntitlementPhase folds every DNS entitlement in the list down to the one
// that decides the outcome, returning "" when there are none.
//
// It takes the BEST phase, not the first and not the worst, and that direction
// is deliberate: entitlement is a capability, so one Active grant is enough no
// matter what else exists alongside it. Reading only the first match would let
// list order decide — and because recognition deliberately accepts both the
// conventional serviceRef and the legacy bare "dns", two objects really can
// match. A stale rejected "dns" listed ahead of a live
// "dns-networking-miloapis-com" would otherwise lock the user out of a service
// they are entitled to.
func bestEntitlementPhase(list *unstructured.UnstructuredList) string {
	rank := map[string]int{
		entitlementPhaseRejected:        1,
		entitlementPhasePendingApproval: 2,
		entitlementPhaseActive:          3,
	}

	best, bestRank := "", 0
	for i := range list.Items {
		item := &list.Items[i]
		if !isDNSEntitlement(item) {
			continue
		}
		if phase := entitlementPhase(item); rank[phase] > bestRank {
			best, bestRank = phase, rank[phase]
		}
	}
	return best
}

// isDNSEntitlement reports whether a ServiceEntitlement is the DNS one.
func isDNSEntitlement(obj *unstructured.Unstructured) bool {
	return dnsServiceRefAliases[entitlementServiceRef(obj)]
}

// Entitlement phases published by the service-catalog reconciler.
//
// Enabling DNS is self-service: a real enable returns Active immediately with
// no approval step. PendingApproval and Rejected are therefore handled but
// unverified against a live platform in those states — the end-to-end harness
// drives them against a stand-in CRD, which pins the branch but not the
// platform's real wording or timing. Treat both as best-effort until a
// provider-gated service exercises them.
const (
	entitlementPhaseActive          = "Active"
	entitlementPhasePendingApproval = "PendingApproval"
	entitlementPhaseRejected        = "Rejected"
)

// entitlementWatchTimeout bounds how long the interactive path waits for the
// reconciler to publish the entitlement's Ready condition after Create.
const entitlementWatchTimeout = 15 * time.Second

// serviceEntitlementGVK addresses service-catalog's ServiceEntitlement. The
// object is handled unstructured because go.miloapis.com/service-catalog is not
// a dependency of this module; switching to the typed client is a drop-in
// change if it ever becomes one.
var serviceEntitlementGVK = schema.GroupVersionKind{
	Group:   "services.miloapis.com",
	Version: "v1alpha1",
	Kind:    "ServiceEntitlement",
}

// EnsureDNSEntitlement is the pre-flight the plugin runs before any DNS API
// call: it verifies the active project has an Active ServiceEntitlement for the
// DNS service, and offers to enable one when it does not.
//
//   - project == "": no-op. Platform-scoped calls are not project-entitled.
//   - Active: proceed.
//   - PendingApproval: stop, pointing at `datumctl services list`.
//   - Rejected: stop, pointing at `datumctl services enable`.
//   - none found, or the API is not served in this control plane: prompt and
//     enable on a TTY, else return an actionable error rather than hanging.
//
// out should be cmd.ErrOrStderr() and in should be cmd.InOrStdin(), so prompts
// never pollute the structured output contract on stdout.
func EnsureDNSEntitlement(ctx context.Context, project string, in io.Reader, out io.Writer) error {
	if project == "" {
		return nil
	}

	wc, err := newEntitlementClient(project)
	if err != nil {
		return err
	}

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(serviceEntitlementGVK.GroupVersion().WithKind(serviceEntitlementGVK.Kind + "List"))
	if err := wc.List(ctx, list); err != nil {
		if apimeta.IsNoMatchError(err) {
			// The service-catalog API is not served in this project's control
			// plane — treat it as "no entitlement exists yet".
			return promptAndRequestEntitlement(ctx, project, wc, in, out)
		}
		// An auth or RBAC failure here is not "the API is unreachable", and
		// telling the user to check their connection when their token expired
		// sends them the wrong way. Let the shared classifier decide, and only
		// fall back to the unreachable wording when it has nothing better.
		if classified := ClassifyError(err); classified.Code() != ExitError {
			return classified
		}
		return NewCLIError(ExitUnavailable,
			fmt.Sprintf("checking the DNS service entitlement for project %q: %v", project, err)).
			WithFix("verify you are logged in (datumctl login) and the project is reachable.").
			WithCause(err)
	}

	switch bestEntitlementPhase(list) {
	case entitlementPhaseActive:
		return nil
	case entitlementPhasePendingApproval:
		return pendingApprovalErr(project)
	case entitlementPhaseRejected:
		return NewCLIError(ExitForbidden,
			fmt.Sprintf("the DNS entitlement request for project %q was rejected", project)).
			WithFix(fmt.Sprintf("submit a new request with:\n       datumctl services enable %s --wait", dnsServiceIdentifier))
	}

	return promptAndRequestEntitlement(ctx, project, wc, in, out)
}

// promptAndRequestEntitlement handles the "no entitlement yet" case. On a TTY it
// asks whether to enable DNS and, if confirmed, creates the ServiceEntitlement
// and watches briefly for it to reach a terminal phase. Non-interactively it
// returns an actionable error rather than blocking on an unanswerable prompt,
// so a CI job fails fast instead of hanging.
func promptAndRequestEntitlement(ctx context.Context, project string, wc client.WithWatch, in io.Reader, out io.Writer) error {
	if NonInteractive(in) {
		return notEnabledErr(project)
	}

	_, _ = fmt.Fprintf(out, "DNS is not enabled for project %q.\n", project)
	_, _ = fmt.Fprint(out, "Would you like to enable it now? [y/N]: ")

	// readLine, not a bufio Scanner: this prompt runs in PersistentPreRunE,
	// ahead of whatever confirmation the command itself asks for, and a
	// buffered reader here would swallow that second answer.
	answer, err := readLine(in)
	if err != nil {
		return err
	}
	if !isAffirmative(answer) {
		return notEnabledErr(project)
	}

	_, _ = fmt.Fprintf(out, "Enabling DNS for project %q...\n", project)

	entitlement := newEntitlementObject()
	if err := wc.Create(ctx, entitlement); err != nil {
		// Something else created it between the List above and here — a
		// concurrent command, or a race with the portal. It exists now, which
		// is what we were trying to achieve, so fall through to the watch
		// rather than failing.
		if !apierrors.IsAlreadyExists(err) {
			if classified := ClassifyError(err); classified.Code() != ExitError {
				return classified
			}
			return NewCLIError(ExitUnavailable,
				fmt.Sprintf("enabling DNS for project %q: %v", project, err)).
				WithCause(err)
		}
	}

	watchCtx, cancel := context.WithTimeout(ctx, entitlementWatchTimeout)
	defer cancel()

	watchList := &unstructured.UnstructuredList{}
	watchList.SetGroupVersionKind(serviceEntitlementGVK.GroupVersion().WithKind(serviceEntitlementGVK.Kind + "List"))
	watcher, err := wc.Watch(watchCtx, watchList)
	if err != nil {
		// The request was created; we just cannot observe it.
		return pendingApprovalErr(project)
	}
	defer watcher.Stop()

	for {
		select {
		case <-watchCtx.Done():
			_, _ = fmt.Fprintf(out, "\nDNS for project %q has been requested but is not active yet.\n", project)
			_, _ = fmt.Fprint(out, "Run your command again once it becomes active.\n\n")
			_, _ = fmt.Fprint(out, "Check status with: datumctl services list\n")
			return pendingApprovalErr(project)

		case event, open := <-watcher.ResultChan():
			if !open {
				return pendingApprovalErr(project)
			}
			if event.Type != watch.Modified && event.Type != watch.Added {
				continue
			}
			item, isUnstructured := event.Object.(*unstructured.Unstructured)
			if !isUnstructured || !isDNSEntitlement(item) {
				continue
			}
			switch entitlementPhase(item) {
			case entitlementPhaseActive:
				_, _ = fmt.Fprintf(out, "DNS enabled for project %q.\n\n", project)
				return nil
			case entitlementPhaseRejected:
				return NewCLIError(ExitForbidden,
					fmt.Sprintf("the DNS entitlement request for project %q was rejected", project)).
					WithFix(fmt.Sprintf("submit a new request with:\n       datumctl services enable %s --wait", dnsServiceIdentifier))
			case entitlementPhasePendingApproval:
				_, _ = fmt.Fprintf(out, "\nDNS for project %q has been requested but is not active yet.\n", project)
				_, _ = fmt.Fprint(out, "Check status with: datumctl services list\n")
				return pendingApprovalErr(project)
			}
		}
	}
}

// notEnabledErr is the "declined, or cannot prompt" result.
func notEnabledErr(project string) *CLIError {
	return NewCLIError(ExitForbidden, fmt.Sprintf("DNS is not enabled for project %q", project)).
		WithFix(fmt.Sprintf("enable it with:\n       datumctl services enable %s --wait", dnsServiceIdentifier))
}

// pendingApprovalErr is the "requested, not active yet" result.
func pendingApprovalErr(project string) *CLIError {
	return NewCLIError(ExitForbidden, fmt.Sprintf("DNS for project %q is not active yet", project)).
		WithFix(fmt.Sprintf("wait for it to activate with:\n       datumctl services enable %s --wait\n"+
			"       or check the status with:\n       datumctl services list", dnsServiceIdentifier))
}

// newEntitlementObject builds the ServiceEntitlement the pre-flight creates.
// Both metadata.name and spec.serviceRef.name carry the hyphenated form, which
// is what the platform itself creates — not a short name like compute's.
func newEntitlementObject() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(serviceEntitlementGVK)
	obj.SetName(dnsServiceRef)
	_ = unstructured.SetNestedField(obj.Object, dnsServiceRef, "spec", "serviceRef", "name")
	return obj
}

// entitlementServiceRef reads spec.serviceRef.name.
func entitlementServiceRef(obj *unstructured.Unstructured) string {
	name, _, _ := unstructured.NestedString(obj.Object, "spec", "serviceRef", "name")
	return name
}

// entitlementPhase reads status.phase.
func entitlementPhase(obj *unstructured.Unstructured) string {
	phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
	return phase
}

// newEntitlementClient builds a watch-capable client against the active
// project's control plane, reusing the plugin's transport contract: the same URL
// construction as NewClient and a fresh token from the credentials helper.
func newEntitlementClient(project string) (client.WithWatch, error) {
	pluginCtx := plugin.Context()
	if pluginCtx.APIHost == "" {
		return nil, NewCLIError(ExitUnavailable,
			"cannot check the DNS service entitlement: DATUM_API_HOST is not set").
			WithFix("run this through datumctl:\n       datumctl dns ...")
	}

	token, err := plugin.Token()
	if err != nil {
		return nil, NewCLIError(ExitUnavailable, fmt.Sprintf("getting credentials: %v", err)).
			WithFix("re-run `datumctl login` and try again.").
			WithCause(err)
	}

	cfg := &rest.Config{
		Host:            ProjectControlPlaneURL(pluginCtx.APIHost, project),
		BearerToken:     token,
		UserAgent:       UserAgent(),
		TLSClientConfig: tlsClientConfig(),
	}

	// No Scheme is set: controller-runtime falls back to the client-go scheme,
	// which carries the list metadata the unstructured path needs.
	wc, err := client.NewWithWatch(cfg, client.Options{})
	if err != nil {
		return nil, NewCLIError(ExitUnavailable, fmt.Sprintf("building entitlement client: %v", err)).WithCause(err)
	}
	return wc, nil
}
