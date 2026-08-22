// SPDX-License-Identifier: AGPL-3.0-only

package util

import (
	"errors"
	"os"
	"testing"
)

func TestTLSClientConfigIsInertByDefault(t *testing.T) {
	// Production sets no CA file, and must keep verifying against the host's
	// root store exactly as it did before the seam existed.
	if err := os.Unsetenv(CAFileEnv); err != nil {
		t.Fatal(err)
	}
	cfg := tlsClientConfig()
	if cfg.CAFile != "" {
		t.Errorf("CAFile = %q, want empty when %s is unset", cfg.CAFile, CAFileEnv)
	}
	if cfg.Insecure {
		t.Errorf("Insecure = true; there is deliberately no way to switch verification off")
	}
	if len(cfg.CAData) != 0 {
		t.Errorf("CAData is populated, want empty")
	}
}

func TestTLSClientConfigHonoursTheCAFile(t *testing.T) {
	t.Setenv(CAFileEnv, "/etc/ssl/private-root.pem")
	cfg := tlsClientConfig()
	if cfg.CAFile != "/etc/ssl/private-root.pem" {
		t.Errorf("CAFile = %q, want the value of %s", cfg.CAFile, CAFileEnv)
	}
	if cfg.Insecure {
		t.Errorf("Insecure = true; adding a root must never disable verification")
	}
}

func TestNewClientWithoutAProjectIsAUsageError(t *testing.T) {
	c, err := NewClient("")
	if c != nil {
		t.Errorf("NewClient(\"\") returned a client")
	}
	var cliErr *CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("error is %T, want *CLIError", err)
	}
	if cliErr.Code() != ExitUsage {
		t.Errorf("code = %d, want %d", cliErr.Code(), ExitUsage)
	}
	if cliErr.Fix() == "" {
		t.Errorf("a missing project should come with a fix naming --project")
	}
}
