// SPDX-License-Identifier: AGPL-3.0-only

// Package plugin_test is the end-to-end harness for the datumctl-dns plugin.
//
// It exists because everything else about the plugin is tested one level below
// where the plugin actually lives. Unit tests prove the helpers behave; this
// proves the artefact behaves: the real binary, exec'd as a subprocess the way
// datumctl execs it, against a real API server serving this repo's real CRDs.
//
// Three things are deliberately real, because each has already hidden a bug or
// could plausibly hide one:
//
//   - The binary. Cobra's exit-code behaviour (a bad flag exiting 1, a typo'd
//     subcommand exiting 0) is invisible to a test that calls Command() in
//     process and inspects the returned error.
//   - The API server, with the CRDs from config/crd/bases. That exercises the
//     OpenAPI schema, the CEL rules on domainName and dnsZoneRef, and MinItems
//     on spec.records — none of which a fake client reproduces. It is also how
//     we established that the status default the design doc described does not
//     reach the served schema at all: controller-gen drops a default on a
//     status that has a status subresource, so a fresh DNSRecordSet comes back
//     with an entirely empty status rather than conditions stamped at the Unix
//     epoch. See TestFreshRecordSetHasNoDefaultedConditions.
//   - The credentials helper. plugin.Token() shells out; a stub script proves
//     the exec path and the argument shape rather than assuming them.
package plugin_test

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// Fixed identifiers the harness injects, so assertions can name them.
const (
	testProject = "acme-prod"
	testOrg     = "acme"
	testToken   = "test-bearer-token-a1b2c3"

	// dnsServiceRef must match util's conventional spec.serviceRef.name. It is
	// duplicated rather than exported: the point of an end-to-end test is to
	// pin the wire value independently of the constant under test, so a rename
	// there fails here loudly instead of silently agreeing.
	dnsServiceRef = "dns-networking-miloapis-com"
)

// serviceEntitlementGVK addresses the stand-in CRD in testdata.
var serviceEntitlementGVK = schema.GroupVersionKind{
	Group:   "services.miloapis.com",
	Version: "v1alpha1",
	Kind:    "ServiceEntitlement",
}

// h is the harness shared by every test in the package. Tests here never run in
// parallel: several of them mutate cluster-wide state (the entitlement) and
// read the proxy's request log, both of which are process-global.
var h *harness

// harness bundles the running API server, the proxy that gives it a
// production-shaped URL, the built binary, and the fake datumctl environment.
type harness struct {
	env *envtest.Environment
	// cfg talks to envtest directly, with admin credentials. Tests use it to
	// arrange fixtures; the code under test never sees it.
	cfg *rest.Config
	// k8s is an admin client over cfg, for arranging and inspecting fixtures.
	k8s client.Client
	// proxy presents envtest at a project control-plane URL.
	proxy *controlPlaneProxy
	// binary is the built datumctl-dns.
	binary string
	// apiHost is what DATUM_API_HOST is set to: the proxy's host:port.
	apiHost string
	// caFile holds the proxy's self-signed certificate, for DATUM_CA_FILE.
	caFile string
	// helper is the stub credentials helper; helperLog records its invocations.
	helper    string
	helperLog string
}

func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harness setup failed: %v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

