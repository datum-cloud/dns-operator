// SPDX-License-Identifier: AGPL-3.0-only

package rdata

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// MaxTTL is the largest value a TTL may take: RFC 2181 §8 defines the field as
// a 31-bit unsigned quantity. The API applies no bounds at all, so this is a
// CLI-side rule.
const MaxTTL = int64(2147483647)

// ttlUnits is the suffix vocabulary a TTL may be written in, largest first.
// It is RFC 2308 §4's set, which is what zone files and every other DNS CLI
// use — Go's time.ParseDuration knows "s", "m" and "h" but not "d" or "w", so
// this is parsed here rather than delegated.
var ttlUnits = []struct {
	suffix string
	secs   int64
}{
	{"w", 604800},
	{"d", 86400},
	{"h", 3600},
	{"m", 60},
	{"s", 1},
}

// ParseTTL accepts a bare number of seconds ("300") or a duration built from
// the units in ttlUnits, either single ("5m", "1h", "1d") or compound
// ("1h30m"). An empty string or "auto" means "let the backend choose", which is
// represented as a nil TTL and resolves to 300s in internal/pdns.
//
// Every spelling FormatTTL emits parses back to the same number, so a TTL read
// off `record list` can be pasted straight into `--ttl`.
//
// Unlike the portal, no rounding onto a preset ladder happens here: 240 stays
// 240.
func ParseTTL(s string) (*int64, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" || v == "auto" {
		return nil, nil
	}
	secs, err := parseTTLSeconds(v, s)
	if err != nil {
		return nil, err
	}
	if err := checkTTLRange(secs, s); err != nil {
		return nil, err
	}
	return &secs, nil
}

// ParseTTLSeconds reads a TTL as a plain number of seconds, for callers that
// have no "auto" to represent and want the number rather than a pointer. Zone
// files are the case: a TTL slot there is always a value.
//
// The accepted spellings are exactly ParseTTL's, so the grammar cannot drift
// between what `--ttl` takes and what a zone file may contain.
func ParseTTLSeconds(s string) (int64, error) {
	secs, err := parseTTLSeconds(strings.ToLower(strings.TrimSpace(s)), s)
	if err != nil {
		return 0, err
	}
	if err := checkTTLRange(secs, s); err != nil {
		return 0, err
	}
	return secs, nil
}

// parseTTLSeconds reads v, already lowercased, as seconds. orig is the
// untouched input, used so errors quote back what the user actually typed.
func parseTTLSeconds(v, orig string) (int64, error) {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n, nil
	}
	// A fractional TTL is handed to time.ParseDuration, which understands the
	// decimal point that the unit loop below does not. "1.5h" is a perfectly
	// good TTL — 5400 whole seconds — and only a value that lands between
	// seconds is actually a mistake, so the two are reported differently.
	if strings.Contains(v, ".") {
		d, err := time.ParseDuration(v)
		if err != nil {
			return 0, badTTL(orig)
		}
		if d%time.Second != 0 {
			return 0, errf("TTL %q is not a whole number of seconds", orig)
		}
		return int64(d / time.Second), nil
	}
	// A leading sign is carried through the unit parse rather than rejected as
	// a stray character, so "-5m" earns the same "is negative" as "-5" does.
	neg := strings.HasPrefix(v, "-")
	v = strings.TrimPrefix(strings.TrimPrefix(v, "-"), "+")
	var total int64
	for i := 0; i < len(v); {
		start := i
		for i < len(v) && v[i] >= '0' && v[i] <= '9' {
			i++
		}
		if i == start || i >= len(v) {
			return 0, badTTL(orig)
		}
		mult, ok := unitSeconds(v[i])
		if !ok {
			return 0, badTTL(orig)
		}
		n, err := strconv.ParseInt(v[start:i], 10, 64)
		if err != nil {
			return 0, badTTL(orig)
		}
		total += n * mult
		// Checked inside the loop as well as after it, so a compound TTL
		// cannot overflow int64 on its way to the range check.
		if total > MaxTTL {
			return 0, &Error{Msg: fmt.Sprintf("TTL %q exceeds the maximum of %d seconds", orig, MaxTTL), err: ErrTTLRange}
		}
		i++
	}
	if neg {
		return -total, nil
	}
	return total, nil
}

func unitSeconds(c byte) (int64, bool) {
	for _, u := range ttlUnits {
		if u.suffix[0] == c {
			return u.secs, true
		}
	}
	return 0, false
}

func badTTL(orig string) error {
	return fixf(
		"give TTL in seconds (\"300\") or as a duration (\"5m\", \"1h\", \"1d\"), or \"auto\"",
		"invalid TTL %q", orig,
	)
}

// ErrTTLRange marks a TTL that parsed cleanly but fell outside 0..MaxTTL.
// Callers that render their own wording — a zone file has a line number and a
// syntax to point at — need to tell "not a TTL at all" from "a TTL that is too
// large", because only the second is worth quoting a range for.
var ErrTTLRange = errors.New("TTL out of range")

func checkTTLRange(secs int64, orig string) error {
	if secs < 0 {
		return &Error{Msg: fmt.Sprintf("TTL %q is negative", orig), err: ErrTTLRange}
	}
	if secs > MaxTTL {
		return &Error{Msg: fmt.Sprintf("TTL %q exceeds the maximum of %d seconds", orig, MaxTTL), err: ErrTTLRange}
	}
	return nil
}

// FormatTTL renders a TTL for display. A nil TTL is the backend default and
// renders as "Auto".
func FormatTTL(ttl *int64) string {
	if ttl == nil {
		return "Auto"
	}
	return FormatSeconds(*ttl)
}

// FormatSeconds renders a duration in seconds with its unit, always. A bare
// "5" in a TTL column leaves the reader guessing at seconds versus minutes, so
// every value carries a suffix.
//
// The largest unit that divides evenly wins, which turns the values people
// actually use into the shape they think in: 300 -> "5m", 3600 -> "1h", 86400
// -> "1d". Anything that does not divide evenly stays in seconds rather than
// becoming a compound like "1m30s" — one unit is easier to compare down a
// column, and the exact number is what a TTL means.
func FormatSeconds(secs int64) string {
	if secs > 0 {
		for _, u := range ttlUnits {
			if secs%u.secs == 0 {
				return strconv.FormatInt(secs/u.secs, 10) + u.suffix
			}
		}
	}
	return strconv.FormatInt(secs, 10) + "s"
}
