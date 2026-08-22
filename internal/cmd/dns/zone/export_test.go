// SPDX-License-Identifier: AGPL-3.0-only

package zone

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/bind"
	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// exportFixture is a zone with something of every shape export has to get
// right: the apex and named owners, several values under one name, a TXT string
// longer than a character-string, and the platform's own SOA and NS sets.
func exportFixture() []*dnsv1alpha1.DNSRecordSet {
	return []*dnsv1alpha1.DNSRecordSet{
		bulkSet(dnsv1alpha1.RRTypeSOA, dnsv1alpha1.RecordEntry{
			Name: "@", TTL: ttlOf(3600),
			SOA: &dnsv1alpha1.SOARecordSpec{
				MName: "ns1.datum.net.", RName: "hostmaster.example.com.",
				Serial: 2024010101, Refresh: 10800, Retry: 3600, Expire: 604800, TTL: 3600,
			},
		}),
		bulkSet(dnsv1alpha1.RRTypeNS,
			dnsv1alpha1.RecordEntry{Name: "@", TTL: ttlOf(3600), NS: &dnsv1alpha1.NSRecordSpec{Content: "ns1.datum.net."}},
			dnsv1alpha1.RecordEntry{Name: "@", TTL: ttlOf(3600), NS: &dnsv1alpha1.NSRecordSpec{Content: "ns2.datum.net."}},
		),
		bulkSet(dnsv1alpha1.RRTypeA,
			aRecord("@", "203.0.113.10", ttlOf(300)),
			aRecord("www", "203.0.113.11", ttlOf(300)),
			aRecord("www", "203.0.113.12", ttlOf(300)),
			aRecord("api", "203.0.113.20", nil),
		),
		bulkSet(dnsv1alpha1.RRTypeMX, dnsv1alpha1.RecordEntry{
			Name: "@", TTL: ttlOf(3600),
			MX: &dnsv1alpha1.MXRecordSpec{Preference: 10, Exchange: "mail.example.com."},
		}),
		bulkSet(dnsv1alpha1.RRTypeTXT,
			dnsv1alpha1.RecordEntry{
				Name: "@", TTL: ttlOf(300),
				TXT: &dnsv1alpha1.TXTRecordSpec{Content: `"v=spf1 include:_spf.example.com ~all"`},
			},
			dnsv1alpha1.RecordEntry{
				Name: "dkim", TTL: ttlOf(300),
				TXT: &dnsv1alpha1.TXTRecordSpec{Content: `"p=` + strings.Repeat("A", 300) + `"`},
			},
		),
	}
}

func TestExportToStdout(t *testing.T) {
	objs := exportFixture()
	c := newFakeClient(t, bulkZone(),
		objs[0], objs[1], objs[2], objs[3], objs[4])
	h := newHarness(t, c)

	if err := h.run("zone", "export", importDomain); err != nil {
		t.Fatalf("export: %v\n%s", err, h.err.String())
	}
	out := h.out.String()

	for _, want := range []string{
		"$ORIGIN example.com.",
		"$TTL 300",
		"IN SOA ns1.datum.net. hostmaster.example.com.",
		"IN NS ns1.datum.net.",
		"IN A 203.0.113.10",
		"IN MX 10 mail.example.com.",
	} {
		if !strings.Contains(collapseSpaces(out), want) {
			t.Errorf("export is missing %q:\n%s", want, out)
		}
	}

	// A TXT value longer than a character-string is chunked, not truncated and
	// not double-quoted.
	if !strings.Contains(out, `"p=`+strings.Repeat("A", 253)+`" "`) {
		t.Errorf("the long TXT record was not chunked into character-strings:\n%s", out)
	}
}

