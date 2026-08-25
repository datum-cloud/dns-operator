// SPDX-License-Identifier: AGPL-3.0-only

package plugin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// result is the observable behaviour of one plugin invocation: what automation
// branches on, and what a person reads.
type result struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// runPlugin execs the built binary with the datumctl environment injected.
//
// Stdin is /dev/null, which is what makes the non-interactive path real: it is
// an *os.File that is not a terminal, exactly as it would be in CI, so
// util.NonInteractive reports true for the reason it would in production.
func runPlugin(t *testing.T, args ...string) result {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, h.binary, args...)
	cmd.Dir = repoRoot()

	env := os.Environ()
	// Drop anything that would make the run non-deterministic, then inject the
	// harness's view of the platform.
	env = filterEnv(env, "CI", "DATUM_", util.CAFileEnv)
	for k, v := range pluginEnv() {
		env = append(env, k+"="+v)
	}
	cmd.Env = env

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("opening %s: %v", os.DevNull, err)
	}
	defer func() { _ = devNull.Close() }()
	cmd.Stdin = devNull

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	res := result{Stdout: stdout.String(), Stderr: stderr.String()}

	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		res.ExitCode = 0
	case errors.As(runErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		t.Fatalf("running %v: %v\nstdout:\n%s\nstderr:\n%s", args, runErr, res.Stdout, res.Stderr)
	}
	return res
}

// filterEnv drops entries whose key matches any of the given prefixes.
func filterEnv(env []string, prefixes ...string) []string {
	kept := env[:0:0]
	for _, entry := range env {
		drop := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(entry, prefix) {
				drop = true
				break
			}
		}
		if !drop {
			kept = append(kept, entry)
		}
	}
	return kept
}

// hasCommand reports whether the binary registers the named subcommand. The
// wave-2 command tests are gated on this so they activate the moment
// zone/record are wired into root.go, with no edit here.
func hasCommand(t *testing.T, name string) bool {
	t.Helper()
	help := runPlugin(t, "--help")
	for _, line := range strings.Split(help.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == name {
			return true
		}
	}
	return false
}

// requireCommand skips a test until the named subcommand exists.
func requireCommand(t *testing.T, name string) {
	t.Helper()
	if !hasCommand(t, name) {
		t.Skipf("the %q command is not registered yet (wave 2); this case activates automatically once it is", name)
	}
}

