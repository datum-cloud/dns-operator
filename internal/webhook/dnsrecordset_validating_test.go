// SPDX-License-Identifier: AGPL-3.0-only

package webhook

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	mccontext "sigs.k8s.io/multicluster-runtime/pkg/context"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

var errClusterNotEngaged = errors.New("cluster not engaged")

func validatorScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return scheme
}

func txtRecordSet(name string, created time.Time, owners ...string) *dnsv1alpha1.DNSRecordSet {
	return recordSet(name, "my-zone", dnsv1alpha1.RRTypeTXT, created, owners...)
}

func recordSet(
	name, zoneRef string,
	rrType dnsv1alpha1.RRType,
	created time.Time,
	owners ...string,
) *dnsv1alpha1.DNSRecordSet {
	records := make([]dnsv1alpha1.RecordEntry, 0, len(owners))
	for _, owner := range owners {
		records = append(records, dnsv1alpha1.RecordEntry{
			Name: owner,
			TXT:  &dnsv1alpha1.TXTRecordSpec{Content: "v=spf1 -all"},
		})
	}
	return &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zoneRef},
			RecordType: rrType,
			Records:    records,
		},
	}
}

func TestDNSRecordSetValidator_ValidateCreate(t *testing.T) {
	t.Parallel()

	scheme := validatorScheme(t)
	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "my-zone", Namespace: "default"},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "example.com"},
	}
	otherZone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "other-zone", Namespace: "default"},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "example.net"},
	}
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	incumbent := txtRecordSet("incumbent", base, "www")

	tests := []struct {
		name        string
		stored      []runtime.Object
		rs          *dnsv1alpha1.DNSRecordSet
		wantRefused bool
		wantIn      []string
	}{
		{
			name:   "uncontested name is accepted",
			stored: []runtime.Object{zone, incumbent},
			rs:     txtRecordSet("newcomer", base.Add(time.Hour), "mail"),
		},
		{
			name:        "name held by another set is refused",
			stored:      []runtime.Object{zone, incumbent},
			rs:          txtRecordSet("newcomer", base.Add(time.Hour), "www"),
			wantRefused: true,
			wantIn:      []string{"www.example.com.", "incumbent", "spec.records[0].name"},
		},
		{
			name:        "fqdn spelling of a held relative name is refused",
			stored:      []runtime.Object{zone, incumbent},
			rs:          txtRecordSet("newcomer", base.Add(time.Hour), "www.example.com."),
			wantRefused: true,
			wantIn:      []string{"www.example.com.", "incumbent"},
		},
		{
			name:        "apex spellings collide",
			stored:      []runtime.Object{zone, txtRecordSet("apex-owner", base, "@")},
			rs:          txtRecordSet("newcomer", base.Add(time.Hour), "example.com."),
			wantRefused: true,
			wantIn:      []string{"example.com.", "apex-owner"},
		},
		{
			name:        "case differences collide",
			stored:      []runtime.Object{zone, incumbent},
			rs:          txtRecordSet("newcomer", base.Add(time.Hour), "WWW"),
			wantRefused: true,
			wantIn:      []string{"www.example.com.", "incumbent"},
		},
		{
			name:   "same name under a different record type is accepted",
			stored: []runtime.Object{zone, incumbent},
			rs:     recordSet("newcomer", "my-zone", dnsv1alpha1.RRTypeA, base.Add(time.Hour), "www"),
		},
		{
			name:   "same name in a different zone is accepted",
			stored: []runtime.Object{zone, otherZone, incumbent},
			rs:     recordSet("newcomer", "other-zone", dnsv1alpha1.RRTypeTXT, base.Add(time.Hour), "www"),
		},
		{
			name:        "only the contested name of several is named",
			stored:      []runtime.Object{zone, incumbent},
			rs:          txtRecordSet("newcomer", base.Add(time.Hour), "mail", "www", "ftp"),
			wantRefused: true,
			wantIn:      []string{"spec.records[1].name", "www.example.com.", "incumbent"},
		},
		{
			name: "oldest claimant is named as the holder",
			stored: []runtime.Object{
				zone,
				txtRecordSet("younger", base.Add(2*time.Hour), "www"),
				incumbent,
			},
			rs:          txtRecordSet("newcomer", base.Add(3*time.Hour), "www"),
			wantRefused: true,
			wantIn:      []string{"incumbent"},
		},
		{
			name:   "a deleting claimant does not hold the name",
			stored: []runtime.Object{zone, deleting(txtRecordSet("incumbent", base, "www"))},
			rs:     txtRecordSet("newcomer", base.Add(time.Hour), "www"),
		},
		{
			name:   "missing zone is not refused",
			stored: []runtime.Object{incumbent},
			rs:     txtRecordSet("newcomer", base.Add(time.Hour), "www"),
		},
		{
			name:        "creation older than the holder is still refused",
			stored:      []runtime.Object{zone, incumbent},
			rs:          txtRecordSet("newcomer", base.Add(-time.Hour), "www"),
			wantRefused: true,
			wantIn:      []string{"incumbent"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := &DNSRecordSetValidator{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tc.stored...).Build(),
			}
			_, err := v.ValidateCreate(context.Background(), tc.rs)
			assertRefusal(t, err, tc.wantRefused, tc.wantIn)
		})
	}
}

