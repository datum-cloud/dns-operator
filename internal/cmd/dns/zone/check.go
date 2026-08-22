// SPDX-License-Identifier: AGPL-3.0-only

package zone

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/miekg/dns"

	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// defaultCheckTimeout bounds each live query. --check must never be the reason
// a command hangs.
const defaultCheckTimeout = 5 * time.Second

// Probe outcomes, in the words the report prints.
const (
	probeAuthoritative = "authoritative"
	probeAnswered      = "answered, not authoritative"
	probeNoRecords     = "no NS records"
	probeRefused       = "refused"
	probeFailed        = "server failure"
	probeNoSuchZone    = "does not serve this zone"
	probeUnreachable   = "unreachable"
)

// liveVerdict is what the live queries establish about the registrar's
// delegation, independent of anything the control plane believes.
type liveVerdict int

const (
	// liveInconclusive means the queries did not settle the question.
	liveInconclusive liveVerdict = iota
	// liveDelegated means public DNS returns the assigned nameservers.
	liveDelegated
	// liveNotDelegated means public DNS answered, and the answer is not ours.
	// This is direct evidence about the registrar that the control plane may
	// not have, and it is enough on its own to earn the instruction block.
	liveNotDelegated
)

// liveResult is the outcome of the live check, returned so the caller can act
// on it. A check that finds the problem and does not surface the remedy has
// told the user half of what they came for.
type liveResult struct {
	// verdict is what the public lookup established.
	verdict liveVerdict
	// public is the nameserver set public DNS returned, in its own spelling.
	public []string
}

// nsProbe is the result of asking one nameserver whether it serves the zone.
type nsProbe struct {
	// Server is the nameserver that was queried.
	Server string
	// State is one of the probe* constants.
	State string
	// Detail explains the state in a few words, empty when there is nothing to
	// add.
	Detail string
}

// The live network calls are variables so tests exercise the report without
// touching a resolver.
var (
	probeNameserver = liveProbeNameserver
	lookupPublicNS  = liveLookupPublicNS
)

// printLiveCheck reports what the network says, as opposed to what the control
// plane believes.
//
// The two questions are genuinely different: the assigned nameservers can be
// serving the zone perfectly while the registrar still points somewhere else,
// and the control plane cannot see the difference.
func printLiveCheck(ctx context.Context, out io.Writer, domain string, d util.Delegation, timeout time.Duration) liveResult {
	if timeout <= 0 {
		timeout = defaultCheckTimeout
	}

	_, _ = fmt.Fprintf(out, "Live check\n")
	if len(d.Expected) == 0 {
		_, _ = fmt.Fprintf(out, "  no nameservers assigned yet — nothing to query\n")
		return liveResult{}
	}

	tw := util.NewTabWriter(out)
	for _, ns := range d.Expected {
		p := probeNameserver(ctx, ns, domain, timeout)
		line := p.State
		if p.Detail != "" {
			line += " — " + p.Detail
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\n", ns, line)
	}
	_ = tw.Flush()

	public, source, err := lookupPublicNS(ctx, domain, timeout)
	_, _ = fmt.Fprintf(out, "\nPublic delegation (%s)\n", source)
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(out, "  could not resolve NS for %s — %v\n", domain, err)
		return liveResult{}
	case len(public) == 0:
		// No delegation at all is not the same as a delegation pointing
		// elsewhere: the registrar may simply not have been configured yet,
		// which is still something the user has to act on.
		_, _ = fmt.Fprintf(out, "  %s has no NS records in public DNS yet\n", domain)
		return liveResult{verdict: liveNotDelegated}
	}

	sort.Strings(public)
	for _, ns := range public {
		_, _ = fmt.Fprintf(out, "  %s\n", ns)
	}
	if publicMatchesExpected(public, d.Expected) {
		_, _ = fmt.Fprintf(out, "\n  Public DNS delegates %s to its assigned nameservers.\n", domain)
		return liveResult{verdict: liveDelegated, public: public}
	}
	_, _ = fmt.Fprintf(out, "\n  Public DNS does not yet delegate %s to its assigned nameservers.\n", domain)
	_, _ = fmt.Fprintf(out, "  Registrar changes can take up to 48 hours to propagate.\n")
	return liveResult{verdict: liveNotDelegated, public: public}
}

// publicMatchesExpected reports whether every assigned nameserver appears in
// what public DNS returns. Extra nameservers at the registrar are not a
// mismatch — plenty of zones are served by more than one provider during a
// migration.
func publicMatchesExpected(public, expected []string) bool {
	if len(expected) == 0 {
		return false
	}
	have := make(map[string]bool, len(public))
	for _, ns := range public {
		have[strings.TrimSuffix(strings.ToLower(ns), ".")] = true
	}
	for _, ns := range expected {
		if !have[strings.TrimSuffix(strings.ToLower(ns), ".")] {
			return false
		}
	}
	return true
}

// exchange sends one query and retries over TCP when the answer comes back
// truncated. A zone with several nameservers can overflow a UDP response, and
// counting the records in a truncated answer undercounts the delegation.
func exchange(ctx context.Context, msg *dns.Msg, addr string, timeout time.Duration) (*dns.Msg, error) {
	udp := &dns.Client{Timeout: timeout}
	resp, _, err := udp.ExchangeContext(ctx, msg, addr)
	if err != nil {
		return nil, err
	}
	if !resp.Truncated {
		return resp, nil
	}
	tcp := &dns.Client{Timeout: timeout, Net: "tcp"}
	retried, _, err := tcp.ExchangeContext(ctx, msg, addr)
	if err != nil {
		// The truncated answer is still better than nothing.
		return resp, nil
	}
	return retried, nil
}

