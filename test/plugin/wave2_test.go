// SPDX-License-Identifier: AGPL-3.0-only

package plugin_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// Command-level cases for the wave-2 surface.
//
// Each one is gated on requireCommand, so it skips with an explanatory message
// until the command is registered in root.go and then starts running with no
// edit here. Adding a new case means adding a table entry, not new plumbing.
//
// These are the cases the API-layer tests in api_test.go already cover one
// level down; what they add is the part only a subprocess can prove — the exit
// code the shell sees and the exact bytes on each stream.

func TestZoneListEmitsRawObjectsAsJSON(t *testing.T) {
	requireCommand(t, "zone")
	createZone(t, "json-example-com", "json-example.com")

	res := runPlugin(t, "zone", "list", "-o", "json")
	if res.ExitCode != util.ExitOK {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", res.ExitCode, res.Stderr)
	}

	// -o json must emit the raw API objects, not the flattened table rows.
	var decoded map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &decoded); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\ngot:\n%s", err, res.Stdout)
	}
	if _, hasKind := decoded["kind"]; !hasKind {
		if _, hasItems := decoded["items"]; !hasItems {
			t.Errorf("JSON output carries neither kind nor items; it does not look like a raw API object:\n%s", res.Stdout)
		}
	}
	if !strings.Contains(res.Stdout, "json-example.com") {
		t.Errorf("JSON output does not include the created zone:\n%s", res.Stdout)
	}
}

func TestZoneDescribeMissingZoneExitsFour(t *testing.T) {
	requireCommand(t, "zone")

	res := runPlugin(t, "zone", "describe", "definitely-not-a-zone.example")
	if res.ExitCode != util.ExitNotFound {
		t.Fatalf("exit = %d, want %d (DNS_NOT_FOUND)\nstderr:\n%s",
			res.ExitCode, util.ExitNotFound, res.Stderr)
	}
	for _, want := range []string{"Error:", "exit status 4   # DNS_NOT_FOUND"} {
		if !strings.Contains(res.Stderr, want) {
			t.Errorf("stderr does not contain %q\ngot:\n%s", want, res.Stderr)
		}
	}
	if res.Stdout != "" {
		t.Errorf("stdout = %q, want empty on an error", res.Stdout)
	}
}

func TestEntitlementRefusalThroughTheCLI(t *testing.T) {
	requireCommand(t, "zone")
	withoutEntitlement(t)

	res := runPlugin(t, "zone", "list")
	if res.ExitCode != util.ExitForbidden {
		t.Fatalf("exit = %d, want %d (DNS_FORBIDDEN)\nstderr:\n%s",
			res.ExitCode, util.ExitForbidden, res.Stderr)
	}
	for _, want := range []string{
		`DNS is not enabled for project "acme-prod"`,
		"datumctl services enable dns.networking.miloapis.com",
		"exit status 3   # DNS_FORBIDDEN",
	} {
		if !strings.Contains(res.Stderr, want) {
			t.Errorf("stderr does not contain %q\ngot:\n%s", want, res.Stderr)
		}
	}
	// The pre-flight must never block on a prompt in a non-interactive session.
	if strings.Contains(res.Stderr, "[y/N]") {
		t.Errorf("the pre-flight prompted non-interactively:\n%s", res.Stderr)
	}
}

func TestUnknownSubcommandSuggestsTheRealOne(t *testing.T) {
	// The suggestion needs at least one registered command to suggest. Until
	// wave 2 lands, TestExitCodes covers the exit code and message; this covers
	// the "did you mean" block.
	requireCommand(t, "zone")

	res := runPlugin(t, "zne")
	if res.ExitCode != util.ExitUsage {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", res.ExitCode, util.ExitUsage, res.Stderr)
	}
	for _, want := range []string{"Did you mean this?", "zone"} {
		if !strings.Contains(res.Stderr, want) {
			t.Errorf("stderr does not contain %q\ngot:\n%s", want, res.Stderr)
		}
	}
}

