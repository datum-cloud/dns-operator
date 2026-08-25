// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Exit codes are a stable contract: automation branches on them. 0 is success;
// each documented non-zero code names a distinct failure class. A bulk
// operation that partially fails must never exit 0.
const (
	ExitOK          = 0 // success
	ExitError       = 1 // generic / unexpected error (DNS_ERROR)
	ExitUsage       = 2 // bad flags, arguments, or client-side rdata validation (DNS_USAGE)
	ExitForbidden   = 3 // HTTP 403 RBAC denial (DNS_FORBIDDEN)
	ExitNotFound    = 4 // HTTP 404, zone or record not found (DNS_NOT_FOUND)
	ExitConflict    = 5 // HTTP 409, or a record owned by another set (DNS_CONFLICT)
	ExitInvalid     = 6 // HTTP 400/422 admission rejection (DNS_INVALID)
	ExitUnavailable = 8 // transport / connection failure (DNS_UNAVAILABLE)
	ExitAborted     = 9 // user declined a confirmation (DNS_ABORTED)
)

// nameError is the symbolic name for the generic failure class, and the
// fallback for any code outside the contract.
const nameError = "DNS_ERROR"

// exitCodeNames maps each exit code to its documented symbolic name, rendered in
// the trailing "exit status N   # DNS_X" line so scripts and humans read the
// same contract.
var exitCodeNames = map[int]string{
	ExitOK:          "OK",
	ExitError:       nameError,
	ExitUsage:       "DNS_USAGE",
	ExitForbidden:   "DNS_FORBIDDEN",
	ExitNotFound:    "DNS_NOT_FOUND",
	ExitConflict:    "DNS_CONFLICT",
	ExitInvalid:     "DNS_INVALID",
	ExitUnavailable: "DNS_UNAVAILABLE",
	ExitAborted:     "DNS_ABORTED",
}

// ExitCodeName returns the symbolic name for an exit code, or DNS_ERROR for one
// that is not part of the contract.
func ExitCodeName(code int) string {
	if name, known := exitCodeNames[code]; known {
		return name
	}
	return exitCodeNames[ExitError]
}

// CLIError carries a rendered, human-facing message plus a precise exit code. It
// is what RunE handlers return so that main can print a clean message (no Go
// stack trace) and exit with the contractual code.
//
// datumctl's own UserError is not importable from a plugin — it lives in
// datumctl/internal/errors — so this is the local equivalent.
type CLIError struct {
	// code is the contractual exit code.
	code int
	// msg is the primary "Error:" line.
	msg string
	// fix is an optional remediation block printed under "Fix:".
	fix string
	// cause is retained for --verbose rendering only.
	cause error
}

// NewCLIError builds a CLIError with the given exit code and message.
func NewCLIError(code int, msg string) *CLIError {
	return &CLIError{code: code, msg: msg}
}

// WithFix attaches a remediation block, rendered under "Fix:".
func (e *CLIError) WithFix(fix string) *CLIError {
	e.fix = fix
	return e
}

// WithCause attaches the underlying error, shown only under --verbose.
func (e *CLIError) WithCause(err error) *CLIError {
	e.cause = err
	return e
}

func (e *CLIError) Error() string { return e.msg }

// Code returns the contractual exit code.
func (e *CLIError) Code() int { return e.code }

// Fix returns the remediation block, empty when there is none.
func (e *CLIError) Fix() string { return e.fix }

// Unwrap exposes the retained cause to errors.Is/As.
func (e *CLIError) Unwrap() error { return e.cause }

// UsageErrorf builds a usage (exit 2) error.
func UsageErrorf(format string, a ...any) *CLIError {
	return NewCLIError(ExitUsage, fmt.Sprintf(format, a...))
}