func TestDNSRecordSetValidator_ValidateUpdate(t *testing.T) {
	t.Parallel()

	scheme := validatorScheme(t)
	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "my-zone", Namespace: "default"},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "example.com"},
	}
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	incumbent := txtRecordSet("incumbent", base, "www")

	tests := []struct {
		name        string
		stored      []runtime.Object
		oldRS       *dnsv1alpha1.DNSRecordSet
		newRS       *dnsv1alpha1.DNSRecordSet
		wantRefused bool
		wantIn      []string
	}{
		{
			name:        "adding a held name is refused",
			stored:      []runtime.Object{zone, incumbent},
			oldRS:       txtRecordSet("newcomer", base.Add(time.Hour), "mail"),
			newRS:       txtRecordSet("newcomer", base.Add(time.Hour), "mail", "www"),
			wantRefused: true,
			wantIn:      []string{"spec.records[1].name", "www.example.com.", "incumbent"},
		},
		{
			name:   "a set that already holds the contested name stays editable",
			stored: []runtime.Object{zone, incumbent},
			oldRS:  txtRecordSet("conflicted", base.Add(time.Hour), "www"),
			newRS:  withContent(txtRecordSet("conflicted", base.Add(time.Hour), "www"), "v=spf1 include:a -all"),
		},
		{
			name:   "a conflicted set can drop other records",
			stored: []runtime.Object{zone, incumbent},
			oldRS:  txtRecordSet("conflicted", base.Add(time.Hour), "www", "mail"),
			newRS:  txtRecordSet("conflicted", base.Add(time.Hour), "www"),
		},
		{
			name:   "a conflicted set can drop the contested record",
			stored: []runtime.Object{zone, incumbent},
			oldRS:  txtRecordSet("conflicted", base.Add(time.Hour), "www", "mail"),
			newRS:  txtRecordSet("conflicted", base.Add(time.Hour), "mail"),
		},
		{
			name:   "respelling a name the set already holds is accepted",
			stored: []runtime.Object{zone, incumbent},
			oldRS:  txtRecordSet("conflicted", base.Add(time.Hour), "www"),
			newRS:  txtRecordSet("conflicted", base.Add(time.Hour), "www.example.com."),
		},
		{
			name:   "unchanged spec is accepted",
			stored: []runtime.Object{zone, incumbent},
			oldRS:  txtRecordSet("incumbent", base, "www"),
			newRS:  txtRecordSet("incumbent", base, "www"),
		},
		{
			name:        "changing record type onto a held name is refused",
			stored:      []runtime.Object{zone, recordSet("a-owner", "my-zone", dnsv1alpha1.RRTypeA, base, "www")},
			oldRS:       txtRecordSet("mover", base.Add(time.Hour), "www"),
			newRS:       recordSet("mover", "my-zone", dnsv1alpha1.RRTypeA, base.Add(time.Hour), "www"),
			wantRefused: true,
			wantIn:      []string{"a-owner", "www.example.com."},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := &DNSRecordSetValidator{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(tc.stored...).Build(),
			}
			_, err := v.ValidateUpdate(context.Background(), tc.oldRS, tc.newRS)
			assertRefusal(t, err, tc.wantRefused, tc.wantIn)
		})
	}
}

