// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"sigs.k8s.io/yaml"
)

// OutputFormat is the requested rendering for a command's data.
//
// table is for a person at a terminal, wide adds columns, json and yaml are the
// stable machine contract carrying the raw API objects, and name emits bare
// identifiers for xargs and command substitution.
type OutputFormat string

// emDash is what an empty or unknowable cell renders as, so a column never
// collapses and "we do not know" is visibly different from "nothing".
const emDash = "—"

const (
	OutputTable OutputFormat = "table"
	OutputWide  OutputFormat = "wide"
	OutputJSON  OutputFormat = "json"
	OutputYAML  OutputFormat = "yaml"
	OutputName  OutputFormat = "name"
)

// AllOutputFormats is the full set, used when a command allows every format.
func AllOutputFormats() []OutputFormat {
	return []OutputFormat{OutputTable, OutputWide, OutputJSON, OutputYAML, OutputName}
}

// ParseOutputFormat validates s against the allowed formats, defaulting to the
// full set when none are given. An unrecognised value is a usage error, since
// silently falling back to a table would hide a typo from a script.
func ParseOutputFormat(s string, allowed ...OutputFormat) (OutputFormat, error) {
	if len(allowed) == 0 {
		allowed = AllOutputFormats()
	}
	f := OutputFormat(s)
	for _, a := range allowed {
		if f == a {
			return f, nil
		}
	}

	names := make([]string, len(allowed))
	for i, a := range allowed {
		names[i] = string(a)
	}
	return "", UsageErrorf("invalid output format %q — must be one of: %s", s, strings.Join(names, ", "))
}

// IsMachine reports whether the format is a machine contract, so callers can
// suppress footers, colour, and other human-facing decoration.
func (f OutputFormat) IsMachine() bool {
	return f == OutputJSON || f == OutputYAML || f == OutputName
}

// PrintJSON serialises obj as indented JSON and writes it to w.
func PrintJSON(w io.Writer, obj any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(obj); err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
	return nil
}

// PrintYAML serialises obj as YAML and writes it to w.
func PrintYAML(w io.Writer, obj any) error {
	b, err := yaml.Marshal(obj)
	if err != nil {
		return fmt.Errorf("encoding YAML: %w", err)
	}
	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("writing YAML: %w", err)
	}
	return nil
}

// OrDash renders an empty string as an em dash, so a table never shows a blank
// cell that reads as a rendering bug.
func OrDash(s string) string {
	if s == "" {
		return emDash
	}
	return s
}
