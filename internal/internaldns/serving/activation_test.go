// SPDX-License-Identifier: AGPL-3.0-only

package serving

import (
	"os"
	"testing"
)

func TestActivationOwnershipGatesAbandonedUpdatesWithoutHeartbeat(t *testing.T) {
	dir := t.TempDir()
	tx := FileTransaction{MarkerPath: activationMarkerPath(dir)}
	if err := tx.beginActivation(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.abandonActivation() })
	start, retain, release := daemonPermission(dir)
	release()
	if start || !retain {
		t.Fatal("installation allowed a new process or stopped approved in-memory DNS")
	}
	if err := tx.filesInstalled(); err != nil {
		t.Fatal(err)
	}
	start, retain, release = daemonPermission(dir)
	release()
	if !start || !retain {
		t.Fatal("live installer could not verify its complete configuration")
	}
	if err := tx.abandonActivation(); err != nil {
		t.Fatal(err)
	}
	start, retain, release = daemonPermission(dir)
	release()
	if start || retain {
		t.Fatal("abandoned activation retained query processes")
	}
	if err := os.Remove(tx.MarkerPath); err != nil {
		t.Fatal(err)
	}
	start, retain, release = daemonPermission(dir)
	release()
	if !start || !retain {
		t.Fatal("completed configuration required agent ownership")
	}
}
