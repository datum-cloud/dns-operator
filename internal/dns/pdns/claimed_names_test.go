// SPDX-License-Identifier: AGPL-3.0-only

package pdns

import (
	"context"
	"strconv"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// A record set that stops claiming an owner name removes it from PowerDNS only
// when no other record set still claims it. When one does, the name is handed
// to that holder: its records are written in the same PATCH that removes the
// names nobody claims. docs/architecture/record-ownership.md, "When ownership
// moves", is the rule.

// holderRecordSet is another record set of the same type claiming www with
// 192.0.2.2, at a generation and UID of its own, so its ownership notes differ
// from rs's.
func holderRecordSet() *dnsv1alpha1.DNSRecordSet {
	return &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "holder", Namespace: "default", UID: types.UID("holder-uid"), Generation: 4},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeA,
			Records: []dnsv1alpha1.RecordEntry{{
				Name: "www",
				A:    &dnsv1alpha1.ARecordSpec{Content: "192.0.2.2"},
			}},
		},
	}
}

// writtenBy is an RRset as the zone read returns it after owner wrote it.
func writtenBy(name, owner, uid string, generation int64, content string) zoneRRset {
	return zoneRRset{
		Name:    name,
		Type:    "A",
		TTL:     300,
		Records: []zoneRRsetRecord{{Content: content}},
		Comments: []zoneRRsetComment{
			{Account: ACCOUNT_OWNER, Content: owner, ModifiedAt: 100},
			{Account: ACCOUNT_OBSERVED_GENERATION, Content: strconv.FormatInt(generation, 10), ModifiedAt: 100},
			{Account: ACCOUNT_OBJECT_UID, Content: uid, ModifiedAt: 100},
		},
	}
}

// changesByName flattens the PATCHes a call sent, leaving out the
// comment-clearing REPLACE that follows every DELETE.
func changesByName(t *testing.T, stub *pdnsStub) map[string]rrset {
	t.Helper()
	out := map[string]rrset{}
	for _, patch := range stub.patches {
		for _, rr := range patch.RRSets {
			if rr.ChangeType == changeTypeReplace && rr.Records == nil {
				continue
			}
			if _, twice := out[rr.Name]; twice {
				t.Fatalf("%s is changed twice: %+v", rr.Name, stub.patches)
			}
			out[rr.Name] = rr
		}
	}
	return out
}

func assertHandedToHolder(t *testing.T, rr rrset, found bool) {
	t.Helper()
	if !found {
		t.Fatal("www was left as it was; it holds the removed record set's records, which nothing asks for")
	}
	if rr.ChangeType != changeTypeReplace {
		t.Fatalf("www must be handed to its holder, got a %s", rr.ChangeType)
	}
	if len(rr.Records) != 1 || rr.Records[0].Content != "192.0.2.2" {
		t.Fatalf("www must carry the holder's records, got %+v", rr.Records)
	}
	notes := map[string]string{}
	for _, comment := range rr.Comments {
		notes[comment.Account] = comment.Content
	}
	want := map[string]string{
		ACCOUNT_OWNER:               "default:holder",
		ACCOUNT_OBSERVED_GENERATION: "4",
		ACCOUNT_OBJECT_UID:          "holder-uid",
	}
	for account, content := range want {
		if notes[account] != content {
			t.Fatalf("www must carry the holder's notes, so its own reconcile finds it done: got %v", notes)
		}
	}
}