func TestDNSRecordSetValidator_ValidateDelete(t *testing.T) {
	t.Parallel()

	scheme := validatorScheme(t)
	zone := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "my-zone", Namespace: "default"},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "example.com"},
	}
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	v := &DNSRecordSetValidator{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithRuntimeObjects(zone, txtRecordSet("incumbent", base, "www")).Build(),
	}
	if _, err := v.ValidateDelete(context.Background(), txtRecordSet("conflicted", base.Add(time.Hour), "www")); err != nil {
		t.Fatalf("ValidateDelete refused a conflicted set: %v", err)
	}
}

func TestDNSRecordSetValidator_projectClusterClient(t *testing.T) {
	t.Parallel()

	scheme := validatorScheme(t)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	projectClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(
		&dnsv1alpha1.DNSZone{
			ObjectMeta: metav1.ObjectMeta{Name: "my-zone", Namespace: "default"},
			Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: "project.example.com"},
		},
		txtRecordSet("incumbent", base, "www"),
	).Build()
	localClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	newcomer := txtRecordSet("newcomer", base.Add(time.Hour), "www")

	t.Run("conflict in the project control plane is refused", func(t *testing.T) {
		t.Parallel()
		v := &DNSRecordSetValidator{
			Manager: &fakeMCManager{
				clusters: map[multicluster.ClusterName]cluster.Cluster{
					"/proj-1": &fakeCluster{client: projectClient},
				},
			},
			Client: localClient,
		}
		ctx := mccontext.WithCluster(context.Background(), "proj-1")
		_, err := v.ValidateCreate(ctx, newcomer.DeepCopy())
		assertRefusal(t, err, true, []string{"www.project.example.com.", "incumbent"})
	})

	t.Run("unreachable cluster does not refuse the write", func(t *testing.T) {
		t.Parallel()
		v := &DNSRecordSetValidator{
			Manager: &fakeMCManager{err: errClusterNotEngaged},
			Client:  localClient,
		}
		ctx := mccontext.WithCluster(context.Background(), "proj-1")
		if _, err := v.ValidateCreate(ctx, newcomer.DeepCopy()); err != nil {
			t.Fatalf("ValidateCreate: %v", err)
		}
	})
}

func assertRefusal(t *testing.T, err error, wantRefused bool, wantIn []string) {
	t.Helper()
	if !wantRefused {
		if err != nil {
			t.Fatalf("write refused unexpectedly: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatal("write accepted, want refusal")
	}
	for _, want := range wantIn {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err.Error(), want)
		}
	}
}

func deleting(rs *dnsv1alpha1.DNSRecordSet) *dnsv1alpha1.DNSRecordSet {
	now := metav1.Now()
	rs.DeletionTimestamp = &now
	rs.Finalizers = []string{"dns.networking.miloapis.com/test"}
	return rs
}

func withContent(rs *dnsv1alpha1.DNSRecordSet, content string) *dnsv1alpha1.DNSRecordSet {
	for i := range rs.Spec.Records {
		rs.Spec.Records[i].TXT = &dnsv1alpha1.TXTRecordSpec{Content: content}
	}
	return rs
}
