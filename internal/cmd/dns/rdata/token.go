// SPDX-License-Identifier: AGPL-3.0-only

package rdata

import "strings"

// token is one whitespace-separated field of a presentation-format line.
type token struct {
	// text is the token with surrounding quotes removed and backslash escapes
	// resolved.
	text string
	// quoted records whether the token was written as a quoted
	// character-string.
	quoted bool
	// start is the byte offset of the token in the original input, including
	// its opening quote.
	start int
	// raw is the token exactly as it was written, escapes and quotes intact.
	//
	// Host names need it. A DNS name may escape a dot to keep it inside a
	// label — "first\.last.example.com." is one mailbox, not two labels — and
	// resolving that escape into text would silently turn it into a different
	// name. Fields that hold a host name read raw; fields that hold opaque
	// data read text.
	raw string
}

// hostToken returns the spelling of tok to use for a field that holds a host
// name: the raw source for an unquoted token, so escapes survive, and the
// resolved text for a quoted one, where the quotes are the delimiter.
func hostToken(tok token) string {
	if tok.quoted {
		return tok.text
	}
	return tok.raw
}

// tokenize splits presentation-format rdata into tokens, honouring quoted
// character-strings and backslash escapes. When comments is true a ";" that
// begins an unquoted token ends the line, as it does in a zone file.
func tokenize(s string, comments bool) ([]token, error) {
	var out []token
	i := 0
	for i < len(s) {
		for i < len(s) && isSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			break
		}
		start := i
		if s[i] == '"' {
			i++
			var b strings.Builder
			closed := false
			for i < len(s) {
				c := s[i]
				if c == '\\' && i+1 < len(s) {
					ch, n := unescapeAt(s, i)
					b.WriteByte(ch)
					i += n
					continue
				}
				if c == '"' {
					i++
					closed = true
					break
				}
				b.WriteByte(c)
				i++
			}
			if !closed {
				return nil, errf("unterminated quoted string in %q", s)
			}
			out = append(out, token{text: b.String(), quoted: true, start: start, raw: s[start:i]})
			continue
		}
		if comments && s[i] == ';' {
			break
		}
		var b strings.Builder
		for i < len(s) && !isSpace(s[i]) {
			if s[i] == '\\' && i+1 < len(s) {
				ch, n := unescapeAt(s, i)
				b.WriteByte(ch)
				i += n
				continue
			}
			b.WriteByte(s[i])
			i++
		}
		out = append(out, token{text: b.String(), quoted: false, start: start, raw: s[start:i]})
	}
	return out, nil
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

// quoteTXT renders s as a zone-file character-string, escaping everything that
// would otherwise terminate or corrupt it. Semicolons are escaped the same way
// internal/pdns escapes them, so a value that round-trips through this function
// survives quoteIfNeeded untouched.
//
// Control characters become \DDD decimal escapes, as RFC 1035 §5.1 requires. A
// character-string may not span lines, so emitting a newline literally produces
// a zone file that no parser — this repository's bind scanner included — can
// read back: `zone export` would write a file that fails on re-import, and the
// failure would surface long after the export that caused it. A carriage return
// is the same story with a subtler ending, since a scanner that trims CR at the
// line boundary would eat it silently instead of erroring.
func quoteTXT(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' || c == '"' || c == ';':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			// \DDD is exactly three decimal digits.
			b.WriteByte('\\')
			b.WriteByte('0' + c/100)
			b.WriteByte('0' + (c/10)%10)
			b.WriteByte('0' + c%10)
		default:
			// Bytes at 0x80 and above are left alone: they are UTF-8
			// continuation bytes far more often than they are anything a
			// zone file needs escaped, and escaping them per byte would make
			// every non-ASCII value unreadable for no gain.
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// unescapeAt resolves the escape sequence starting at the backslash s[i] and
// returns the byte it denotes together with the number of input bytes consumed.
// It handles the \DDD decimal form as well as the single-character form.
func unescapeAt(s string, i int) (byte, int) {
	if i+3 < len(s) && isDigit(s[i+1]) && isDigit(s[i+2]) && isDigit(s[i+3]) {
		v := int(s[i+1]-'0')*100 + int(s[i+2]-'0')*10 + int(s[i+3]-'0')
		if v <= 255 {
			return byte(v), 4
		}
	}
	return s[i+1], 2
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// chunk255 splits s into <=255-byte pieces, the maximum length of a single
// zone-file character-string (RFC 1035 §3.3).
func chunk255(s string) []string {
	if len(s) <= 255 {
		return []string{s}
	}
	var out []string
	for len(s) > 255 {
		out = append(out, s[:255])
		s = s[255:]
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}
