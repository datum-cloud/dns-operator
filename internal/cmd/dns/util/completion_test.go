// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestWithCompletionDeadlineReturnsResults(t *testing.T) {
	names, directive := withCompletionDeadline(func() []string {
		return []string{"example.com", "acme.io"}
	})

	if len(names) != 2 || names[0] != "example.com" {
		t.Errorf("names = %v, want the two zones", names)
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want ShellCompDirectiveNoFileComp", directive)
	}
}

func TestWithCompletionDeadlineGivesUpQuietly(t *testing.T) {
	// A completion that outlives its deadline must return nothing rather than
	// freeze the user's terminal mid-Tab.
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })

	start := time.Now()
	names, directive := withCompletionDeadline(func() []string {
		<-blocked
		return []string{"too-late.example"}
	})
	elapsed := time.Since(start)

	if names != nil {
		t.Errorf("names = %v, want nil after the deadline", names)
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v; even the give-up path must suppress file completion", directive)
	}
	if elapsed > CompletionTimeout*2 {
		t.Errorf("waited %v, want to give up near %v", elapsed, CompletionTimeout)
	}
}

func TestCompletionTimeoutIsShorterThanARequest(t *testing.T) {
	// The two deadlines exist for different reasons and must not converge: a
	// slow command is tolerable, a frozen Tab is not.
	if CompletionTimeout >= RequestTimeout {
		t.Errorf("CompletionTimeout (%v) must be well under RequestTimeout (%v)",
			CompletionTimeout, RequestTimeout)
	}
}

func TestCompleteEnumSuppressesFileCompletion(t *testing.T) {
	fn := CompleteEnum("table", "json")
	got, directive := fn(&cobra.Command{}, nil, "")

	if len(got) != 2 {
		t.Errorf("values = %v, want both", got)
	}
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Errorf("directive = %v, want ShellCompDirectiveNoFileComp", directive)
	}
}
