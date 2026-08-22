// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"context"
	"fmt"
	"io"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// Polling parameters. They are variables so tests do not spend two minutes
// discovering that a fake client never changes its mind.
var (
	defaultWaitTimeout = 2 * time.Minute
	pollInterval       = 2 * time.Second
)

// waitForProgrammed blocks until the owner name reports Programmed, or the
// backend reports why it will not.
//
// DNS programming is asynchronous: the write returns as soon as the object is
// accepted, long before PowerDNS has the record. A command that claims success
// there is a command the user stops believing the first time `dig` disagrees
// with it.
func waitForProgrammed(
	ctx context.Context,
	c client.Client,
	zone *dnsv1alpha1.DNSZone,
	t dnsv1alpha1.RRType,
	ownerName string,
	out io.Writer,
	timeout time.Duration,
) error {
	if timeout <= 0 {
		timeout = defaultWaitTimeout
	}
	deadline := time.Now().Add(timeout)
	who := ownerDisplay(ownerName, zone.Spec.DomainName)

	_, _ = fmt.Fprintf(out, "  waiting for %s %s to be programmed...\n", who, t)

	var lastWord, lastDetail string
	for {
		set, err := findSet(ctx, c, zone, t, ownerName)
		if err != nil {
			return err
		}

		if set != nil {
			word, detail := util.RecordStatusInZone(set, ownerName, zone.Spec.DomainName)
			lastWord, lastDetail = word, detail

			switch word {
			case util.StatusProgrammed:
				_, _ = fmt.Fprintf(out, "  %s %s  %s\n", who, t, word)
				return nil
			case util.StatusConflict, util.StatusNotOwner:
				_, _ = fmt.Fprintf(out, "  %s %s  %s\n", who, t, word)
				return util.NewCLIError(util.ExitConflict, detail).
					WithFix(fmt.Sprintf("inspect it with: datumctl dns record describe %s %s %s",
						zone.Spec.DomainName, displayName(ownerName), t))
			case util.StatusError, util.StatusRejected:
				_, _ = fmt.Fprintf(out, "  %s %s  %s\n", who, t, word)
				return util.NewCLIError(util.ExitError, detail).
					WithFix(fmt.Sprintf("inspect it with: datumctl dns record describe %s %s %s",
						zone.Spec.DomainName, displayName(ownerName), t))
			}
		}

		if !time.Now().Before(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return util.NewCLIError(util.ExitError, "cancelled while waiting for the record to be programmed").
				WithCause(ctx.Err())
		case <-time.After(pollInterval):
		}
	}

	msg := fmt.Sprintf("timed out after %s waiting for %s %s to be programmed", timeout, who, t)
	if lastWord != "" {
		msg = fmt.Sprintf("%s — last status was %s", msg, lastWord)
	}
	fix := fmt.Sprintf("the record was written; check on it with: datumctl dns record describe %s %s %s",
		zone.Spec.DomainName, displayName(ownerName), t)
	if lastDetail != "" {
		fix = lastDetail + "\n       " + fix
	}
	return util.NewCLIError(util.ExitError, msg).WithFix(fix)
}
