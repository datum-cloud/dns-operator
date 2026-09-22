// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRecordRenderRequiresFields(t *testing.T) {
	deps := fixtureDeps(fixtureReader())
	tests := map[string]RecordRenderInput{
		"no zone":      {OwnerName: "www", Type: "A", Content: "203.0.113.10"},
		"no ownerName": {Zone: "healthy-com", Type: "A", Content: "203.0.113.10"},
		"no type":      {Zone: "healthy-com", OwnerName: "www", Content: "203.0.113.10"},
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := recordRender(deps)(context.Background(), nil, in); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestRecordRenderRejectsUnsupportedType(t *testing.T) {
	deps := fixtureDeps(fixtureReader())
	_, _, err := recordRender(deps)(context.Background(), nil, RecordRenderInput{
		Zone: "healthy-com", OwnerName: "www", Type: "TLSA",
	})
	if err == nil {
		t.Fatal("want an error for an unsupported type")
	}
	if !strings.Contains(err.Error(), "TLSA") {
		t.Errorf("error = %q, want it to name the rejected type", err.Error())
	}
}

func TestRecordRenderRequiresContentForSimpleTypes(t *testing.T) {
	deps := fixtureDeps(fixtureReader())
	for _, rrType := range []string{"A", "AAAA", "CNAME", "NS", "TXT"} {
		t.Run(rrType, func(t *testing.T) {
			_, _, err := recordRender(deps)(context.Background(), nil, RecordRenderInput{
				Zone: "healthy-com", OwnerName: "www", Type: rrType,
			})
			if err == nil {
				t.Fatalf("want an error when content is missing for %s", rrType)
			}
		})
	}
}

func TestRecordRenderRequiresStructuredValueForComplexTypes(t *testing.T) {
	deps := fixtureDeps(fixtureReader())
	for _, rrType := range []string{"MX", "SRV", "CAA"} {
		t.Run(rrType, func(t *testing.T) {
			_, _, err := recordRender(deps)(context.Background(), nil, RecordRenderInput{
				Zone: "healthy-com", OwnerName: "www", Type: rrType,
			})
			if err == nil {
				t.Fatalf("want an error when %s's structured value is missing", rrType)
			}
		})
	}
}

func TestRecordRenderNewObject(t *testing.T) {
	// A type with no existing record set in the fixture zone, so this
	// exercises the "creates a new object" branch rather than the
	// "merge into the existing set" branch.
	deps := fixtureDeps(fixtureReader())
	_, out, err := recordRender(deps)(context.Background(), nil, RecordRenderInput{
		Zone: "healthy-com", OwnerName: "app", Type: "AAAA", Content: "2001:db8::1",
	})
	if err != nil {
		t.Fatalf("dns_record_render: %v", err)
	}
	if out.ObjectName != "healthy-com-aaaa" {
		t.Errorf("ObjectName = %q, want healthy-com-aaaa", out.ObjectName)
	}
	if !strings.Contains(out.Manifest, "kind: DNSRecordSet") {
		t.Errorf("Manifest = %q, want it to carry kind: DNSRecordSet", out.Manifest)
	}
	if !strings.Contains(out.Manifest, "2001:db8::1") {
		t.Errorf("Manifest = %q, want it to carry the content", out.Manifest)
	}
	if strings.Contains(out.Manifest, "status:") {
		t.Errorf("Manifest = %q, want status stripped", out.Manifest)
	}
	if len(out.Notes) == 0 || !strings.Contains(out.Notes[len(out.Notes)-1], "creates a new") {
		t.Errorf("Notes = %v, want a note that this creates a new object", out.Notes)
	}
}

func TestRecordRenderFindsExistingSetOfTheSameType(t *testing.T) {
	// fixtureReader's healthy-com zone already has a DNSRecordSet of type A
	// (www-healthy).
	deps := fixtureDeps(fixtureReader())
	_, out, err := recordRender(deps)(context.Background(), nil, RecordRenderInput{
		Zone: "healthy-com", OwnerName: "app", Type: "A", Content: "203.0.113.10",
	})
	if err != nil {
		t.Fatalf("dns_record_render: %v", err)
	}
	joined := strings.Join(out.Notes, " ")
	if !strings.Contains(joined, "www-healthy") {
		t.Errorf("Notes = %v, want a note naming the existing www-healthy record set", out.Notes)
	}
}

func TestRecordRenderWarnsOnCNAMEApex(t *testing.T) {
	deps := fixtureDeps(fixtureReader())
	_, out, err := recordRender(deps)(context.Background(), nil, RecordRenderInput{
		Zone: "healthy-com", OwnerName: "@", Type: "CNAME", Content: "target.example.net.",
	})
	if err != nil {
		t.Fatalf("dns_record_render: %v", err)
	}
	joined := strings.Join(out.Warnings, " ")
	if !strings.Contains(joined, "apex") {
		t.Errorf("Warnings = %v, want an apex warning", out.Warnings)
	}
	if !strings.Contains(joined, "coexist") {
		t.Errorf("Warnings = %v, want the coexistence warning too", out.Warnings)
	}
}

func TestRecordRenderCNAMEAtNonApexOnlyWarnsCoexistence(t *testing.T) {
	deps := fixtureDeps(fixtureReader())
	_, out, err := recordRender(deps)(context.Background(), nil, RecordRenderInput{
		Zone: "healthy-com", OwnerName: "www", Type: "CNAME", Content: "target.example.net.",
	})
	if err != nil {
		t.Fatalf("dns_record_render: %v", err)
	}
	if len(out.Warnings) != 1 {
		t.Fatalf("Warnings = %v, want exactly the coexistence warning", out.Warnings)
	}
	if strings.Contains(out.Warnings[0], "apex") {
		t.Errorf("Warnings = %v, want no apex warning at a non-apex name", out.Warnings)
	}
}

func TestRecordRenderTTLNotes(t *testing.T) {
	deps := fixtureDeps(fixtureReader())

	_, withoutTTL, err := recordRender(deps)(context.Background(), nil, RecordRenderInput{
		Zone: "healthy-com", OwnerName: "app", Type: "A", Content: "203.0.113.10",
	})
	if err != nil {
		t.Fatalf("dns_record_render: %v", err)
	}
	if !strings.Contains(strings.Join(withoutTTL.Notes, " "), "300 seconds") {
		t.Errorf("Notes = %v, want the default-300s note when TTL is omitted", withoutTTL.Notes)
	}

	ttl := int64(600)
	_, withTTL, err := recordRender(deps)(context.Background(), nil, RecordRenderInput{
		Zone: "healthy-com", OwnerName: "app", Type: "A", Content: "203.0.113.10", TTL: &ttl,
	})
	if err != nil {
		t.Fatalf("dns_record_render: %v", err)
	}
	if !strings.Contains(strings.Join(withTTL.Notes, " "), "600 seconds") {
		t.Errorf("Notes = %v, want the given TTL echoed back", withTTL.Notes)
	}
}

func TestRecordRenderMXSRVCAA(t *testing.T) {
	deps := fixtureDeps(fixtureReader())

	_, mx, err := recordRender(deps)(context.Background(), nil, RecordRenderInput{
		Zone: "healthy-com", OwnerName: "@", Type: "MX",
		MX: &RecordRenderMX{Preference: 10, Exchange: "mail.example.net."},
	})
	if err != nil {
		t.Fatalf("dns_record_render (MX): %v", err)
	}
	if !strings.Contains(mx.Manifest, "mail.example.net.") {
		t.Errorf("MX manifest = %q, want the exchange", mx.Manifest)
	}

	_, srv, err := recordRender(deps)(context.Background(), nil, RecordRenderInput{
		Zone: "healthy-com", OwnerName: "_sip._tcp", Type: "SRV",
		SRV: &RecordRenderSRV{Priority: 10, Weight: 5, Port: 5060, Target: "sip.example.net."},
	})
	if err != nil {
		t.Fatalf("dns_record_render (SRV): %v", err)
	}
	if !strings.Contains(srv.Manifest, "sip.example.net.") {
		t.Errorf("SRV manifest = %q, want the target", srv.Manifest)
	}

	_, caa, err := recordRender(deps)(context.Background(), nil, RecordRenderInput{
		Zone: "healthy-com", OwnerName: "@", Type: "CAA",
		CAA: &RecordRenderCAA{Flag: 0, Tag: "issue", Value: "letsencrypt.org"},
	})
	if err != nil {
		t.Fatalf("dns_record_render (CAA): %v", err)
	}
	if !strings.Contains(caa.Manifest, "letsencrypt.org") {
		t.Errorf("CAA manifest = %q, want the value", caa.Manifest)
	}
}

func TestRecordRenderFailsWhenDepsUnavailable(t *testing.T) {
	failing := func(context.Context) (ToolDeps, error) { return ToolDeps{}, errors.New("no project") }
	if _, _, err := recordRender(failing)(context.Background(), nil, RecordRenderInput{
		Zone: "x", OwnerName: "y", Type: "A", Content: "203.0.113.10",
	}); err == nil {
		t.Error("dns_record_render: want an error when deps fail")
	}
}
