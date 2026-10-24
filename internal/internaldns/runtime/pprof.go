// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"net/http"
	_ "net/http/pprof"
	"os"
)

// startLocalProfiler serves Go profiles on loopback only, for a port-forward
// during staging performance work. INTERNAL_DNS_PPROF_ADDRESS="" disables it.
func startLocalProfiler() {
	address, set := os.LookupEnv("INTERNAL_DNS_PPROF_ADDRESS")
	if !set {
		address = "127.0.0.1:6060"
	}
	if address == "" {
		return
	}
	go func() { _ = http.ListenAndServe(address, nil) }()
}
