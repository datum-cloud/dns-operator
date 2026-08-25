// SPDX-License-Identifier: AGPL-3.0-only

// Command datumctl-dns is the `datumctl dns` plugin binary.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"go.datum.net/datumctl/plugin"

	"go.miloapis.com/dns-operator/internal/cmd/dns"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// version is set at build time via ldflags.
var version = "dev"

func main() {
	// The manifest is served before Cobra so `--plugin-manifest` answers even
	// when flag parsing would reject the rest of the command line.
	plugin.ServeManifest(plugin.Manifest{
		Name:          "dns",
		Version:       version,
		Description:   "Manage DNS zones and records on Datum Cloud",
		APIVersion:    1,
		MinAPIVersion: 1,
	})

	// Ctrl-C cancels the in-flight request rather than killing the process mid
	// write, and surfaces as DNS_ABORTED instead of a generic failure.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dns.Version = version

	root := dns.Command()
	if err := root.ExecuteContext(ctx); err != nil {
		verbose, _ := root.PersistentFlags().GetBool("verbose")
		os.Exit(util.RenderExit(root.ErrOrStderr(), err, verbose))
	}
}