func TestRecordListFlattensRecordSets(t *testing.T) {
	requireCommand(t, "record")

	zone := createZone(t, "records-example-com", "records-example.com")
	createRecordSet(t, "records-example-com-a", zone.Name, "A",
		recordEntry("www", "203.0.113.10"),
		recordEntry("api", "203.0.113.11"))

	res := runPlugin(t, "record", "list", "records-example.com")
	if res.ExitCode != util.ExitOK {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", res.ExitCode, res.Stderr)
	}
	// Records, not record sets: both owner names appear as their own rows.
	for _, want := range []string{"www", "api", "203.0.113.10", "203.0.113.11"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout does not contain %q\ngot:\n%s", want, res.Stdout)
		}
	}
}

// Apply is the declarative path, so the property that matters is convergence:
// applying a file reports and makes exactly the changes it printed, and
// applying the same file again is a no-op. A diff tool that is not idempotent
// silently rewrites the zone on every CI run.
func TestRecordApplyConvergesAndIsIdempotent(t *testing.T) {
	requireCommand(t, "record")
	createZone(t, "apply-example-com", "apply-example.com")

	zoneFile := filepath.Join(t.TempDir(), "apply-example.com.zone")
	contents := "$ORIGIN apply-example.com.\n$TTL 300\n" +
		"@    300 IN MX    10 mail.apply-example.com.\n" +
		"www  300 IN A     203.0.113.10\n" +
		"www  300 IN A     203.0.113.11\n"
	if err := os.WriteFile(zoneFile, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing the zone file: %v", err)
	}

	t.Run("dry run prints the diff and writes nothing", func(t *testing.T) {
		res := runPlugin(t, "record", "apply", "apply-example.com", "-f", zoneFile, "--dry-run")
		if res.ExitCode != util.ExitOK {
			t.Fatalf("exit = %d, want 0\nstderr:\n%s", res.ExitCode, res.Stderr)
		}
		for _, want := range []string{"203.0.113.10", "203.0.113.11", "mail.apply-example.com.", "Dry run"} {
			if !strings.Contains(res.Stdout, want) {
				t.Errorf("diff does not mention %q\ngot:\n%s", want, res.Stdout)
			}
		}

		// Nothing was written, so the zone is still empty. Asserting on the
		// empty-state banner rather than on the absence of an address: the
		// "Get started" hint quotes an example IP, so a substring check for one
		// matches the empty output too.
		after := runPlugin(t, "record", "list", "apply-example.com")
		if !strings.Contains(after.Stdout, "No records found") {
			t.Errorf("--dry-run wrote records:\n%s", after.Stdout)
		}
	})

	t.Run("apply converges", func(t *testing.T) {
		res := runPlugin(t, "record", "apply", "apply-example.com", "-f", zoneFile)
		if res.ExitCode != util.ExitOK {
			t.Fatalf("exit = %d, want 0\nstderr:\n%s", res.ExitCode, res.Stderr)
		}

		after := runPlugin(t, "record", "list", "apply-example.com")
		for _, want := range []string{"203.0.113.10", "203.0.113.11", "10 mail.apply-example.com."} {
			if !strings.Contains(after.Stdout, want) {
				t.Errorf("record list does not show %q after apply\ngot:\n%s", want, after.Stdout)
			}
		}
	})

	t.Run("re-applying the same file changes nothing", func(t *testing.T) {
		res := runPlugin(t, "record", "apply", "apply-example.com", "-f", zoneFile)
		if res.ExitCode != util.ExitOK {
			t.Fatalf("exit = %d, want 0\nstderr:\n%s", res.ExitCode, res.Stderr)
		}
		if !strings.Contains(res.Stdout, "No changes") {
			t.Errorf("a second apply of an unchanged file reported changes:\n%s", res.Stdout)
		}
	})
}