// run performs setup, runs the suite, and tears down. It is separate from
// TestMain so that deferred cleanup actually runs before os.Exit.
func run(m *testing.M) (int, error) {
	dir, err := os.MkdirTemp("", "datumctl-dns-e2e")
	if err != nil {
		return 0, fmt.Errorf("creating temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	h = &harness{}

	// 1. A real API server with this repo's real CRDs, plus the ServiceEntitlement
	//    stand-in the entitlement pre-flight needs.
	h.env = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "config", "crd", "bases"),
			filepath.Join("testdata"),
		},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: envtestBinaryDir(),
	}
	h.cfg, err = h.env.Start()
	if err != nil {
		return 0, fmt.Errorf("starting envtest (run `make setup-envtest`): %w", err)
	}
	defer func() { _ = h.env.Stop() }()

	if err := dnsv1alpha1.AddToScheme(scheme.Scheme); err != nil {
		return 0, fmt.Errorf("registering dns scheme: %w", err)
	}
	if err := apiextensionsv1.AddToScheme(scheme.Scheme); err != nil {
		return 0, fmt.Errorf("registering apiextensions scheme: %w", err)
	}
	h.k8s, err = client.New(h.cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		return 0, fmt.Errorf("building admin client: %w", err)
	}

	// 2. The proxy, which is what makes the production URL shape testable.
	h.proxy, err = newControlPlaneProxy(h.cfg)
	if err != nil {
		return 0, fmt.Errorf("starting control-plane proxy: %w", err)
	}
	defer h.proxy.Close()
	h.apiHost = h.proxy.HostPort()

	h.caFile = filepath.Join(dir, "proxy-ca.pem")
	if err := writeCertPEM(h.caFile, h.proxy.Certificate()); err != nil {
		return 0, fmt.Errorf("writing proxy CA: %w", err)
	}

	// 3. The stub credentials helper, so plugin.Token() runs for real.
	h.helperLog = filepath.Join(dir, "helper.log")
	h.helper, err = writeCredentialsHelper(dir, h.helperLog)
	if err != nil {
		return 0, fmt.Errorf("writing credentials helper: %w", err)
	}

	// 4. The binary under test.
	h.binary = filepath.Join(dir, "datumctl-dns")
	if err := buildPlugin(h.binary); err != nil {
		return 0, fmt.Errorf("building the plugin: %w", err)
	}

	// The in-process tests read the same injected environment the subprocess
	// does, so util.NewClient exercises its real code path.
	for k, v := range pluginEnv() {
		if err := os.Setenv(k, v); err != nil {
			return 0, fmt.Errorf("setting %s: %w", k, err)
		}
	}
	// CI would make every prompt non-interactive by fiat; the tests that care
	// about interactivity control it explicitly through the reader they pass.
	if err := os.Unsetenv("CI"); err != nil {
		return 0, fmt.Errorf("unsetting CI: %w", err)
	}

	// The default state is "DNS is entitled", so command tests exercise the
	// happy path. The pre-flight's own tests remove it and put it back.
	if err := setEntitlement(context.Background(), "Active"); err != nil {
		return 0, fmt.Errorf("seeding the entitlement: %w", err)
	}

	return m.Run(), nil
}

// pluginEnv is the environment datumctl injects into a plugin, pointed at the
// harness instead of the real platform.
func pluginEnv() map[string]string {
	return map[string]string{
		"DATUM_API_HOST":           h.apiHost,
		"DATUM_PROJECT":            testProject,
		"DATUM_ORG":                testOrg,
		"DATUM_PLUGIN_API_VERSION": "1",
		"DATUM_CREDENTIALS_HELPER": h.helper,
		util.CAFileEnv:             h.caFile,
	}
}

// buildPlugin compiles cmd/datumctl-dns to the given path. The Makefile has a
// build-plugin target, but it writes to a fixed location in bin/ and is owned
// by another agent; going through `go build` keeps this harness self-contained
// and lets it write to a temp dir.
func buildPlugin(out string) error {
	cmd := exec.Command("go", "build", "-o", out, "./cmd/datumctl-dns")
	cmd.Dir = repoRoot()
	if combined, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build: %w\n%s", err, combined)
	}
	return nil
}

// writeCredentialsHelper writes a stub that answers `auth get-token` the way
// datumctl does, and appends its arguments to a log so a test can prove the
// plugin invoked it with the documented shape.
func writeCredentialsHelper(dir, logPath string) (string, error) {
	path := filepath.Join(dir, "fake-datumctl")
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
if [ "$1" = "auth" ] && [ "$2" = "get-token" ]; then
  echo %q
  exit 0
fi
echo "fake-datumctl: unexpected args: $*" >&2
exit 1
`, logPath, testToken)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		return "", err
	}
	return path, nil
}

// writeCertPEM writes a certificate in the PEM form a CA bundle expects.
func writeCertPEM(path string, cert *x509.Certificate) error {
	block := &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}
	return os.WriteFile(path, pem.EncodeToMemory(block), 0o600)
}

// envtestBinaryDir locates the kubebuilder assets, mirroring the controller
// suite so both work from an IDE without KUBEBUILDER_ASSETS set.
func envtestBinaryDir() string {
	if fromEnv := os.Getenv("KUBEBUILDER_ASSETS"); fromEnv != "" {
		return ""
	}
	base := filepath.Join(repoRoot(), "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return filepath.Join(base, entry.Name())
		}
	}
	return ""
}

// repoRoot resolves the module root from this file's compile-time path, so the
// harness does not depend on the working directory a test runner chooses.
func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ".."
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}
