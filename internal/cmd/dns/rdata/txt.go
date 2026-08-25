// SPDX-License-Identifier: AGPL-3.0-only

package rdata

import (
	"strings"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// TXT has two representations and the package is strict about which is which.
//
//   - The LOGICAL form is the string the user typed and the string a human
//     reads: `v=DMARC1; p=none`. This is what TXTRecordSpec.Content holds
//     everywhere inside this package, and what Key, Equal, Render, Fields and
//     Validate all operate on.
//   - The WIRE form is zone-file presentation format: one or more quoted
//     character-strings, escaped, each at most 255 bytes:
//     `"v=DMARC1\; p=none"`. This is what must be stored in the API, because
//     pdns.quoteIfNeeded passes an already-quoted value through untouched and
//     mangles everything else.
//
// TXTContentForAPI converts logical to wire; TXTContentFromAPI converts back.
// Every read path decodes defensively through txtLogical, so a value written by
// something else — the portal, kubectl, an earlier version of this CLI — still
// compares, renders and validates as the logical string it encodes. Without
// that, `record delete <zone> @ TXT "hello world"` matches nothing: the user
// types the value they created, and the stored form is a different string.

// TXTContentForAPI returns the value to store in txt.content.
//
// pdns.quoteIfNeeded wraps the stored content in a single quoted string unless
// it is already quoted end to end, which breaks four things at once: a value
// over 255 bytes exceeds the maximum length of a zone-file character-string; an
// embedded quote is left bare and terminates the string early; a trailing
// backslash escapes the closing quote and runs the corruption past the end of
// the value; and an embedded backslash is read back as an escape, silently
// altering the value.
//
// Handing it the already-quoted, already-chunked wire form sidesteps all four —
// quoteIfNeeded passes it through unchanged.
//
// Note that the semicolon case, the one anybody checks by hand, behaves
// identically whether or not this function is used: quoteIfNeeded escapes
// semicolons itself. A caller who skips it therefore passes every obvious
// manual test and ships the other four landmines, which is why the write path
// must not be left to remember.
func TXTContentForAPI(content string) string { return renderTXT(content) }

// TXTContentFromAPI converts a stored txt.content back to its logical form. It
// is the inverse of TXTContentForAPI, and a no-op on a value that is already
// logical.
func TXTContentFromAPI(stored string) string { return txtLogical(stored) }

// txtLogical decodes wire form to logical form, leniently: a value that does
// not parse as a complete sequence of quoted character-strings is returned
// unchanged, because it is already logical.
//
// One case is irreducibly ambiguous: a logical value whose literal first and
// last characters are quotes is indistinguishable from the wire form of its own
// contents, and is read as the latter. That ambiguity is not resolvable here,
// because pdns.quoteIfNeeded makes exactly the same guess — a value submitted
// any other way is corrupted at the backend instead. Interior quotes are not
// affected; only a value wrapped in them end to end.
func txtLogical(s string) string {
	if !strings.HasPrefix(s, `"`) {
		return s
	}
	decoded, err := decodeTXTStrings(s)
	if err != nil {
		return s
	}
	return decoded
}

// parseTXTValue is the strict decoder used when the input came from a user.
// Where txtLogical shrugs at a value it cannot decode, this reports it: text
// that starts as a quoted string and then runs on unquoted is a quoting
// mistake, and guessing at it would submit something the user did not write.
func parseTXTValue(v string) (string, error) {
	if !strings.HasPrefix(v, `"`) {
		// An unquoted value is taken verbatim, which is what a user typing
		// --data or a shell-quoted positional means.
		return v, nil
	}
	decoded, err := decodeTXTStrings(v)
	if err != nil {
		return "", err
	}
	return decoded, nil
}

// decodeTXTStrings concatenates a sequence of quoted character-strings.
func decodeTXTStrings(v string) (string, error) {
	toks, err := tokenize(v, false)
	if err != nil {
		return "", err
	}
	if len(toks) == 0 {
		return "", errf("TXT value %q has no content", v)
	}
	var b strings.Builder
	for _, tok := range toks {
		if !tok.quoted {
			return "", fixf(
				"quote the whole value, or every character-string in it",
				"TXT value %q mixes quoted and unquoted text", v,
			)
		}
		b.WriteString(tok.text)
	}
	return b.String(), nil
}

// renderTXT encodes a logical value as wire form: escaped, and split into
// <=255-byte character-strings (RFC 1035 §3.3). It decodes first so that
// encoding an already-encoded value is idempotent rather than adding a layer of
// quoting every time a record is read and written back.
func renderTXT(content string) string {
	parts := chunk255(txtLogical(content))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, quoteTXT(p))
	}
	return strings.Join(out, " ")
}

// EntryForAPI returns a copy of e with every field converted to the form the
// API must store. Today that means TXT and nothing else, but callers should use
// it rather than TXTContentForAPI directly: a write path that says "encode this
// entry" keeps working if another type ever grows a wire form, where a write
// path that special-cases TXT does not.
//
// Call it once, immediately before submitting. It is idempotent.
func EntryForAPI(t dnsv1alpha1.RRType, e dnsv1alpha1.RecordEntry) dnsv1alpha1.RecordEntry {
	if t == dnsv1alpha1.RRTypeTXT && e.TXT != nil {
		spec := *e.TXT
		spec.Content = TXTContentForAPI(spec.Content)
		e.TXT = &spec
	}
	return e
}

// EntryFromAPI returns a copy of e with every field converted back to its
// logical form.
//
// Read paths do not have to call it — Key, Equal, Render, Fields and Validate
// all decode defensively, so an entry straight off the API behaves correctly
// without it. It exists for a caller that wants to hold or hand on the logical
// value itself, and to make the pairing with EntryForAPI visible.
func EntryFromAPI(t dnsv1alpha1.RRType, e dnsv1alpha1.RecordEntry) dnsv1alpha1.RecordEntry {
	if t == dnsv1alpha1.RRTypeTXT && e.TXT != nil {
		spec := *e.TXT
		spec.Content = TXTContentFromAPI(spec.Content)
		e.TXT = &spec
	}
	return e
}
