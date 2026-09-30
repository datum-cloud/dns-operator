// SPDX-License-Identifier: AGPL-3.0-only

package pdns

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

var (
	bareConflict    = &pdnsAPIError{Status: 422, Body: `{"error": "RRset www.example.com. IN CNAME: Conflicts with pre-existing RRset"}`}
	cnameHeld       = &pdnsAPIError{Status: 422, Body: `{"error": "RRset www.example.com. IN A: Conflicts with pre-existing CNAME RRset"}`}
	multiConflict   = &pdnsAPIError{Status: 422, Body: `{"error": "Multiple errors found in RRset", "errors": ["RRset x.example.com. IN NS: duplicate record with content \"ns1.\"", "RRset www.example.com. IN CNAME: Conflicts with pre-existing RRset"]}`}
	multiNoConflict = &pdnsAPIError{Status: 422, Body: `{"error": "Multiple errors found in RRset", "errors": ["RRset x.example.com. IN NS: duplicate record with content \"ns1.\""]}`}
	duplicateRecord = &pdnsAPIError{Status: 422, Body: `{"error": "RRset x. IN NS: duplicate record with content \"ns1.\""}`}
)

func TestFriendlyMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "nil error returns empty string",
			err:  nil,
			want: "",
		},
		{
			name: "non-pdns error returns generic message",
			err:  errors.New("some internal error"),
			want: "Failed to apply DNS record. It will be retried automatically.",
		},
		{
			name: "422 conflict with pre-existing RRset",
			err:  &pdnsAPIError{Status: 422, Body: `{"error": "RRset www.example.com. IN ALIAS: Conflicts with pre-existing RRset"}`},
			want: "A conflicting record already exists for this name. Remove the existing record and try again.",
		},
		{
			name: "422 CNAME conflict",
			err:  &pdnsAPIError{Status: 422, Body: `{"error": "RRset www.example.com. IN CNAME: Conflicts with pre-existing RRset"}`},
			want: "A conflicting record already exists for this name. Remove the existing record and try again.",
		},
		{
			name: "422 invalid character in TXT record content",
			err:  &pdnsAPIError{Status: 422, Body: `{"error": "Invalid character ';' in record content '\"=DMARC1; p=none; rua=mailto:admin@example.com\"'"}`},
			want: "The record content contains an invalid character. TXT records containing semicolons or special characters must be properly quoted.",
		},
		{
			name: "422 not in zone",
			err:  &pdnsAPIError{Status: 422, Body: `{"error": "Not in zone"}`},
			want: "The record name is outside the zone. Check that the name belongs to this DNS zone.",
		},
		{
			name: "422 empty body falls back to generic 422 message",
			err:  &pdnsAPIError{Status: 422, Body: ""},
			want: "The DNS record was rejected as invalid. Check the record type and value.",
		},
		{
			name: "422 unknown body surfaces the specific pdns detail",
			err:  &pdnsAPIError{Status: 422, Body: `{"error": "Some other validation error"}`},
			want: "The DNS record was rejected as invalid: Some other validation error",
		},
		{
			name: "404",
			err:  &pdnsAPIError{Status: 404, Body: `{"error": "Not found"}`},
			want: "The DNS zone could not be found. It may still be provisioning.",
		},
		{
			name: "500",
			err:  &pdnsAPIError{Status: 500, Body: "internal server error"},
			want: "An internal error occurred while applying the record. It will be retried automatically.",
		},
		{
			name: "503 is treated as a server error",
			err:  &pdnsAPIError{Status: 503, Body: ""},
			want: "An internal error occurred while applying the record. It will be retried automatically.",
		},
		{
			name: "conflict wrapped with context",
			err:  fmt.Errorf("applying zone example.com: %w", bareConflict),
			want: "A conflicting record already exists for this name. Remove the existing record and try again.",
		},
		{
			name: "conflict joined with another error",
			err:  errors.Join(errors.New("zone read failed"), bareConflict),
			want: "A conflicting record already exists for this name. Remove the existing record and try again.",
		},
		{
			name: "conflict inside a multi-RRset response",
			err:  multiConflict,
			want: "A conflicting record already exists for this name. Remove the existing record and try again.",
		},
		{
			name: "record refused by a CNAME at the name",
			err:  cnameHeld,
			want: "A conflicting record already exists for this name. Remove the existing record and try again.",
		},
		{
			name: "multi-RRset response without a conflict surfaces its reasons",
			err:  multiNoConflict,
			want: "The DNS record was rejected as invalid: RRset x.example.com. IN NS: duplicate record with content \"ns1.\"",
		},
		{
			name: "unexpected 4xx status code with no matching body",
			err:  &pdnsAPIError{Status: 409, Body: ""},
			want: "Failed to apply DNS record. It will be retried automatically.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := FriendlyMessage(tt.err)
			if got != tt.want {
				t.Errorf("FriendlyMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsConflict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"non-pdns error", errors.New("boom"), false},
		{
			"ALIAS coexistence conflict",
			&pdnsAPIError{Status: 422, Body: `{"error": "RRset www.ab.dk. IN ALIAS: Conflicts with pre-existing RRset"}`},
			true,
		},
		{
			"non-conflict 422 (duplicate record)",
			&pdnsAPIError{Status: 422, Body: `{"error": "RRset x. IN NS: duplicate record with content \"ns1.\""}`},
			false,
		},
		{"bare conflict", bareConflict, true},
		{"record refused by a CNAME at the name", cnameHeld, true},
		{"wrapped conflict", fmt.Errorf("patch: %w", bareConflict), true},
		{"conflict joined with another error", errors.Join(errors.New("boom"), bareConflict), true},
		{"conflict joined behind a wrapped non-conflict", errors.Join(fmt.Errorf("first: %w", duplicateRecord), fmt.Errorf("second: %w", bareConflict)), true},
		{"multi-RRset response containing one conflict", multiConflict, true},
		{"multi-RRset response without a conflict", multiNoConflict, false},
		{"joined errors without a conflict", errors.Join(errors.New("boom"), duplicateRecord), false},
		{
			"non-422 status",
			&pdnsAPIError{Status: 500, Body: `{"error": "Conflicts with pre-existing RRset"}`},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsConflict(tt.err); got != tt.want {
				t.Errorf("IsConflict() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsTransient(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"connection refused", errors.New("dial tcp: connection refused"), true},
		{"server error", &pdnsAPIError{Status: 503}, true},
		{"rate limited", &pdnsAPIError{Status: 429}, true},
		{"wrapped server error", fmt.Errorf("patch: %w", &pdnsAPIError{Status: 500}), true},
		{"invalid record", &pdnsAPIError{Status: 422, Body: `{"error": "Invalid character"}`}, false},
		{"zone not found", &pdnsAPIError{Status: 404}, false},
		{"bad request", &pdnsAPIError{Status: 400}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsTransient(tt.err); got != tt.want {
				t.Errorf("IsTransient() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRefusedRRSets(t *testing.T) {
	t.Parallel()

	www := rrsetKey{name: "www.example.com.", typ: "CNAME"}
	tests := []struct {
		name string
		err  error
		want []rrsetKey
	}{
		{"nil", nil, []rrsetKey{}},
		{"bare conflict", bareConflict, []rrsetKey{www}},
		{"record refused by a CNAME at the name", cnameHeld, []rrsetKey{{name: "www.example.com.", typ: "A"}}},
		{"wrapped conflict", fmt.Errorf("patch: %w", bareConflict), []rrsetKey{www}},
		{"conflict joined with another error", errors.Join(errors.New("boom"), bareConflict), []rrsetKey{www}},
		{"multi-RRset response containing one conflict", multiConflict, []rrsetKey{www}},
		{"multi-RRset response without a conflict", multiNoConflict, []rrsetKey{}},
		{"non-422 status", &pdnsAPIError{Status: 500, Body: bareConflict.Body}, []rrsetKey{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := refusedRRSets(tt.err); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("refusedRRSets() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPowerDNS51ResponseBodies(t *testing.T) {
	t.Parallel()

	refused := &pdnsAPIError{Status: 422, Body: `{"error": "RRset www.example.com. IN CNAME: Conflicts with pre-existing RRset"}`}
	if !IsConflict(refused) {
		t.Fatal("PowerDNS 5.1 conflict body is no longer recognised as a conflict")
	}
	if got := refusedRRSets(refused); !reflect.DeepEqual(got, []rrsetKey{{name: "www.example.com.", typ: "CNAME"}}) {
		t.Fatalf("refusedRRSets() = %v", got)
	}
	if IsTransient(refused) {
		t.Fatal("a conflict must not be treated as transient")
	}

	invalid := &pdnsAPIError{Status: 422, Body: `{"error": "Multiple errors found in RRset", "errors": ["RRset x.example.com. IN NS: Duplicate record in RRset x.example.com. IN NS with content \"ns1.example.net.\"", "RRset x.example.com. IN NS: Record x.example.com/NS 'ns1.example.net': Not in expected format (parsed as 'ns1.example.net.')"]}`}
	if IsConflict(invalid) {
		t.Fatal("a PowerDNS 5.1 multi-error validation body is not a conflict")
	}
	msg := FriendlyMessage(invalid)
	if !strings.Contains(msg, "Duplicate record") || !strings.Contains(msg, "Not in expected format") {
		t.Fatalf("expected every reason in the message, got %q", msg)
	}
}

func TestJoinedRefusalAndTransientFailure(t *testing.T) {
	t.Parallel()

	err := errors.Join(bareConflict, &pdnsAPIError{Status: 503})
	if !refuses(err, rrsetKey{name: "www.example.com.", typ: "CNAME"}) {
		t.Fatal("the refusal in a joined error must still name the refused rrset")
	}
	if !IsTransient(err) {
		t.Fatal("the transient part of a joined error must keep it retryable")
	}
	if IsTransient(errors.Join(bareConflict, duplicateRecord)) {
		t.Fatal("a joined error of permanent rejections is not transient")
	}
}

func TestFriendlyMessage_IncludesEveryJoinedReason(t *testing.T) {
	t.Parallel()

	err := errors.Join(duplicateRecord, &pdnsAPIError{Status: 422, Body: `{"error": "Some other validation error"}`})
	msg := FriendlyMessage(err)
	if !strings.Contains(msg, "duplicate record") || !strings.Contains(msg, "Some other validation error") {
		t.Fatalf("expected both reasons in the message, got %q", msg)
	}
}
