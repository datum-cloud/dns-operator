// SPDX-License-Identifier: AGPL-3.0-only

package pdns

import (
	"encoding/json"
	"errors"
	"strings"
)

// pdnsErrorBody is the JSON structure returned in PowerDNS API error responses.
type pdnsErrorBody struct {
	Error string `json:"error"`
}

// FriendlyMessage returns a user-readable message for a PowerDNS API error.
// The raw technical error is preserved in operator logs; only the translated
// message is written to the DNSRecordSet status condition so end users see
// something actionable rather than internal API details.
//
// If err is not a *pdnsAPIError, a generic fallback message is returned.
func FriendlyMessage(err error) string {
	if err == nil {
		return ""
	}

	var apiErr *pdnsAPIError
	if !errors.As(err, &apiErr) {
		return "Failed to apply DNS record. It will be retried automatically."
	}

	// Extract the error field from the JSON body when present.
	var body pdnsErrorBody
	if apiErr.Body != "" {
		_ = json.Unmarshal([]byte(apiErr.Body), &body)
	}
	detail := body.Error

	switch {
	case strings.Contains(detail, "Conflicts with pre-existing RRset"):
		return "A conflicting record already exists for this name. Remove the existing record and try again."
	case strings.Contains(detail, "Invalid character"):
		return "The record content contains an invalid character. TXT records containing semicolons or special characters must be properly quoted."
	case strings.Contains(detail, "Not in zone"):
		return "The record name is outside the zone. Check that the name belongs to this DNS zone."
	case strings.Contains(detail, "RRset") && strings.Contains(detail, "IN CNAME"):
		return "A CNAME record conflicts with an existing record at this name."
	case strings.Contains(detail, "RRset") && strings.Contains(detail, "IN ALIAS"):
		return "An ALIAS record conflicts with an existing record at this name."
	case apiErr.Status == 422:
		return "The DNS record was rejected as invalid. Check the record type and value."
	case apiErr.Status == 404:
		return "The DNS zone could not be found. It may still be provisioning."
	case apiErr.Status >= 500:
		return "An internal error occurred while applying the record. It will be retried automatically."
	default:
		return "Failed to apply DNS record. It will be retried automatically."
	}
}
