// SPDX-License-Identifier: AGPL-3.0-only
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"go.miloapis.com/dns-operator/internal/discovery"
)

func main() {
	domainFlag := flag.String("domain", "", "Zone/domain to discover (e.g. example.com)")
	timeoutFlag := flag.Duration("timeout", 15*time.Second, "Timeout for DNS discovery")
	flag.Parse()

	fmt.Println("discovery CLI starting")
	domain := *domainFlag
	if domain == "" {
		// Accept positional arg as fallback: cmd/discovery <domain>
		if flag.NArg() > 0 {
			domain = flag.Arg(0)
		}
	}
	if domain == "" {
		fmt.Fprintln(os.Stderr, "error: domain is required\nUsage: discovery -domain example.com [-timeout 10s]")
		flag.PrintDefaults()
		os.Exit(2)
	}

	fmt.Printf("target domain: %s\n", domain)
	ctx, cancel := context.WithTimeout(context.Background(), *timeoutFlag)
	defer cancel()

	start := time.Now()
	type result struct {
		sets []interface{}
		err  error
	}
	resCh := make(chan result, 1)
	go func() {
		rs, err := discovery.DiscoverZoneRecords(context.Background(), domain)
		asAny := make([]interface{}, len(rs))
		for i := range rs {
			asAny[i] = rs[i]
		}
		resCh <- result{sets: asAny, err: err}
	}()

	var (
		recordSetsAny []interface{}
		err           error
	)
	select {
	case r := <-resCh:
		recordSetsAny, err = r.sets, r.err
	case <-ctx.Done():
		elapsed := time.Since(start)
		fmt.Fprintf(os.Stderr, "discovery timed out for %q after %s\n", domain, elapsed)
		os.Exit(1)
	}
	elapsed := time.Since(start)
	if err != nil {
		fmt.Fprintf(os.Stderr, "discovery failed for %q after %s: %v\n", domain, elapsed, err)
		os.Exit(1)
	}

	fmt.Printf("discovery succeeded for %q in %s\n", domain, elapsed)
	fmt.Printf("discovered record set count: %d\n", len(recordSetsAny))

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(recordSetsAny); err != nil {
		fmt.Fprintf(os.Stderr, "failed to encode results: %v\n", err)
		os.Exit(1)
	}
}
