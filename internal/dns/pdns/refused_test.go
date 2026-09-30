// SPDX-License-Identifier: AGPL-3.0-only

package pdns

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

type coexistencePDNS struct {
	mu        sync.Mutex
	rrsets    map[rrsetKey]zoneRRset
	down      bool
	patchDown int
	patches   int
}

func newCoexistencePDNS(t *testing.T, seed ...zoneRRset) (*coexistencePDNS, *Client) {
	t.Helper()
	fake := &coexistencePDNS{rrsets: map[rrsetKey]zoneRRset{}}
	for _, rr := range seed {
		fake.rrsets[rrsetKey{name: rr.Name, typ: rr.Type}] = rr
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/servers/localhost/zones/example.com.", func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if fake.down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		switch r.Method {
		case http.MethodGet:
			out := zoneResponse{Name: exampleCom}
			for _, rr := range fake.rrsets {
				out.RRSets = append(out.RRSets, rr)
			}
			_ = json.NewEncoder(w).Encode(out)
		case http.MethodPatch:
			fake.patches++
			if fake.patchDown != 0 {
				w.WriteHeader(fake.patchDown)
				_, _ = w.Write([]byte(`{"error": "Rejected"}`))
				return
			}
			body, _ := io.ReadAll(r.Body)
			var req patchZoneRequest
			_ = json.Unmarshal(body, &req)
			for _, rr := range req.RRSets {
				if rr.ChangeType != changeTypeReplace {
					continue
				}
				for key := range fake.rrsets {
					if key.name == rr.Name && key.typ != rr.Type && (key.typ == "CNAME" || rr.Type == "CNAME") {
						w.WriteHeader(http.StatusUnprocessableEntity)
						_, _ = fmt.Fprintf(w, `{"error": "RRset %s IN %s: Conflicts with pre-existing RRset"}`, rr.Name, rr.Type)
						return
					}
				}
			}
			for _, rr := range req.RRSets {
				key := rrsetKey{name: rr.Name, typ: rr.Type}
				if rr.ChangeType == changeTypeDelete {
					delete(fake.rrsets, key)
					continue
				}
				fake.rrsets[key] = zoneRRset{Name: rr.Name, Type: rr.Type, Comments: rr.Comments}
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return fake, NewClient(s.URL, "k")
}

func (f *coexistencePDNS) has(name, typ string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.rrsets[rrsetKey{name: name, typ: typ}]
	return ok
}

func (f *coexistencePDNS) remove(name, typ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rrsets, rrsetKey{name: name, typ: typ})
}

func (f *coexistencePDNS) setDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = down
}

func cnameRecordSet(owners ...string) dnsv1alpha1.DNSRecordSet {
	records := make([]dnsv1alpha1.RecordEntry, 0, len(owners))
	for _, owner := range owners {
		records = append(records, dnsv1alpha1.RecordEntry{
			Name:  owner,
			CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "target.example.net."},
		})
	}
	return dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "aliases", Namespace: "default", UID: types.UID("cname-uid"), Generation: 1},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeCNAME,
			Records:    records,
		},
	}
}

func programmedCondition(t *testing.T, statuses []dnsv1alpha1.RecordSetStatus, owner string) metav1.Condition {
	t.Helper()
	for _, st := range statuses {
		if st.Name == owner {
			return st.Conditions[0]
		}
	}
	t.Fatalf("no status for owner %q in %+v", owner, statuses)
	return metav1.Condition{}
}

func blockingARecord(owner string) zoneRRset {
	return zoneRRset{
		Name:     owner,
		Type:     "A",
		Records:  []zoneRRsetRecord{{Content: "192.0.2.1"}},
		Comments: []zoneRRsetComment{{Account: ACCOUNT_OWNER, Content: "default:web"}},
	}
}

func TestEnsureRecordSet_PublishesTheRestWhenOneNameIsRefused(t *testing.T) {
	t.Parallel()

	fake, c := newCoexistencePDNS(t, blockingARecord("www.example.com."))
	rs := cnameRecordSet("api", "docs", "www")

	statuses, err := c.EnsureRecordSet(context.Background(), testZone, rs)
	if err != nil {
		t.Fatalf("EnsureRecordSet error: %v", err)
	}

	for _, owner := range []string{"api", "docs"} {
		if cond := programmedCondition(t, statuses, owner); cond.Status != metav1.ConditionTrue {
			t.Fatalf("owner %q should be published beside the refused name, got %+v", owner, cond)
		}
		if !fake.has(owner+".example.com.", "CNAME") {
			t.Fatalf("owner %q missing from PowerDNS", owner)
		}
	}

	if fake.has("www.example.com.", "CNAME") {
		t.Fatal("a CNAME must never be published beside another type at the same name")
	}
	cond := programmedCondition(t, statuses, "www")
	if cond.Status != metav1.ConditionFalse || cond.Reason != "Conflict" {
		t.Fatalf("expected the refused name to be marked Conflict, got %+v", cond)
	}
	if !strings.Contains(cond.Message, "www.example.com") || !strings.Contains(cond.Message, "A record of DNSRecordSet web") {
		t.Fatalf("expected the message to name the refused name and its holder, got %q", cond.Message)
	}
}

