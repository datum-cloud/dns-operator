// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// NonInteractive reports whether prompts cannot be answered: CI is set, or the
// reader is a file that is not a terminal (a pipe, a redirect, /dev/null).
//
// A reader that is not an *os.File at all — a buffer a caller wired up
// deliberately, as tests do — counts as answerable, because someone went out of
// their way to supply the answers.
func NonInteractive(in io.Reader) bool {
	if _, ci := os.LookupEnv("CI"); ci {
		return true
	}
	f, isFile := in.(*os.File)
	if !isFile {
		return false
	}
	return !term.IsTerminal(int(f.Fd()))
}

// AssumeYes reads the --yes persistent flag from the command's root.
func AssumeYes(cmd *cobra.Command) bool {
	yes, _ := cmd.Root().PersistentFlags().GetBool("yes")
	return yes
}

// ConfirmYesNo is the low-blast-radius gate: a single record deletion, an
// overwrite. It returns true to proceed.
//
// A non-interactive session proceeds without prompting — the prompt cannot be
// answered there and the action is recoverable. The high-blast-radius gate,
// ConfirmTyped, refuses instead.
//
// Prompts go to out, which should be cmd.ErrOrStderr(), so they never pollute
// the -o json|yaml data contract on stdout.
func ConfirmYesNo(in io.Reader, out io.Writer, prompt string, defaultYes bool) (bool, error) {
	if NonInteractive(in) {
		return true, nil
	}

	suffix := "[y/N]"
	if defaultYes {
		suffix = "[Y/n]"
	}
	if _, err := fmt.Fprintf(out, "%s %s: ", prompt, suffix); err != nil {
		return false, fmt.Errorf("writing prompt: %w", err)
	}

	line, err := readLine(in)
	if err != nil {
		return false, err
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		return defaultYes, nil
	}
	return isAffirmative(answer), nil
}

// readLine reads one line from in without buffering past the newline.
//
// bufio.Reader cannot be used here. It reads ahead by design, so a reader
// constructed per prompt swallows whatever follows the answer it consumed and
// discards it when the reader goes out of scope. Two prompts in one command —
// the entitlement pre-flight followed by a delete confirmation — would then see
// the first answer and nothing at all, and the second prompt would silently
// take its default. Reading a byte at a time is slower and entirely irrelevant
// at the scale of a human typing one word.
func readLine(in io.Reader) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return b.String(), nil
			}
			b.WriteByte(buf[0])
		}
		if err != nil {
			if err == io.EOF {
				return b.String(), nil
			}
			return b.String(), fmt.Errorf("reading confirmation: %w", err)
		}
	}
}

// isAffirmative reports whether a typed answer means yes. Anything else,
// including an unrecognised word, means no.
func isAffirmative(answer string) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// ConfirmTyped is the high-blast-radius gate, used by zone deletion: the user
// must type want exactly. A non-interactive session refuses, because nobody can
// type it there — the caller is expected to have honoured --yes before calling.
// The friction is intentional for the most destructive action.
func ConfirmTyped(in io.Reader, out io.Writer, prompt, want string) (bool, error) {
	if NonInteractive(in) {
		return false, NewCLIError(ExitAborted,
			"refusing to perform a destructive action non-interactively without confirmation").
			WithFix(fmt.Sprintf("re-run with --yes to confirm %q.", want))
	}

	if prompt != "" {
		if _, err := fmt.Fprintln(out, prompt); err != nil {
			return false, fmt.Errorf("writing prompt: %w", err)
		}
	}
	if _, err := fmt.Fprintf(out, "Type %q to confirm: ", want); err != nil {
		return false, fmt.Errorf("writing prompt: %w", err)
	}

	line, err := readLine(in)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(line) != want {
		return false, NewCLIError(ExitAborted, "confirmation did not match; aborted")
	}
	return true, nil
}
