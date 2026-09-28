// SPDX-License-Identifier: AGPL-3.0-only

package pdns

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// Two record sets of one type claim one name. However the one that goes stops
// claiming it, the name must end up holding the other's records, with the
// other's ownership note, on the backend the control plane runs. One container
// serves every case; each case uses a name of its own.
func TestPDNS_LMDB_ARemovedNameGoesToTheRecordSetStillClaimingIt(t *testing.T) {
	// No t.Parallel(): container + real PDNS.
	baseURL, apiKey, stop := startPDNSLMDB(t)
	defer stop()

	client := NewClient(baseURL, apiKey)
	ctx := context.Background()
	if err := client.CreateZone(ctx, "example.com", []string{"ns1.example.net", "ns2.example.net"}); err != nil {
		t.Fatalf("CreateZone: %v", err)
	}

	claimant := func(name, owner, content string) dnsv1alpha1.DNSRecordSet {
		rs := aRecordSet(1, owner)
		rs.Name, rs.UID = name, types.UID(name+"-uid")
		rs.Spec.Records[0].A.Content = content
		return rs
	}
	holdersOf := func(owner string, rs dnsv1alpha1.DNSRecordSet) map[string]*dnsv1alpha1.DNSRecordSet {
		return map[string]*dnsv1alpha1.DNSRecordSet{QualifyOwner(owner, "example.com"): &rs}
	}
	ensure := func(rs dnsv1alpha1.DNSRecordSet, holders map[string]*dnsv1alpha1.DNSRecordSet) {
		t.Helper()
		if _, err := client.EnsureRecordSet(ctx, testZone, rs, holders); err != nil {
			t.Fatalf("EnsureRecordSet %s: %v", rs.Name, err)
		}
	}
	// held reports the records PowerDNS lists at owner, and the record set its
	// ownership note names.
	held := func(owner string) (records []string, noteOwner string) {
		t.Helper()
		sets, err := client.GetZoneRRSets(ctx, "example.com")
		if err != nil {
			t.Fatalf("GetZoneRRSets: %v", err)
		}
		for _, set := range sets {
			if set.Type != "A" || set.Name != QualifyOwner(owner, "example.com") {
				continue
			}
			for _, rec := range set.Records {
				records = append(records, rec.Content)
			}
			return records, rrsetOwnerRef(set)
		}
		return nil, ""
	}

	cases := []struct {
		name  string
		owner string
		// remove makes the later writer, or the earlier one, stop claiming owner.
		remove func(earlier, later dnsv1alpha1.DNSRecordSet) (survivor dnsv1alpha1.DNSRecordSet)
	}{
		{
			name:  "the record set that did not write it last is deleted",
			owner: "one",
			remove: func(earlier, later dnsv1alpha1.DNSRecordSet) dnsv1alpha1.DNSRecordSet {
				if err := client.DeleteRecordSet(ctx, testZone, earlier, holdersOf("one", later)); err != nil {
					t.Fatalf("DeleteRecordSet: %v", err)
				}
				return later
			},
		},
		{
			name:  "the record set that wrote it last is deleted",
			owner: "two",
			remove: func(earlier, later dnsv1alpha1.DNSRecordSet) dnsv1alpha1.DNSRecordSet {
				if err := client.DeleteRecordSet(ctx, testZone, later, holdersOf("two", earlier)); err != nil {
					t.Fatalf("DeleteRecordSet: %v", err)
				}
				return earlier
			},
		},
		{
			name:  "the record set that wrote it last drops it from its spec",
			owner: "three",
			remove: func(earlier, later dnsv1alpha1.DNSRecordSet) dnsv1alpha1.DNSRecordSet {
				later.Generation++
				later.Spec.Records[0].Name = "three-renamed"
				ensure(later, holdersOf("three", earlier))
				return earlier
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			earlier := claimant("earlier-"+tc.owner, tc.owner, "192.0.2.1")
			later := claimant("later-"+tc.owner, tc.owner, "192.0.2.2")
			ensure(earlier, holdersOf(tc.owner, later))
			ensure(later, holdersOf(tc.owner, earlier))

			survivor := tc.remove(earlier, later)

			records, noteOwner := held(tc.owner)
			want := survivor.Spec.Records[0].A.Content
			if len(records) != 1 || records[0] != want {
				t.Fatalf("%s holds %v, want %s's record %s", tc.owner, records, survivor.Name, want)
			}
			if noteOwner != recordSetOwnerRef(survivor) {
				t.Fatalf("%s's ownership note names %q, want %q", tc.owner, noteOwner, recordSetOwnerRef(survivor))
			}
		})
	}

	// The control: with nobody else claiming it, a deleted record set's name goes.
	t.Run("a name no other record set claims is deleted", func(t *testing.T) {
		alone := claimant("alone", "four", "192.0.2.4")
		ensure(alone, nil)
		if err := client.DeleteRecordSet(ctx, testZone, alone, nil); err != nil {
			t.Fatalf("DeleteRecordSet: %v", err)
		}
		if records, _ := held("four"); records != nil {
			t.Fatalf("four still holds %v after its only claimant was deleted", records)
		}
	})
}
