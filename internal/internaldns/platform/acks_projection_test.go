// SPDX-License-Identifier: AGPL-3.0-only

package platform

import (
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/internaldns/model"
)

func TestOneSinkProjectsEachManifest(t *testing.T) {
	regions := map[string][]model.Member{"central/s": nil, "east/s": nil}
	central := &AckSink{Region: "central", Shard: "s", PublicationRegions: regions}
	east := &AckSink{Region: "east", Shard: "s", PublicationRegions: regions}
	manifest := func(targets ...string) *dnsv1alpha1.DNSPublicationManifest {
		m := &dnsv1alpha1.DNSPublicationManifest{}
		for _, region := range targets {
			m.Spec.ServingTargets = append(m.Spec.ServingTargets, dnsv1alpha1.DNSPublicationTarget{Region: region, Shard: "s"})
		}
		return m
	}
	for _, tc := range []struct {
		name          string
		manifest      *dnsv1alpha1.DNSPublicationManifest
		central, east bool
	}{
		{"both regions", manifest("east", "central"), true, false},
		{"east only", manifest("east"), false, true},
		{"no targets", manifest(), true, false},
		{"unconfigured region first", manifest("alpha", "east"), false, true},
	} {
		if got := central.projectsManifest(tc.manifest); got != tc.central {
			t.Errorf("%s: central projects = %v, want %v", tc.name, got, tc.central)
		}
		if got := east.projectsManifest(tc.manifest); got != tc.east {
			t.Errorf("%s: east projects = %v, want %v", tc.name, got, tc.east)
		}
	}

	single := &AckSink{Region: "central", Shard: "s"}
	if !single.projectsManifest(manifest("central")) {
		t.Error("a sole sink projects every manifest")
	}
}
