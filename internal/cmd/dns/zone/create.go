// SPDX-License-Identifier: AGPL-3.0-only

package zone

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// Waiting for nameserver assignment. The interval is a variable so tests do not
// pay for it.
var (
	waitInterval    = 2 * time.Second
	defaultWaitTime = 2 * time.Minute
)

func createCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <domain>",
		Short: "Create a DNS zone",
		Long: `Create a DNS zone for a domain.

The command waits for Datum to assign nameservers, because a zone is not usable
until it has them and you cannot delegate the domain without knowing what they
are. Pass --no-wait to return immediately.

A zone's domain name is immutable: there is no ` + "`zone update`" + `, and changing the
domain means creating a new zone.`,
		Example: `  # Create a zone and print the nameservers to set at the registrar
  datumctl dns zone create example.com

  # Validate against the API server without creating anything
  datumctl dns zone create example.com --dry-run

  # Non-blocking, with a description
  datumctl dns zone create example.com --no-wait --description "production apex"`,
		Args: cobra.ExactArgs(1),
		RunE: runCreate,
	}

	cmd.Flags().String("description", "", "Human-readable description, stored as the kubernetes.io/description annotation")
	cmd.Flags().String("class", DefaultZoneClass, "DNSZoneClass to provision the zone with")
	cmd.Flags().Bool("wait", true, "Wait for nameservers to be assigned")
	cmd.Flags().Bool("no-wait", false, "Return as soon as the zone is created, without waiting for nameservers")
	cmd.Flags().Duration("timeout", defaultWaitTime, "How long to wait for nameservers")
	cmd.Flags().Bool("dry-run", false, "Submit the zone for server-side validation without creating it")

	_ = cmd.RegisterFlagCompletionFunc("class", completeZoneClasses)

	return cmd
}

func runCreate(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	project := util.ProjectFromCmd(cmd)

	class, _ := cmd.Flags().GetString("class")
	desc, _ := cmd.Flags().GetString("description")
	waitFlag, _ := cmd.Flags().GetBool("wait")
	noWait, _ := cmd.Flags().GetBool("no-wait")
	timeout, _ := cmd.Flags().GetDuration("timeout")
	dryRun, _ := cmd.Flags().GetBool("dry-run")

	// Input is lowercased rather than rejected: spec.domainName is
	// lowercase-only at admission, and "Example.com" is what a registrar page
	// shows.
	domain := normalizeDomain(args[0])
	if err := validateDomain(domain, args[0]); err != nil {
		return err
	}
	// 0 previously meant "the default", silently. A caller who writes
	// --timeout 0 means something by it, and neither reading is safe to guess.
	if waitFlag && !noWait && timeout <= 0 {
		return util.UsageErrorf("--timeout must be greater than zero").
			WithFix("pass a duration such as --timeout 5m, or --no-wait to return immediately.")
	}
	if class == "" {
		return util.UsageErrorf("--class cannot be empty").
			WithFix(fmt.Sprintf("omit the flag to use the default class %q.", DefaultZoneClass))
	}

	c, err := clientFactory(project)
	if err != nil {
		return err
	}

	if err := checkDomainFree(ctx, c, project, domain); err != nil {
		return err
	}
	if err := checkClassExists(ctx, c, class); err != nil {
		return err
	}

	z := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			// The domain itself is not a legal object name — dots and length
			// both bite — so the name is derived, exactly as the portal
			// derives it, and the domain lives in the spec.
			Name:      zoneObjectName(domain),
			Namespace: util.ResourceNamespace,
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       domain,
			DNSZoneClassName: class,
		},
	}
	if desc != "" {
		z.Annotations = map[string]string{descriptionAnnotation: desc}
	}

	var opts []client.CreateOption
	if dryRun {
		opts = append(opts, client.DryRunAll)
	}
	if err := c.Create(ctx, z, opts...); err != nil {
		return createError(err, domain, class)
	}

	out := cmd.OutOrStdout()
	if dryRun {
		_, _ = fmt.Fprintf(out, "zone/%s validated — dry run, nothing was created\n", domain)
		_, _ = fmt.Fprintf(out, "  class:  %s\n", class)
		_, _ = fmt.Fprintf(out, "  object: %s\n", z.Name)
		return nil
	}

	_, _ = fmt.Fprintf(out, "zone/%s created\n", domain)

	if noWait || !waitFlag {
		_, _ = fmt.Fprintf(out, "\nNext steps:\n")
		_, _ = fmt.Fprintf(out, "  Get the nameservers:  datumctl dns zone nameservers %s\n", domain)
		return nil
	}

	assigned, err := waitForNameservers(ctx, c, z.Name, domain, timeout, cmd.ErrOrStderr())
	if err != nil {
		return err
	}

	printAssignedNameservers(out, domain, assigned)
	return nil
}

