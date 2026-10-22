package serving

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func runReloadHelper(t *testing.T, failures int, waitSeconds string) (string, error) {
	t.Helper()
	bin := t.TempDir()
	counter := filepath.Join(bin, "attempts")
	fake := `#!/bin/sh
count=$(cat "` + counter + `" 2>/dev/null || echo 0)
count=$((count + 1))
echo "$count" > "` + counter + `"
if [ "$count" -le ` + strconv.Itoa(failures) + ` ]; then
  exit 1
fi
echo INTERNAL_DNS_APPLIED
`
	if err := os.WriteFile(filepath.Join(bin, "dnsdist"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join("..", "..", "..", "config", "internal-dns", "commands", "internal-dns-reload-dnsdist"), "node")
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "INTERNAL_DNS_RELOAD_WAIT_SECONDS="+waitSeconds)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func TestReloadDNSDistWaitsForTheConsole(t *testing.T) {
	if output, err := runReloadHelper(t, 3, "10"); err != nil {
		t.Fatalf("reload failed while the console was starting: %v: %s", err, output)
	}
}

func TestReloadDNSDistExplainsAConsoleThatNeverAnswers(t *testing.T) {
	output, err := runReloadHelper(t, 1000, "1")
	if err == nil {
		t.Fatal("reload succeeded without the console")
	}
	if !strings.Contains(output, "no response from the dnsdist console") {
		t.Fatalf("reload failure did not explain itself: %q", output)
	}
}
