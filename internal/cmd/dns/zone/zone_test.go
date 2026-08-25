// SPDX-License-Identifier: AGPL-3.0-only

package zone

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

const testProject = "acme-prod"

// harness is one command invocation wired to a fake API.
type harness struct {
	root *cobra.Command
	out  *bytes.Buffer
	err  *bytes.Buffer
	in   *bytes.Buffer
}

// newHarness builds a root command carrying the persistent flags the real
// plugin root provides, with the zone group attached and the API client
// replaced by c.
func newHarness(t *testing.T, c client.Client) *harness {
	t.Helper()

	prev := clientFactory
	clientFactory = func(string) (client.Client, error) { return c, nil }
	t.Cleanup(func() { clientFactory = prev })

	root := &cobra.Command{Use: "dns", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("project", testProject, "")
	root.PersistentFlags().String("org", "", "")
	root.PersistentFlags().StringP("output", "o", "table", "")
	root.PersistentFlags().BoolP("verbose", "v", false, "")
	root.PersistentFlags().BoolP("quiet", "q", false, "")
	root.PersistentFlags().BoolP("yes", "y", false, "")
	root.AddCommand(Command())

	h := &harness{root: root, out: &bytes.Buffer{}, err: &bytes.Buffer{}, in: &bytes.Buffer{}}
	root.SetOut(h.out)
	root.SetErr(h.err)
	root.SetIn(h.in)
	return h
}

// run executes the command line and returns its error.
func (h *harness) run(args ...string) error {
	h.root.SetArgs(args)
	return h.root.Execute()
}

// newFakeClient builds a fake client with the plugin's scheme and the
// server-side field index the record-set listing depends on.
func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return newFakeClientWith(t, interceptor.Funcs{}, objs...)
}

func newFakeClientWith(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()

	scheme, err := util.NewScheme()
	if err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithIndex(&dnsv1alpha1.DNSRecordSet{}, zoneRefField, func(o client.Object) []string {
			return []string{o.(*dnsv1alpha1.DNSRecordSet).Spec.DNSZoneRef.Name}
		}).
		WithInterceptorFuncs(funcs).
		Build()
}

// zoneOption mutates a test zone.
type zoneOption func(*dnsv1alpha1.DNSZone)

// newZone builds a DNSZone for tests: programmed, delegated, and 14 days old
// unless an option says otherwise.
func newZone(objName, domain string, opts ...zoneOption) *dnsv1alpha1.DNSZone {
	z := &dnsv1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{
			Name:              objName,
			Namespace:         util.ResourceNamespace,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-14 * 24 * time.Hour)),
		},
		Spec: dnsv1alpha1.DNSZoneSpec{
			DomainName:       domain,
			DNSZoneClassName: DefaultZoneClass,
		},
		Status: dnsv1alpha1.DNSZoneStatus{
			Nameservers: []string{"ns1.datum.net.", "ns2.datum.net."},
			Conditions: []metav1.Condition{{
				Type:               "Programmed",
				Status:             metav1.ConditionTrue,
				Reason:             "Programmed",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
	for _, opt := range opts {
		opt(z)
	}
	return z
}

// delegated links the zone to a Domain publishing the given nameservers.
func delegated(hostnames ...string) zoneOption {
	return func(z *dnsv1alpha1.DNSZone) {
		ref := &dnsv1alpha1.DomainRef{Name: kebabCase(z.Spec.DomainName)}
		for _, h := range hostnames {
			ref.Status.Nameservers = append(ref.Status.Nameservers,
				networkingv1alpha.Nameserver{Hostname: h})
		}
		z.Status.DomainRef = ref
	}
}

// pending replaces the zone's conditions with a Programmed=False/Pending one.
func pending() zoneOption {
	return func(z *dnsv1alpha1.DNSZone) {
		z.Status.Nameservers = nil
		z.Status.Conditions = []metav1.Condition{{
			Type:               "Programmed",
			Status:             metav1.ConditionFalse,
			Reason:             "Pending",
			LastTransitionTime: metav1.Now(),
		}}
	}
}

// Fixture messages, fixed so the helpers below read as states rather than as
// string plumbing.
const (
	brokenMessage   = "the backend refused the zone"
	rejectedMessage = "DNSZone claimed by another resource"
)

// broken marks the zone Programmed=False with a backend error.
func broken() zoneOption {
	return func(z *dnsv1alpha1.DNSZone) {
		z.Status.Conditions = []metav1.Condition{{
			Type:               "Programmed",
			Status:             metav1.ConditionFalse,
			Reason:             "PDNSError",
			Message:            brokenMessage,
			LastTransitionTime: metav1.Now(),
		}}
	}
}

// rejected marks the zone Accepted=False, the state admission control puts a
// zone into when its domain is already claimed. It is terminal: the object
// exists and will never program.
func rejected() zoneOption {
	return func(z *dnsv1alpha1.DNSZone) {
		z.Status.Nameservers = nil
		z.Status.Conditions = []metav1.Condition{{
			Type:               util.CondAccepted,
			Status:             metav1.ConditionFalse,
			Reason:             "DNSZoneInUse",
			Message:            rejectedMessage,
			LastTransitionTime: metav1.Now(),
		}}
	}
}

// withRecordCount sets status.recordCount, which counts record entries.
func withRecordCount(n int) zoneOption {
	return func(z *dnsv1alpha1.DNSZone) { z.Status.RecordCount = n }
}

// withAge backdates the creation timestamp.
func withAge(d time.Duration) zoneOption {
	return func(z *dnsv1alpha1.DNSZone) {
		z.CreationTimestamp = metav1.NewTime(time.Now().Add(-d))
	}
}

// newRecordSet builds a record set holding count entries of one type.
func newRecordSet(name, zoneObjName string, rrType dnsv1alpha1.RRType, count int) *dnsv1alpha1.DNSRecordSet {
	rs := &dnsv1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: util.ResourceNamespace},
		Spec: dnsv1alpha1.DNSRecordSetSpec{
			DNSZoneRef: corev1.LocalObjectReference{Name: zoneObjName},
			RecordType: rrType,
		},
	}
	for i := 0; i < count; i++ {
		rs.Spec.Records = append(rs.Spec.Records, dnsv1alpha1.RecordEntry{Name: "@"})
	}
	return rs
}

