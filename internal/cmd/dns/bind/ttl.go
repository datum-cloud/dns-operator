// SPDX-License-Identifier: AGPL-3.0-only

package bind

import (
	"strconv"
	"strings"
)

// maxTTL is the largest value a 32-bit unsigned TTL field can hold (RFC 2181
// restricts it to 31 bits, which is the same ceiling every provider enforces).
const maxTTL = 2147483647

// unitSeconds is BIND's TTL suffix vocabulary (RFC 2308 §4). Go's
// time.ParseDuration, which rdata.ParseTTL uses, knows "s", "m" and "h" but not
// "d" or "w", and zone files in the wild are full of "1D" and "1W".
var unitSeconds = map[byte]int64{
	's': 1,
	'm': 60,
	'h': 3600,
	'd': 86400,
	'w': 604800,
}

// looksLikeTTL reports whether tok occupies the TTL slot rather than the class
// or type slot. Every TTL spelling starts with a digit and no RR type or class
// name does, which makes the test exact rather than heuristic.
func looksLikeTTL(tok string) bool {
	return tok != "" && tok[0] >= '0' && tok[0] <= '9'
}

// parseTTL reads a zone-file TTL: a bare number of seconds ("3600"), a single
// suffixed unit ("1h", "2W"), or a concatenation of them ("1h30m", "1w2d").
func parseTTL(s string) (int64, error) {
	if s == "" {
		return 0, errf("TTL is empty")
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < 0 || n > maxTTL {
			return 0, errf("TTL %q is outside the range 0 to %d", s, maxTTL)
		}
		return n, nil
	}

	var total int64
	lower := strings.ToLower(s)
	i := 0
	for i < len(lower) {
		start := i
		for i < len(lower) && lower[i] >= '0' && lower[i] <= '9' {
			i++
		}
		if i == start || i >= len(lower) {
			return 0, badTTL(s)
		}
		mult, ok := unitSeconds[lower[i]]
		if !ok {
			return 0, badTTL(s)
		}
		n, err := strconv.ParseInt(lower[start:i], 10, 64)
		if err != nil {
			return 0, badTTL(s)
		}
		total += n * mult
		if total > maxTTL {
			return 0, errf("TTL %q is outside the range 0 to %d", s, maxTTL)
		}
		i++
	}
	return total, nil
}

func badTTL(s string) error {
	return &Error{
		Msg: "TTL " + strconv.Quote(s) + " is not a number of seconds or a duration",
		Fix: "a TTL is seconds (\"3600\") or a duration built from s, m, h, d and w (\"1h\", \"1w2d\")",
	}
}

func ttlPtr(v int64) *int64 { return &v }