// checkDomainFree fails early when the project already has a zone for this
// domain. The generated object name would not collide, so the API server would
// happily create a second zone that then loses the race for the domain and
// parks on Accepted=False/DNSZoneInUse forever.
func checkDomainFree(ctx context.Context, c client.Client, project, domain string) error {
	var list dnsv1alpha1.DNSZoneList
	if err := c.List(ctx, &list, client.InNamespace(util.ResourceNamespace)); err != nil {
		return util.ClassifyError(fmt.Errorf("listing zones: %w", err))
	}
	for i := range list.Items {
		if normalizeDomain(list.Items[i].Spec.DomainName) != domain {
			continue
		}
		return util.NewCLIError(util.ExitConflict,
			fmt.Sprintf("project %s already has a zone for %q", project, domain)).
			WithFix(fmt.Sprintf("inspect the existing zone:\n       datumctl dns zone describe %s", domain))
	}
	return nil
}

// checkClassExists turns a mistyped --class into an error at submit time.
//
// Without it the zone is created and then parks on Accepted=False, because a
// missing class is a reconcile failure rather than an admission failure. A
// caller who cannot list classes is not blocked: the check is an improvement on
// the error, not a permission requirement.
func checkClassExists(ctx context.Context, c client.Client, class string) error {
	var list dnsv1alpha1.DNSZoneClassList
	if err := c.List(ctx, &list); err != nil {
		return nil
	}
	if len(list.Items) == 0 {
		return nil
	}

	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Name == class {
			return nil
		}
		names = append(names, list.Items[i].Name)
	}
	sort.Strings(names)

	return util.NewCLIError(util.ExitNotFound, fmt.Sprintf("zone class %q not found", class)).
		WithFix("available classes: " + strings.Join(names, ", "))
}

// createError maps a rejected create into the plugin's error vocabulary.
func createError(err error, domain, class string) error {
	ce := util.ClassifyError(fmt.Errorf("creating zone: %w", err))
	switch ce.Code() {
	case util.ExitConflict:
		return util.NewCLIError(util.ExitConflict,
			fmt.Sprintf("the domain %q is already claimed", domain)).
			WithFix("a domain can belong to one zone at a time, and the claim does not\n" +
				"       clear itself — delete the zone that holds it, then create this one.").
			WithCause(err)
	case util.ExitNotFound:
		return util.NewCLIError(util.ExitNotFound, fmt.Sprintf("zone class %q not found", class)).
			WithFix("list the available classes:\n       datumctl get dnszoneclasses").
			WithCause(err)
	default:
		return ce
	}
}

// pollFailureTolerance is how many consecutive Get failures the wait absorbs
// before giving up. A two-minute poll should not be killed by one blip.
const pollFailureTolerance = 3

