// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestNonInteractive(t *testing.T) {
	t.Run("a wired-up reader is answerable", func(t *testing.T) {
		t.Setenv("CI", "")
		// t.Setenv sets the variable, so unset it explicitly for this case.
		if err := unsetCI(t); err != nil {
			t.Fatal(err)
		}
		if NonInteractive(strings.NewReader("y\n")) {
			t.Errorf("NonInteractive(strings.Reader) = true, want false")
		}
	})

	t.Run("CI forces non-interactive", func(t *testing.T) {
		t.Setenv("CI", "true")
		if !NonInteractive(strings.NewReader("y\n")) {
			t.Errorf("NonInteractive() = false under CI, want true")
		}
	})
}

func TestConfirmYesNo(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		defaultYes bool
		want       bool
		wantPrompt string
	}{
		{name: "y", input: "y\n", want: true, wantPrompt: "Delete 3 A records for www.example.com? [y/N]: "},
		{name: "yes", input: "yes\n", want: true},
		{name: "uppercase Y", input: "Y\n", want: true},
		{name: "surrounding whitespace", input: "  yes  \n", want: true},
		{name: "n", input: "n\n", want: false},
		{name: "no", input: "no\n", want: false},
		{name: "empty takes the default (no)", input: "\n", want: false},
		{name: "empty takes the default (yes)", input: "\n", defaultYes: true, want: true},
		{name: "EOF takes the default (no)", input: "", want: false},
		{name: "EOF takes the default (yes)", input: "", defaultYes: true, want: true},
		{name: "garbage is a no", input: "maybe\n", want: false},
		{
			name:       "the default is visible in the prompt",
			input:      "\n",
			defaultYes: true,
			want:       true,
			wantPrompt: "Delete 3 A records for www.example.com? [Y/n]: ",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := unsetCI(t); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			got, err := ConfirmYesNo(strings.NewReader(tc.input), &out,
				"Delete 3 A records for www.example.com?", tc.defaultYes)
			if err != nil {
				t.Fatalf("ConfirmYesNo returned %v", err)
			}
			if got != tc.want {
				t.Errorf("ConfirmYesNo(%q) = %v, want %v", tc.input, got, tc.want)
			}
			if tc.wantPrompt != "" && out.String() != tc.wantPrompt {
				t.Errorf("prompt = %q, want %q", out.String(), tc.wantPrompt)
			}
		})
	}
}

func TestConfirmYesNoNonInteractiveProceeds(t *testing.T) {
	t.Setenv("CI", "true")
	var out bytes.Buffer
	got, err := ConfirmYesNo(strings.NewReader(""), &out, "Delete?", false)
	if err != nil {
		t.Fatalf("ConfirmYesNo returned %v", err)
	}
	if !got {
		t.Errorf("ConfirmYesNo = false non-interactively, want true (the low-blast-radius gate proceeds)")
	}
	if out.Len() != 0 {
		t.Errorf("a prompt was written non-interactively: %q", out.String())
	}
}

func TestConfirmTyped(t *testing.T) {
	const zone = "example.com"
	const prompt = "Deleting zone example.com will also delete all 12 DNS records it contains."

	tests := []struct {
		name     string
		input    string
		want     bool
		wantErr  bool
		wantCode int
	}{
		{name: "an exact match proceeds", input: "example.com\n", want: true},
		{name: "surrounding whitespace is trimmed", input: "  example.com  \n", want: true},
		{name: "a mismatch aborts", input: "example.net\n", wantErr: true, wantCode: ExitAborted},
		{name: "empty input aborts", input: "\n", wantErr: true, wantCode: ExitAborted},
		{name: "EOF aborts", input: "", wantErr: true, wantCode: ExitAborted},
		{name: "a y/n answer does not count", input: "y\n", wantErr: true, wantCode: ExitAborted},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := unsetCI(t); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			got, err := ConfirmTyped(strings.NewReader(tc.input), &out, prompt, zone)

			if tc.wantErr {
				var ce *CLIError
				if !errors.As(err, &ce) {
					t.Fatalf("ConfirmTyped returned %v, want a CLIError", err)
				}
				if ce.Code() != tc.wantCode {
					t.Errorf("code = %d, want %d", ce.Code(), tc.wantCode)
				}
				if got {
					t.Errorf("ConfirmTyped returned true alongside an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ConfirmTyped returned %v", err)
			}
			if !got {
				t.Errorf("ConfirmTyped(%q) = false, want true", tc.input)
			}
			want := prompt + "\nType \"example.com\" to confirm: "
			if out.String() != want {
				t.Errorf("prompt = %q, want %q", out.String(), want)
			}
		})
	}
}

