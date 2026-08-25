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
