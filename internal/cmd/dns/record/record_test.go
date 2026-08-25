// SPDX-License-Identifier: AGPL-3.0-only

package record

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"go.datum.net/datumctl/plugin"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

const (
	testZoneObject = "example-com"
	testDomain     = "example.com"
)

// harness wires a record command tree to a fake API server, capturing the
// streams the way the plugin's main would.
type harness struct {
	root   *cobra.Command
	out    *bytes.Buffer
	errOut *bytes.Buffer
	client client.Client
	stdin  string
}

// newHarness builds the command tree over a fake client seeded with objs.
func newHarness(t *testing.T, objs ...client.Object) *harness {
	t.Helper()
	return newHarnessWithInterceptor(t, nil, objs...)
}

func newHarnessWithInterceptor(t *testing.T, ic *interceptor.Funcs, objs ...client.Object) *harness {
	t.Helper()

	scheme, err := util.NewScheme()
	if err != nil {
		t.Fatalf("building scheme: %v", err)
	}

	// The commands query through the CRD's selectable fields, so the fake needs
	// the same indexes the API server maintains.
	builder := fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&dnsv1alpha1.DNSRecordSet{}, fieldZoneRef, func(o client.Object) []string {
			return []string{o.(*dnsv1alpha1.DNSRecordSet).Spec.DNSZoneRef.Name}
		}).
		WithIndex(&dnsv1alpha1.DNSRecordSet{}, fieldRecordType, func(o client.Object) []string {
			return []string{string(o.(*dnsv1alpha1.DNSRecordSet).Spec.RecordType)}
		}).
		WithIndex(&dnsv1alpha1.DNSZone{}, fieldDomainName, func(o client.Object) []string {
			return []string{o.(*dnsv1alpha1.DNSZone).Spec.DomainName}
		}).
		WithObjects(objs...)
	if ic != nil {
		builder = builder.WithInterceptorFuncs(*ic)
	}
	c := builder.Build()

	h := &harness{out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, client: c}

	original := clientFactory
	clientFactory = func(string) (client.Client, error) { return c, nil }
	t.Cleanup(func() { clientFactory = original })

	return h
}

// newRoot builds a fresh command tree. Cobra records which flags were set on
// the Command object itself, so a harness that reused one tree across two runs
// would leak the first run's flags into the second.
func (h *harness) newRoot() *cobra.Command {
	root := plugin.NewRootCmd("dns", "test")
	root.PersistentFlags().BoolP("verbose", "v", false, "")
	root.PersistentFlags().BoolP("quiet", "q", false, "")
	root.PersistentFlags().BoolP("yes", "y", false, "")
	root.SilenceUsage = true
	root.SilenceErrors = true
	root.AddCommand(Command())
	root.SetOut(h.out)
	root.SetErr(h.errOut)
	root.SetIn(strings.NewReader(h.stdin))
	return root
}

// run executes one command line and returns the error it produced.
func (h *harness) run(args ...string) error {
	h.out.Reset()
	h.errOut.Reset()
	h.root = h.newRoot()
	h.root.SetArgs(args)
	return h.root.ExecuteContext(context.Background())
}

// answer preloads the confirmation prompt's input for the next run.
func (h *harness) answer(s string) { h.stdin = s }

func (h *harness) stdout() string { return h.out.String() }
func (h *harness) stderr() string { return h.errOut.String() }

// interactive guarantees the confirmation prompts are reachable: NonInteractive
// treats a set CI variable as "nobody can answer".
func interactive(t *testing.T) {
	t.Helper()
	t.Setenv("CI", "")
	if err := os.Unsetenv("CI"); err != nil {
		t.Fatalf("unsetting CI: %v", err)
	}
}

// getSet reads a record set back out of the fake API server.
func (h *harness) getSet(t *testing.T, name string) *dnsv1alpha1.DNSRecordSet {
	t.Helper()
	var rs dnsv1alpha1.DNSRecordSet
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: util.ResourceNamespace, Name: name}, &rs); err != nil {
		t.Fatalf("getting record set %q: %v", name, err)
	}
	return &rs
}

func (h *harness) setMissing(t *testing.T, name string) bool {
	t.Helper()
	var rs dnsv1alpha1.DNSRecordSet
	err := h.client.Get(context.Background(), client.ObjectKey{Namespace: util.ResourceNamespace, Name: name}, &rs)
	return err != nil
}

