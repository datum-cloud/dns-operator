// SPDX-License-Identifier: AGPL-3.0-only

package plugin_test

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	networkingv1alpha "go.datum.net/network-services-operator/api/v1alpha"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	"go.miloapis.com/dns-operator/internal/cmd/dns/util"
)

// The tests in this file drive util directly, in process, but against the same
// real API server and the same production URL construction the binary uses.
//
// They exist because the command layer that would exercise these paths through
// the CLI does not exist yet — zone and record are wave 2. Rather than defer
// the coverage, they pin the layer underneath: real client construction, real
// CRD admission, real status defaulting, real error classification. When the
// commands land, the CLI cases in cli_test.go cover the same ground from the
// outside and these remain as the narrower regression net.

// nonTTY returns a reader that is an *os.File and is not a terminal, which is
// what makes util.NonInteractive report true for the reason it would in CI.
// A bytes.Buffer would not: the harness treats a deliberately wired-up reader
// as answerable.
func nonTTY(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("opening %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestNewClientReachesTheProjectControlPlane(t *testing.T) {
	h.proxy.Reset()

	c, err := util.NewClient(testProject)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	zone := createZone(t, "example-com", "example.com")

	var list dnsv1alpha1.DNSZoneList
	if err := c.List(t.Context(), &list, client.InNamespace(util.ResourceNamespace)); err != nil {
		t.Fatalf("listing zones: %v", err)
	}

	var found bool
	for i := range list.Items {
		if list.Items[i].Spec.DomainName == zone.Spec.DomainName {
			found = true
		}
	}
	if !found {
		t.Errorf("the zone created through the admin client is not visible through the plugin's client")
	}

	// Everything the plugin sent must have gone to the project control-plane
	// URL. A single stray request outside it would mean the URL construction is
	// not actually being exercised.
	requests := h.proxy.Requests()
	if len(requests) == 0 {
		t.Fatal("the proxy saw no traffic; the client is not going through it")
	}
	for _, r := range requests {
		if !r.Matched {
			t.Errorf("request to %q did not match the control-plane URL shape", r.Path)
		}
		if r.Project != testProject {
			t.Errorf("request addressed project %q, want %q", r.Project, testProject)
		}
	}
}

func TestClientSendsAFreshTokenFromTheCredentialsHelper(t *testing.T) {
	h.proxy.Reset()

	c, err := util.NewClient(testProject)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	var list dnsv1alpha1.DNSZoneList
	if err := c.List(t.Context(), &list, client.InNamespace(util.ResourceNamespace)); err != nil {
		t.Fatalf("listing zones: %v", err)
	}

	requests := h.proxy.Requests()
	if len(requests) == 0 {
		t.Fatal("no requests observed")
	}
	for _, r := range requests {
		if r.Token != testToken {
			t.Errorf("request to %q carried token %q, want the helper's %q", r.Path, r.Token, testToken)
		}
		if r.UserAgent != util.UserAgent() {
			t.Errorf("User-Agent = %q, want %q", r.UserAgent, util.UserAgent())
		}
	}

	// The helper is a real subprocess; its log proves the documented argument
	// shape rather than assuming it.
	log, err := os.ReadFile(h.helperLog)
	if err != nil {
		t.Fatalf("reading the helper log: %v", err)
	}
	if !strings.Contains(string(log), "auth get-token") {
		t.Errorf("the credentials helper was not invoked as `auth get-token`; log:\n%s", log)
	}
}

func TestNotFoundClassifiesToExitFour(t *testing.T) {
	c, err := util.NewClient(testProject)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var zone dnsv1alpha1.DNSZone
	getErr := c.Get(t.Context(),
		client.ObjectKey{Namespace: util.ResourceNamespace, Name: "no-such-zone"}, &zone)
	if getErr == nil {
		t.Fatal("getting a nonexistent zone succeeded")
	}

	cliErr := util.ClassifyError(getErr)
	if cliErr.Code() != util.ExitNotFound {
		t.Errorf("code = %d (%s), want %d (DNS_NOT_FOUND)",
			cliErr.Code(), util.ExitCodeName(cliErr.Code()), util.ExitNotFound)
	}

	var out bytes.Buffer
	if got := util.RenderExit(&out, getErr, false); got != util.ExitNotFound {
		t.Errorf("RenderExit returned %d, want %d", got, util.ExitNotFound)
	}
	if !strings.Contains(out.String(), "exit status 4   # DNS_NOT_FOUND") {
		t.Errorf("rendered output does not carry the contract line:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "no-such-zone") {
		t.Errorf("rendered output does not name the missing zone:\n%s", out.String())
	}
}

// The CRD's CEL rules and OpenAPI schema are the reason this harness applies
// the real CRDs. A fake client admits all of these.
func TestRealAdmissionRejectionsClassifyToExitSix(t *testing.T) {
	ensureNamespace(t)

	c, err := util.NewClient(testProject)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	tests := []struct {
		name string
		obj  client.Object
		want string
	}{
		{
			name: "a domain name with no dot violates the CEL rule",
			obj: &dnsv1alpha1.DNSZone{
				ObjectMeta: metav1.ObjectMeta{Name: "no-dot", Namespace: util.ResourceNamespace},
				Spec: dnsv1alpha1.DNSZoneSpec{
					DomainName:       "localhost",
					DNSZoneClassName: "datum-external-global-dns",
				},
			},
			want: "two segments",
		},
		{
			name: "an uppercase domain name violates the pattern",
			obj: &dnsv1alpha1.DNSZone{
				ObjectMeta: metav1.ObjectMeta{Name: "upper", Namespace: util.ResourceNamespace},
				Spec: dnsv1alpha1.DNSZoneSpec{
					DomainName:       "Example.COM",
					DNSZoneClassName: "datum-external-global-dns",
				},
			},
			want: "spec.domainName",
		},
		{
			name: "an empty records list violates MinItems",
			obj: &dnsv1alpha1.DNSRecordSet{
				ObjectMeta: metav1.ObjectMeta{Name: "empty-records", Namespace: util.ResourceNamespace},
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					DNSZoneRef: corev1.LocalObjectReference{Name: "example-com"},
					RecordType: dnsv1alpha1.RRTypeA,
					Records:    nil,
				},
			},
			want: "spec.records",
		},
		{
			name: "an empty dnsZoneRef name violates the CEL rule",
			obj: &dnsv1alpha1.DNSRecordSet{
				ObjectMeta: metav1.ObjectMeta{Name: "no-zone-ref", Namespace: util.ResourceNamespace},
				Spec: dnsv1alpha1.DNSRecordSetSpec{
					DNSZoneRef: corev1.LocalObjectReference{Name: ""},
					RecordType: dnsv1alpha1.RRTypeA,
					Records: []dnsv1alpha1.RecordEntry{
						{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.10"}},
					},
				},
			},
			want: "dnsZoneRef.name must be set",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			createErr := c.Create(t.Context(), tc.obj)
			if createErr == nil {
				t.Fatalf("the API server admitted an object it should have rejected")
			}

			cliErr := util.ClassifyError(createErr)
			if cliErr.Code() != util.ExitInvalid {
				t.Errorf("code = %d (%s), want %d (DNS_INVALID)\nserver said: %v",
					cliErr.Code(), util.ExitCodeName(cliErr.Code()), util.ExitInvalid, createErr)
			}
			if !strings.Contains(cliErr.Error(), tc.want) {
				t.Errorf("message %q does not mention %q", cliErr.Error(), tc.want)
			}
		})
	}
}

// The design doc, and the brief built from it, both state that a freshly
// created DNSRecordSet carries CRD-defaulted conditions stamped at
// 1970-01-01T00:00:00Z. Against the real served CRD that is NOT what happens.
//
// api/v1alpha1/dnsrecordset_types.go:201 does carry the
// +kubebuilder:default marker, but controller-gen does not emit it into the
// CRD — regenerating produces a file byte-identical to the committed one, with
// no default anywhere under status. So the API server stamps nothing, and a
// fresh record set comes back with an entirely empty status.
//
// Both behaviours are pinned here: what the server actually does today, and
// that the epoch guard still holds if an epoch timestamp ever does arrive.
func TestFreshRecordSetHasNoDefaultedConditions(t *testing.T) {
	createZone(t, "epoch-example-com", "epoch-example.com")

	rs := createRecordSet(t, "epoch-example-com-a", "epoch-example-com", dnsv1alpha1.RRTypeA,
		dnsv1alpha1.RecordEntry{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.10"}})

	c, err := util.NewClient(testProject)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	var fetched dnsv1alpha1.DNSRecordSet
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(rs), &fetched); err != nil {
		t.Fatalf("reading the record set back: %v", err)
	}

	if len(fetched.Status.Conditions) != 0 {
		t.Errorf("status.conditions = %+v, want empty.\n"+
			"If this now fails, the CRD default reached the served schema — "+
			"update the doc note and re-point this test at the epoch case.",
			fetched.Status.Conditions)
	}

	// The CLI must render a status for a record the backend has not reached,
	// whichever way the emptiness arrives.
	word, detail := util.RecordStatus(&fetched, "www")
	if word != util.StatusPending {
		t.Errorf("RecordStatus word = %q, want %q", word, util.StatusPending)
	}
	if detail == "" {
		t.Errorf("RecordStatus detail is empty; a pending record should say what it is waiting for")
	}
}

// The epoch guard, exercised against the real API server rather than a
// hand-built struct: an epoch lastTransitionTime written through the status
// subresource must survive serialization and still render as an em dash.
func TestEpochConditionSurvivesARealRoundTrip(t *testing.T) {
	createZone(t, "roundtrip-example-com", "roundtrip-example.com")
	rs := createRecordSet(t, "roundtrip-example-com-a", "roundtrip-example-com", dnsv1alpha1.RRTypeA,
		dnsv1alpha1.RecordEntry{Name: "www", A: &dnsv1alpha1.ARecordSpec{Content: "203.0.113.10"}})

	epoch := metav1.NewTime(time.Unix(0, 0).UTC())
	rs.Status = dnsv1alpha1.DNSRecordSetStatus{
		Conditions: []metav1.Condition{
			{
				Type: "Accepted", Status: metav1.ConditionUnknown,
				Reason: "Pending", Message: "Waiting for controller",
				LastTransitionTime: epoch,
			},
			{
				Type: "Programmed", Status: metav1.ConditionUnknown,
				Reason: "Pending", Message: "Waiting for controller",
				LastTransitionTime: epoch,
			},
		},
	}
	if err := h.k8s.Status().Update(t.Context(), rs); err != nil {
		t.Fatalf("writing the epoch status: %v", err)
	}

	c, err := util.NewClient(testProject)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	var fetched dnsv1alpha1.DNSRecordSet
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(rs), &fetched); err != nil {
		t.Fatalf("reading the record set back: %v", err)
	}

	programmed := apimeta.FindStatusCondition(fetched.Status.Conditions, "Programmed")
	if programmed == nil {
		t.Fatalf("the Programmed condition did not round-trip; status: %+v", fetched.Status)
	}
	if !util.IsNeverTransitioned(programmed.LastTransitionTime) {
		t.Errorf("lastTransitionTime = %v, want the epoch to be recognised as never-transitioned",
			programmed.LastTransitionTime)
	}
	if got := util.RelativeAge(programmed.LastTransitionTime); got != "—" {
		t.Errorf("RelativeAge = %q, want an em dash; a real API server just returned the epoch", got)
	}
	if got := util.RelativeAgeVerbose(programmed.LastTransitionTime); got != "—" {
		t.Errorf("RelativeAgeVerbose = %q, want a bare em dash", got)
	}

	// With no per-owner-name status, the record still reads Pending rather than
	// anything derived from the rolled-up condition.
	if word, _ := util.RecordStatus(&fetched, "www"); word != util.StatusPending {
		t.Errorf("RecordStatus word = %q, want %q", word, util.StatusPending)
	}
}

func TestZoneStatusAndDelegationAgainstARealStatusWrite(t *testing.T) {
	zone := createZone(t, "delegated-com", "delegated.com")

	// Status is a subresource on DNSZone, so this exercises the real write path
	// rather than a struct the test filled in.
	zone.Status = dnsv1alpha1.DNSZoneStatus{
		Nameservers: []string{"ns1.datum.net.", "ns2.datum.net."},
		RecordCount: 12,
		Conditions: []metav1.Condition{{
			Type:               "Programmed",
			Status:             metav1.ConditionTrue,
			Reason:             "Programmed",
			Message:            "Zone programmed",
			LastTransitionTime: metav1.Now(),
			ObservedGeneration: zone.Generation,
		}},
		DomainRef: &dnsv1alpha1.DomainRef{Name: "delegated-com"},
	}
	if err := h.k8s.Status().Update(t.Context(), zone); err != nil {
		t.Fatalf("writing zone status: %v", err)
	}

	c, err := util.NewClient(testProject)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	var fetched dnsv1alpha1.DNSZone
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(zone), &fetched); err != nil {
		t.Fatalf("reading the zone back: %v", err)
	}

	word, detail := util.ZoneStatus(&fetched)
	if word != util.StatusOK {
		t.Errorf("ZoneStatus word = %q, want %q", word, util.StatusOK)
	}
	if want := "zone programmed, 12 records live"; detail != want {
		t.Errorf("ZoneStatus detail = %q, want %q", detail, want)
	}

	// The Domain exists but its nameservers have not been observed yet, which
	// is "we have not looked", not "the registrar points elsewhere".
	d := util.DelegationState(&fetched)
	if d.State != util.DelegationUnknown {
		t.Errorf("delegation state = %q, want %q for an unobserved registrar", d.State, util.DelegationUnknown)
	}
	if !d.Linked {
		t.Errorf("Linked = false, but the zone has a DomainRef")
	}
	if d.Total != 2 {
		t.Errorf("Total = %d, want 2", d.Total)
	}
}