// The whole point of export is that it feeds apply. Re-reading it must yield
// the same records.
func TestExportRoundTrips(t *testing.T) {
	objs := exportFixture()
	c := newFakeClient(t, bulkZone(), objs[0], objs[1], objs[2], objs[3], objs[4])
	h := newHarness(t, c)

	if err := h.run("zone", "export", importDomain); err != nil {
		t.Fatalf("export: %v", err)
	}

	res, err := bind.Parse(strings.NewReader(h.out.String()), importDomain, nil)
	if err != nil {
		t.Fatalf("re-parsing the export: %v", err)
	}
	if len(res.Unsupported) != 0 {
		t.Errorf("re-parse found unsupported records: %+v", res.Unsupported)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("re-parse produced warnings: %v", res.Warnings)
	}

	want := 0
	for _, o := range objs {
		want += len(o.Spec.Records)
	}
	if len(res.Records) != want {
		t.Errorf("round trip yielded %d records, want %d", len(res.Records), want)
	}
}

func TestExportToFile(t *testing.T) {
	objs := exportFixture()
	c := newFakeClient(t, bulkZone(), objs[2])
	h := newHarness(t, c)

	path := filepath.Join(t.TempDir(), "example.com.zone")
	if err := h.run("zone", "export", importDomain, "--file", path); err != nil {
		t.Fatalf("export: %v\n%s", err, h.err.String())
	}
	if h.out.String() != "" {
		t.Errorf("--file still wrote the zone to stdout:\n%s", h.out.String())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the exported file: %v", err)
	}
	if !strings.Contains(string(data), "203.0.113.10") {
		t.Errorf("the exported file is missing its records:\n%s", data)
	}
	if !strings.Contains(h.err.String(), path) {
		t.Errorf("the destination was not reported:\n%s", h.err.String())
	}
}

// An empty zone still exports a usable file rather than nothing at all.
func TestExportEmptyZone(t *testing.T) {
	c := newFakeClient(t, bulkZone())
	h := newHarness(t, c)

	if err := h.run("zone", "export", importDomain); err != nil {
		t.Fatalf("export: %v\n%s", err, h.err.String())
	}
	if !strings.Contains(h.out.String(), "$ORIGIN example.com.") {
		t.Errorf("an empty zone exported nothing usable:\n%s", h.out.String())
	}
}

func TestExportUnknownZone(t *testing.T) {
	c := newFakeClient(t)
	h := newHarness(t, c)

	err := h.run("zone", "export", "nope.example")
	assertExitCode(t, err, util.ExitNotFound)
}

// collapseSpaces squeezes the emitter's column padding so an assertion can name
// the line's content rather than its alignment.
func collapseSpaces(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, strings.Join(strings.Fields(line), " "))
	}
	return strings.Join(out, "\n")
}

// A TXT record stored chunked must export as the one logical value it denotes,
// re-chunked by the emitter — not as the escaped, double-quoted stored bytes.
func TestExportDecodesChunkedTXT(t *testing.T) {
	key := "v=DKIM1; k=rsa; p=" + strings.Repeat("M", 400)
	c := newFakeClient(t, bulkZone(),
		bulkSet(dnsv1alpha1.RRTypeTXT, dnsv1alpha1.RecordEntry{
			Name: "sel._domainkey", TTL: ttlOf(300),
			TXT: rdata.EntryForAPI(dnsv1alpha1.RRTypeTXT,
				dnsv1alpha1.RecordEntry{TXT: &dnsv1alpha1.TXTRecordSpec{Content: key}}).TXT,
		}))
	h := newHarness(t, c)

	if err := h.run("zone", "export", importDomain); err != nil {
		t.Fatalf("export: %v\n%s", err, h.err.String())
	}
	if strings.Contains(h.out.String(), `\"`) {
		t.Errorf("the stored quoting was escaped again on export:\n%s", h.out.String())
	}

	res, err := bind.Parse(strings.NewReader(h.out.String()), importDomain, nil)
	if err != nil {
		t.Fatalf("re-parsing the export: %v", err)
	}
	if got := res.Records[0].Entry.TXT.Content; got != key {
		t.Errorf("the exported DKIM key does not read back:\n got %.60q…\nwant %.60q…", got, key)
	}
}
