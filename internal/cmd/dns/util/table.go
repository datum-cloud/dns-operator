// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"io"
	"text/tabwriter"
)

// NewTabWriter returns a *tabwriter.Writer configured for command table output,
// matching the column spacing the compute plugin uses. Rows separate cells with
// a tab; the caller must Flush.
func NewTabWriter(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
}

// TruncateCell shortens a table cell to max display columns, ending it with an
// ellipsis when it does not fit. It reports whether anything was cut.
//
// Long values are not merely ugly. tabwriter sizes a column to its widest cell,
// so a single 400-byte DKIM key pushes STATUS off the right of every other row
// in the table — the cost of one long value is paid by every short one.
//
// Truncation counts runes rather than bytes so a multi-byte value is never cut
// mid-character, and the ellipsis is included in the budget so the column is
// never wider than max.
func TruncateCell(s string, max int) (string, bool) {
	if max <= 0 {
		return s, false
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s, false
	}
	return string(runes[:max-1]) + "…", true
}
