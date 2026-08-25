// SPDX-License-Identifier: AGPL-3.0-only

// Package zone implements the `datumctl dns zone` command group: the zone
// lifecycle (list, create, describe, nameservers, delete) plus the bulk
// import/export paths.
package zone

import (
	"context"
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

const (
	// DefaultZoneClass is the class the portal always sends. Exposing --class
	// keeps the door open without making every user learn the vocabulary.
	DefaultZoneClass = "datum-external-global-dns"

	// descriptionAnnotation is where a zone's description lives. There is no
	// spec field for it — the portal writes the annotation and so do we.
	descriptionAnnotation = "kubernetes.io/description"

	// zoneRefField is the server-side selectable field on DNSRecordSet that
	// scopes a record-set list to one zone.
	zoneRefField = "spec.dnsZoneRef.name"
)

// clientFactory builds the API client. It is a variable so tests can inject a
// fake client instead of requiring plugin credentials and a network.
var clientFactory = func(project string) (client.Client, error) {
	return util.NewClient(project)
}

// Command returns the `zone` command group. Running the group with no
// subcommand lists, matching the muscle memory `datumctl compute workloads`
// builds.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "zone",
		Aliases: []string{"zones", "z"},
		Short:   "Manage DNS zones",
		Long: `Create, inspect, and delete DNS zones.

Running the group with no subcommand lists zones, so ` + "`datumctl dns zone`" + ` and
` + "`datumctl dns zone list`" + ` are the same command.`,
		Example: `  # List every zone in the project
  datumctl dns zone

  # Create a zone and wait for its nameservers
  datumctl dns zone create example.com

  # Show delegation state
  datumctl dns zone describe example.com`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return nil
			}
			return unknownSubcommandError(cmd, args[0])
		},
		RunE: runList,
	}

	// The bare group is `list`, so it carries the same flags.
	addListFlags(cmd)

	cmd.AddCommand(
		listCommand(),
		createCommand(),
		describeCommand(),
		nameserversCommand(),
		deleteCommand(),
		importCommand(),
		exportCommand(),
	)

	return cmd
}

// unknownSubcommandError rejects an unrecognized subcommand as a usage failure
// rather than letting the group silently list, which would hide the typo.
func unknownSubcommandError(cmd *cobra.Command, name string) *util.CLIError {
	msg := fmt.Sprintf("unknown command %q for %q", name, cmd.CommandPath())
	if suggestions := cmd.SuggestionsFor(name); len(suggestions) > 0 {
		msg += "\n\nDid you mean this?\n\t" + strings.Join(suggestions, "\n\t")
	}
	return util.NewCLIError(util.ExitUsage, msg)
}

// domainNamePattern mirrors the CRD's pattern on spec.domainName. Checking it
// client-side turns an admission rejection into a message that names the
// offending input.
var domainNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// normalizeDomain lowercases a domain and strips the root dot. Users paste
// "Example.com." from a registrar page; spec.domainName is lowercase-only and
// dot-free at admission.
func normalizeDomain(domain string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}

// validateDomain checks a domain against the CRD's own rules before the round
// trip, so a typo comes back as a usage error with the input quoted.
// validateDomain checks a domain against the CRD's own rules before the round
// trip, so a typo comes back as a usage error.
//
// raw is what the user typed; every message quotes that rather than the
// normalized form, because being told that "example.com." is invalid when you
// wrote "example.com.." sends you looking in the wrong place.
func validateDomain(domain, raw string) error {
	if raw == "" {
		raw = domain
	}
	switch {
	case domain == "":
		return util.UsageErrorf("a domain name is required").
			WithFix("give the zone's domain:\n       datumctl dns zone create example.com")
	case len(domain) > 253:
		return util.UsageErrorf("domain %q is longer than 253 characters", raw)
	case !strings.Contains(domain, "."):
		return util.UsageErrorf("domain %q must have at least two segments separated by dots", raw).
			WithFix("use the registrable domain, for example \"example.com\".")
	case !domainNamePattern.MatchString(domain):
		return util.UsageErrorf("domain %q is not a valid domain name", raw).
			WithFix("use lowercase letters, digits, hyphens, and dots only —\n       labels may not start or end with a hyphen.")
	}

	// The CRD pattern accepts a label of any length, so client and server agree
	// on a name that DNS itself will never serve. Catching it here costs a
	// round trip and saves a zone that can never work.
	for _, label := range strings.Split(domain, ".") {
		if len(label) > 63 {
			return util.UsageErrorf("domain %q has a label longer than 63 characters", raw).
				WithFix("each dot-separated label in a domain name is limited to 63 characters.")
		}
	}
	return nil
}