func TestConfirmTypedRefusesNonInteractively(t *testing.T) {
	t.Setenv("CI", "true")
	var out bytes.Buffer
	got, err := ConfirmTyped(strings.NewReader("example.com\n"), &out, "prompt", "example.com")
	if got {
		t.Errorf("ConfirmTyped = true non-interactively, want a refusal")
	}
	var ce *CLIError
	if !errors.As(err, &ce) {
		t.Fatalf("ConfirmTyped returned %v, want a CLIError", err)
	}
	if ce.Code() != ExitAborted {
		t.Errorf("code = %d, want %d", ce.Code(), ExitAborted)
	}
	if !strings.Contains(ce.Fix(), "--yes") {
		t.Errorf("fix = %q, want it to name --yes", ce.Fix())
	}
	if out.Len() != 0 {
		t.Errorf("a prompt was written non-interactively: %q", out.String())
	}
}

// unsetCI removes CI for the duration of a test. Go's own test runner is often
// invoked with CI set, which would otherwise short-circuit every prompt.
func unsetCI(t *testing.T) error {
	t.Helper()
	t.Setenv("CI", "")
	return osUnsetenv("CI")
}

// osUnsetenv is a thin indirection so unsetCI reads cleanly next to t.Setenv,
// which can only set a variable and never remove one.
func osUnsetenv(key string) error { return os.Unsetenv(key) }

// Two prompts in one command must both see their answer. A buffered reader
// constructed per prompt reads ahead and discards the remainder, so the second
// prompt silently takes its default — which for a delete confirmation means
// declining a deletion the user already agreed to, or worse, the reverse.
func TestSequentialPromptsEachSeeTheirAnswer(t *testing.T) {
	if err := unsetCI(t); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader("y\ny\n")
	var out bytes.Buffer

	first, err := ConfirmYesNo(in, &out, "First?", false)
	if err != nil {
		t.Fatalf("first prompt: %v", err)
	}
	if !first {
		t.Errorf("first = false, want true")
	}

	second, err := ConfirmYesNo(in, &out, "Second?", false)
	if err != nil {
		t.Fatalf("second prompt: %v", err)
	}
	if !second {
		t.Errorf("second = false, want true — the first prompt consumed the second answer")
	}
}

// The same reader feeding a yes/no gate and then a typed gate, which is the
// entitlement-preflight-then-zone-delete sequence.
func TestYesNoThenTypedConfirmation(t *testing.T) {
	if err := unsetCI(t); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader("y\nexample.com\n")
	var out bytes.Buffer

	ok, err := ConfirmYesNo(in, &out, "Enable DNS?", false)
	if err != nil || !ok {
		t.Fatalf("yes/no gate: %v (ok=%v)", err, ok)
	}

	typed, err := ConfirmTyped(in, &out, "This deletes every record.", "example.com")
	if err != nil {
		t.Fatalf("typed gate: %v", err)
	}
	if !typed {
		t.Errorf("typed = false, want true — the first prompt consumed the zone name")
	}
}

// A reader with no trailing newline still yields its content.
func TestReadLineWithoutTrailingNewline(t *testing.T) {
	if err := unsetCI(t); err != nil {
		t.Fatal(err)
	}
	got, err := ConfirmYesNo(strings.NewReader("yes"), &bytes.Buffer{}, "?", false)
	if err != nil {
		t.Fatalf("ConfirmYesNo: %v", err)
	}
	if !got {
		t.Errorf("an answer with no trailing newline was not read")
	}
}