// The record set being deleted wrote www last, so the RRset carries its
// records. Deleting them would leave the holder reporting Programmed=True for a
// name that no longer resolves.
func TestDeleteRecordSet_HandsAClaimedNameToItsHolder(t *testing.T) {
	t.Parallel()

	stub, c := newPDNSStub(t, zoneResponse{
		Name: exampleCom,
		RRSets: []zoneRRset{
			writtenBy("www.example.com.", "default:rs", "uid", 1, "1.2.3.4"),
			writtenBy("gone.example.com.", "default:rs", "uid", 1, "1.2.3.4"),
		},
	})

	holders := map[string]*dnsv1alpha1.DNSRecordSet{"www.example.com.": holderRecordSet()}
	if err := c.DeleteRecordSet(context.Background(), testZone, aRecordSet(1, "www"), holders); err != nil {
		t.Fatalf("DeleteRecordSet error: %v", err)
	}

	if len(stub.patches) != 1 {
		t.Fatalf("the hand-over and the delete belong in one PATCH, got %d", len(stub.patches))
	}
	changes := changesByName(t, stub)
	www, found := changes["www.example.com."]
	assertHandedToHolder(t, www, found)
	if gone := changes["gone.example.com."]; gone.ChangeType != changeTypeDelete {
		t.Fatalf("a name nobody else claims is still deleted, got %+v", gone)
	}
}

// The holder wrote www last, so the RRset is already its own. Nothing about www
// needs to change.
func TestDeleteRecordSet_LeavesANameItsHolderAlreadyWrote(t *testing.T) {
	t.Parallel()

	stub, c := newPDNSStub(t, zoneResponse{
		Name:   exampleCom,
		RRSets: []zoneRRset{writtenBy("www.example.com.", "default:holder", "holder-uid", 4, "192.0.2.2")},
	})

	holders := map[string]*dnsv1alpha1.DNSRecordSet{"www.example.com.": holderRecordSet()}
	if err := c.DeleteRecordSet(context.Background(), testZone, aRecordSet(1, "www"), holders); err != nil {
		t.Fatalf("DeleteRecordSet error: %v", err)
	}
	if len(stub.patches) != 0 {
		t.Fatalf("www already carries its holder's records, so nothing should be written: %+v", stub.patches)
	}
}

// A record set that drops a name from its spec prunes it the same way a delete
// removes it, so the same rule applies.
func TestEnsureRecordSet_HandsADroppedNameToItsHolder(t *testing.T) {
	t.Parallel()

	stub, c := newPDNSStub(t, zoneResponse{
		Name: exampleCom,
		RRSets: []zoneRRset{
			writtenBy("www.example.com.", "default:rs", "uid", 1, "1.2.3.4"),
			writtenBy("api.example.com.", "default:rs", "uid", 2, "1.2.3.4"),
		},
	})

	holders := map[string]*dnsv1alpha1.DNSRecordSet{"www.example.com.": holderRecordSet()}
	if _, err := c.EnsureRecordSet(context.Background(), testZone, aRecordSet(2, "api"), holders); err != nil {
		t.Fatalf("EnsureRecordSet error: %v", err)
	}

	changes := changesByName(t, stub)
	www, found := changes["www.example.com."]
	assertHandedToHolder(t, www, found)
	if api, rewritten := changes["api.example.com."]; rewritten {
		t.Fatalf("api is already written at this generation and should be left alone, got %+v", api)
	}
}

// A holder whose records for the name build nothing cannot take it over, so the
// name is deleted as though nobody claimed it, as the per-name reconciler did.
func TestDeleteRecordSet_DeletesANameItsHolderHasNoRecordsFor(t *testing.T) {
	t.Parallel()

	stub, c := newPDNSStub(t, zoneResponse{
		Name:   exampleCom,
		RRSets: []zoneRRset{writtenBy("www.example.com.", "default:rs", "uid", 1, "1.2.3.4")},
	})

	empty := holderRecordSet()
	empty.Spec.Records[0].A = nil
	holders := map[string]*dnsv1alpha1.DNSRecordSet{"www.example.com.": empty}
	if err := c.DeleteRecordSet(context.Background(), testZone, aRecordSet(1, "www"), holders); err != nil {
		t.Fatalf("DeleteRecordSet error: %v", err)
	}
	if www := changesByName(t, stub)["www.example.com."]; www.ChangeType != changeTypeDelete {
		t.Fatalf("www must be deleted, got %+v", www)
	}
}