// ClassifyError maps an arbitrary error from an API call into a CLIError with
// the right exit code. It stays generic on purpose: callers that can add DNS
// context should build a richer CLIError themselves and fall back here only for
// the unexpected.
func ClassifyError(err error) *CLIError {
	if err == nil {
		return nil
	}
	var already *CLIError
	if errors.As(err, &already) {
		// A wrapper adds context the CLIError was built without — "listing
		// zones: <...>" says which call failed. Keep the code and the fix,
		// and carry the wrapper's words through, without mutating the
		// original, which a caller may still hold.
		if outer := err.Error(); outer != already.msg {
			return &CLIError{code: already.code, msg: outer, fix: already.fix, cause: already.cause}
		}
		return already
	}

	switch code := httpStatusCode(err); {
	case code == 401:
		// The single most common failure a real user hits. Without this branch
		// it fell through to the generic case and printed a bare "token
		// expired" with no way out.
		return NewCLIError(ExitForbidden, fmt.Sprintf("not authenticated: %s", apiMessage(err))).
			WithFix("your session has expired — re-run:\n       datumctl login").
			WithCause(err)
	case code == 403:
		return NewCLIError(ExitForbidden, fmt.Sprintf("not authorized: %s", apiMessage(err))).
			WithFix("verify the active org and project, and your RBAC on the DNS API.").
			WithCause(err)
	case code == 404:
		return NewCLIError(ExitNotFound, apiMessage(err)).WithCause(err)
	case code == 409:
		return NewCLIError(ExitConflict, fmt.Sprintf("conflict: %s", apiMessage(err))).WithCause(err)
	case code == 400, code == 422:
		return NewCLIError(ExitInvalid, fmt.Sprintf("invalid request: %s", apiMessage(err))).WithCause(err)
	case code == 429:
		return NewCLIError(ExitUnavailable, fmt.Sprintf("rate limited: %s", apiMessage(err))).
			WithFix("wait a moment and try again.").
			WithCause(err)
	case code >= 500:
		// Retryable for the same reason a dial failure is: the request never
		// got a verdict. Automation that retries on DNS_UNAVAILABLE should
		// retry a 503 exactly as it retries a refused connection.
		return NewCLIError(ExitUnavailable, fmt.Sprintf("the DNS API is unavailable: %s", apiMessage(err))).
			WithFix("this is a server-side failure — retry, and check the Datum status page if it persists.").
			WithCause(err)
	}

	// No HTTP status: a transport failure, or the command was interrupted.
	if errors.Is(err, context.Canceled) {
		return NewCLIError(ExitAborted, "cancelled").WithCause(err)
	}
	if isTransportError(err) {
		return NewCLIError(ExitUnavailable, fmt.Sprintf("cannot reach the DNS API: %s", err)).
			WithFix("check connectivity and that you are logged in (datumctl login).").
			WithCause(err)
	}
	return NewCLIError(ExitError, err.Error()).WithCause(err)
}

// RenderExit prints err in the plugin's error format and returns the exit code
// the process should use. A nil error prints nothing and returns ExitOK.
//
//	Error: the A records for example.com changed while this command was running
//	Fix:   re-run the command — someone else modified the same record type.
//	exit status 5   # DNS_CONFLICT
//
// The underlying cause is printed only under --verbose, because for the common
// case it is Kubernetes plumbing the reader cannot act on.
func RenderExit(w io.Writer, err error, verbose bool) int {
	if err == nil {
		return ExitOK
	}

	ce := ClassifyError(err)
	_, _ = fmt.Fprintf(w, "Error: %s\n", ce.msg)
	if ce.fix != "" {
		_, _ = fmt.Fprintf(w, "Fix:   %s\n", ce.fix)
	}
	if verbose && ce.cause != nil && ce.cause.Error() != ce.msg {
		_, _ = fmt.Fprintf(w, "Cause: %v\n", ce.cause)
	}
	_, _ = fmt.Fprintf(w, "exit status %d   # %s\n", ce.code, ExitCodeName(ce.code))
	return ce.code
}

// httpStatusCode extracts the HTTP status code from a Kubernetes API error,
// returning 0 when the error does not carry one (e.g. a transport failure).
func httpStatusCode(err error) int {
	if status, isStatus := asAPIStatus(err); isStatus {
		return int(status.Status().Code)
	}
	return 0
}

// asAPIStatus unwraps err to a Kubernetes APIStatus when possible.
func asAPIStatus(err error) (apierrors.APIStatus, bool) {
	if err == nil {
		return nil, false
	}
	var s apierrors.APIStatus
	if errors.As(err, &s) {
		return s, true
	}
	return nil, false
}

// apiMessage returns the server-provided status message when available, else the
// raw error string.
func apiMessage(err error) string {
	if status, isStatus := asAPIStatus(err); isStatus {
		if m := status.Status().Message; m != "" {
			return m
		}
	}
	return err.Error()
}

// isTransportError reports whether an error is a network failure rather than a
// rejection the API server chose to send.
//
// This matches on error *type*, not on substrings of the message. The previous
// substring list included "tls" and "eof", which appear in text that has
// nothing to do with the network: every TLSA diagnostic contains "tls", so a
// client-side "invalid TLSA digest" was classified as unreachable-API and told
// the user to check their connection. Matching a rendered message is guessing
// at a value the sender never promised.
func isTransportError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return true
	}
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return true
	}
	// An unexpected EOF mid-response is a truncated connection, not a verdict.
	return errors.Is(err, io.ErrUnexpectedEOF)
}
