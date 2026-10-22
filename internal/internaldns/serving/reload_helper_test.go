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
	arguments := filepath.Join(bin, "arguments")
	fake := `#!/bin/sh
printf '%s\n' "$@" > "` + arguments + `"
count=$(cat "` + counter + `" 2>/dev/null || echo 0)
count=$((count + 1))
echo "$count" > "` + counter + `"
if [ "$count" -le ` + strconv.Itoa(failures) + ` ]; then
  exit 1
fi
echo 'Warning: a console notice on a good reload' >&2
echo INTERNAL_DNS_APPLIED
`
	if err := os.WriteFile(filepath.Join(bin, "dnsdist"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join("..", "..", "..", "config", "internal-dns", "commands", "internal-dns-reload-dnsdist"), "node")
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "INTERNAL_DNS_RELOAD_WAIT_SECONDS="+waitSeconds)
	output, err := cmd.CombinedOutput()
	got, readErr := os.ReadFile(arguments)
	if readErr != nil {
		t.Fatal(readErr)
	}
	config := "/etc/internal-dns/node-dnsdist.conf"
	want := strings.Join([]string{"-C", config, "-c", "-e", `dofile("` + config + `"); return "INTERNAL_DNS_APPLIED"`}, "\n") + "\n"
	if string(got) != want {
		t.Fatalf("dnsdist arguments = %q, want %q", got, want)
	}
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
