// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"

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

func vpc(name string) *networkingv1alpha.Network {
	return &networkingv1alpha.Network{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: util.ResourceNamespace, UID: types.UID("uid-" + name),
	}}
}

func vpcContext(name, vpcName, managedZone string) *dnsv1alpha1.DNSResolverContext {
	rc := &dnsv1alpha1.DNSResolverContext{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: util.ResourceNamespace},
		Spec:       dnsv1alpha1.DNSResolverContextSpec{ConsumerID: "project-uid/uid-" + vpcName},
	}
	rc.Status.ManagedNamespace.DNSZoneRef.Name = managedZone
	return rc
}

func association(name, zone, rc string) *dnsv1alpha1.DNSZoneAssociation {
	a := pendingAssociation(name, zone, rc)
	a.Status.ResolvedDNSZoneRef.Name = zone
	a.Status.ResolvedResolverContextRef.Name = rc
	return a
}

func pendingAssociation(name, zone, rc string) *dnsv1alpha1.DNSZoneAssociation {
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
		vpc("app-a"),
		vpc("app-b"),
		privateZone("managed-a", "datum.internal"),
		privateZone("managed-b", "datum.internal"),
		privateZone("prod-a", "prod.internal"),
		privateZone("prod-b", "prod.internal"),
		vpcContext("ctx-a", "app-a", "managed-a"),
		vpcContext("ctx-b", "app-b", "managed-b"),
		association("prod-a", "prod-a", "ctx-a"),
		association("prod-b", "prod-b", "ctx-b"),
	}
}

func deleting(z *dnsv1alpha1.DNSZone) *dnsv1alpha1.DNSZone {
	now := metav1.Now()
	z.DeletionTimestamp = &now
	z.Finalizers = []string{"test"}
	return z
}

func requireZones(t *testing.T, h *harness, want ...string) {
	t.Helper()
	got := recordSetZones(t, h)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("record sets landed in %v, want %v", got, want)
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
	for _, want := range []string{"managed-a", "managed-b", "VPC app-a", "VPC app-b", "--vpc"} {
		if !strings.Contains(ce.Error()+ce.Fix(), want) {
			t.Errorf("error does not mention %q:\n%s\n%s", want, ce.Error(), ce.Fix())
		}
	}
	requireZones(t, h)
}

func TestVPCPicksItsOwnZone(t *testing.T) {
	h := newHarness(t, twoVPCFixture()...)

	requireNoError(t, h.run("record", "create", "datum.internal", "db", "A", "10.0.0.5", "--vpc", "app-b"))
	requireZones(t, h, "managed-b")
}

func TestVPCFollowsAcceptedAssociations(t *testing.T) {
	h := newHarness(t, twoVPCFixture()...)

	requireNoError(t, h.run("record", "create", "prod.internal", "db", "A", "10.0.0.5", "--vpc", "app-b"))
	requireZones(t, h, "prod-b")
}

func TestPendingAssociationDoesNotCount(t *testing.T) {
	h := newHarness(t,
		vpc("app-a"),
		privateZone("managed-a", "datum.internal"),
		privateZone("managed-b", "datum.internal"),
		vpcContext("ctx-a", "app-a", ""),
		pendingAssociation("b-to-a", "managed-b", "ctx-a"),
	)

	_ = requireExit(t, h.run("record", "create", "datum.internal", "db", "A", "10.0.0.5", "--vpc", "app-a"), util.ExitNotFound)
	requireZones(t, h)
}

func TestContextLabelsDoNotDecideTheVPC(t *testing.T) {
	ctxB := vpcContext("ctx-b", "app-b", "managed-b")
	ctxB.Labels = map[string]string{"networking.datumapis.com/network": "app-a"}
	h := newHarness(t,
		vpc("app-a"),
		vpc("app-b"),
		privateZone("managed-a", "datum.internal"),
		privateZone("managed-b", "datum.internal"),
		ctxB,
	)

	_ = requireExit(t, h.run("record", "create", "datum.internal", "db", "A", "10.0.0.5", "--vpc", "app-a"), util.ExitNotFound)
	requireZones(t, h)
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
	requireZones(t, h, "managed-a")
}

func TestObjectNameWithAnotherVPCIsRefused(t *testing.T) {
	h := newHarness(t, twoVPCFixture()...)

	_ = requireExit(t, h.run("record", "create", "managed-a", "db", "A", "10.0.0.5", "--vpc", "app-b"), util.ExitNotFound)
	requireZones(t, h)
}

func TestDeletingPrivateZoneStillMakesADomainAmbiguous(t *testing.T) {
	objs := twoVPCFixture()
	objs[2] = deleting(privateZone("managed-a", "datum.internal"))
	h := newHarness(t, objs...)

	ce := requireExit(t, h.run("record", "create", "datum.internal", "db", "A", "10.0.0.5"), util.ExitUsage)
	if !strings.Contains(ce.Fix(), "being deleted") {
		t.Errorf("listing does not flag the zone being deleted:\n%s", ce.Fix())
	}
	requireZones(t, h)

	requireNoError(t, h.run("record", "create", "datum.internal", "db", "A", "10.0.0.5", "--vpc", "app-b"))
	requireZones(t, h, "managed-b")
}

func TestDeletingPublicZoneDoesNotMakeADomainAmbiguous(t *testing.T) {
	live := testZone()
	gone := deleting(live.DeepCopy())
	gone.Name = "old-" + live.Name
	h := newHarness(t, live, gone)

	requireNoError(t, h.run("record", "create", testDomain, "db", "A", "10.0.0.5"))
	requireZones(t, h, live.Name)
}

func TestDomainMatchingAnotherZonesObjectNameIsAmbiguous(t *testing.T) {
	h := newHarness(t,
		privateZone("corp.example", "datum.internal"),
		privateZone("managed-c", "corp.example"),
	)

	_ = requireExit(t, h.run("record", "create", "corp.example", "db", "A", "10.0.0.5"), util.ExitUsage)
	requireZones(t, h)
}

func TestPublicZoneSharingADomainIsNamedByObject(t *testing.T) {
	public := testZone()
	h := newHarness(t, public, vpc("app-a"), privateZone("managed-a", testDomain), vpcContext("ctx-a", "app-a", "managed-a"))

	_ = requireExit(t, h.run("record", "list", testDomain), util.ExitUsage)

	requireNoError(t, h.run("record", "list", public.Name))
	if strings.Contains(h.stdout(), "record create "+testDomain+" ") ||
		!strings.Contains(h.stdout(), "record create "+public.Name+" ") {
		t.Fatalf("next steps do not name the public zone by object:\n%s", h.stdout())
	}
}

func TestVPCOnAPublicZone(t *testing.T) {
	h := newHarness(t, zoneFixture()...)

	ce := requireExit(t, h.run("record", "list", testDomain, "--vpc", "typo"), util.ExitUsage)
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
