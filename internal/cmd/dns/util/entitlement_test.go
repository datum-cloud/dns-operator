// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// entitlementFor builds a ServiceEntitlement carrying the given serviceRef.
func entitlementFor(serviceRef string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(serviceEntitlementGVK)
	obj.SetName(serviceRef)
	if serviceRef != "" {
		_ = unstructured.SetNestedField(obj.Object, serviceRef, "spec", "serviceRef", "name")
	}
	return obj
}

func TestIsDNSEntitlement(t *testing.T) {
	tests := []struct {
		name       string
		serviceRef string
		want       bool
	}{
		{
			name:       "the conventional hyphenated form",
			serviceRef: "dns-networking-miloapis-com",
			want:       true,
		},
		{
			name:       "the legacy bare form compute uses",
			serviceRef: "dns",
			want:       true,
		},
		{
			name:       "another service is not ours",
			serviceRef: "ipam-miloapis-com",
			want:       false,
		},
		{
			name:       "a near miss is not ours",
			serviceRef: "networking-datumapis-com",
			want:       false,
		},
		{
			name:       "the dotted identifier is not a serviceRef",
			serviceRef: "dns.networking.miloapis.com",
			want:       false,
		},
		{
			name:       "an entitlement with no serviceRef",
			serviceRef: "",
			want:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDNSEntitlement(entitlementFor(tc.serviceRef)); got != tc.want {
				t.Errorf("isDNSEntitlement(%q) = %v, want %v", tc.serviceRef, got, tc.want)
			}
		})
	}
}

func TestNewEntitlementObjectUsesTheConventionalRef(t *testing.T) {
	obj := newEntitlementObject()

	// Pinned as literals, not against the constants, so a rename or a slip back
	// to a compute-style short name fails here. These are the exact strings a
	// live platform produced when DNS was enabled on a real project.
	if got := obj.GetName(); got != "dns-networking-miloapis-com" {
		t.Errorf("metadata.name = %q, want %q — the platform creates the hyphenated form, not a short name",
			got, "dns-networking-miloapis-com")
	}
	if got := entitlementServiceRef(obj); got != "dns-networking-miloapis-com" {
		t.Errorf("spec.serviceRef.name = %q, want %q", got, "dns-networking-miloapis-com")
	}

	// Recognition is lenient, but creation must not be: a new entitlement
	// always uses the convention, never the legacy alias.
	if got := entitlementServiceRef(obj); got != dnsServiceRef {
		t.Errorf("spec.serviceRef.name = %q, want %q", got, dnsServiceRef)
	}
	if obj.GetName() != dnsServiceRef {
		t.Errorf("metadata.name = %q, want %q", obj.GetName(), dnsServiceRef)
	}
	if got := obj.GroupVersionKind(); got != serviceEntitlementGVK {
		t.Errorf("GVK = %v, want %v", got, serviceEntitlementGVK)
	}
	if !isDNSEntitlement(obj) {
		t.Errorf("an object we just created is not recognised as the DNS entitlement")
	}
}

func TestEntitlementPhase(t *testing.T) {
	obj := entitlementFor(dnsServiceRef)
	if got := entitlementPhase(obj); got != "" {
		t.Errorf("phase of an object with no status = %q, want empty", got)
	}
	if err := unstructured.SetNestedField(obj.Object, entitlementPhaseActive, "status", "phase"); err != nil {
		t.Fatal(err)
	}
	if got := entitlementPhase(obj); got != entitlementPhaseActive {
		t.Errorf("phase = %q, want %q", got, entitlementPhaseActive)
	}
}

// The hint strings are the whole point of the pre-flight: a user who cannot use
// DNS needs the exact command that fixes it. `datumctl services enable` takes
// the dotted service identifier, never the hyphenated serviceRef.
func TestEntitlementHintsNameTheServiceIdentifier(t *testing.T) {
	tests := []struct {
		name string
		err  *CLIError
	}{
		{name: "not enabled", err: notEnabledErr("acme-prod")},
		{name: "pending approval", err: pendingApprovalErr("acme-prod")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err.Code() != ExitForbidden {
				t.Errorf("code = %d, want %d", tc.err.Code(), ExitForbidden)
			}
			if !strings.Contains(tc.err.Error(), `"acme-prod"`) {
				t.Errorf("message %q does not name the project", tc.err.Error())
			}
			fix := tc.err.Fix()
			if !strings.Contains(fix, dnsServiceIdentifier) {
				t.Errorf("fix %q does not name the service identifier %q", fix, dnsServiceIdentifier)
			}
			if strings.Contains(fix, dnsServiceRef) {
				t.Errorf("fix %q leaks the serviceRef; `services enable` takes the identifier", fix)
			}
			if !strings.Contains(fix, "--wait") {
				t.Errorf("fix %q does not mention --wait", fix)
			}
		})
	}
}

// The same first-match shape as findOwnerCondition, in the other direction.
// Recognition deliberately accepts both the conventional serviceRef and the
// legacy bare "dns", so two objects really can match — and entitlement is a
// capability, so one Active grant is enough regardless of what sits beside it.
func TestBestEntitlementPhase(t *testing.T) {
	list := func(entries ...[2]string) *unstructured.UnstructuredList {
		l := &unstructured.UnstructuredList{}
		for _, e := range entries {
			obj := entitlementFor(e[0])
			if e[1] != "" {
				_ = unstructured.SetNestedField(obj.Object, e[1], "status", "phase")
			}
			l.Items = append(l.Items, *obj)
		}
		return l
	}

	tests := []struct {
		name string
		in   *unstructured.UnstructuredList
		want string
	}{
		{
			name: "no entitlements at all",
			in:   list(),
			want: "",
		},
		{
			name: "only another service's entitlement",
			in:   list([2]string{"ipam-miloapis-com", "Active"}),
			want: "",
		},
		{
			name: "a single active grant",
			in:   list([2]string{"dns-networking-miloapis-com", "Active"}),
			want: "Active",
		},
		{
			// The regression this guards: a stale rejected legacy object listed
			// first would otherwise lock the user out of a service they hold.
			name: "a stale rejected grant does not mask a live one",
			in: list(
				[2]string{"dns", "Rejected"},
				[2]string{"dns-networking-miloapis-com", "Active"}),
			want: "Active",
		},
		{
			name: "order does not matter",
			in: list(
				[2]string{"dns-networking-miloapis-com", "Active"},
				[2]string{"dns", "Rejected"}),
			want: "Active",
		},
		{
			name: "pending beats rejected",
			in: list(
				[2]string{"dns", "Rejected"},
				[2]string{"dns-networking-miloapis-com", "PendingApproval"}),
			want: "PendingApproval",
		},
		{
			name: "active beats pending",
			in: list(
				[2]string{"dns", "PendingApproval"},
				[2]string{"dns-networking-miloapis-com", "Active"}),
			want: "Active",
		},
		{
			name: "rejected alone is still rejected",
			in:   list([2]string{"dns-networking-miloapis-com", "Rejected"}),
			want: "Rejected",
		},
		{
			name: "an entitlement with no phase yet does not count",
			in:   list([2]string{"dns-networking-miloapis-com", ""}),
			want: "",
		},
		{
			name: "another service's active grant never counts",
			in: list(
				[2]string{"ipam-miloapis-com", "Active"},
				[2]string{"dns-networking-miloapis-com", "Rejected"}),
			want: "Rejected",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := bestEntitlementPhase(tc.in); got != tc.want {
				t.Errorf("bestEntitlementPhase = %q, want %q", got, tc.want)
			}
		})
	}
}
