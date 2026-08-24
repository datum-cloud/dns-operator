// SPDX-License-Identifier: AGPL-3.0-only

package bind

import (
	"errors"
	"strconv"

	"go.miloapis.com/dns-operator/internal/cmd/dns/rdata"
)

// looksLikeTTL reports whether tok occupies the TTL slot rather than the class
// or type slot. Every TTL spelling starts with a digit and no RR type or class
// name does, which makes the test exact rather than heuristic.
func looksLikeTTL(tok string) bool {
	return tok != "" && tok[0] >= '0' && tok[0] <= '9'
}

// parseTTL reads a zone-file TTL: a bare number of seconds ("3600"), a single
// suffixed unit ("1h", "2W"), or a concatenation of them ("1h30m", "1w2d").
//
// The grammar lives in rdata, which is the CLI's one definition of what a TTL
// may be written as, so a value accepted by `--ttl` is accepted in a zone file
// and the reverse. Only the error wording is bind's, because a zone file has a
// line number and a syntax to point at.
func parseTTL(s string) (int64, error) {
	if s == "" {
		return 0, errf("TTL is empty")
	}
	secs, err := rdata.ParseTTLSeconds(s)
	switch {
	case errors.Is(err, rdata.ErrTTLRange):
		return 0, errf("TTL %q is outside the range 0 to %d", s, rdata.MaxTTL)
	case err != nil:
		return 0, badTTL(s)
	}
	return secs, nil
}

func badTTL(s string) error {
	return &Error{
		Msg: "TTL " + strconv.Quote(s) + " is not a number of seconds or a duration",
		Fix: "a TTL is seconds (\"3600\") or a duration built from s, m, h, d and w (\"1h\", \"1w2d\")",
	}
}

func ttlPtr(v int64) *int64 { return &v }
