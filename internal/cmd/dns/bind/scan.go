// SPDX-License-Identifier: AGPL-3.0-only

package bind

import (
	"bufio"
	"io"
	"strings"
)

// logicalLine is one zone-file statement: a directive or a record, with its
// comments stripped and any parenthesised continuation folded onto one line.
type logicalLine struct {
	// text is the statement with comments removed, parentheses replaced by
	// spaces, and continuation lines joined. Quoting and escaping inside the
	// text survive untouched, because rdata owns their interpretation.
	text string
	// line is the 1-based number of the physical line the statement starts on,
	// which is the number every error and Unsupported entry reports.
	line int
	// raw is the statement's physical lines joined with a space, comments
	// intact, for reporting a line back to the user as they wrote it.
	raw string
	// inherits records that the statement began with whitespace, which in a
	// zone file means "same owner name as the previous record" (RFC 1035 §5.1).
	inherits bool
}

// scan splits a zone file into logical lines.
//
// It is deliberately a character scanner rather than a set of line-shape
// heuristics: a ";" inside a quoted TXT string is not a comment, a "(" inside
// one does not open a continuation, and the owner-name inheritance rule turns
// on leading whitespace, all of which a regexp pass over whole lines gets
// wrong.
func scan(r io.Reader) ([]logicalLine, error) {
	var (
		out       []logicalLine
		cur       strings.Builder
		rawParts  []string
		depth     int
		startLine int
		inherits  bool
	)

	sc := bufio.NewScanner(r)
	// Zone files carry long TXT and TLSA values; the default 64KiB token limit
	// is generous but the cap is raised so a DKIM key never truncates silently.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNo := 0
	for sc.Scan() {
		lineNo++
		phys := strings.TrimRight(sc.Text(), "\r")

		stripped, newDepth, err := stripLine(phys, depth)
		if err != nil {
			return nil, &Error{Line: lineNo, Msg: err.Error()}
		}

		starting := depth == 0 && cur.Len() == 0
		if starting && newDepth == 0 && strings.TrimSpace(stripped) == "" {
			// A blank or comment-only line between statements. It carries no
			// owner name, so it must not reset inheritance either way.
			continue
		}
		if starting {
			startLine = lineNo
			inherits = phys != "" && (phys[0] == ' ' || phys[0] == '\t')
			rawParts = rawParts[:0]
		}

		if cur.Len() > 0 {
			cur.WriteByte(' ')
		}
		cur.WriteString(strings.TrimSpace(stripped))
		rawParts = append(rawParts, strings.TrimSpace(phys))
		depth = newDepth

		if depth == 0 {
			text := strings.TrimSpace(collapseSpaces(cur.String()))
			if text != "" {
				out = append(out, logicalLine{
					text:     text,
					line:     startLine,
					raw:      strings.Join(rawParts, " "),
					inherits: inherits,
				})
			}
			cur.Reset()
			rawParts = rawParts[:0]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if depth > 0 {
		return nil, &Error{
			Line: startLine,
			Msg:  "unbalanced \"(\" — the record is never closed",
			Fix:  "close the parenthesised value with a \")\"",
		}
	}
	return out, nil
}

// stripLine removes the comment from one physical line and replaces the
// parentheses that open and close a continuation with spaces, returning the
// remaining text and the new nesting depth. A ";", "(" or ")" inside a quoted
// character-string is data, not syntax.
func stripLine(s string, depth int) (string, int, error) {
	var b strings.Builder
	b.Grow(len(s))
	inQuote := false

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			b.WriteByte(c)
			b.WriteByte(s[i+1])
			i++
		case c == '"':
			inQuote = !inQuote
			b.WriteByte(c)
		case inQuote:
			b.WriteByte(c)
		case c == ';':
			// The rest of the line is a comment. A ";" inside a quoted string
			// never reaches here — the inQuote case above claims it first.
			return b.String(), depth, nil
		case c == '(':
			depth++
			b.WriteByte(' ')
		case c == ')':
			if depth == 0 {
				return "", 0, errf("unbalanced \")\"")
			}
			depth--
			b.WriteByte(' ')
		default:
			b.WriteByte(c)
		}
	}
	if inQuote {
		return "", 0, errf("unterminated quoted string — a character-string may not span lines")
	}
	return b.String(), depth, nil
}

// collapseSpaces reduces runs of whitespace outside quoted strings to a single
// space, so a record folded out of a parenthesised block reads as one line.
func collapseSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inQuote := false
	lastSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			b.WriteByte(c)
			b.WriteByte(s[i+1])
			i++
			lastSpace = false
			continue
		}
		if c == '"' {
			inQuote = !inQuote
			b.WriteByte(c)
			lastSpace = false
			continue
		}
		if !inQuote && (c == ' ' || c == '\t') {
			if !lastSpace {
				b.WriteByte(' ')
			}
			lastSpace = true
			continue
		}
		b.WriteByte(c)
		lastSpace = false
	}
	return b.String()
}

// token is one whitespace-separated field of a statement, with the byte offset
// it starts at so the caller can slice the rdata out of the original text with
// its quoting intact.
type token struct {
	text  string
	start int
}

// splitFields tokenises a statement, treating a quoted character-string as one
// field. Unlike rdata's tokenizer it keeps the quotes in token.text, because
// the only consumers here are the owner/TTL/class/type slots, which are never
// quoted, and the offset used to slice the rdata.
func splitFields(s string) []token {
	var out []token
	i := 0
	for i < len(s) {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			break
		}
		start := i
		inQuote := false
		var b strings.Builder
		for i < len(s) {
			c := s[i]
			if c == '\\' && i+1 < len(s) {
				b.WriteByte(c)
				b.WriteByte(s[i+1])
				i += 2
				continue
			}
			if c == '"' {
				inQuote = !inQuote
				b.WriteByte(c)
				i++
				continue
			}
			if !inQuote && (c == ' ' || c == '\t') {
				break
			}
			b.WriteByte(c)
			i++
		}
		out = append(out, token{text: b.String(), start: start})
	}
	return out
}
