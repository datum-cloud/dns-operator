package runtime

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dnsv1alpha1 "go.miloapis.com/dns-operator/api/v1alpha1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func deploymentObjects(t *testing.T, name string) []*unstructured.Unstructured {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "internal-dns", name))
	if err != nil {
		t.Fatal(err)
	}
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var objects []*unstructured.Unstructured
	for {
		var object unstructured.Unstructured
		err := decoder.Decode(&object.Object)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if object.GetKind() != "" {
			objects = append(objects, &object)
		}
	}
	return objects
}

// Use the Kubernetes RBAC authorizer and the actual shipped Role/ClusterRole,
// rather than accepting reconciliation under an administrator kubeconfig.
func TestDeploymentRolesAuthorizeWorkerAndDiscoveryBoundaries(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("set KUBEBUILDER_ASSETS to verify packaged roles with the Kubernetes RBAC authorizer")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd", "bases")}, ErrorIfCRDPathMissing: true}
	environment.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	cfg, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme, err := newScheme()
	if err != nil {
		t.Fatal(err)
	}
	if err := authorizationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, namespace := range []string{"example-project", "internal-dns-system"} {
		if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
			t.Fatal(err)
		}
	}
	verifyProjectReferenceSchemas(t, ctx, cl)
	for _, object := range deploymentObjects(t, "rbac.yaml") {
		if err := cl.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	for _, identity := range []struct{ name, role string }{{"product", "internal-dns-product-writer"}, {"issuer", "internal-dns-grant-issuer"}} {
		binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: identity.name + "-test", Namespace: "example-project"}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: identity.name, Namespace: "example-project"}}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: identity.role}}
		if err := cl.Create(ctx, binding); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		principal, resource, subresource, verb string
		allowed                                bool
	}{
		{"internal-dns-control-plane", "dnsrecordsets", "", "list", true},
		{"internal-dns-control-plane", "dnsrecordsets", "", "watch", true},
		{"internal-dns-control-plane", "dnszones", "", "create", true},
		{"internal-dns-control-plane", "dnszones", "", "patch", true},
		{"internal-dns-control-plane", "dnszoneassociations", "", "create", true},
		{"internal-dns-control-plane", "dnszones", "status", "patch", true},
		{"internal-dns-control-plane", "dnszoneassociations", "status", "patch", true},
		{"internal-dns-control-plane", "dnsnamingpolicies", "status", "patch", true},
		{"internal-dns-control-plane", "dnsregistrations", "status", "patch", true},
		{"internal-dns-control-plane", "dnscontributiongrants", "status", "patch", true},
		{"internal-dns-control-plane", "dnspublicationmanifests", "", "create", true},
		{"product", "dnsresolvercontexts", "", "get", true},
		{"product", "dnsresolvercontexts", "", "list", true},
		{"product", "dnsmanagednamespaces", "", "get", true},
		{"product", "dnsmanagednamespaces", "", "list", true},
		{"product", "dnsmanagednamespaces", "", "patch", false},
		{"product", "dnsmanagednamespaces", "status", "patch", false},
		{"product", "dnsnamingpolicies", "", "get", true},
		{"product", "dnsnamingpolicies", "", "list", true},
		{"product", "dnsnamingpolicies", "", "patch", false},
		{"product", "dnsnamingpolicies", "status", "patch", false},
		{"product", "dnsregistrations", "", "create", true},
		{"product", "dnsrecordcontributions", "status", "patch", true},
		{"product", "dnscontributiongrants", "", "get", true},
		{"product", "dnscontributiongrants", "", "create", false},
		{"product", "dnscontributiongrants", "status", "patch", false},
		{"product", "dnsresolvercontexts", "", "patch", false},
		{"product", "dnsresolveraccessbindings", "", "create", false},
		{"product", "dnspublicationmanifests", "", "create", false},
		{"issuer", "dnscontributiongrants", "", "create", true},
		{"issuer", "dnsregistrations", "", "update", true},
		{"issuer", "dnscontributiongrants", "status", "patch", false},
		{"issuer", "dnsrecordcontributions", "", "create", false},
		{"issuer", "dnsresolveraccessbindings", "", "create", false},
	}
	for _, tc := range cases {
		namespace := "example-project"
		if tc.principal == "internal-dns-control-plane" {
			namespace = "internal-dns-system"
		}
		user := "system:serviceaccount:" + namespace + ":" + tc.principal
		deadline := time.Now().Add(5 * time.Second)
		for {
			sar := &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{User: user, Groups: []string{"system:authenticated", "system:serviceaccounts", "system:serviceaccounts:" + namespace}, ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: "example-project", Group: "dns.networking.miloapis.com", Resource: tc.resource, Subresource: tc.subresource, Verb: tc.verb}}}
			if err := cl.Create(ctx, sar); err != nil {
				t.Fatal(err)
			}
			if sar.Status.Allowed == tc.allowed {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s %s %s/%s allowed=%v want=%v: %s", user, tc.verb, tc.resource, tc.subresource, sar.Status.Allowed, tc.allowed, sar.Status.Reason)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func TestEveryMemberCanCreateAllRuntimeConsumers(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "internal-dns", "nats-server.conf.example"))
	if err != nil {
		t.Fatal(err)
	}
	contents := string(data)
	start := strings.Index(contents, `user: "internal-dns-control-plane"`)
	if start < 0 {
		t.Fatal("missing control-plane broker identity")
	}
	controlPolicy := strings.SplitN(contents[start:], "\n    },", 2)[0]
	controlACK := "$JS.ACK.DNS_PRIVATE." + durable(roleControlPlane, "us-central1", "shared-0", "acks") + ".>"
	if !strings.Contains(controlPolicy, `"`+controlACK+`"`) {
		t.Errorf("control plane cannot acknowledge its member-ACK consumer: %s", controlACK)
	}
	if strings.Contains(controlPolicy, `"$JS.ACK.DNS_PRIVATE.>"`) {
		t.Error("control plane can acknowledge another serving member's consumers")
	}
	for _, member := range []string{"node-us-central1-0", "regional-front-us-central1-0", "regional-bind-us-central1-0", "regional-bind-us-central1-1"} {
		start := strings.Index(contents, `user: "`+member+`"`)
		if start < 0 {
			t.Fatalf("missing member %s", member)
		}
		policy := strings.SplitN(contents[start:], "\n    },", 2)[0]
		for _, suffix := range []string{"serving", "records", "acks"} {
			consumer := member + "-" + suffix
			for _, subject := range []string{
				"$JS.API.CONSUMER.INFO.DNS_PRIVATE." + consumer,
				"$JS.API.CONSUMER.CREATE.DNS_PRIVATE." + consumer,
				"$JS.API.CONSUMER.CREATE.DNS_PRIVATE." + consumer + ".>",
				"$JS.API.CONSUMER.MSG.NEXT.DNS_PRIVATE." + consumer,
				"$JS.ACK.DNS_PRIVATE." + consumer + ".>",
			} {
				if !strings.Contains(policy, `"`+subject+`"`) {
					t.Errorf("%s lacks startup/fetch permission %s", member, subject)
				}
			}
		}
		for _, broad := range []string{`"$JS.API.>"`, `"dns.private.records.>"`, `"dns.private.serving.>"`, `"dns.private.acks.>"`} {
			if strings.Contains(policy, broad) {
				t.Errorf("member %s has authority to write %s", member, broad)
			}
		}
	}
}

