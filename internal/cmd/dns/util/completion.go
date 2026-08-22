// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

// CompleteZoneNames completes the zone positional from the API, offering the
// domain names users actually type rather than the object names.
//
// Every path returns ShellCompDirectiveNoFileComp, including the error paths, so
// the shell never falls back to filename completion. Completion failures are
// silent: a broken token or an unreachable API must not print an error into the
// middle of the user's command line.
func CompleteZoneNames(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return withCompletionDeadline(func() []string {
		c, err := NewClient(ProjectFromCmd(cmd))
		if err != nil {
			return nil
		}

		var list dnsv1alpha1.DNSZoneList
		if err := c.List(cmd.Context(), &list, client.InNamespace(ResourceNamespace)); err != nil {
			return nil
		}

		names := make([]string, 0, len(list.Items))
		for i := range list.Items {
			z := &list.Items[i]
			if z.Spec.DomainName != "" {
				names = append(names, z.Spec.DomainName)
				continue
			}
			names = append(names, z.Name)
		}
		return names
	})
}

// CompletionTimeout bounds how long any API-backed completion may take.
//
// It is much shorter than RequestTimeout because the cost of waiting is
// different here: a slow command looks slow, but a slow completion looks like a
// hung terminal. The user pressed Tab and the shell stopped responding, with
// nothing on screen to say why or that anything is happening at all. Offering
// no completions quickly is strictly better than offering the right ones after
// the user has given up.
//
// This also covers the part RequestTimeout cannot. NewClient shells out to the
// credentials helper through plugin.Token, which takes no context and cannot be
// interrupted; bounding the whole operation is the only way to put a ceiling on
// it.
const CompletionTimeout = 3 * time.Second

// withCompletionDeadline runs fn and returns its result, or no completions if
// it takes longer than CompletionTimeout.
//
// A timed-out fn keeps running in its goroutine, because the call it is blocked
// in cannot be cancelled. That is acceptable only because this runs in a
// short-lived completion process that is about to exit; it would be a leak
// anywhere else.
func withCompletionDeadline(fn func() []string) ([]string, cobra.ShellCompDirective) {
	done := make(chan []string, 1)
	go func() { done <- fn() }()

	timer := time.NewTimer(CompletionTimeout)
	defer timer.Stop()

	select {
	case names := <-done:
		return names, cobra.ShellCompDirectiveNoFileComp
	case <-timer.C:
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}

// CompleteEnum returns a completion function for a static value set, used for
// enum flags such as --output and --status.
func CompleteEnum(allowed ...string) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return allowed, cobra.ShellCompDirectiveNoFileComp
	}
}