// nsRecordsFor pulls the NS records for one owner name out of a response,
// looking in both the answer and the authority section: a parent nameserver
// returns the delegation as authority, not as an answer.
func nsRecordsFor(resp *dns.Msg, domain string) []string {
	var out []string
	want := dns.CanonicalName(dns.Fqdn(domain))
	for _, rr := range append(append([]dns.RR{}, resp.Answer...), resp.Ns...) {
		ns, isNS := rr.(*dns.NS)
		if !isNS || dns.CanonicalName(ns.Hdr.Name) != want {
			continue
		}
		out = append(out, ns.Ns)
	}
	return out
}

// liveProbeNameserver asks one nameserver directly whether it is authoritative
// for the zone.
//
// The query goes to the server itself with recursion disabled, because the
// question is "does this server serve this zone", and a recursive answer from
// somewhere else would answer a different one. The AA bit is the whole point of
// the check.
func liveProbeNameserver(ctx context.Context, server, domain string, timeout time.Duration) nsProbe {
	host := strings.TrimSuffix(strings.TrimSpace(server), ".")
	p := nsProbe{Server: server}

	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(domain), dns.TypeNS)
	msg.RecursionDesired = false

	resp, err := exchange(ctx, msg, net.JoinHostPort(host, "53"), timeout)
	if err != nil {
		p.State = probeUnreachable
		p.Detail = err.Error()
		return p
	}

	// A refusal or a failure is not "this zone has no NS records" — it is the
	// server declining or breaking, and telling the two apart is the
	// difference between "your zone is empty" and "your zone is not loaded".
	switch resp.Rcode {
	case dns.RcodeSuccess:
	case dns.RcodeRefused:
		p.State = probeRefused
		p.Detail = "the server refused the query"
		return p
	case dns.RcodeNameError:
		p.State = probeNoSuchZone
		p.Detail = "the server answered NXDOMAIN for " + domain
		return p
	case dns.RcodeServerFailure:
		p.State = probeFailed
		p.Detail = "the server returned SERVFAIL"
		return p
	default:
		p.State = probeFailed
		p.Detail = "the server returned " + dns.RcodeToString[resp.Rcode]
		return p
	}

	answers := len(nsRecordsFor(resp, domain))
	switch {
	case answers == 0:
		p.State = probeNoRecords
		p.Detail = fmt.Sprintf("%s answered without NS records", host)
	case resp.Authoritative:
		p.State = probeAuthoritative
		p.Detail = pluralize(answers, "NS record", "NS records") + " for " + domain
	default:
		p.State = probeAnswered
		p.Detail = "the server did not claim authority for this zone"
	}
	return p
}

// liveLookupPublicNS establishes what the public DNS delegates the zone to, and
// reports where the answer came from.
//
// The parent's nameservers are asked first, because the delegation lives at the
// parent and nowhere else. Asking a recursive resolver instead answers a
// subtly different question: once the resolver has the zone cached it returns
// the zone's OWN apex NS records, which are the ones Datum publishes — so a
// domain still delegated to the old provider can look correctly delegated.
// That is exactly the migration case this command exists for, so the recursive
// answer is the fallback, not the default, and the source is always named.
func liveLookupPublicNS(ctx context.Context, domain string, timeout time.Duration) ([]string, string, error) {
	if parent := parentZone(domain); parent != "" {
		if ns, err := delegationFromParent(ctx, domain, parent, timeout); err == nil {
			return ns, "as delegated by the " + parent + " nameservers", nil
		}
	}

	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var r net.Resolver
	found, err := r.LookupNS(lookupCtx, domain)
	if err != nil {
		// "no such host" is an answer, not a failure: the domain has no
		// delegation yet.
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return nil, resolverSource, nil
		}
		return nil, resolverSource, err
	}

	out := make([]string, 0, len(found))
	for _, ns := range found {
		out = append(out, ns.Host)
	}
	return out, resolverSource, nil
}

// resolverSource names the weaker answer, so a reader knows the difference.
const resolverSource = "as your resolver sees it, which may be the zone's own NS records"

// parentZone returns the domain one label up, empty for a single label.
func parentZone(domain string) string {
	d := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if i := strings.Index(d, "."); i >= 0 && i+1 < len(d) {
		return d[i+1:]
	}
	return ""
}

// parentProbeLimit bounds how many of the parent's nameservers are tried before
// giving up and falling back.
const parentProbeLimit = 3

// delegationFromParent asks the parent zone's nameservers what this domain is
// delegated to, with recursion disabled so the answer is the parent's own
// referral rather than somebody's cache.
func delegationFromParent(ctx context.Context, domain, parent string, timeout time.Duration) ([]string, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var r net.Resolver
	parentNS, err := r.LookupNS(lookupCtx, parent)
	if err != nil {
		return nil, err
	}
	if len(parentNS) == 0 {
		return nil, errors.New("the parent zone publishes no nameservers")
	}

	msg := new(dns.Msg)
	msg.SetQuestion(dns.Fqdn(domain), dns.TypeNS)
	msg.RecursionDesired = false

	for i, p := range parentNS {
		if i >= parentProbeLimit {
			break
		}
		addr := net.JoinHostPort(strings.TrimSuffix(p.Host, "."), "53")
		resp, exErr := exchange(ctx, msg, addr, timeout)
		if exErr != nil {
			continue
		}
		// NXDOMAIN from the parent is a real answer: nothing is delegated.
		if resp.Rcode == dns.RcodeNameError {
			return nil, nil
		}
		if resp.Rcode != dns.RcodeSuccess {
			continue
		}
		return nsRecordsFor(resp, domain), nil
	}
	return nil, errors.New("no parent nameserver answered")
}
