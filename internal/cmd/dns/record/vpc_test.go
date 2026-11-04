// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

func privateZone(name, domain string) *dnsv1alpha1.DNSZone {
	return &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: util.ResourceNamespace},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       domain,
			DNSZoneClassName: "private",
			Visibility:       dnsv1alpha1.DNSZoneVisibilityPrivate,
		},
	}
}

func vpcContext(name, vpc, managedZone string) *dnsv1alpha1.DNSResolverContext {
	rc := &dnsv1alpha1.DNSResolverContext{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: util.ResourceNamespace,
			Labels:    map[string]string{util.NetworkLabel: vpc},
		},
		Spec: dnsv1alpha1.DNSResolverContextSpec{ConsumerID: name},
	}
	rc.Status.ManagedNamespace.DNSZoneRef.Name = managedZone
	return rc
}

func association(name, zone, rc string) *dnsv1alpha1.DNSZoneAssociation {
	return &dnsv1alpha1.DNSZoneAssociation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: util.ResourceNamespace},
		Spec: dnsv1alpha1.DNSZoneAssociationSpec{
			DNSZoneRef:         dnsv1alpha1.DNSNameReference{Name: zone},
			ResolverContextRef: dnsv1alpha1.DNSNameReference{Name: rc},
		},
	}
}

func twoVPCFixture() []client.Object {
	return []client.Object{
		privateZone("managed-a", "datum.internal"),
		privateZone("managed-b", "datum.internal"),
		privateZone("prod", "prod.internal"),
		vpcContext("ctx-a", "app-a", "managed-a"),
		vpcContext("ctx-b", "app-b", "managed-b"),
		association("prod-b", "prod", "ctx-b"),
	}
}

func recordSetZones(t *testing.T, h *harness) []string {
	t.Helper()
	var list dnsv1alpha1.DNSRecordSetList
	if err := h.client.List(context.Background(), &list); err != nil {
		t.Fatalf("listing record sets: %v", err)
	}
	zones := make([]string, 0, len(list.Items))
	for i := range list.Items {
		zones = append(zones, list.Items[i].Spec.DNSZoneRef.Name)
	}
	return zones
}

func TestSharedDomainRequiresVPC(t *testing.T) {
	h := newHarness(t, twoVPCFixture()...)

	ce := requireExit(t, h.run("record", "create", "datum.internal", "db", "A", "10.0.0.5"), util.ExitUsage)
	for _, want := range []string{"managed-a", "managed-b", "app-a", "app-b", "--vpc"} {
		if !strings.Contains(ce.Error()+ce.Fix(), want) {
			t.Errorf("error does not mention %q:\n%s\n%s", want, ce.Error(), ce.Fix())
		}
	}
	if zones := recordSetZones(t, h); len(zones) != 0 {
		t.Fatalf("an ambiguous create wrote record sets to %v", zones)
	}
}

func TestVPCPicksItsOwnZone(t *testing.T) {
	h := newHarness(t, twoVPCFixture()...)

	requireNoError(t, h.run("record", "create", "datum.internal", "db", "A", "10.0.0.5", "--vpc", "app-b"))
	zones := recordSetZones(t, h)
	if len(zones) != 1 || zones[0] != "managed-b" {
		t.Fatalf("record sets landed in %v, want [managed-b]", zones)
	}
}

func TestVPCFollowsAssociations(t *testing.T) {
	h := newHarness(t, twoVPCFixture()...)

	_ = requireExit(t, h.run("record", "list", "prod.internal", "--vpc", "app-a"), util.ExitNotFound)
	requireNoError(t, h.run("record", "list", "prod.internal", "--vpc", "app-b"))
}

func TestUnknownVPC(t *testing.T) {
	h := newHarness(t, twoVPCFixture()...)

	ce := requireExit(t, h.run("record", "list", "datum.internal", "--vpc", "nope"), util.ExitNotFound)
	if !strings.Contains(ce.Error(), `"nope"`) {
		t.Fatalf("error does not name the VPC: %v", ce)
	}
}

func TestObjectNameStillSelectsAZone(t *testing.T) {
	h := newHarness(t, twoVPCFixture()...)

	requireNoError(t, h.run("record", "create", "managed-a", "db", "A", "10.0.0.5"))
	zones := recordSetZones(t, h)
	if len(zones) != 1 || zones[0] != "managed-a" {
		t.Fatalf("record sets landed in %v, want [managed-a]", zones)
	}
}

func TestTerminatingZoneDoesNotMakeADomainAmbiguous(t *testing.T) {
	gone := privateZone("managed-old", "datum.internal")
	now := metav1.Now()
	gone.DeletionTimestamp = &now
	gone.Finalizers = []string{"test"}
	h := newHarness(t, privateZone("managed-a", "datum.internal"), gone)

	requireNoError(t, h.run("record", "create", "datum.internal", "db", "A", "10.0.0.5"))
	zones := recordSetZones(t, h)
	if len(zones) != 1 || zones[0] != "managed-a" {
		t.Fatalf("record sets landed in %v, want [managed-a]", zones)
	}
}

func TestVPCOnAPublicZone(t *testing.T) {
	h := newHarness(t, append(zoneFixture(), twoVPCFixture()...)...)

	ce := requireExit(t, h.run("record", "list", testDomain, "--vpc", "app-a"), util.ExitUsage)
	if !strings.Contains(ce.Error(), "public zone") {
		t.Fatalf("error does not say the zone is public: %v", ce)
	}
}

func TestHintsNameAPrivateZoneByObject(t *testing.T) {
	h := newHarness(t, twoVPCFixture()...)

	requireNoError(t, h.run("record", "list", "datum.internal", "--vpc", "app-b"))
	if !strings.Contains(h.stdout(), "datumctl dns record create managed-b ") {
		t.Fatalf("next steps do not name the zone by object:\n%s", h.stdout())
	}
}
