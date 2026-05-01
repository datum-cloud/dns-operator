// SPDX-License-Identifier: AGPL-3.0-only

package pdns

import (
	"errors"
	"testing"
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
			name: "422 unknown body falls back to generic 422 message",
			err:  &pdnsAPIError{Status: 422, Body: `{"error": "Some other validation error"}`},
			want: "The DNS record was rejected as invalid. Check the record type and value.",
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
