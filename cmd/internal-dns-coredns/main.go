// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"go.miloapis.com/dns-operator/internal/internaldns/coredns"
	"runtime/debug"
)

func main() {
	flags := flag.NewFlagSet("coredns", flag.ExitOnError)
	var path string
	var validate, version bool
	flags.StringVar(&path, "config", "", "Absolute path to the DNS context snapshot")
	flags.BoolVar(&validate, "validate", false, "Validate the DNS context snapshot and exit")
	flags.BoolVar(&version, "version", false, "Print the CoreDNS version and exit")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if version {
		version := "unknown"
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, dep := range info.Deps {
				if dep.Path == "github.com/coredns/coredns" {
					version = dep.Version
				}
			}
		}
		fmt.Printf("CoreDNS %s; dnscontext plugin\n", version)
		return
	}
	var err error
	if validate {
		_, err = coredns.Load(path)
	} else {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		err = coredns.Run(ctx, path)
	}
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "CoreDNS: %v\n", err)
		os.Exit(1)
	}
}
