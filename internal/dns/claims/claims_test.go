// SPDX-License-Identifier: AGPL-3.0-only

package claims

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

func TestPrecedes(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rs := func(name string, created time.Time) *dnsv1alpha1.DNSRecordSet {
		return &dnsv1alpha1.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(created)}}
	}
	for _, tc := range []struct {
		a, b *dnsv1alpha1.DNSRecordSet
		want bool
	}{
		{rs("b", base), rs("a", base.Add(time.Second)), true},
		{rs("a", base.Add(time.Second)), rs("b", base), false},
		{rs("a", base), rs("b", base), true},
		{rs("b", base), rs("a", base), false},
	} {
		if got := Precedes(tc.a, tc.b); got != tc.want {
			t.Errorf("Precedes(%s at %s, %s at %s) = %v, want %v",
				tc.a.Name, tc.a.CreationTimestamp, tc.b.Name, tc.b.CreationTimestamp, got, tc.want)
		}
	}
}
