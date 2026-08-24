// SPDX-License-Identifier: AGPL-3.0-only

// Package rdata parses, validates, and renders DNS record data for the
// `datumctl dns` plugin.
//
// Everything here is client-side. The API server admits a DNSRecordSet whose
// typed field does not match spec.recordType, and internal/pdns then skips the
// entry while building its RRset payload — the result is a record that does not
// exist and carries no error condition. Validate exists to make that class of
// mistake an error before the object is written.
//
// Two notations are supported for every type: zone-file presentation format
// (ParseValue/Render) and named flags (RegisterFlags/FromFlags). They produce
// identical RecordEntry values, TXT included — --data runs through the same
// decoder as a positional value, so a quoted character-string is presentation
// format either way and anything else is taken literally either way.
//
// One field has a second representation on the wire. TXTRecordSpec.Content
// holds the LOGICAL string everywhere in this package; the API must be given
// the encoded form. See txt.go, TXTContentForAPI and TXTContentFromAPI.
package rdata

import (
	"fmt"
	"sort"
	"strings"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// Error is an actionable validation error. Msg is the one-line problem
// statement; Fix, when set, is the suggested remedy the CLI prints on a
// following "Fix:" line.
type Error struct {
	Msg string
	Fix string
	// err, when set, is a sentinel this error stands for, so callers can ask
	// errors.Is what kind of validation failure it was without matching on the
	// message. It never appears in Error(): the text a user reads is Msg alone.
	err error
}

func (e *Error) Error() string { return e.Msg }

func (e *Error) Unwrap() error { return e.err }

func errf(format string, args ...any) error {
	return &Error{Msg: fmt.Sprintf(format, args...)}
}

func fixf(fix string, format string, args ...any) error {
	return &Error{Msg: fmt.Sprintf(format, args...), Fix: fix}
}

// FixFor returns the suggested remedy attached to err, or "" when err carries
// none. Callers render it as a "Fix:" line beneath the error.
func FixFor(err error) string {
	for err != nil {
		if e, ok := err.(*Error); ok {
			return e.Fix
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return ""
		}
		err = u.Unwrap()
	}
	return ""
}

// supportedTypes is the RRType enum from the CRD, in the order the enum marker
// declares it.
var supportedTypes = []dnsv1alpha1.RRType{
	dnsv1alpha1.RRTypeA,
	dnsv1alpha1.RRTypeAAAA,
	dnsv1alpha1.RRTypeALIAS,
	dnsv1alpha1.RRTypeCNAME,
	dnsv1alpha1.RRTypeTXT,
	dnsv1alpha1.RRTypeMX,
	dnsv1alpha1.RRTypeSRV,
	dnsv1alpha1.RRTypeCAA,
	dnsv1alpha1.RRTypeNS,
	dnsv1alpha1.RRTypeSOA,
	dnsv1alpha1.RRTypePTR,
	dnsv1alpha1.RRTypeTLSA,
	dnsv1alpha1.RRTypeHTTPS,
	dnsv1alpha1.RRTypeSVCB,
}

// SupportedTypes returns every RR type the API accepts.
func SupportedTypes() []dnsv1alpha1.RRType {
	out := make([]dnsv1alpha1.RRType, len(supportedTypes))
	copy(out, supportedTypes)
	return out
}

// ParseRRType resolves a user-supplied type name case-insensitively.
func ParseRRType(s string) (dnsv1alpha1.RRType, error) {
	want := strings.ToUpper(strings.TrimSpace(s))
	for _, t := range supportedTypes {
		if string(t) == want {
			return t, nil
		}
	}
	return "", errf("unsupported record type %q, must be one of %s", s, typeList())
}

func typeList() string {
	names := make([]string, 0, len(supportedTypes))
	for _, t := range supportedTypes {
		names = append(names, string(t))
	}
	return strings.Join(names, ", ")
}

// structured types are the ones whose rdata has more than one field, and which
// the CLI therefore teaches with named flags rather than positionally.
var structured = map[dnsv1alpha1.RRType]bool{
	dnsv1alpha1.RRTypeMX:    true,
	dnsv1alpha1.RRTypeSRV:   true,
	dnsv1alpha1.RRTypeCAA:   true,
	dnsv1alpha1.RRTypeTLSA:  true,
	dnsv1alpha1.RRTypeHTTPS: true,
	dnsv1alpha1.RRTypeSVCB:  true,
	dnsv1alpha1.RRTypeSOA:   true,
}

// IsStructured reports whether t is taught with named flags.
func IsStructured(t dnsv1alpha1.RRType) bool { return structured[t] }

// singleValued types hold at most one value per owner name. internal/pdns keeps
// the first non-empty CNAME/ALIAS entry and drops the rest, and overwrites the
// SOA rrset so the last entry wins — in every case data the user supplied is
// discarded without an error, so the CLI rejects it up front.
var singleValued = map[dnsv1alpha1.RRType]bool{
	dnsv1alpha1.RRTypeCNAME: true,
	dnsv1alpha1.RRTypeALIAS: true,
	dnsv1alpha1.RRTypeSOA:   true,
}

// IsSingleValued reports whether t permits only one value per owner name.
func IsSingleValued(t dnsv1alpha1.RRType) bool { return singleValued[t] }

// field describes one type-specific field of a RecordEntry.
type field struct {
	typ  dnsv1alpha1.RRType
	json string
	set  func(*dnsv1alpha1.RecordEntry) bool
}

// allFields enumerates every type-specific field so Validate can detect both a
// missing field and extra fields belonging to another type.
var allFields = []field{
	{dnsv1alpha1.RRTypeA, "a", func(e *dnsv1alpha1.RecordEntry) bool { return e.A != nil }},
	{dnsv1alpha1.RRTypeAAAA, "aaaa", func(e *dnsv1alpha1.RecordEntry) bool { return e.AAAA != nil }},
	{dnsv1alpha1.RRTypeALIAS, "alias", func(e *dnsv1alpha1.RecordEntry) bool { return e.ALIAS != nil }},
	{dnsv1alpha1.RRTypeCNAME, "cname", func(e *dnsv1alpha1.RecordEntry) bool { return e.CNAME != nil }},
	{dnsv1alpha1.RRTypeTXT, "txt", func(e *dnsv1alpha1.RecordEntry) bool { return e.TXT != nil }},
	{dnsv1alpha1.RRTypeMX, "mx", func(e *dnsv1alpha1.RecordEntry) bool { return e.MX != nil }},
	{dnsv1alpha1.RRTypeSRV, "srv", func(e *dnsv1alpha1.RecordEntry) bool { return e.SRV != nil }},
	{dnsv1alpha1.RRTypeCAA, "caa", func(e *dnsv1alpha1.RecordEntry) bool { return e.CAA != nil }},
	{dnsv1alpha1.RRTypeNS, "ns", func(e *dnsv1alpha1.RecordEntry) bool { return e.NS != nil }},
	{dnsv1alpha1.RRTypeSOA, "soa", func(e *dnsv1alpha1.RecordEntry) bool { return e.SOA != nil }},
	{dnsv1alpha1.RRTypePTR, "ptr", func(e *dnsv1alpha1.RecordEntry) bool { return e.PTR != nil }},
	{dnsv1alpha1.RRTypeTLSA, "tlsa", func(e *dnsv1alpha1.RecordEntry) bool { return e.TLSA != nil }},
	{dnsv1alpha1.RRTypeHTTPS, "https", func(e *dnsv1alpha1.RecordEntry) bool { return e.HTTPS != nil }},
	{dnsv1alpha1.RRTypeSVCB, "svcb", func(e *dnsv1alpha1.RecordEntry) bool { return e.SVCB != nil }},
}

// setFields returns the JSON names of every type-specific field set on e, in
// declaration order.
func setFields(e dnsv1alpha1.RecordEntry) []string {
	var out []string
	for _, f := range allFields {
		if f.set(&e) {
			out = append(out, f.json)
		}
	}
	return out
}

func jsonFieldFor(t dnsv1alpha1.RRType) string {
	for _, f := range allFields {
		if f.typ == t {
			return f.json
		}
	}
	return strings.ToLower(string(t))
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