func TestEnsureRecordSet_PublishesARefusedRecordOnceTheBlockerIsGone(t *testing.T) {
	t.Parallel()

	fake, c := newCoexistencePDNS(t, blockingARecord("www.example.com."))
	rs := cnameRecordSet("www")

	statuses, err := c.EnsureRecordSet(context.Background(), testZone, rs)
	if err != nil {
		t.Fatalf("EnsureRecordSet error: %v", err)
	}
	refused := programmedCondition(t, statuses, "www")
	if refused.Status != metav1.ConditionFalse {
		t.Fatalf("expected the record to be refused, got %+v", refused)
	}

	rs.Status.RecordSets = statuses
	statuses, err = c.EnsureRecordSet(context.Background(), testZone, rs)
	if err != nil {
		t.Fatalf("EnsureRecordSet error: %v", err)
	}
	again := programmedCondition(t, statuses, "www")
	if !again.LastTransitionTime.Equal(&refused.LastTransitionTime) {
		t.Fatalf("a record still refused must keep when it started waiting: %v, then %v", refused.LastTransitionTime, again.LastTransitionTime)
	}

	fake.remove("www.example.com.", "A")
	rs.Status.RecordSets = statuses
	statuses, err = c.EnsureRecordSet(context.Background(), testZone, rs)
	if err != nil {
		t.Fatalf("EnsureRecordSet error: %v", err)
	}
	if cond := programmedCondition(t, statuses, "www"); cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected the record to be published once its blocker is gone, got %+v", cond)
	}
	if !fake.has("www.example.com.", "CNAME") {
		t.Fatal("record missing from PowerDNS after the blocker was removed")
	}
}

func TestEnsureRecordSet_RecoversFromAPowerDNSOutage(t *testing.T) {
	t.Parallel()

	fake, c := newCoexistencePDNS(t)
	rs := cnameRecordSet("www")

	fake.setDown(true)
	if _, err := c.EnsureRecordSet(context.Background(), testZone, rs); err == nil {
		t.Fatal("expected an error while PowerDNS is down")
	}

	fake.setDown(false)
	statuses, err := c.EnsureRecordSet(context.Background(), testZone, rs)
	if err != nil {
		t.Fatalf("EnsureRecordSet after recovery: %v", err)
	}
	if cond := programmedCondition(t, statuses, "www"); cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected the record to be published after PowerDNS recovers, got %+v", cond)
	}
}

func TestKeepTransitionTimes(t *testing.T) {
	t.Parallel()

	earlier := metav1.NewTime(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	previous := []dnsv1alpha1.RecordSetStatus{
		{Name: "same", Conditions: []metav1.Condition{{Type: "Programmed", Status: metav1.ConditionFalse, LastTransitionTime: earlier}}},
		{Name: "flipped", Conditions: []metav1.Condition{{Type: "Programmed", Status: metav1.ConditionFalse, LastTransitionTime: earlier}}},
	}
	current := []dnsv1alpha1.RecordSetStatus{
		makeProgrammedStatus("same", metav1.ConditionFalse, "Conflict", "held"),
		makeProgrammedStatus("flipped", metav1.ConditionTrue, "Programmed", "ok"),
		makeProgrammedStatus("new", metav1.ConditionFalse, "Conflict", "held"),
	}

	got := keepTransitionTimes(current, previous)
	if !got[0].Conditions[0].LastTransitionTime.Equal(&earlier) {
		t.Fatalf("unchanged status must keep its transition time, got %v", got[0].Conditions[0].LastTransitionTime)
	}
	if got[1].Conditions[0].LastTransitionTime.Equal(&earlier) {
		t.Fatal("a status that flipped must take a new transition time")
	}
	if got[2].Conditions[0].LastTransitionTime.Equal(&earlier) {
		t.Fatal("a new owner must take a new transition time")
	}
}

func (f *coexistencePDNS) setPatchStatus(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patchDown = status
}

func TestEnsureRecordSet_ReturnsTransientPatchFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    int
		wantError bool
	}{
		{name: "PowerDNS unavailable", status: http.StatusServiceUnavailable, wantError: true},
		{name: "PowerDNS internal error", status: http.StatusInternalServerError, wantError: true},
		{name: "record rejected as invalid", status: http.StatusUnprocessableEntity, wantError: false},
		{name: "bad request", status: http.StatusBadRequest, wantError: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake, c := newCoexistencePDNS(t)
			fake.setPatchStatus(tt.status)

			statuses, err := c.EnsureRecordSet(context.Background(), testZone, cnameRecordSet("www"))
			if (err != nil) != tt.wantError {
				t.Fatalf("EnsureRecordSet error = %v, want error %v", err, tt.wantError)
			}
			if cond := programmedCondition(t, statuses, "www"); cond.Status != metav1.ConditionFalse || cond.Reason != "PDNSError" {
				t.Fatalf("expected the failed name to be marked PDNSError, got %+v", cond)
			}

			fake.setPatchStatus(0)
			statuses, err = c.EnsureRecordSet(context.Background(), testZone, cnameRecordSet("www"))
			if err != nil {
				t.Fatalf("EnsureRecordSet after recovery: %v", err)
			}
			if cond := programmedCondition(t, statuses, "www"); cond.Status != metav1.ConditionTrue {
				t.Fatalf("expected the record to publish once PowerDNS accepts it, got %+v", cond)
			}
		})
	}
}
