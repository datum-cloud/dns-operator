// SPDX-License-Identifier: AGPL-3.0-only

package display

import (
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildFQDN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		recordName string
		zoneDomain string
		want       string
	}{
		{name: "subdomain", recordName: "www", zoneDomain: "example.com", want: "www.example.com"},
		{name: "apex record", recordName: "@", zoneDomain: "example.com", want: "example.com"},
		{name: "nested subdomain", recordName: "api.v2", zoneDomain: "example.com", want: "api.v2.example.com"},
		{name: "dmarc", recordName: "_dmarc", zoneDomain: "datum.net", want: "_dmarc.datum.net"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := BuildFQDN(tt.recordName, tt.zoneDomain)
			if got != tt.want {
				t.Errorf("BuildFQDN(%q, %q) = %q, want %q", tt.recordName, tt.zoneDomain, got, tt.want)
			}
		})
	}
}

func TestComputeDisplayName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		rs         *dnsv1alpha1.DNSRecordSet
		zoneDomain string
		want       string
	}{
		{
			name: "single record",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					Records: []dnsv1alpha1.RecordEntry{{Name: "www"}},
				},
			},
			zoneDomain: "example.com",
			want:       "www.example.com",
		},
		{
			name: "multiple records same name",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "www"},
						{Name: "www"},
					},
				},
			},
			zoneDomain: "example.com",
			want:       "www.example.com",
		},
		{
			name: "multiple records different names",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "www"},
						{Name: "api"},
					},
				},
			},
			zoneDomain: "example.com",
			want:       "www.example.com, api.example.com",
		},
		{
			name: "apex record",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					Records: []dnsv1alpha1.RecordEntry{{Name: "@"}},
				},
			},
			zoneDomain: "example.com",
			want:       "example.com",
		},
		{
			name: "empty records",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{Records: nil},
			},
			zoneDomain: "example.com",
			want:       "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ComputeDisplayName(tt.rs, tt.zoneDomain)
			if got != tt.want {
				t.Errorf("ComputeDisplayName() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestComputeDisplayValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rs   *dnsv1alpha1.DNSRecordSet
		want string
	}{
		{
			name: "A record single IP",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeA,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.10"}}},
				},
			},
			want: "192.0.2.10",
		},
		{
			name: "A record multiple IPs",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeA,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.10"}},
						{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.11"}},
					},
				},
			},
			want: "192.0.2.10, 192.0.2.11",
		},
		{
			name: "CNAME record",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeCNAME,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "api", CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "api.internal.example.com"}}},
				},
			},
			want: "api.internal.example.com",
		},
		{
			name: "MX records",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeMX,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "@", MX: &dnsv1alpha1.MXRecordSpec{Preference: 10, Exchange: "mail.example.com"}},
					},
				},
			},
			want: "10 mail.example.com",
		},
		{
			name: "TXT record short",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeTXT,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "@", TXT: &dnsv1alpha1.TXTRecordSpec{Content: "v=spf1 include:_spf.example.com ~all"}}},
				},
			},
			want: "\"v=spf1 include:_spf.example.com ~all\"",
		},
		{
			name: "TXT record truncated",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeTXT,
					Records: []dnsv1alpha1.RecordEntry{{
						Name: "@",
						TXT:  &dnsv1alpha1.TXTRecordSpec{Content: stringsRepeat("a", 80)},
					}},
				},
			},
			want: "\"" + stringsRepeat("a", 57) + "...\"",
		},
		{
			name: "NS record",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeNS,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "sub", NS: &dnsv1alpha1.NSRecordSpec{Content: "ns1.example.com"}}},
				},
			},
			want: "ns1.example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ComputeDisplayValue(tt.rs)
			if got != tt.want {
				t.Errorf("ComputeDisplayValue() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractIPAddresses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rs      *dnsv1alpha1.DNSRecordSet
		wantIPs []string
	}{
		{
			name: "A records",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeA,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "@", A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"}},
						{Name: "@", A: &dnsv1alpha1.ARecordSpec{Content: "5.6.7.8"}},
					},
				},
			},
			wantIPs: []string{"1.2.3.4", "5.6.7.8"},
		},
		{
			name: "AAAA records",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeAAAA,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "@", AAAA: &dnsv1alpha1.AAAARecordSpec{Content: "::1"}},
					},
				},
			},
			wantIPs: []string{"::1"},
		},
		{
			name: "CNAME returns nil",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeCNAME,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "www", CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "target.example.com"}}},
				},
			},
			wantIPs: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ExtractIPAddresses(tt.rs)
			if len(got) != len(tt.wantIPs) {
				t.Fatalf("ExtractIPAddresses() = %v, want %v", got, tt.wantIPs)
			}
			for i := range got {
				if got[i] != tt.wantIPs[i] {
					t.Errorf("ExtractIPAddresses()[%d] = %q, want %q", i, got[i], tt.wantIPs[i])
				}
			}
		})
	}
}

func TestEnsureAnnotations(t *testing.T) {
	t.Parallel()

	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "www"},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			RecordType: dnsv1alpha1.RRTypeA,
			Records:    []dnsv1alpha1.RecordEntry{{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "192.0.2.10"}}},
		},
	}

	if !EnsureAnnotations(rs, "example.com") {
		t.Fatal("expected EnsureAnnotations to modify on first call")
	}
	if got := rs.Annotations[AnnotationDisplayName]; got != "www.example.com" {
		t.Errorf("display-name = %q, want www.example.com", got)
	}
	if got := rs.Annotations[AnnotationDisplayValue]; got != "192.0.2.10" {
		t.Errorf("display-value = %q, want 192.0.2.10", got)
	}
	if EnsureAnnotations(rs, "example.com") {
		t.Fatal("expected EnsureAnnotations to be idempotent")
	}
}

func TestExtractCNAMETarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rs   *dnsv1alpha1.DNSRecordSet
		want string
	}{
		{
			name: "CNAME record returns target",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeCNAME,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "api", CNAME: &dnsv1alpha1.CNAMERecordSpec{Content: "api.internal.example.com"}},
					},
				},
			},
			want: "api.internal.example.com",
		},
		{
			name: "A record returns empty",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeA,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"}},
					},
				},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ExtractCNAMETarget(tt.rs)
			if got != tt.want {
				t.Errorf("ExtractCNAMETarget() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractMXHosts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		rs   *dnsv1alpha1.DNSRecordSet
		want string
	}{
		{
			name: "multiple MX records",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeMX,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "@", MX: &dnsv1alpha1.MXRecordSpec{Preference: 10, Exchange: "mail.example.com"}},
						{Name: "@", MX: &dnsv1alpha1.MXRecordSpec{Preference: 20, Exchange: "mail2.example.com"}},
					},
				},
			},
			want: "10 mail.example.com, 20 mail2.example.com",
		},
		{
			name: "non-MX record returns empty",
			rs: &dnsv1alpha1.DNSRecordSet{
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					RecordType: dnsv1alpha1.RRTypeA,
					Records:    []dnsv1alpha1.RecordEntry{{Name: "@", A: &dnsv1alpha1.ARecordSpec{Content: "1.2.3.4"}}},
				},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ExtractMXHosts(tt.rs)
			if got != tt.want {
				t.Errorf("ExtractMXHosts() = %q, want %q", got, tt.want)
			}
		})
	}
}

func stringsRepeat(s string, n int) string {
	b := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		b = append(b, s...)
	}
	return string(b)
}
