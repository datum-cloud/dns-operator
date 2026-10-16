// SPDX-License-Identifier: AGPL-3.0-only

package controlplane

import (
	"fmt"
	"strings"
	"testing"

	"github.com/miekg/dns"
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

func TestRecordContentPreservesTypedRDATA(t *testing.T) {
	tests := []struct {
		name     string
		typeName dnsv1alpha1.RRType
		entry    dnsv1alpha1.RecordEntry
		contains []string
	}{
		{
			name:     "srv target",
			typeName: dnsv1alpha1.RRTypeSRV,
			entry:    dnsv1alpha1.RecordEntry{Name: "_api._tcp", SRV: &dnsv1alpha1.SRVRecordSpec{Priority: 10, Weight: 20, Port: 8443, Target: "backend.example"}},
			contains: []string{"10 20 8443 backend.example."},
		},
		{
			name:     "svcb params",
			typeName: dnsv1alpha1.RRTypeSVCB,
			entry:    dnsv1alpha1.RecordEntry{Name: "svc", SVCB: &dnsv1alpha1.HTTPSRecordSpec{Priority: 1, Target: "backend.example", Params: map[string]string{"alpn": "h2,h3", "port": "8443", "ech": "AEj+DQ=="}}},
			contains: []string{"alpn=h2,h3", "port=8443", `ech="AEj+DQ=="`},
		},
		{
			name:     "https params",
			typeName: dnsv1alpha1.RRTypeHTTPS,
			entry:    dnsv1alpha1.RecordEntry{Name: "svc", HTTPS: &dnsv1alpha1.HTTPSRecordSpec{Priority: 1, Target: ".", Params: map[string]string{"no-default-alpn": "", "ipv4hint": "192.0.2.1,192.0.2.2"}}},
			contains: []string{"no-default-alpn", "ipv4hint=192.0.2.1,192.0.2.2"},
		},
		{
			name:     "txt special bytes",
			typeName: dnsv1alpha1.RRTypeTXT,
			entry:    dnsv1alpha1.RecordEntry{Name: "txt", TXT: &dnsv1alpha1.TXTRecordSpec{Content: "quote=\" slash=\\ line=\n snowman=☃"}},
			contains: []string{`\"`, `\\`, `\010`, "☃"},
		},
		{
			name:     "caa quoting",
			typeName: dnsv1alpha1.RRTypeCAA,
			entry:    dnsv1alpha1.RecordEntry{Name: "@", CAA: &dnsv1alpha1.CAARecordSpec{Flag: 0, Tag: "issue", Value: `ca.example; note="quoted" path=\root`}},
			contains: []string{`\;`, `\"quoted\"`, `\\root`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := recordContent(tt.typeName, tt.entry)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tt.contains {
				if !strings.Contains(content, want) {
					t.Fatalf("content %q does not preserve %q", content, want)
				}
			}
			if _, err := dns.NewRR(fmt.Sprintf("owner.example. 300 IN %s %s", tt.typeName, content)); err != nil {
				t.Fatalf("rendered RDATA is not parseable: %q: %v", content, err)
			}
		})
	}
}
