// SPDX-License-Identifier: AGPL-3.0-only

package pdns

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
)

// pdnsErrorBody is the JSON structure returned in PowerDNS API error responses.
type pdnsErrorBody struct {
	Error  string   `json:"error"`
	Errors []string `json:"errors"`
}

const conflictPhrase = "Conflicts with pre-existing"

func apiErrorsIn(err error) []*pdnsAPIError {
	if err == nil {
		return nil
	}
	if apiErr, ok := err.(*pdnsAPIError); ok {
		return []*pdnsAPIError{apiErr}
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		out := make([]*pdnsAPIError, 0)
		for _, inner := range wrapped.Unwrap() {
			out = append(out, apiErrorsIn(inner)...)
		}
		return out
	case interface{ Unwrap() error }:
		return apiErrorsIn(wrapped.Unwrap())
	}
	var apiErr *pdnsAPIError
	if errors.As(err, &apiErr) {
		return []*pdnsAPIError{apiErr}
	}
	return nil
}

func (e *pdnsAPIError) reasons() []string {
	if e.Body == "" {
		return nil
	}
	var body pdnsErrorBody
	if json.Unmarshal([]byte(e.Body), &body) != nil {
		return nil
	}
	if len(body.Errors) > 0 {
		return body.Errors
	}
	if body.Error == "" {
		return nil
	}
	return []string{body.Error}
}

func (e *pdnsAPIError) isConflict() bool {
	if e.Status != http.StatusUnprocessableEntity {
		return false
	}
	for _, reason := range e.reasons() {
		if strings.Contains(reason, conflictPhrase) {
			return true
		}
	}
	return false
}

// NewAPIError builds a PowerDNS API error carrying an HTTP status and the raw
// response body. The client produces these itself; this constructor exists so
// code outside this package — controllers and their tests — can build the error
// shapes that IsConflict and FriendlyMessage classify.
func NewAPIError(status int, body string) error {
	return &pdnsAPIError{Status: status, Body: body}
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

	apiErrs := apiErrorsIn(err)
	if len(apiErrs) == 0 {
		return "Failed to apply DNS record. It will be retried automatically."
	}
	for _, e := range apiErrs {
		if e.isConflict() {
			return "A conflicting record already exists for this name. Remove the existing record and try again."
		}
	}
	apiErr := apiErrs[0]
	detail := strings.Join(apiErr.reasons(), "; ")

	switch {
	case strings.Contains(detail, conflictPhrase):
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
		if detail != "" {
			return "The DNS record was rejected as invalid: " + detail
		}
		return "The DNS record was rejected as invalid. Check the record type and value."
	case apiErr.Status == 404:
		return "The DNS zone could not be found. It may still be provisioning."
	case apiErr.Status >= 500:
		return "An internal error occurred while applying the record. It will be retried automatically."
	default:
		return "Failed to apply DNS record. It will be retried automatically."
	}
}

// IsConflict reports whether err is a PowerDNS rejection caused by a record
// coexistence conflict — a CNAME or ALIAS RRset that cannot share an owner name
// with a pre-existing RRset. This is distinct from a malformed payload: the
// record is well-formed but conflicts with existing data at the same name.
func IsConflict(err error) bool {
	for _, apiErr := range apiErrorsIn(err) {
		if apiErr.isConflict() {
			return true
		}
	}
	return false
}

func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *pdnsAPIError
	if !errors.As(err, &apiErr) {
		return true
	}
	return apiErr.Status >= http.StatusInternalServerError || apiErr.Status == http.StatusTooManyRequests
}

var refusedRRSetPattern = regexp.MustCompile(`RRset (\S+) IN (\S+): Conflicts with pre-existing`)

func refusedRRSets(err error) []rrsetKey {
	out := make([]rrsetKey, 0)
	for _, apiErr := range apiErrorsIn(err) {
		if apiErr.Status != http.StatusUnprocessableEntity {
			continue
		}
		for _, reason := range apiErr.reasons() {
			if m := refusedRRSetPattern.FindStringSubmatch(reason); m != nil {
				out = append(out, rrsetKey{name: strings.ToLower(m[1]), typ: strings.ToUpper(m[2])})
			}
		}
	}
	return out
}

func refuses(err error, key rrsetKey) bool {
	for _, refused := range refusedRRSets(err) {
		if refused == key {
			return true
		}
	}
	return false
}
