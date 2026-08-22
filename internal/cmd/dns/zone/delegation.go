// SPDX-License-Identifier: AGPL-3.0-only

package zone

import (
	"fmt"
	"io"

	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// Per-nameserver annotations in the describe and nameservers views.
const (
	nsSetAtRegistrar    = "set at registrar"
	nsNotSetAtRegistrar = "not set at registrar"
	nsRegistrarUnknown  = "unknown"
)

// delegationSummary is the one-line "<State> — <explanation>" a describe view
// puts next to the Delegation label.
func delegationSummary(d util.Delegation) string {
	switch d.State {
	case util.DelegationComplete:
		return fmt.Sprintf("%s — all %s set at the registrar",
			d.State, pluralize(d.Total, "nameserver", "nameservers"))
	case util.DelegationPartial, util.DelegationIncomplete:
		return fmt.Sprintf("%s — %d of %d nameservers set at the registrar",
			d.State, d.SetCount, d.Total)
	default:
		if d.Total == 0 {
			return fmt.Sprintf("%s — no nameservers assigned yet", d.State)
		}
		// Two different reasons the state is Unknown, and the difference is
		// the whole point: no Domain object to check against at all, versus a
		// Domain whose nameservers nobody has looked at yet. Neither is
		// evidence about the registrar, so neither sentence may claim any.
		if d.Linked {
			return fmt.Sprintf("%s — the registrar's nameservers have not been checked yet", d.State)
		}
		return fmt.Sprintf("%s — no linked domain to check the registrar against", d.State)
	}
}

// nameserverAnnotation labels one assigned nameserver with whether the
// registrar publishes it.
//
// The guard is the computed state rather than the presence of a Domain object,
// because a linked Domain with nothing observed yet is equally uninformative: a
// nameserver absent from an empty observation list has not been shown to be
// missing from the registrar, it has not been looked for. Labelling it "not set
// at registrar" states a fact about a third party's configuration on no
// evidence at all.
func nameserverAnnotation(d util.Delegation, nameserver string) string {
	if d.State == util.DelegationUnknown {
		return nsRegistrarUnknown
	}
	if d.IsSet(nameserver) {
		return nsSetAtRegistrar
	}
	return nsNotSetAtRegistrar
}

// delegationNeedsAction reports whether the user has something to do at their
// registrar: we compared the two lists and they do not match.
//
// Unknown is deliberately excluded. It means the comparison never happened —
// no nameservers assigned, no linked Domain, or a Domain nobody has observed
// yet — and the last of those is the ordinary state of a zone in the minutes
// after it is created. Printing "set these nameservers at your registrar" there
// sends a user to fix something that may already be correct.
func delegationNeedsAction(d util.Delegation) bool {
	return d.State == util.DelegationIncomplete || d.State == util.DelegationPartial
}

// printNameserverList writes the assigned nameservers with their registrar
// annotation, indented under a heading the caller has already printed.
func printNameserverList(w io.Writer, d util.Delegation) {
	if len(d.Expected) == 0 {
		_, _ = fmt.Fprintf(w, "  none assigned yet\n")
		return
	}
	tw := util.NewTabWriter(w)
	for _, ns := range d.Expected {
		_, _ = fmt.Fprintf(tw, "  %s\t%s\n", ns, nameserverAnnotation(d, ns))
	}
	_ = tw.Flush()
}

// printDelegationInstructions writes the registrar instruction block.
//
// Callers must gate this on a state that means "we looked": telling someone to
// go change settings at their registrar is only honest once the registrar has
// actually been observed pointing somewhere else.
func printDelegationInstructions(w io.Writer, domain string, d util.Delegation) {
	if len(d.Expected) == 0 {
		return
	}

	_, _ = fmt.Fprintf(w, "Set these nameservers at your domain registrar:\n")
	for _, ns := range d.Expected {
		_, _ = fmt.Fprintf(w, "  %s\n", ns)
	}

	_, _ = fmt.Fprintf(w, "\nCurrently delegated to:\n")
	if len(d.Observed) == 0 {
		_, _ = fmt.Fprintf(w, "  unknown — no registrar nameservers observed for this domain\n")
	} else {
		for _, ns := range d.Observed {
			_, _ = fmt.Fprintf(w, "  %s\n", ns)
		}
	}

	_, _ = fmt.Fprintf(w, "\nRe-check with: datumctl dns zone nameservers %s --check\n", domain)
}