func TestPluginManifest(t *testing.T) {
	res := runPlugin(t, "--plugin-manifest")

	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", res.ExitCode, res.Stderr)
	}

	var manifest struct {
		Name          string `json:"name"`
		Version       string `json:"version"`
		Description   string `json:"description"`
		APIVersion    int    `json:"api_version"`
		MinAPIVersion int    `json:"min_api_version"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &manifest); err != nil {
		t.Fatalf("manifest is not valid JSON: %v\ngot:\n%s", err, res.Stdout)
	}

	if manifest.Name != "dns" {
		t.Errorf("name = %q, want %q", manifest.Name, "dns")
	}
	if manifest.APIVersion != 1 {
		t.Errorf("api_version = %d, want 1", manifest.APIVersion)
	}
	if manifest.MinAPIVersion != 1 {
		t.Errorf("min_api_version = %d, want 1", manifest.MinAPIVersion)
	}
	if manifest.Description == "" {
		t.Errorf("description is empty; datumctl lists plugins by it")
	}
	// The manifest must be served before cobra parses anything, so it works
	// even alongside a command line cobra would reject.
	if res.Stderr != "" {
		t.Errorf("stderr = %q, want empty", res.Stderr)
	}
}

func TestPluginManifestPrecedesFlagParsing(t *testing.T) {
	// ServeManifest scans os.Args itself, before Execute, precisely so this
	// works. A regression would make datumctl unable to enumerate a plugin
	// whose flags changed.
	res := runPlugin(t, "--plugin-manifest", "--not-a-real-flag")

	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, `"name": "dns"`) {
		t.Errorf("stdout does not carry the manifest:\n%s", res.Stdout)
	}
}

func TestExitCodes(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantExit     int
		wantStderr   []string
		wantNoStderr []string
	}{
		{
			name:     "an unknown flag is a usage error",
			args:     []string{"--nope"},
			wantExit: util.ExitUsage,
			wantStderr: []string{
				"Error: unknown flag: --nope",
				"Fix:   run `dns --help` to see the available flags.",
				"exit status 2   # DNS_USAGE",
			},
		},
		{
			name:     "an unknown subcommand is a usage error",
			args:     []string{"zne"},
			wantExit: util.ExitUsage,
			wantStderr: []string{
				`Error: unknown command "zne" for "dns"`,
				"exit status 2   # DNS_USAGE",
			},
		},
		{
			name:     "a malformed flag value is a usage error",
			args:     []string{"--output"},
			wantExit: util.ExitUsage,
			wantStderr: []string{
				"Error: flag needs an argument: --output",
				"exit status 2   # DNS_USAGE",
			},
		},
		{
			name:         "help succeeds and prints no error",
			args:         []string{"--help"},
			wantExit:     util.ExitOK,
			wantNoStderr: []string{"Error:", "exit status"},
		},
		{
			name:         "the bare command prints usage and succeeds",
			args:         nil,
			wantExit:     util.ExitOK,
			wantNoStderr: []string{"Error:", "exit status"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := runPlugin(t, tc.args...)

			if res.ExitCode != tc.wantExit {
				t.Errorf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s",
					res.ExitCode, tc.wantExit, res.Stdout, res.Stderr)
			}
			for _, want := range tc.wantStderr {
				if !strings.Contains(res.Stderr, want) {
					t.Errorf("stderr does not contain %q\ngot:\n%s", want, res.Stderr)
				}
			}
			for _, unwanted := range tc.wantNoStderr {
				if strings.Contains(res.Stderr, unwanted) {
					t.Errorf("stderr unexpectedly contains %q\ngot:\n%s", unwanted, res.Stderr)
				}
			}
		})
	}
}

func TestErrorsGoToStderrNotStdout(t *testing.T) {
	// `-o json` piped to a parser must never receive a human error message.
	res := runPlugin(t, "--nope")

	if res.Stdout != "" {
		t.Errorf("stdout = %q, want empty; errors belong on stderr", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "Error:") {
		t.Errorf("stderr does not carry the error:\n%s", res.Stderr)
	}
}

func TestHelpListsThePersistentFlags(t *testing.T) {
	res := runPlugin(t, "--help")

	for _, flag := range []string{
		"--org", "--project", "-o, --output", "-v, --verbose",
		"-q, --quiet", "--color", "-y, --yes",
	} {
		if !strings.Contains(res.Stdout, flag) {
			t.Errorf("help does not document %s\ngot:\n%s", flag, res.Stdout)
		}
	}
	if !strings.Contains(res.Stdout, "table|wide|json|yaml|name") {
		t.Errorf("help does not advertise every output format\ngot:\n%s", res.Stdout)
	}
}

func TestCompletionHooksAreSilentAndSucceed(t *testing.T) {
	// A completion hook must never print a diagnostic: the shell would paste it
	// into the user's command line. It must also not run the entitlement
	// pre-flight, which is why this passes with no API traffic required.
	for _, hook := range []string{"__complete", "__completeNoDesc"} {
		t.Run(hook, func(t *testing.T) {
			res := runPlugin(t, hook, "")

			if res.ExitCode != 0 {
				t.Errorf("exit = %d, want 0\nstderr:\n%s", res.ExitCode, res.Stderr)
			}
			if strings.Contains(res.Stderr, "Error:") {
				t.Errorf("a completion hook printed an error:\n%s", res.Stderr)
			}
			if !strings.Contains(res.Stdout, ":") {
				t.Errorf("stdout does not carry a completion directive:\n%s", res.Stdout)
			}
		})
	}
}

// version must answer when nothing else can. It is what a user runs while
// debugging a broken login or an unreachable control plane, so the entitlement
// pre-flight — which would refuse — must not run for it.
func TestVersionRunsWithoutAnEntitlement(t *testing.T) {
	withoutEntitlement(t)

	res := runPlugin(t, "version")

	if res.ExitCode != util.ExitOK {
		t.Fatalf("exit = %d, want 0\nstderr:\n%s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "datumctl-dns") {
		t.Errorf("stdout = %q, want the version line", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "dns.networking.miloapis.com/v1alpha1") {
		t.Errorf("stdout does not name the API version: %q", res.Stdout)
	}
	if res.Stderr != "" {
		t.Errorf("stderr = %q, want empty", res.Stderr)
	}
}

// The same, with the datumctl environment stripped entirely: no API host, no
// credentials helper, no project.
func TestVersionRunsWithNoDatumEnvironment(t *testing.T) {
	cmd := exec.Command(h.binary, "version")
	cmd.Dir = repoRoot()
	cmd.Env = filterEnv(os.Environ(), "DATUM_", util.CAFileEnv)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("version failed with no DATUM_* set: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "datumctl-dns") {
		t.Errorf("output = %q", out)
	}
}

// The version the command prints and the version the host reads from the
// manifest must be the same string; they are separate code paths over one
// ldflags variable.
func TestVersionMatchesTheManifest(t *testing.T) {
	manifest := runPlugin(t, "--plugin-manifest")
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(manifest.Stdout), &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}

	version := runPlugin(t, "version")
	if !strings.Contains(version.Stdout, m.Version) {
		t.Errorf("version prints %q but the manifest reports %q", strings.TrimSpace(version.Stdout), m.Version)
	}
}