// getZone resolves the domain a user typed to the DNSZone object that carries
// it. The object name is generated at creation and nobody memorises it, so the
// lookup is by spec.domainName with an object-name fallback for the user who
// pasted one out of `kubectl get`.
func getZone(ctx context.Context, c client.Client, project, domain string) (*dnsv1alpha1.DNSZone, error) {
	want := normalizeDomain(domain)

	var list dnsv1alpha1.DNSZoneList
	if err := c.List(ctx, &list, client.InNamespace(util.ResourceNamespace)); err != nil {
		return nil, fmt.Errorf("listing zones: %w", err)
	}

	for i := range list.Items {
		if normalizeDomain(list.Items[i].Spec.DomainName) == want {
			return &list.Items[i], nil
		}
	}
	for i := range list.Items {
		if list.Items[i].Name == domain {
			return &list.Items[i], nil
		}
	}

	return nil, util.NewCLIError(util.ExitNotFound,
		fmt.Sprintf("zone %q not found in project %s", domain, project)).
		WithFix("list the zones in this project:\n       datumctl dns zone list")
}

// zoneRecordSets lists the record sets belonging to a zone, using the
// server-side field selector the CRD declares.
func zoneRecordSets(ctx context.Context, c client.Client, z *dnsv1alpha1.DNSZone) ([]dnsv1alpha1.DNSRecordSet, error) {
	var list dnsv1alpha1.DNSRecordSetList
	err := c.List(ctx, &list,
		client.InNamespace(util.ResourceNamespace),
		client.MatchingFields{zoneRefField: z.Name},
	)
	if err != nil {
		return nil, fmt.Errorf("listing record sets: %w", err)
	}
	return list.Items, nil
}

// zoneDisplayName is the name a user typed and expects to see: the domain,
// falling back to the object name for a zone whose spec is somehow empty.
func zoneDisplayName(z *dnsv1alpha1.DNSZone) string {
	if z.Spec.DomainName != "" {
		return z.Spec.DomainName
	}
	return z.Name
}

// listFailureReason renders why a record-set listing failed, as a short
// lowercase phrase that reads inside a sentence. It exists so a view that
// cannot count records can say why instead of reporting zero.
func listFailureReason(err error) string {
	ce := util.ClassifyError(err)
	if ce == nil {
		return "the reason is not known"
	}
	switch ce.Code() {
	case util.ExitForbidden:
		return "you are not authorized to list record sets in this project"
	case util.ExitUnavailable:
		return "the DNS API could not be reached"
	default:
		return strings.TrimSpace(ce.Error())
	}
}

// countRecordEntries sums the record entries across a zone's record sets.
//
// This is deliberately the same arithmetic the operator uses for
// status.recordCount: entries, not DNSRecordSet objects. One set holds every
// record of one type for the whole zone, so counting objects would report "3"
// for a zone with thirty records.
func countRecordEntries(sets []dnsv1alpha1.DNSRecordSet) int {
	total := 0
	for i := range sets {
		total += len(sets[i].Spec.Records)
	}
	return total
}

// pluralize renders "1 record" / "N records" style counts.
func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// description returns a zone's description annotation, empty when unset.
func description(z *dnsv1alpha1.DNSZone) string {
	return z.Annotations[descriptionAnnotation]
}

// objectNameMaxBase is how much of the domain survives into the generated
// object name, leaving room for the "-" and the six-character random suffix
// within the portal's 30-character budget.
const objectNameMaxBase = 23

// suffixAlphabet is the portal's: lowercase letters and digits, so the result
// is a valid DNS-1123 label wherever it lands.
const suffixAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// randomSuffix is a variable so tests can make generated names deterministic.
var randomSuffix = func() string { return randomString(6) }

// zoneObjectName derives the Kubernetes object name from the domain, matching
// the portal's scheme: kebab-case the domain, truncate, and append a random
// suffix.
//
// The raw domain cannot be the object name — dots are legal in a DNS name and
// illegal in most Kubernetes name positions — and a deterministic name would
// make "delete and recreate" race against the finalizer of the object that is
// still going away.
func zoneObjectName(domain string) string {
	base := kebabCase(domain)
	if len(base) > objectNameMaxBase {
		base = strings.Trim(base[:objectNameMaxBase], "-")
	}
	if base == "" {
		base = "zone"
	}
	return base + "-" + randomSuffix()
}

// nonNameChars matches everything a kebab-cased object name may not contain.
var nonNameChars = regexp.MustCompile(`[^a-z0-9-]+`)

// repeatedDashes collapses runs of hyphens left behind by substitution.
var repeatedDashes = regexp.MustCompile(`-+`)

// kebabCase lowercases s and reduces it to the characters a DNS-1123 name
// allows, turning dots and underscores into hyphens.
func kebabCase(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer(".", "-", "_", "-", " ", "-").Replace(s)
	s = nonNameChars.ReplaceAllString(s, "")
	s = repeatedDashes.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// randomString returns n characters from suffixAlphabet.
func randomString(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand does not fail in practice; a fixed suffix still yields a
		// valid name, and the API server rejects a genuine collision.
		return strings.Repeat("x", n)
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = suffixAlphabet[int(b)%len(suffixAlphabet)]
	}
	return string(out)
}
