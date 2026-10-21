// SPDX-License-Identifier: AGPL-3.0-only

package pdns

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func notedRRSet(name, typ, owner string) zoneRRset {
	rr := zoneRRset{Name: name, Type: typ, TTL: 300, Records: []zoneRRsetRecord{{Content: "kept"}}}
	if owner != "" {
		rr.Comments = []zoneRRsetComment{{Account: ACCOUNT_OWNER, Content: owner}}
	}
	return rr
}

func deletedKeys(stub *pdnsStub) []rrsetKey {
	out := make([]rrsetKey, 0)
	for _, patch := range stub.patches {
		for _, rr := range patch.RRSets {
			if rr.ChangeType == changeTypeDelete {
				out = append(out, rrsetKey{name: rr.Name, typ: rr.Type})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].name != out[j].name {
			return out[i].name < out[j].name
		}
		return out[i].typ < out[j].typ
	})
	return out
}

func changeIndex(patch patchZoneRequest, name, typ, changeType string) int {
	for i, rr := range patch.RRSets {
		if rr.Name == name && rr.Type == typ && rr.ChangeType == changeType {
			return i
		}
	}
	return -1
}

func prunedCount(t *testing.T, recordType string) float64 {
	t.Helper()
	var m dto.Metric
	if err := otherTypeRecordsPruned.WithLabelValues(recordType).Write(&m); err != nil {
		t.Fatalf("read the prune counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

func TestEnsureRecordSet_PrunesEveryRecordItWroteAndNoOther(t *testing.T) {
	t.Parallel()

	stub, c := newPDNSStub(t, zoneResponse{Name: exampleCom, RRSets: []zoneRRset{
		notedRRSet("www.example.com.", "ALIAS", "default:rs"),
		notedRRSet("gone.example.com.", "TXT", "default:rs"),
		notedRRSet("stale.example.com.", "A", "default:rs"),
		notedRRSet("www.example.com.", "TXT", "default:other"),
		notedRRSet("www.example.com.", "MX", ""),
		notedRRSet("other.example.com.", "A", "default:other"),
	}})

	if _, err := c.EnsureRecordSet(context.Background(), testZone, aRecordSet(2, "www", "api")); err != nil {
		t.Fatalf("EnsureRecordSet error: %v", err)
	}

	want := []rrsetKey{
		{name: "gone.example.com.", typ: "TXT"},
		{name: "stale.example.com.", typ: "A"},
		{name: "www.example.com.", typ: "ALIAS"},
	}
	if got := deletedKeys(stub); !reflect.DeepEqual(got, want) {
		t.Fatalf("deleted %v, want every record this record set wrote and no longer wants, and nothing else", got)
	}

	for _, patch := range stub.patches {
		rewrite := changeIndex(patch, "www.example.com.", "A", changeTypeReplace)
		if rewrite < 0 {
			continue
		}
		if prune := changeIndex(patch, "www.example.com.", "ALIAS", changeTypeDelete); prune < 0 || prune > rewrite {
			t.Fatalf("the old ALIAS at www must be deleted ahead of the A that replaces it, in the same PATCH: %+v", patch.RRSets)
		}
		return
	}
	t.Fatal("no PATCH wrote the A at www")
}

func TestDeleteRecordSet_RemovesEveryRecordItWroteAndNoOther(t *testing.T) {
	t.Parallel()

	stub, c := newPDNSStub(t, zoneResponse{Name: exampleCom, RRSets: []zoneRRset{
		notedRRSet("www.example.com.", "A", "default:rs"),
		notedRRSet("www.example.com.", "ALIAS", "default:rs"),
		notedRRSet("old.example.com.", "TXT", "default:rs"),
		notedRRSet("api.example.com.", "A", ""),
		notedRRSet("www.example.com.", "TXT", "default:other"),
		notedRRSet("www.example.com.", "MX", ""),
	}})

	if err := c.DeleteRecordSet(context.Background(), testZone, aRecordSet(1, "www", "api")); err != nil {
		t.Fatalf("DeleteRecordSet error: %v", err)
	}

	want := []rrsetKey{
		{name: "api.example.com.", typ: "A"},
		{name: "old.example.com.", typ: "TXT"},
		{name: "www.example.com.", typ: "A"},
		{name: "www.example.com.", typ: "ALIAS"},
	}
	if got := deletedKeys(stub); !reflect.DeepEqual(got, want) {
		t.Fatalf("deleted %v, want every record this record set wrote, its own type at its names, and nothing else", got)
	}
}

func TestEnsureRecordSet_ReplacesItsOwnRecordOfAnotherType(t *testing.T) {
	t.Parallel()

	fake, c := newCoexistencePDNS(t, notedRRSet("www.example.com.", "ALIAS", "default:aliases"))

	statuses, err := c.EnsureRecordSet(context.Background(), testZone, cnameRecordSet("www"))
	if err != nil {
		t.Fatalf("EnsureRecordSet error: %v", err)
	}
	if cond := programmedCondition(t, statuses, "www"); cond.Status != metav1.ConditionTrue {
		t.Fatalf("a record set that changed from ALIAS to CNAME must publish, got %+v", cond)
	}
	if !fake.has("www.example.com.", "CNAME") || fake.has("www.example.com.", "ALIAS") {
		t.Fatal("www must hold the new CNAME and not the old ALIAS")
	}
	if fake.patches != 1 {
		t.Fatalf("the type change took %d PATCHes, want the delete and the replace in one", fake.patches)
	}
}

func TestEnsureRecordSet_KeepsItsOldRecordWhileTheNewOneIsRefused(t *testing.T) {
	t.Parallel()

	fake, c := newCoexistencePDNS(t,
		notedRRSet("www.example.com.", "ALIAS", "default:aliases"),
		blockingARecord("www.example.com."),
	)

	statuses, err := c.EnsureRecordSet(context.Background(), testZone, cnameRecordSet("api", "www"))
	if err != nil {
		t.Fatalf("EnsureRecordSet error: %v", err)
	}
	cond := programmedCondition(t, statuses, "www")
	if cond.Status != metav1.ConditionFalse || cond.Reason != "Conflict" {
		t.Fatalf("expected the CNAME beside another record set's A to be refused, got %+v", cond)
	}
	if !strings.Contains(cond.Message, "A record of DNSRecordSet web") || strings.Contains(cond.Message, "DNSRecordSet aliases") {
		t.Fatalf("the message must name the record that blocks, not the record set's own old record: %q", cond.Message)
	}
	if !fake.has("www.example.com.", "ALIAS") {
		t.Fatal("the refused change removed the old ALIAS, so www stopped serving it")
	}
	if cond := programmedCondition(t, statuses, "api"); cond.Status != metav1.ConditionTrue {
		t.Fatalf("api must publish beside the refused name, got %+v", cond)
	}
}

func TestPrunesOfAnotherTypeAreCountedOnceApplied(t *testing.T) {
	ctx := context.Background()
	aliases, records := prunedCount(t, "ALIAS"), prunedCount(t, "A")

	fake, c := newCoexistencePDNS(t,
		notedRRSet("api.example.com.", "ALIAS", "default:aliases"),
		blockingARecord("api.example.com."),
	)
	if _, err := c.EnsureRecordSet(ctx, testZone, cnameRecordSet("api")); err != nil {
		t.Fatalf("EnsureRecordSet error: %v", err)
	}
	if got := prunedCount(t, "ALIAS") - aliases; got != 0 {
		t.Fatalf("counted %v ALIAS prunes while the change that carries it was refused, want 0", got)
	}

	fake.remove("api.example.com.", "A")
	if _, err := c.EnsureRecordSet(ctx, testZone, cnameRecordSet("api")); err != nil {
		t.Fatalf("EnsureRecordSet error: %v", err)
	}
	if got := prunedCount(t, "ALIAS") - aliases; got != 1 {
		t.Fatalf("counted %v ALIAS prunes once the change applied, want 1", got)
	}

	stub, d := newPDNSStub(t, zoneResponse{Name: exampleCom, RRSets: []zoneRRset{
		notedRRSet("www.example.com.", "A", "default:rs"),
		notedRRSet("www.example.com.", "ALIAS", "default:rs"),
	}})
	if err := d.DeleteRecordSet(ctx, testZone, aRecordSet(1, "www")); err != nil {
		t.Fatalf("DeleteRecordSet error: %v", err)
	}
	if len(stub.patches) != 1 {
		t.Fatalf("expected 1 PATCH, got %d", len(stub.patches))
	}
	if got := prunedCount(t, "ALIAS") - aliases; got != 2 {
		t.Fatalf("counted %v ALIAS prunes after the delete, want 2", got)
	}
	if got := prunedCount(t, "A") - records; got != 0 {
		t.Fatalf("counted %v A prunes: a record of the record set's own type is not another type's prune", got)
	}
}
