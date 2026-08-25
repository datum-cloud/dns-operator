// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"go.datum.net/datumctl/plugin"
	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
)

const (
	resourceManagerGroup   = "resourcemanager.miloapis.com"
	resourceManagerVersion = "v1alpha1"

	// ResourceNamespace is the namespace used for all resource operations within
	// a project's virtual control plane. The project slug routes to the right
	// control plane; within it, everything lives in "default".
	ResourceNamespace = "default"
)

// ProjectControlPlaneURL returns the virtual control-plane URL for a project.
func ProjectControlPlaneURL(apiHost, projectID string) string {
	return fmt.Sprintf("https://%s/apis/%s/%s/projects/%s/control-plane",
		apiHost, resourceManagerGroup, resourceManagerVersion, projectID)
}

// NewClient builds a Kubernetes client targeting the project's virtual control
// plane. The bearer token is fetched fresh on every call because plugin tokens
// are short-lived.
func NewClient(project string) (client.Client, error) {
	if project == "" {
		return nil, NewCLIError(ExitUsage, "no project set").
			WithFix("pass --project, or set a default with:\n       datumctl config set project <name>")
	}

	ctx := plugin.Context()
	if ctx.APIHost == "" {
		return nil, NewCLIError(ExitUnavailable, "DATUM_API_HOST is not set").
			WithFix("run this through datumctl:\n       datumctl dns ...")
	}

	token, err := plugin.Token()
	if err != nil {
		return nil, NewCLIError(ExitUnavailable, fmt.Sprintf("getting credentials: %v", err)).
			WithFix("re-run `datumctl login` and try again.").
			WithCause(err)
	}

	scheme, err := NewScheme()
	if err != nil {
		return nil, err
	}

	cfg := &rest.Config{
		Host:            ProjectControlPlaneURL(ctx.APIHost, project),
		BearerToken:     token,
		UserAgent:       UserAgent(),
		TLSClientConfig: tlsClientConfig(),
		Timeout:         RequestTimeout,
	}

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, NewCLIError(ExitUnavailable, fmt.Sprintf("building API client: %v", err)).WithCause(err)
	}
	return c, nil
}

// NewScheme returns a runtime scheme carrying every group the plugin reads: the
// DNS API itself plus network-services for the Domain objects a zone's
// delegation state is read from.
func NewScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := dnsv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering dns scheme: %w", err)
	}
	if err := networkingv1alpha.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("registering networking scheme: %w", err)
	}
	return scheme, nil
}

// CAFileEnv names a PEM bundle to verify the API server's certificate against.
// It exists for API servers whose certificate is not signed by a public root: a
// private or self-hosted deployment, and the end-to-end test harness, which
// proxies a locally generated certificate.
//
// The bundle REPLACES the host's root store for this client; client-go does not
// append to it. So a bundle that omits the public roots will not verify a
// public certificate — which is the safe direction to be wrong in, but is worth
// knowing before setting it.
//
// Verification itself is never weakened. There is no corresponding
// skip-verification knob, deliberately: an env var that silently disables
// certificate checking is a credential-exfiltration footgun, and nothing needs
// one. When the variable is unset, TLS behaves exactly as it did before this
// existed — the host's root store, fully verified.
const CAFileEnv = "DATUM_CA_FILE"

// tlsClientConfig returns the TLS settings for an API client, which are the
// zero value unless DATUM_CA_FILE is set.
func tlsClientConfig() rest.TLSClientConfig {
	return rest.TLSClientConfig{CAFile: os.Getenv(CAFileEnv)}
}

// RequestTimeout bounds a single API request.
//
// Without it a black-holed API server — one that accepts the connection and
// then never answers — hangs the command forever. That is bad in a script and
// worse in shell completion, where it freezes the user's terminal mid-tab with
// no indication of why. It is per request, not per command, so the polling that
// backs --wait is unaffected.
const RequestTimeout = 30 * time.Second

// UserAgent identifies the plugin to the API server.
func UserAgent() string {
	return "datumctl-dns"
}

// ProjectFromCmd reads the --project persistent flag from the command's root.
func ProjectFromCmd(cmd *cobra.Command) string {
	project, _ := cmd.Root().PersistentFlags().GetString("project")
	return project
}

// OrgFromCmd reads the --org persistent flag from the command's root.
func OrgFromCmd(cmd *cobra.Command) string {
	org, _ := cmd.Root().PersistentFlags().GetString("org")
	return org
}
