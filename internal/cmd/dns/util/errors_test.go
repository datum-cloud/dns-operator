// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	zoneResource   = schema.GroupResource{Group: "dns.networking.miloapis.com", Resource: "dnszones"}
	recordResource = schema.GroupResource{Group: "dns.networking.miloapis.com", Resource: "dnsrecordsets"}
)

func TestClassifyError(t *testing.T) {
	tests := []struct {
		name     string
		in       error
		wantCode int
		wantMsg  string
	}{
		{
			name:     "nil stays nil",
			in:       nil,
			wantCode: ExitOK,
		},
		{
			name:     "403 is forbidden",
			in:       apierrors.NewForbidden(zoneResource, "example.com", errors.New("no access")),
			wantCode: ExitForbidden,
			wantMsg:  `not authorized: dnszones.dns.networking.miloapis.com "example.com" is forbidden: no access`,
		},
		{
			name:     "404 is not found",
			in:       apierrors.NewNotFound(zoneResource, "example.com"),
			wantCode: ExitNotFound,
			wantMsg:  `dnszones.dns.networking.miloapis.com "example.com" not found`,
		},
		{
			name:     "409 is conflict",
			in:       apierrors.NewConflict(recordResource, "example-com-a", errors.New("object was modified")),
			wantCode: ExitConflict,
		},
		{
			name:     "400 is invalid",
			in:       apierrors.NewBadRequest("recordType must be set"),
			wantCode: ExitInvalid,
			wantMsg:  "invalid request: recordType must be set",
		},
		{
			name: "422 is invalid",
			in: apierrors.NewInvalid(
				schema.GroupKind{Group: "dns.networking.miloapis.com", Kind: "DNSZone"},
				"example.com", nil),
			wantCode: ExitInvalid,
		},
		{
			// A real refused dial, not a string that reads like one. Type
			// matching is the whole point: the message is incidental.
			name: "connection refused is unavailable",
			in: &url.Error{Op: "Get", URL: "https://api.datum.net/", Err: &net.OpError{
				Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused"),
			}},
			wantCode: ExitUnavailable,
		},
		{
			name:     "dns resolution failure is unavailable",
			in:       &net.DNSError{Err: "no such host", Name: "api.datum.net", IsNotFound: true},
			wantCode: ExitUnavailable,
		},
		{
			name:     "a deadline is unavailable",
			in:       fmt.Errorf("listing zones: %w", context.DeadlineExceeded),
			wantCode: ExitUnavailable,
		},
		{
			name:     "cancellation is aborted, not a generic failure",
			in:       fmt.Errorf("listing zones: %w", context.Canceled),
			wantCode: ExitAborted,
			wantMsg:  "cancelled",
		},
		{
			// The regression the substring matcher caused: every TLSA
			// diagnostic contains "tls", and the old matcher told the user to
			// check their network connection.
			name:     "a client-side TLSA message is not a network failure",
			in:       errors.New(`invalid TLSA certificate association data "zz": not hexadecimal`),
			wantCode: ExitError,
		},
		{
			name:     "an EOF in a message is not a network failure",
			in:       errors.New("record value must not be eof"),
			wantCode: ExitError,
		},
		{
			name:     "401 is a forbidden with a login fix",
			in:       apierrors.NewUnauthorized("token expired"),
			wantCode: ExitForbidden,
		},
		{
			name:     "429 is unavailable so automation retries it",
			in:       apierrors.NewTooManyRequests("slow down", 1),
			wantCode: ExitUnavailable,
		},
		{
			name:     "503 is unavailable, like a refused dial",
			in:       apierrors.NewServiceUnavailable("backend down"),
			wantCode: ExitUnavailable,
		},
		{
			name:     "anything else is generic",
			in:       errors.New("something went sideways"),
			wantCode: ExitError,
			wantMsg:  "something went sideways",
		},
		{
			// A 500 never produced a verdict, so it is retryable in exactly
			// the way a refused connection is.
			name:     "500 is unavailable",
			in:       apierrors.NewInternalError(errors.New("boom")),
			wantCode: ExitUnavailable,
		},
		{
			name:     "an existing CLIError passes through untouched",
			in:       NewCLIError(ExitAborted, "confirmation did not match; aborted"),
			wantCode: ExitAborted,
			wantMsg:  "confirmation did not match; aborted",
		},
		{
			// The wrapper says which call failed, which the inner error was
			// built without. Keep both: the code from the CLIError, the words
			// from the wrapper.
			name:     "a wrapped CLIError keeps its code and gains the wrapper's context",
			in:       fmt.Errorf("listing zones: %w", NewCLIError(ExitForbidden, "not authorized")),
			wantCode: ExitForbidden,
			wantMsg:  "listing zones: not authorized",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyError(tc.in)
			if tc.in == nil {
				if got != nil {
					t.Fatalf("ClassifyError(nil) = %#v, want nil", got)
				}
				return
			}
			if got.Code() != tc.wantCode {
				t.Errorf("code = %d (%s), want %d (%s)",
					got.Code(), ExitCodeName(got.Code()), tc.wantCode, ExitCodeName(tc.wantCode))
			}
			if tc.wantMsg != "" && got.Error() != tc.wantMsg {
				t.Errorf("message = %q, want %q", got.Error(), tc.wantMsg)
			}
		})
	}
}

