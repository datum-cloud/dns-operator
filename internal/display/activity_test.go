// SPDX-License-Identifier: AGPL-3.0-only

package display

import (
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func aRecord(name, ip string) dnsv1alpha1.RecordEntry {
	return dnsv1alpha1.RecordEntry{Name: name, A: &dnsv1alpha1.ARecordSpec{Content: ip}}
}

func aRS(records ...dnsv1alpha1.RecordEntry) *dnsv1alpha1.DNSRecordSet {
	return &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "zone-a"},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeA,
			Records:    records,
		},
	}
}

func TestComputeActivityDiff(t *testing.T) {
	t.Parallel()

	zone := "dodik.me"
	www := aRecord("www", "192.168.1.1")
	app := aRecord("app", "192.168.1.1")
	app2 := aRecord("app", "10.0.0.1")

	tests := []struct {
		name string
		old  *dnsv1alpha1.DNSRecordSet
		new  *dnsv1alpha1.DNSRecordSet
		want ActivityDiff
	}{
		{
			name: "add second hostname",
			old:  aRS(www),
			new:  aRS(www, app),
			want: ActivityDiff{
				Change: ActivityChangeAdded,
				Name:   "app.dodik.me",
				Value:  "192.168.1.1",
			},
		},
		{
			name: "remove hostname leaving sibling",
			old:  aRS(www, app),
			new:  aRS(www),
			want: ActivityDiff{
				Change: ActivityChangeRemoved,
				Name:   "app.dodik.me",
				Value:  "192.168.1.1",
			},
		},
		{
			name: "update value on existing hostname",
			old:  aRS(www, app),
			new:  aRS(www, app2),
			want: ActivityDiff{
				Change: ActivityChangeUpdated,
				Name:   "app.dodik.me",
				Value:  "10.0.0.1",
			},
		},
		{
			name: "no records change",
			old:  aRS(www),
			new:  aRS(www),
			want: ActivityDiff{},
		},
		{
			name: "nil old",
			old:  nil,
			new:  aRS(www),
			want: ActivityDiff{},
		},
		{
			name: "mixed add and remove",
			old:  aRS(www),
			new:  aRS(app),
			want: ActivityDiff{
				Change: ActivityChangeUpdated,
				Name:   "app.dodik.me, www.dodik.me",
				Value:  "192.168.1.1",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ComputeActivityDiff(tt.old, tt.new, zone)
			if got != tt.want {
				t.Errorf("ComputeActivityDiff() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestEnsureActivityAnnotations(t *testing.T) {
	t.Parallel()

	old := aRS(aRecord("www", "192.168.1.1"))
	rs := aRS(aRecord("www", "192.168.1.1"), aRecord("app", "192.168.1.1"))
	if !EnsureActivityAnnotations(rs, old, "dodik.me") {
		t.Fatal("expected annotations to change")
	}
	if got := rs.Annotations[AnnotationActivityChange]; got != ActivityChangeAdded {
		t.Errorf("activity-change = %q, want %q", got, ActivityChangeAdded)
	}
	if got := rs.Annotations[AnnotationActivityName]; got != "app.dodik.me" {
		t.Errorf("activity-name = %q, want app.dodik.me", got)
	}
	if got := rs.Annotations[AnnotationActivityValue]; got != "192.168.1.1" {
		t.Errorf("activity-value = %q, want 192.168.1.1", got)
	}

	// Identical records clear stale activity annotations.
	same := aRS(aRecord("www", "192.168.1.1"))
	same.Annotations = map[string]string{
		AnnotationActivityChange: ActivityChangeAdded,
		AnnotationActivityName:   "app.dodik.me",
		AnnotationActivityValue:  "192.168.1.1",
	}
	if !EnsureActivityAnnotations(same, same.DeepCopy(), "dodik.me") {
		t.Fatal("expected clear to report change")
	}
	if _, ok := same.Annotations[AnnotationActivityChange]; ok {
		t.Fatalf("expected activity annotations cleared, got %v", same.Annotations)
	}
}