// Once the registrar has actually been observed pointing elsewhere, the same
// zone is genuinely Incomplete. This is the case the user should be told to act
// on, and the one the unobserved state above must not be confused with.
func TestDelegationIncompleteOnceTheRegistrarIsObserved(t *testing.T) {
	zone := createZone(t, "observed-com", "observed.com")

	zone.Status = dnsv1alpha1.DNSZoneStatus{
		Nameservers: []string{"ns1.datum.net.", "ns2.datum.net."},
		RecordCount: 3,
		Conditions: []metav1.Condition{{
			Type: "Programmed", Status: metav1.ConditionTrue, Reason: "Programmed",
			LastTransitionTime: metav1.Now(), ObservedGeneration: zone.Generation,
		}},
		DomainRef: &dnsv1alpha1.DomainRef{
			Name: "observed-com",
			Status: dnsv1alpha1.DomainRefStatus{Nameservers: []networkingv1alpha.Nameserver{
				{Hostname: "ns-cloud-a1.googledomains.com."},
				{Hostname: "ns-cloud-a2.googledomains.com."},
			}},
		},
	}
	if err := h.k8s.Status().Update(t.Context(), zone); err != nil {
		t.Fatalf("writing zone status: %v", err)
	}

	c, err := util.NewClient(testProject)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	var fetched dnsv1alpha1.DNSZone
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(zone), &fetched); err != nil {
		t.Fatalf("reading the zone back: %v", err)
	}

	d := util.DelegationState(&fetched)
	if d.State != util.DelegationIncomplete {
		t.Errorf("delegation state = %q, want %q", d.State, util.DelegationIncomplete)
	}
	if d.SetCount != 0 || d.Total != 2 {
		t.Errorf("delegation = %d of %d, want 0 of 2", d.SetCount, d.Total)
	}
}

func TestCompleteZoneNamesQueriesTheRealAPI(t *testing.T) {
	createZone(t, "completion-example-com", "completion-example.com")

	root := newRootForCompletion(t)
	names, directive := util.CompleteZoneNames(root, nil, "")

	if directive != shellCompNoFileComp {
		t.Errorf("directive = %v, want ShellCompDirectiveNoFileComp", directive)
	}
	var found bool
	for _, n := range names {
		if n == "completion-example.com" {
			found = true
		}
	}
	if !found {
		t.Errorf("completion did not offer the zone's domain name; got %v", names)
	}
}