func TestClassifyErrorRetainsCause(t *testing.T) {
	cause := apierrors.NewNotFound(zoneResource, "example.com")
	got := ClassifyError(cause)
	if !errors.Is(got, cause) {
		t.Errorf("ClassifyError dropped the cause; errors.Is = false")
	}
	if !apierrors.IsNotFound(got) {
		t.Errorf("the classified error no longer reads as a 404")
	}
}

func TestUsageErrorf(t *testing.T) {
	err := UsageErrorf("record name %q already includes the zone domain", "www.example.com")
	if err.Code() != ExitUsage {
		t.Errorf("code = %d, want %d", err.Code(), ExitUsage)
	}
	if want := `record name "www.example.com" already includes the zone domain`; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
}

func TestCLIErrorBuilders(t *testing.T) {
	cause := errors.New("underlying")
	err := NewCLIError(ExitConflict, "conflict").WithFix("re-run the command").WithCause(cause)

	if err.Code() != ExitConflict {
		t.Errorf("Code() = %d, want %d", err.Code(), ExitConflict)
	}
	if err.Error() != "conflict" {
		t.Errorf("Error() = %q, want %q", err.Error(), "conflict")
	}
	if err.Fix() != "re-run the command" {
		t.Errorf("Fix() = %q", err.Fix())
	}
	if !errors.Is(err, cause) {
		t.Errorf("WithCause did not make the cause reachable via errors.Is")
	}
}

