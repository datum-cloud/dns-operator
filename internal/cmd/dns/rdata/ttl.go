// SPDX-License-Identifier: AGPL-3.0-only

package rdata

import (
	"strconv"
	"strings"
	"time"
)

// maxTTL is the largest value a TTL may take: RFC 2181 §8 defines the field as
// a 31-bit unsigned quantity. The API applies no bounds at all, so this is a
// CLI-side rule.
const maxTTL = int64(2147483647)

// ParseTTL accepts a bare number of seconds ("300") or a Go duration ("5m",
// "1h"). An empty string or "auto" means "let the backend choose", which is
// represented as a nil TTL and resolves to 300s in internal/pdns.
//
// Unlike the portal, no rounding onto a preset ladder happens here: 240 stays
// 240.
func ParseTTL(s string) (*int64, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" || v == "auto" {
		return nil, nil
	}
	var secs int64
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		secs = n
	} else {
		d, derr := time.ParseDuration(v)
		if derr != nil {
			return nil, fixf(
				"give TTL in seconds (\"300\") or as a duration (\"5m\", \"1h\"), or \"auto\"",
				"invalid TTL %q", s,
			)
		}
		if d%time.Second != 0 {
			return nil, errf("TTL %q is not a whole number of seconds", s)
		}
		secs = int64(d / time.Second)
	}
	if err := checkTTLRange(secs, s); err != nil {
		return nil, err
	}
	return &secs, nil
}

func checkTTLRange(secs int64, orig string) error {
	if secs < 0 {
		return errf("TTL %q is negative", orig)
	}
	if secs > maxTTL {
		return errf("TTL %q exceeds the maximum of %d seconds", orig, maxTTL)
	}
	return nil
}

// FormatTTL renders a TTL for display. A nil TTL is the backend default and
// renders as "Auto".
func FormatTTL(ttl *int64) string {
	if ttl == nil {
		return "Auto"
	}
	return strconv.FormatInt(*ttl, 10)
}