func TestReadOnlyServingImageHasWritableBINDWorkingDirectory(t *testing.T) {
	found := false
	for _, object := range deploymentObjects(t, "fleet.example.yaml") {
		if object.GetKind() != "Deployment" {
			continue
		}
		containers, _, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "containers")
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range containers {
			container := value.(map[string]interface{})
			if container["name"] != "serving" {
				continue
			}
			found = true
			mounts, _, err := unstructured.NestedSlice(container, "volumeMounts")
			if err != nil {
				t.Fatal(err)
			}
			runtimeVolume := ""
			for _, value := range mounts {
				mount := value.(map[string]interface{})
				if mount["mountPath"] == "/tmp" && mount["readOnly"] != true {
					runtimeVolume, _ = mount["name"].(string)
				}
			}
			if runtimeVolume == "" {
				t.Fatal("read-only serving image lacks writable BIND /tmp mount")
			}
			volumes, _, err := unstructured.NestedSlice(object.Object, "spec", "template", "spec", "volumes")
			if err != nil {
				t.Fatal(err)
			}
			writable := false
			for _, value := range volumes {
				volume := value.(map[string]interface{})
				if volume["name"] == runtimeVolume && volume["emptyDir"] != nil {
					writable = true
				}
			}
			if !writable {
				t.Fatal("BIND working-directory mount is not a per-member writable runtime volume")
			}
		}
	}
	if !found {
		t.Fatal("no serving container was checked")
	}
}