func TestRenderExit(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		verbose  bool
		wantCode int
		wantOut  string
	}{
		{
			name:     "nil prints nothing",
			err:      nil,
			wantCode: ExitOK,
			wantOut:  "",
		},
		{
			name: "message, fix, and status line",
			err: NewCLIError(ExitConflict, "the A records for example.com changed while this command was running").
				WithFix("re-run the command — someone else modified the same record type."),
			wantCode: ExitConflict,
			wantOut: "Error: the A records for example.com changed while this command was running\n" +
				"Fix:   re-run the command — someone else modified the same record type.\n" +
				"exit status 5   # DNS_CONFLICT\n",
		},
		{
			name:     "no fix omits the fix line",
			err:      NewCLIError(ExitNotFound, `dnszones "example.com" not found`),
			wantCode: ExitNotFound,
			wantOut: "Error: dnszones \"example.com\" not found\n" +
				"exit status 4   # DNS_NOT_FOUND\n",
		},
		{
			name:     "the cause is hidden without --verbose",
			err:      NewCLIError(ExitError, "listing zones failed").WithCause(errors.New("tls: bad handshake")),
			wantCode: ExitError,
			wantOut: "Error: listing zones failed\n" +
				"exit status 1   # DNS_ERROR\n",
		},
		{
			name:     "the cause shows under --verbose",
			err:      NewCLIError(ExitError, "listing zones failed").WithCause(errors.New("tls: bad handshake")),
			verbose:  true,
			wantCode: ExitError,
			wantOut: "Error: listing zones failed\n" +
				"Cause: tls: bad handshake\n" +
				"exit status 1   # DNS_ERROR\n",
		},
		{
			name:     "a bare error is classified before rendering",
			err:      apierrors.NewNotFound(zoneResource, "example.com"),
			wantCode: ExitNotFound,
			wantOut: "Error: dnszones.dns.networking.miloapis.com \"example.com\" not found\n" +
				"exit status 4   # DNS_NOT_FOUND\n",
		},
		{
			name:     "a usage error names DNS_USAGE",
			err:      UsageErrorf("unknown record type %q", "AAA"),
			wantCode: ExitUsage,
			wantOut: "Error: unknown record type \"AAA\"\n" +
				"exit status 2   # DNS_USAGE\n",
		},
		{
			name:     "an aborted confirmation names DNS_ABORTED",
			err:      NewCLIError(ExitAborted, "confirmation did not match; aborted"),
			wantCode: ExitAborted,
			wantOut: "Error: confirmation did not match; aborted\n" +
				"exit status 9   # DNS_ABORTED\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			code := RenderExit(&buf, tc.err, tc.verbose)
			if code != tc.wantCode {
				t.Errorf("code = %d, want %d", code, tc.wantCode)
			}
			if buf.String() != tc.wantOut {
				t.Errorf("output =\n%q\nwant\n%q", buf.String(), tc.wantOut)
			}
		})
	}
}

func TestExitCodeName(t *testing.T) {
	tests := []struct {
		code int
		want string
	}{
		{ExitOK, "OK"},
		{ExitError, "DNS_ERROR"},
		{ExitUsage, "DNS_USAGE"},
		{ExitForbidden, "DNS_FORBIDDEN"},
		{ExitNotFound, "DNS_NOT_FOUND"},
		{ExitConflict, "DNS_CONFLICT"},
		{ExitInvalid, "DNS_INVALID"},
		{ExitUnavailable, "DNS_UNAVAILABLE"},
		{ExitAborted, "DNS_ABORTED"},
		{7, "DNS_ERROR"},
	}
	for _, tc := range tests {
		if got := ExitCodeName(tc.code); got != tc.want {
			t.Errorf("ExitCodeName(%d) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

// The status metadata helper is exercised indirectly above; this guards the
// unwrap path that makes it work through a fmt.Errorf wrapper.
func TestHTTPStatusCodeThroughWrapper(t *testing.T) {
	err := fmt.Errorf("listing record sets: %w", apierrors.NewConflict(recordResource, "x", errors.New("modified")))
	if got := httpStatusCode(err); got != 409 {
		t.Errorf("httpStatusCode = %d, want 409", got)
	}
}

func TestClassifyErrorThroughWrapperKeepsTheFix(t *testing.T) {
	inner := NewCLIError(ExitConflict, "the A records changed").
		WithFix("re-run the command").
		WithCause(errors.New("resourceVersion mismatch"))
	wrapped := fmt.Errorf("updating record set %q: %w", "example-com-a", inner)

	got := ClassifyError(wrapped)

	if got.Code() != ExitConflict {
		t.Errorf("code = %d, want %d", got.Code(), ExitConflict)
	}
	if want := `updating record set "example-com-a": the A records changed`; got.Error() != want {
		t.Errorf("message = %q, want %q", got.Error(), want)
	}
	if got.Fix() != "re-run the command" {
		t.Errorf("fix = %q, want it carried through the wrapper", got.Fix())
	}
	// The original must not be mutated: a caller may still hold it.
	if inner.Error() != "the A records changed" {
		t.Errorf("ClassifyError mutated the wrapped error: %q", inner.Error())
	}
}
