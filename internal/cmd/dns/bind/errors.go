// SPDX-License-Identifier: AGPL-3.0-only

package bind

import (
	"fmt"

	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
)

// Error is a parse failure tied to the line that caused it.
//
// A zone file is edited by hand, so the line number is the whole point: "line
// 42: MX preference "abc" is not a number between 0 and 65535" is actionable in
// a way that the same sentence without a number is not.
type Error struct {
	// Line is the 1-based physical line the failing statement starts on.
	Line int
	// Msg is the one-line problem statement, without the line prefix.
	Msg string
	// Fix, when set, is the remedy the CLI prints on a following "Fix:" line.
	// It is populated from rdata's own fix text when the failure came from
	// there, so the parser never reinvents advice rdata already gives.
	Fix string
	// cause is the wrapped rdata error, exposed to errors.Is/As.
	cause error
}

func (e *Error) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("line %d: %s", e.Line, e.Msg)
	}
	return e.Msg
}

func (e *Error) Unwrap() error { return e.cause }

// errf builds a bare error with no line attached; scan and parse add the line
// as they return it.
func errf(format string, args ...any) error {
	return &Error{Msg: fmt.Sprintf(format, args...)}
}

// at wraps err with a line number, carrying rdata's fix text across so a
// missing trailing dot still suggests the domain the user meant.
func at(line int, err error) error {
	if e, ok := err.(*Error); ok {
		e.Line = line
		return e
	}
	return &Error{Line: line, Msg: err.Error(), Fix: rdata.FixFor(err), cause: err}
}

// atf builds a line-numbered error from a format string.
func atf(line int, format string, args ...any) error {
	return &Error{Line: line, Msg: fmt.Sprintf(format, args...)}
}

// atFix builds a line-numbered error carrying a remedy.
func atFix(line int, fix, format string, args ...any) error {
	return &Error{Line: line, Msg: fmt.Sprintf(format, args...), Fix: fix}
}

// FixFor returns the remedy attached to err, or "" when it carries none. It
// delegates to rdata for errors that originated there, so a caller rendering a
// "Fix:" line needs only this one function regardless of which layer failed.
func FixFor(err error) string {
	for err != nil {
		if e, ok := err.(*Error); ok && e.Fix != "" {
			return e.Fix
		}
		if fix := rdata.FixFor(err); fix != "" {
			return fix
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return ""
		}
		err = u.Unwrap()
	}
	return ""
}
