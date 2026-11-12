// SPDX-License-Identifier: AGPL-3.0-only

package pubstore

import "testing"

func TestRecordKindNamesEveryStoredKind(t *testing.T) {
	for key, want := range map[string]string{
		"own.platform.owner-a": "own",
		"man.platform.pub-a":   "man",
		"chk.platform.pub-a-0": "chk",
		"out.platform.out-a":   "out",
		"shd.platform.shard-a": "shd",
		regionsKey:             "other",
	} {
		if got := recordKind(key); got != want {
			t.Errorf("recordKind(%q) = %q, want %q", key, got, want)
		}
	}
}