func TestCommandTree(t *testing.T) {
	cmd := Command()

	if got := cmd.Aliases; !equalStrings(got, []string{"zones", "z"}) {
		t.Errorf("group aliases = %v, want [zones z]", got)
	}

	want := map[string][]string{
		"list":        {"ls"},
		"create":      nil,
		"describe":    {"show", "get"},
		"nameservers": {"ns"},
		"delete":      {"rm"},
		"import":      nil,
		"export":      nil,
	}
	got := map[string][]string{}
	for _, sub := range cmd.Commands() {
		got[sub.Name()] = sub.Aliases
	}
	for name, aliases := range want {
		sub, registered := got[name]
		if !registered {
			t.Errorf("subcommand %q is not registered", name)
			continue
		}
		if aliases != nil && !equalStrings(sub, aliases) {
			t.Errorf("%s aliases = %v, want %v", name, sub, aliases)
		}
	}
}

func TestGroupRejectsUnknownSubcommand(t *testing.T) {
	h := newHarness(t, newFakeClient(t))

	err := h.run("zone", "lst")
	if err == nil {
		t.Fatal("expected an error for an unknown subcommand")
	}
	assertExitCode(t, err, util.ExitUsage)
	if !strings.Contains(err.Error(), `unknown command "lst"`) {
		t.Errorf("error = %q, want it to name the unknown command", err.Error())
	}
}

func TestZoneObjectName(t *testing.T) {
	prev := randomSuffix
	randomSuffix = func() string { return "abc123" }
	t.Cleanup(func() { randomSuffix = prev })

	tests := []struct {
		name   string
		domain string
		want   string
	}{
		{name: "dots become hyphens", domain: "example.com", want: "example-com-abc123"},
		{name: "subdomain", domain: "staging.acme.io", want: "staging-acme-io-abc123"},
		{
			name:   "long domain is truncated",
			domain: "a-very-long-domain-name-indeed.example.com",
			want:   "a-very-long-domain-name-abc123",
		},
		{name: "underscores", domain: "my_zone.test", want: "my-zone-test-abc123"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := zoneObjectName(tc.domain); got != tc.want {
				t.Errorf("zoneObjectName(%q) = %q, want %q", tc.domain, got, tc.want)
			}
		})
	}
}

func TestValidateDomain(t *testing.T) {
	tests := []struct {
		name    string
		domain  string
		wantErr bool
	}{
		{name: "apex", domain: "example.com"},
		{name: "subdomain", domain: "staging.acme.io"},
		{name: "hyphens", domain: "my-zone.example.com"},
		{name: "single label", domain: "localhost", wantErr: true},
		{name: "empty", domain: "", wantErr: true},
		{name: "underscore", domain: "my_zone.com", wantErr: true},
		{name: "leading hyphen", domain: "-bad.com", wantErr: true},
		{name: "label over 63 characters", domain: strings.Repeat("a", 64) + ".com", wantErr: true},
		{name: "label of exactly 63 characters", domain: strings.Repeat("a", 63) + ".com"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDomain(tc.domain, tc.domain)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateDomain(%q) = nil, want an error", tc.domain)
				}
				assertExitCode(t, err, util.ExitUsage)
				return
			}
			if err != nil {
				t.Fatalf("validateDomain(%q) = %v, want nil", tc.domain, err)
			}
		})
	}
}

func TestGetZoneNotFound(t *testing.T) {
	c := newFakeClient(t, newZone("example-com-abc123", "example.com"))

	_, err := getZone(t.Context(), c, testProject, "missing.com")
	if err == nil {
		t.Fatal("expected an error for a zone that does not exist")
	}
	assertExitCode(t, err, util.ExitNotFound)

	var ce *util.CLIError
	if !asCLIError(err, &ce) {
		t.Fatalf("error is not a CLIError: %v", err)
	}
	if !strings.Contains(ce.Fix(), "datumctl dns zone list") {
		t.Errorf("fix = %q, want it to suggest listing zones", ce.Fix())
	}
}

func TestGetZoneMatchesCaseInsensitively(t *testing.T) {
	c := newFakeClient(t, newZone("example-com-abc123", "example.com"))

	z, err := getZone(t.Context(), c, testProject, "Example.COM.")
	if err != nil {
		t.Fatalf("getZone: %v", err)
	}
	if z.Spec.DomainName != "example.com" {
		t.Errorf("domain = %q, want example.com", z.Spec.DomainName)
	}
}

// asCLIError unwraps err to the plugin's error type.
func asCLIError(err error, target **util.CLIError) bool { return errors.As(err, target) }

// assertExitCode checks the contractual exit code an error would produce.
func assertExitCode(t *testing.T, err error, want int) {
	t.Helper()
	ce := util.ClassifyError(err)
	if ce == nil {
		t.Fatalf("expected an error with exit code %d, got nil", want)
	}
	if ce.Code() != want {
		t.Errorf("exit code = %d (%s), want %d (%s): %v",
			ce.Code(), util.ExitCodeName(ce.Code()), want, util.ExitCodeName(want), err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