// Export and apply are documented as a closed loop, so exporting a zone and
// applying the result back must be a no-op. If the emitter and the parser
// disagree about any type, this is where it shows.
func TestExportApplyRoundTripIsANoOp(t *testing.T) {
	requireCommand(t, "record")
	createZone(t, "roundtrip-zone-com", "roundtrip-zone.com")

	// Every record carries an explicit TTL. Records left at the default Auto
	// TTL do not survive this round trip today — see
	// TestExportApplyRoundTripWithAutoTTL below — and mixing that in would
	// leave this test asserting the TTL bug rather than what it is for, which
	// is whether the emitter and the parser agree about each record type.
	seed := []struct{ args []string }{
		{[]string{"record", "create", "roundtrip-zone.com", "www", "A", "203.0.113.10", "--ttl", "300"}},
		{[]string{"record", "create", "roundtrip-zone.com", "@", "MX", "--preference", "10",
			"--exchange", "mail.roundtrip-zone.com.", "--ttl", "300"}},
		{[]string{"record", "create", "roundtrip-zone.com", "api", "CNAME", "lb.example.net.", "--ttl", "300"}},
		{[]string{"record", "create", "roundtrip-zone.com", "_dmarc", "TXT", "v=DMARC1; p=none", "--ttl", "300"}},
	}
	for _, s := range seed {
		if res := runPlugin(t, s.args...); res.ExitCode != util.ExitOK {
			t.Fatalf("seeding %v: exit %d\nstderr:\n%s", s.args, res.ExitCode, res.Stderr)
		}
	}

	exported := runPlugin(t, "zone", "export", "roundtrip-zone.com")
	if exported.ExitCode != util.ExitOK {
		t.Fatalf("export: exit %d\nstderr:\n%s", exported.ExitCode, exported.Stderr)
	}

	zoneFile := filepath.Join(t.TempDir(), "roundtrip.zone")
	if err := os.WriteFile(zoneFile, []byte(exported.Stdout), 0o600); err != nil {
		t.Fatalf("writing the exported zone: %v", err)
	}

	res := runPlugin(t, "record", "apply", "roundtrip-zone.com", "-f", zoneFile)
	if res.ExitCode != util.ExitOK {
		t.Fatalf("apply: exit %d\nstderr:\n%s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "No changes") {
		t.Errorf("export → apply was not a no-op; the emitter and parser disagree:\n%s", res.Stdout)
	}
}

// A record left at the default Auto TTL does not survive export → apply.
//
// zone export emits "$TTL 300" and then writes an Auto-TTL record with no
// per-record TTL field. Re-reading that file resolves the record's TTL from the
// $TTL default, producing an explicit 300, which the diff then reports against
// the live Auto as a change:
//
//	→   www   A   Auto → 300   203.0.113.10
//
// So every export → apply cycle rewrites every Auto-TTL record and pins it to
// 300. Auto is the default for any record created without --ttl, so this is the
// common case, and `zone export --help` promises the opposite: "exporting and
// re-applying an untouched file reports no changes."
//
// This test is skipped rather than asserting the current behaviour on purpose.
// Pinning the wrong output in a golden assertion is how a bug outlives the
// review that should have caught it; a skip records that the behaviour is
// unverified and names what must be true instead. Remove the Skip to verify a
// fix.
func TestExportApplyRoundTripWithAutoTTL(t *testing.T) {
	requireCommand(t, "record")
	t.Skip("known bug: export omits the per-record TTL for Auto records, so re-applying diffs Auto → 300")

	createZone(t, "auto-ttl-com", "auto-ttl.com")

	if res := runPlugin(t, "record", "create", "auto-ttl.com", "www", "A", "203.0.113.10"); res.ExitCode != util.ExitOK {
		t.Fatalf("seeding: exit %d\nstderr:\n%s", res.ExitCode, res.Stderr)
	}

	exported := runPlugin(t, "zone", "export", "auto-ttl.com")
	zoneFile := filepath.Join(t.TempDir(), "auto-ttl.zone")
	if err := os.WriteFile(zoneFile, []byte(exported.Stdout), 0o600); err != nil {
		t.Fatalf("writing the exported zone: %v", err)
	}

	res := runPlugin(t, "record", "apply", "auto-ttl.com", "-f", zoneFile, "--dry-run")
	if !strings.Contains(res.Stdout, "No changes") {
		t.Errorf("export → apply rewrote an Auto-TTL record:\n%s", res.Stdout)
	}
}
