// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"context"
	"errors"
	"net"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// recordSelectors captures the field selectors every List carried, so the tests
// can prove the filtering happened on the server.
func recordSelectors(seen *[]string) interceptor.Funcs {
	return interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			var options client.ListOptions
			options.ApplyOptions(opts)
			if options.FieldSelector != nil {
				*seen = append(*seen, options.FieldSelector.String())
			} else {
				*seen = append(*seen, "<none>")
			}
			return c.List(ctx, list, opts...)
		},
	}
}

// TestReadsUseTheServerSideSelectors — a zone with a thousand records must not
// be pulled down whole to show one, and both fields are declared selectable on
// the CRD precisely so it does not have to be.
func TestReadsUseTheServerSideSelectors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "zone lookup and every type",
			args: []string{"record", "list", testDomain},
			want: []string{
				"spec.domainName=example.com",
				"spec.dnsZoneRef.name=example-com",
			},
		},
		{
			name: "one query per requested type",
			args: []string{"record", "list", testDomain, "--type", "A,MX"},
			want: []string{
				"spec.domainName=example.com",
				"spec.dnsZoneRef.name=example-com,spec.recordType=A",
				"spec.dnsZoneRef.name=example-com,spec.recordType=MX",
			},
		},
		{
			name: "a mutation fetches only its own bucket",
			args: []string{"record", "create", testDomain, "www", "A", "203.0.113.99"},
			want: []string{
				"spec.domainName=example.com",
				"spec.dnsZoneRef.name=example-com,spec.recordType=A",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var seen []string
			ic := recordSelectors(&seen)
			h := newHarnessWithInterceptor(t, &ic, zoneFixture()...)
			requireNoError(t, h.run(tc.args...))

			// The selector string sorts its requirements, so compare as sets.
			normalized := make([]string, len(seen))
			for i, s := range seen {
				parts := strings.Split(s, ",")
				sort.Strings(parts)
				normalized[i] = strings.Join(parts, ",")
			}
			if strings.Join(normalized, " | ") != strings.Join(tc.want, " | ") {
				t.Errorf("selectors =\n  %v\nwant\n  %v", normalized, tc.want)
			}
		})
	}
}

// TestResolveZoneFallsBackToTheObjectName so a name copied out of kubectl works.
func TestResolveZoneFallsBackToTheObjectName(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", nil)))
	requireNoError(t, h.run("record", "list", testZoneObject))
	mustContain(t, collapse(h.stdout()), "www A Auto 203.0.113.10")
}

func TestResolveZoneIgnoresCaseAndTrailingDot(t *testing.T) {
	h := newHarness(t, testZone(), recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", nil)))
	requireNoError(t, h.run("record", "list", "EXAMPLE.com."))
	mustContain(t, collapse(h.stdout()), "www A Auto 203.0.113.10")
}

// TestWritesLandInTheZonesOwnBucket — the zone reference is what scopes a
// record set, and two zones in one namespace must not collide.
func TestWritesLandInTheZonesOwnBucket(t *testing.T) {
	other := testZone()
	other.Name = "other-example"
	other.Spec.DomainName = "other.example"

	otherA := recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "198.51.100.1", nil))
	otherA.Name = "other-example-a"
	otherA.Spec.DNSZoneRef.Name = "other-example"

	h := newHarness(t, testZone(), other, otherA)
	requireNoError(t, h.run("record", "create", testDomain, "www", "A", "203.0.113.10"))

	if got := h.getSet(t, "example-com-a").Spec.Records[0].A.Content; got != "203.0.113.10" {
		t.Errorf("example.com A = %q", got)
	}
	if got := h.getSet(t, "other-example-a").Spec.Records[0].A.Content; got != "198.51.100.1" {
		t.Errorf("other.example was modified: %q", got)
	}
}

// TestTransportFailureSurvivesTheGerundWrapper — the reads wrap their errors
// with "listing record sets: %w" for context, and util now classifies transport
// failures by type rather than by string. The wrapper must not hide the type,
// or a refused connection would exit 1 instead of 8 and automation that retries
// on DNS_UNAVAILABLE would give up.
func TestTransportFailureSurvivesTheGerundWrapper(t *testing.T) {
	ic := interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			if _, isRecordSets := l.(*dnsv1alpha1.DNSRecordSetList); isRecordSets {
				return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
			}
			return c.List(ctx, l, opts...)
		},
	}

	h := newHarnessWithInterceptor(t, &ic, testZone())
	ce := requireExit(t, h.run("record", "list", testDomain), util.ExitUnavailable)
	mustContain(t, ce.Error(), "cannot reach the DNS API")
	mustContain(t, ce.Error(), "listing record sets")
	mustContain(t, ce.Fix(), "check connectivity")
}

// TestZoneMembershipIsTheReferenceNotTheName.
//
// Protection rests on two facts a third party cannot spell differently: which
// zone a set belongs to (spec.dnsZoneRef, a reference the OPERATOR sets) and the
// record's shape. This pins the first half — listSets is the only way a
// DNSRecordSet enters this package, and it always selects on that field, so
// classify never has to re-check membership.
//
// Mutation check: drop the fieldZoneRef selector from listSets and the foreign
// SOA appears in this zone's listing.
func TestZoneMembershipIsTheReferenceNotTheName(t *testing.T) {
	other := testZone()
	other.Name = "other-example"
	other.Spec.DomainName = "other.example"

	// Named as if it belonged to our zone, but referencing the other one. The
	// name is a spelling; the reference is the fact.
	impostor := recordSet(dnsv1alpha1.RRTypeSOA, dnsv1alpha1.RecordEntry{
		Name: "@", TTL: ttl(3600),
		SOA: &dnsv1alpha1.SOARecordSpec{MName: "ns1.elsewhere.net.", RName: "hostmaster.other.example.", Serial: 1},
	})
	impostor.Name = testZoneObject + "-soa"
	impostor.Spec.DNSZoneRef.Name = "other-example"

	h := newHarness(t, testZone(), other, impostor,
		recordSet(dnsv1alpha1.RRTypeA, aEntry("www", "203.0.113.10", nil)))

	requireNoError(t, h.run("record", "list", testDomain))
	mustNotContain(t, h.stdout(), "ns1.elsewhere.net.")
	mustNotContain(t, h.stdout(), markerPlatform)
	mustContain(t, collapse(h.stdout()), "www A Auto 203.0.113.10")

	// And it is visible from the zone that actually owns it.
	requireNoError(t, h.run("record", "list", "other.example"))
	mustContain(t, h.stdout(), "ns1.elsewhere.net.")
	mustContain(t, h.stdout(), markerPlatform)
}