func verifyProjectReferenceSchemas(t *testing.T, ctx context.Context, cl client.Client) {
	t.Helper()
	zone := &dnsv1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "example-project"}, Spec: dnsv1alpha1.DNSZoneSpec{DomainName: "schema.internal", Visibility: dnsv1alpha1.DNSZoneVisibilityPrivate, DNSZoneClassName: "private"}}
	if err := cl.Create(ctx, zone); err != nil {
		t.Fatal(err)
	}
	ref := dnsv1alpha1.DNSObjectReference{Name: zone.Name, UID: zone.UID}
	contextRef := dnsv1alpha1.DNSObjectReference{Name: "context", UID: "context-uid"}
	cases := []struct {
		name   string
		object client.Object
		valid  bool
	}{
		{"registration-valid", &dnsv1alpha1.DNSRegistration{Spec: dnsv1alpha1.DNSRegistrationSpec{DNSZoneRef: ref, Name: "api", RecordTypes: []dnsv1alpha1.RRType{dnsv1alpha1.RRTypeA}, TTLSeconds: 30, PublicationPolicy: dnsv1alpha1.DNSPublicationPolicyEligibleContributions}}, true},
		{"registration-unpinned", &dnsv1alpha1.DNSRegistration{Spec: dnsv1alpha1.DNSRegistrationSpec{DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: zone.Name}, Name: "api", RecordTypes: []dnsv1alpha1.RRType{dnsv1alpha1.RRTypeA}, TTLSeconds: 30, PublicationPolicy: dnsv1alpha1.DNSPublicationPolicyEligibleContributions}}, false},
		{"registration-ttl31", &dnsv1alpha1.DNSRegistration{Spec: dnsv1alpha1.DNSRegistrationSpec{DNSZoneRef: ref, Name: "api", RecordTypes: []dnsv1alpha1.RRType{dnsv1alpha1.RRTypeA}, TTLSeconds: 31, PublicationPolicy: dnsv1alpha1.DNSPublicationPolicyEligibleContributions}}, false},
		{"association-valid", &dnsv1alpha1.DNSZoneAssociation{Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: ref, ResolverContextRef: contextRef}}, true},
		{"association-unpinned", &dnsv1alpha1.DNSZoneAssociation{Spec: dnsv1alpha1.DNSZoneAssociationSpec{DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: zone.Name}, ResolverContextRef: contextRef}}, false},
		{"naming-valid", &dnsv1alpha1.DNSNamingPolicy{Spec: dnsv1alpha1.DNSNamingPolicySpec{ResolverContextRef: contextRef, AdditionalNames: []dnsv1alpha1.DNSAdditionalNameRule{{RegistrationClass: dnsv1alpha1.DNSRegistrationClassServiceDiscovery, DNSZoneRef: ref, NamePrefix: "services"}}}}, true},
		{"naming-unpinned", &dnsv1alpha1.DNSNamingPolicy{Spec: dnsv1alpha1.DNSNamingPolicySpec{ResolverContextRef: contextRef, AdditionalNames: []dnsv1alpha1.DNSAdditionalNameRule{{RegistrationClass: dnsv1alpha1.DNSRegistrationClassServiceDiscovery, DNSZoneRef: dnsv1alpha1.DNSObjectReference{Name: zone.Name}, NamePrefix: "services"}}}}, false},
	}
	for _, tc := range cases {
		tc.object.SetName(tc.name)
		tc.object.SetNamespace("example-project")
		err := cl.Create(ctx, tc.object)
		if tc.valid && err != nil {
			t.Errorf("valid %s rejected: %v", tc.name, err)
		}
		if !tc.valid && !apierrors.IsInvalid(err) {
			t.Errorf("invalid %s should fail API schema validation: %v", tc.name, err)
		}
	}
}
