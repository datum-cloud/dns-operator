// SPDX-License-Identifier: AGPL-3.0-only

// replay_event republishes a captured immutable envelope verbatim. It is used
// only by the distributed E2E qualification to prove that serving agents keep
// their epoch/revision fences after a control-plane ownership takeover.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/nats-io/nats.go"
)

func main() {
	var url, subject, payloadPath string
	flag.StringVar(&url, "url", "nats://127.0.0.1:14222", "development NATS URL")
	flag.StringVar(&subject, "subject", "", "subject to replay")
	flag.StringVar(&payloadPath, "payload", "", "file containing the exact JSON envelope")
	flag.Parse()
	if subject == "" || payloadPath == "" {
		fmt.Fprintln(os.Stderr, "--subject and --payload are required")
		os.Exit(2)
	}
	payload, err := os.ReadFile(payloadPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read payload: %v\n", err)
		os.Exit(1)
	}
	nc, err := nats.Connect(url, nats.Name("internal-dns-e2e-replay"), nats.Timeout(5*time.Second))
	if err == nil {
		err = nc.Publish(subject, payload)
	}
	if err == nil {
		err = nc.FlushTimeout(5 * time.Second)
	}
	if nc != nil {
		nc.Close()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "replay: %v\n", err)
		os.Exit(1)
	}
}
