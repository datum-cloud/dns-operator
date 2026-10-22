// SPDX-License-Identifier: AGPL-3.0-only

package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestHolderIdentityHelper(t *testing.T) {
	if os.Getenv("INTERNAL_DNS_IDENTITY_HELPER") != "1" {
		return
	}
	fmt.Printf("%s\n%s\n", holderIdentity(), holderIdentity())
}

func TestHolderIdentityIsStablePerProcessAndUniqueAcrossSameHostProcesses(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	run := func() string {
		cmd := exec.Command(executable, "-test.run=^TestHolderIdentityHelper$")
		cmd.Env = append(os.Environ(), "INTERNAL_DNS_IDENTITY_HELPER=1")
		output, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		identities := strings.Fields(string(output))
		if len(identities) < 2 || identities[0] != identities[1] {
			t.Fatalf("holder identity changed within one process: %q", output)
		}
		return identities[0]
	}
	first, second := run(), run()
	if first == second {
		t.Fatalf("same-host processes shared holder identity %q", first)
	}
}
