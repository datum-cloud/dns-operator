// SPDX-License-Identifier: AGPL-3.0-only

package dns

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// Version is the plugin's version, overwritten from main's ldflags-injected
// value. It is a package variable rather than a Command parameter so that
// Command's signature stays stable for the packages compiling against it.
var Version = "dev"

// versionInfo is the machine-readable form, for `version -o json|yaml`.
type versionInfo struct {
	Version    string `json:"version" yaml:"version"`
	APIGroup   string `json:"apiGroup" yaml:"apiGroup"`
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	PluginAPI  int    `json:"pluginApiVersion" yaml:"pluginApiVersion"`
	GoVersion  string `json:"goVersion" yaml:"goVersion"`
	Platform   string `json:"platform" yaml:"platform"`
}

// versionCommand prints the plugin's version.
//
// It touches nothing: no credentials helper, no API call, no entitlement
// pre-flight, and no requirement that any DATUM_* variable is set. That is the
// point of it. A version check is something you reach for while debugging a
// broken control plane or a broken login, which is exactly when a version
// command that needs either of those is useless. `--plugin-manifest` answers
// the same question but is a machine hook for the host, not something to tell a
// person to run.
func versionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the plugin version",
		Long: "Print the plugin version.\n\n" +
			"Runs entirely offline: no credentials, no API call, and no project or\n" +
			"entitlement required, so it still answers when everything else does not.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		Example: "  datumctl dns version\n" +
			"  datumctl dns version -o json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()

			info := versionInfo{
				Version:    Version,
				APIGroup:   dnsv1alpha1.GroupVersion.Group,
				APIVersion: dnsv1alpha1.GroupVersion.Version,
				PluginAPI:  pluginAPIVersion,
				GoVersion:  runtime.Version(),
				Platform:   runtime.GOOS + "/" + runtime.GOARCH,
			}

			format, err := util.ParseOutputFormat(outputFromCmd(cmd),
				util.OutputTable, util.OutputWide, util.OutputJSON, util.OutputYAML)
			if err != nil {
				return err
			}

			switch format {
			case util.OutputJSON:
				return util.PrintJSON(out, info)
			case util.OutputYAML:
				return util.PrintYAML(out, info)
			case util.OutputWide:
				_, _ = fmt.Fprintf(out, "datumctl-dns %s (DNS API %s)\n", info.Version, dnsv1alpha1.GroupVersion)
				_, _ = fmt.Fprintf(out, "  plugin API %d\n", info.PluginAPI)
				_, _ = fmt.Fprintf(out, "  %s %s\n", info.GoVersion, info.Platform)
				return nil
			default:
				_, _ = fmt.Fprintf(out, "datumctl-dns %s (DNS API %s)\n", info.Version, dnsv1alpha1.GroupVersion)
				return nil
			}
		},
	}

	return cmd
}

// outputFromCmd reads the root's -o flag, tolerating a detached command in
// tests where no root has been wired up.
func outputFromCmd(cmd *cobra.Command) string {
	if f := cmd.Root().PersistentFlags().Lookup("output"); f != nil {
		return f.Value.String()
	}
	return string(util.OutputTable)
}