// --- fixtures ---------------------------------------------------------------

func testZone() *dnsv1alpha1.DNSZone {
	return &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: testZoneObject, Namespace: util.ResourceNamespace},
		Spec:       dnsv1alpha1.DNSZoneSpec{DomainName: testDomain},
	}
}

// recordSet builds a set for the zone, with the object name the operator and
// the portal both use.
func recordSet(t dnsv1alpha1.RRType, entries ...dnsv1alpha1.RecordEntry) *dnsv1alpha1.DNSRecordSet {
	return &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testZoneObject + "-" + strings.ToLower(string(t)),
			Namespace: util.ResourceNamespace,
		},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: testZoneObject},
			RecordType: t,
			Records:    entries,
		},
	}
}

func ttl(v int64) *int64 { return &v }

func aEntry(name, ip string, t *int64) dnsv1alpha1.RecordEntry {
	return dnsv1alpha1.RecordEntry{Name: name, TTL: t, A: &dnsv1alpha1.ARecordSpec{Content: ip}}
}

// withOwnerStatus stamps a per-owner-name condition, which is the only place
// the interesting programming outcomes exist.
func withOwnerStatus(rs *dnsv1alpha1.DNSRecordSet, name string, status metav1.ConditionStatus, reason, message string) {
	rs.Status.RecordSets = append(rs.Status.RecordSets, dnsv1alpha1.RecordSetStatus{
		Name: name,
		Conditions: []metav1.Condition{{
			Type:               "Programmed",
			Status:             status,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: metav1.Now(),
		}},
	})
}

func withAcceptedFalse(rs *dnsv1alpha1.DNSRecordSet, message string) {
	rs.Status.Conditions = append(rs.Status.Conditions, metav1.Condition{
		Type:               "Accepted",
		Status:             metav1.ConditionFalse,
		Reason:             "Invalid",
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
}

func withLabels(rs *dnsv1alpha1.DNSRecordSet, kv map[string]string) *dnsv1alpha1.DNSRecordSet {
	rs.Labels = kv
	return rs
}

// --- assertions -------------------------------------------------------------

// collapse squeezes the tabwriter's padding so a row can be asserted on by its
// content rather than by its column widths.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func collapsedLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if c := collapse(l); c != "" {
			out = append(out, c)
		}
	}
	return out
}

func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("output does not contain %q\n--- got ---\n%s", want, got)
	}
}

func mustNotContain(t *testing.T, got, want string) {
	t.Helper()
	if strings.Contains(got, want) {
		t.Errorf("output unexpectedly contains %q\n--- got ---\n%s", want, got)
	}
}

// requireExit asserts the error carries the plugin's contractual exit code.
func requireExit(t *testing.T, err error, want int) *util.CLIError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error with exit code %d (%s), got nil", want, util.ExitCodeName(want))
	}
	ce := util.ClassifyError(err)
	if ce.Code() != want {
		t.Fatalf("exit code = %d (%s), want %d (%s): %v",
			ce.Code(), util.ExitCodeName(ce.Code()), want, util.ExitCodeName(want), err)
	}
	return ce
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCommandTree(t *testing.T) {
	cmd := Command()

	if got := cmd.Aliases; len(got) != 2 || got[0] != "records" || got[1] != "rr" {
		t.Errorf("record aliases = %v, want [records rr]", got)
	}

	want := map[string][]string{
		"list":     {"ls"},
		"create":   nil,
		"set":      nil,
		"delete":   {"rm"},
		"describe": {"show", "get"},
		"apply":    nil,
	}
	for name, aliases := range want {
		sub, _, err := cmd.Find([]string{name})
		if err != nil || sub.Name() != name {
			t.Fatalf("subcommand %q not registered", name)
			continue
		}
		for _, a := range aliases {
			if !sub.HasAlias(a) {
				t.Errorf("%s is missing alias %q", name, a)
			}
		}
	}
}

// errOptimisticLock is the cause a rejected precondition carries.
var errOptimisticLock = errors.New("the object has been modified; please apply your changes to the latest version and try again")

// writeFile is os.WriteFile with the test fixture's permissions.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
