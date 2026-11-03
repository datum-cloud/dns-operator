// SPDX-License-Identifier: AGPL-3.0-only

package validation

import (
	"os/exec"
	"testing"
)

func TestCleanupRequiresCurrentActivationAndPositiveQueries(t *testing.T) {
	command := exec.Command("bash", "test-before-cleanup.sh")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("cleanup gate: %v\n%s", err, output)
	}
}