// waitForNameservers polls until the zone reports its assigned nameservers.
//
// A zone without nameservers cannot be delegated, so returning before they
// exist hands the user a command they have to run again to learn anything.
//
// Every failure exit from here shares one fact: the zone was created. The wait
// happens strictly after a successful Create, so no error from this function
// may suggest re-running the command — that advice fails with "already
// claimed" and leaves the user believing the zone does not exist.
func waitForNameservers(ctx context.Context, c client.Client, name, domain string, timeout time.Duration, progress io.Writer) ([]string, error) {
	if timeout <= 0 {
		timeout = defaultWaitTime
	}
	_, _ = fmt.Fprintf(progress, "Waiting for nameservers to be assigned...\n")

	var assigned []string
	var rejected error
	var lastErr error
	consecutive := 0

	key := types.NamespacedName{Namespace: util.ResourceNamespace, Name: name}
	err := wait.PollUntilContextTimeout(ctx, waitInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			var z dnsv1alpha1.DNSZone
			if err := c.Get(ctx, key, &z); err != nil {
				// A read failure mid-poll is usually transient — a 500, a
				// dropped connection, or replication lag on an object created
				// moments ago. Treating the first one as terminal throws away
				// the remaining timeout for no reason.
				lastErr = err
				consecutive++
				if consecutive >= pollFailureTolerance {
					return false, err
				}
				return false, nil
			}
			consecutive = 0

			// An admission-rejected zone will never get nameservers; waiting
			// out the timeout would hide the reason it failed.
			if accepted := apimeta.FindStatusCondition(z.Status.Conditions, util.CondAccepted); accepted != nil &&
				accepted.Status == metav1.ConditionFalse {
				rejected = util.NewCLIError(util.ExitInvalid,
					fmt.Sprintf("the zone was rejected: %s", accepted.Message))
				return false, rejected
			}

			if len(z.Status.Nameservers) > 0 {
				assigned = z.Status.Nameservers
				return true, nil
			}
			return false, nil
		})

	// The zone exists in every one of these branches, so each fix says what to
	// do with the object that is now sitting in the project.
	checkOnIt := "the zone was created — check on it with:\n       datumctl dns zone describe " + domain

	switch {
	case rejected != nil:
		// DNSZoneInUse and its siblings are terminal: the object was created,
		// is parked on Accepted=False, and will stay there. The user now owns
		// a dead object and needs to be told how to be rid of it.
		return nil, util.NewCLIError(util.ExitInvalid, rejected.Error()).
			WithFix("the zone object was created and will stay in this state — remove it with:\n" +
				"       datumctl dns zone delete " + domain).
			WithCause(rejected)
	case wait.Interrupted(err):
		return nil, util.NewCLIError(util.ExitError,
			fmt.Sprintf("timed out after %s waiting for nameservers to be assigned", timeout)).
			WithFix(checkOnIt)
	case err != nil:
		ce := util.ClassifyError(fmt.Errorf("waiting for nameservers: %w", err))
		return nil, util.NewCLIError(ce.Code(), ce.Error()).WithFix(checkOnIt).WithCause(firstNonNil(lastErr, err))
	}
	return assigned, nil
}

func firstNonNil(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// printAssignedNameservers closes the create with the one thing the user has to
// do next: point the registrar at these hostnames.
func printAssignedNameservers(out io.Writer, domain string, nameservers []string) {
	_, _ = fmt.Fprintf(out, "\nSet these nameservers at your domain registrar:\n")
	for _, ns := range nameservers {
		_, _ = fmt.Fprintf(out, "  %s\n", ns)
	}
	_, _ = fmt.Fprintf(out, "\nThe zone will not resolve until the registrar publishes them.\n")
	_, _ = fmt.Fprintf(out, "\nNext steps:\n")
	_, _ = fmt.Fprintf(out, "  Check delegation:  datumctl dns zone nameservers %s --check\n", domain)
	_, _ = fmt.Fprintf(out, "  Add a record:      datumctl dns record create %s www A 203.0.113.10\n", domain)
}

// completeZoneClasses completes --class from the cluster-scoped class list.
func completeZoneClasses(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	c, err := clientFactory(util.ProjectFromCmd(cmd))
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var list dnsv1alpha1.DNSZoneClassList
	if err := c.List(cmd.Context(), &list); err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}
