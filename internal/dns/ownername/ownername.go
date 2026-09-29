// SPDX-License-Identifier: AGPL-3.0-only

// Package ownername holds the rule that turns an owner name, as a record set
// spells it, into the absolute name the DNS backend keys an RRset on. It
// imports nothing from the operator, so every package that compares owner
// names can use it.
package ownername

import "strings"

// Qualify returns the absolute, lowercase name an owner denotes within zone. It
// accepts every spelling the API allows: "@" or the empty string for the apex,
// a relative label such as "api", or an absolute name ending in a dot. Several
// spellings therefore collapse to one name — "api", "API" and
// "api.example.com." all qualify to "api.example.com." in zone example.com — so
// callers comparing two owner names must compare their qualified forms rather
// than the raw values.
//
// zone carries no trailing dot; its case does not matter.
func Qualify(owner, zone string) string {
	owner, zone = strings.ToLower(owner), strings.ToLower(zone)
	if owner == "@" || owner == "" {
		return zone + "."
	}
	if owner[len(owner)-1] == '.' {
		return owner
	}
	return owner + "." + zone + "."
}
