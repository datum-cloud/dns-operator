// SPDX-License-Identifier: AGPL-3.0-only
// Render the Kubernetes qualification from the shipped DNS configuration and RBAC.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"go.miloapis.com/dns-operator/internal/internaldns/model"
	runtime "go.miloapis.com/dns-operator/internal/internaldns/runtime"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type object = map[string]any

const ns = "internal-dns-system"
const projectNS = "project-e2e"
const image = "internal-dns-kubernetes:e2e"

var out, phase, platform, a, b, ai, bi string
var memberNames = []string{"node-us-central1-0", "regional-front-us-central1-0", "regional-bind-us-central1-0", "regional-bind-us-central1-1"}
var podNames = []string{"internal-dns-node", "internal-dns-regional-front", "internal-dns-regional-bind-0", "internal-dns-regional-bind-1"}

func main() {
	flag.StringVar(&out, "out", ".internal-dns-e2e/kubernetes", "Private generated environment directory")
	flag.StringVar(&phase, "phase", "bootstrap", "bootstrap or configure")
	flag.StringVar(&platform, "platform", "", "Platform admin kubeconfig")
	flag.StringVar(&a, "source-a", "", "Source A admin kubeconfig")
	flag.StringVar(&b, "source-b", "", "Source B admin kubeconfig")
	flag.StringVar(&ai, "source-a-internal", "", "Source A internal admin kubeconfig")
	flag.StringVar(&bi, "source-b-internal", "", "Source B internal admin kubeconfig")
	flag.Parse()
	var err error
	out, err = filepath.Abs(out)
	must(err)
	must(os.MkdirAll(out, 0700))
	switch phase {
	case "bootstrap":
		bootstrap()
	case "configure":
		configure()
	default:
		panic("unknown phase")
	}
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}
func obj(api, kind, name, namespace string) object {
	m := object{"name": name}
	if namespace != "" {
		m["namespace"] = namespace
	}
	if kind == "Namespace" && (name == ns || name == projectNS) {
		m["labels"] = object{"internal-dns.datum.net/qualification": "true"}
	}
	return object{"apiVersion": api, "kind": kind, "metadata": m}
}
func write(name string, value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(out, name), append(data, '\n'), 0600))
}
func list(name string, items []object) {
	write(name, object{"apiVersion": "v1", "kind": "List", "items": items})
}
func appendShippedRBAC(items []object) []object {
	data, err := os.ReadFile("config/internal-dns/rbac.yaml")
	must(err)
	d := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		var o object
		err = d.Decode(&o)
		if err == io.EOF {
			break
		}
		must(err)
		u := unstructured.Unstructured{Object: o}
		if u.GetNamespace() == "example-project" {
			u.SetNamespace(projectNS)
		}
		items = append(items, u.Object)
	}
	return items
}
func certificate(name string, dns []string, usages []string, ca bool, issuer string) object {
	o := obj("cert-manager.io/v1", "Certificate", name, ns)
	spec := object{"secretName": name, "commonName": name, "isCA": ca, "issuerRef": object{"name": issuer, "kind": "Issuer"}, "privateKey": object{"algorithm": "ECDSA", "size": 256}, "duration": "24h", "renewBefore": "8h"}
	if len(dns) > 0 {
		spec["dnsNames"] = dns
	}
	if len(usages) > 0 {
		spec["usages"] = usages
	}
	o["spec"] = spec
	return o
}
func bootstrap() {
	items := []object{obj("v1", "Namespace", ns, ""), obj("v1", "Namespace", projectNS, "")}
	items = appendShippedRBAC(items)
	self := obj("cert-manager.io/v1", "Issuer", "internal-dns-selfsigned", ns)
	self["spec"] = object{"selfSigned": object{}}
	items = append(items, self, certificate("internal-dns-ca", nil, []string{"cert sign", "crl sign"}, true, "internal-dns-selfsigned"))
	issuer := obj("cert-manager.io/v1", "Issuer", "internal-dns-ca", ns)
	issuer["spec"] = object{"ca": object{"secretName": "internal-dns-ca"}}
	items = append(items, issuer)
	items = append(items, certificate("internal-dns-nats-server", []string{"nats." + ns + ".svc", "nats." + ns + ".svc.cluster.local", "*.internal-dns-nats." + ns + ".svc.cluster.local"}, []string{"server auth", "client auth"}, false, "internal-dns-ca"))
	items = append(items, certificate("internal-dns-admission-tls", []string{"internal-dns-admission." + ns + ".svc", "dns-control-control-plane"}, []string{"server auth"}, false, "internal-dns-ca"))
	identities := append([]string{"control-a", "control-b"}, memberNames...)
	for _, name := range identities {
		items = append(items, certificate(name+"-nats", nil, []string{"client auth"}, false, "internal-dns-ca"))
	}
	// Kubernetes Secrets carry the broker ACL passwords and RNDC key; none are
	// emitted into ConfigMaps or test reports.
	users := make([]object, 0, len(identities))
	for _, name := range identities {
		password := randomSecret()
		secret := obj("v1", "Secret", name+"-password", ns)
		secret["stringData"] = object{"password": password}
		items = append(items, secret)
		allow := []string{"dns.private.serving.>", "dns.private.records.>", "$KV.DNS_PRIVATE_SNAPSHOTS.>", "$JS.API.>", "$JS.ACK.DNS_PRIVATE.control-plane-us-central1-shared-0-acks.>", "_INBOX.>"}
		subscribe := []string{"dns.private.acks.>", "_INBOX.>"}
		if !strings.HasPrefix(name, "control-") {
			allow = make([]string, 0, 24)
			allow = append(allow, model.AckSubject("us-central1", "shared-0", name), "$JS.API.STREAM.INFO.DNS_PRIVATE", "$JS.API.DIRECT.GET.KV_DNS_PRIVATE_SNAPSHOTS.>", "$JS.API.STREAM.INFO.KV_DNS_PRIVATE_SNAPSHOTS", "$JS.API.CONSUMER.CREATE.KV_DNS_PRIVATE_SNAPSHOTS.>", "$JS.API.CONSUMER.INFO.KV_DNS_PRIVATE_SNAPSHOTS.>", "$JS.API.CONSUMER.MSG.NEXT.KV_DNS_PRIVATE_SNAPSHOTS.>", "$JS.API.CONSUMER.DELETE.KV_DNS_PRIVATE_SNAPSHOTS.>", "_INBOX.>")
			subscribe = []string{"_INBOX.>"}
			for _, suffix := range []string{"serving", "records", "acks"} {
				c := name + "-" + suffix
				allow = append(allow, "$JS.API.CONSUMER.INFO.DNS_PRIVATE."+c, "$JS.API.CONSUMER.CREATE.DNS_PRIVATE."+c, "$JS.API.CONSUMER.CREATE.DNS_PRIVATE."+c+".>", "$JS.API.CONSUMER.MSG.NEXT.DNS_PRIVATE."+c, "$JS.ACK.DNS_PRIVATE."+c+".>")
			}
		}
		users = append(users, object{"user": name, "password": password, "permissions": object{"publish": object{"allow": allow}, "subscribe": object{"allow": subscribe}}})
	}
	tls := object{"cert_file": "/tls/tls.crt", "key_file": "/tls/tls.key", "ca_file": "/tls/ca.crt", "verify": true}
	cfg := object{"port": 4222, "http_port": 8222, "jetstream": object{"store_dir": "/data"}, "tls": tls, "authorization": object{"users": users}, "cluster": object{"name": "internal-dns-e2e", "port": 6222, "tls": tls, "routes": []string{"nats-route://internal-dns-nats-0.internal-dns-nats." + ns + ".svc.cluster.local:6222", "nats-route://internal-dns-nats-1.internal-dns-nats." + ns + ".svc.cluster.local:6222", "nats-route://internal-dns-nats-2.internal-dns-nats." + ns + ".svc.cluster.local:6222"}}}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	// NATS accepts JSON-style configuration but not JSON's HTML Unicode escapes.
	encoder.SetEscapeHTML(false)
	must(encoder.Encode(cfg))
	secret := obj("v1", "Secret", "internal-dns-nats-config", ns)
	secret["stringData"] = object{"nats.conf": encoded.String()}
	items = append(items, secret)
	rndc := obj("v1", "Secret", "internal-dns-rndc", ns)
	rndc["stringData"] = object{"rndc.key": fmt.Sprintf("key \"internal-dns\" { algorithm hmac-sha256; secret \"%s\"; };\n", randomSecret())}
	items = append(items, rndc)
	for _, name := range []string{"nats", "internal-dns-nats"} {
		s := obj("v1", "Service", name, ns)
		spec := object{"selector": object{"app": "internal-dns-nats"}, "ports": []object{{"name": "client", "port": 4222}, {"name": "cluster", "port": 6222}}}
		if name == "internal-dns-nats" {
			spec["clusterIP"] = "None"
			spec["publishNotReadyAddresses"] = true
		}
		s["spec"] = spec
		items = append(items, s)
	}
	broker := obj("apps/v1", "StatefulSet", "internal-dns-nats", ns)
	broker["spec"] = object{"replicas": 3, "serviceName": "internal-dns-nats", "podManagementPolicy": "Parallel", "selector": object{"matchLabels": object{"app": "internal-dns-nats"}}, "template": object{"metadata": object{"labels": object{"app": "internal-dns-nats"}}, "spec": object{"terminationGracePeriodSeconds": 2, "automountServiceAccountToken": false, "containers": []object{{"name": "nats", "image": "nats@sha256:e4bf19f15fd3218814a4e3c9e0064e1334bd8aa20d5984b9f1a0afd084f8cc00", "command": []string{"nats-server"}, "args": []string{"-c", "/config/nats.conf", "-n", "$(POD_NAME)"}, "env": []object{{"name": "POD_NAME", "valueFrom": object{"fieldRef": object{"fieldPath": "metadata.name"}}}}, "volumeMounts": []object{{"name": "config", "mountPath": "/config", "readOnly": true}, {"name": "tls", "mountPath": "/tls", "readOnly": true}, {"name": "data", "mountPath": "/data"}}, "readinessProbe": object{"httpGet": object{"path": "/healthz?js-enabled-only=true", "port": 8222}}, "resources": object{"requests": object{"cpu": "100m", "memory": "128Mi"}, "limits": object{"memory": "512Mi"}}}}, "volumes": []object{{"name": "config", "secret": object{"secretName": "internal-dns-nats-config"}}, {"name": "tls", "secret": object{"secretName": "internal-dns-nats-server"}}}}}, "volumeClaimTemplates": []object{{"metadata": object{"name": "data"}, "spec": object{"accessModes": []string{"ReadWriteOnce"}, "resources": object{"requests": object{"storage": "1Gi"}}}}}}
	items = append(items, broker)
	for i, name := range podNames {
		role := []string{"node", "regional-dnsdist", "regional-bind", "regional-bind"}[i]
		items = append(items, fleetPod(name, memberNames[i], role))
		for _, vol := range []string{"state", "config"} {
			pvc := obj("v1", "PersistentVolumeClaim", name+"-"+vol, ns)
			pvc["spec"] = object{"accessModes": []string{"ReadWriteOnce"}, "resources": object{"requests": object{"storage": "512Mi"}}}
			items = append(items, pvc)
		}
	}
	// Admission is a real Kubernetes TLS Service, reached by the independent
	// source API servers through a Kind-network NodePort.
	s := obj("v1", "Service", "internal-dns-admission", ns)
	s["spec"] = object{"type": "NodePort", "selector": object{"internal-dns-role": "control-plane"}, "ports": []object{{"name": "https", "port": 443, "targetPort": 9443, "nodePort": 30443}}}
	items = append(items, s)
	for _, name := range []string{"control-a", "control-b"} {
		items = append(items, controller(name))
	}
	probe := obj("v1", "Pod", "internal-dns-probe", ns)
	probe["spec"] = object{"terminationGracePeriodSeconds": 2, "automountServiceAccountToken": false, "securityContext": object{"runAsUser": 65532, "runAsGroup": 65532}, "containers": []object{{"name": "probe", "image": image, "imagePullPolicy": "Never", "command": []string{"/bin/sh", "-c", "sleep infinity"}, "securityContext": object{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": object{"drop": []string{"ALL"}}}}}}
	items = append(items, probe)
	list("platform-bootstrap.json", items)
	for _, project := range []string{"project-a", "project-b"} {
		list(project+"-bootstrap.json", sourceObjects(project))
	}
}
func randomSecret() string {
	data := make([]byte, 32)
	_, err := rand.Read(data)
	must(err)
	return base64.StdEncoding.EncodeToString(data)
}
func mount(name, path string, ro bool) object {
	return object{"name": name, "mountPath": path, "readOnly": ro}
}
func security() object {
	return object{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532, "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": object{"drop": []string{"ALL"}}}
}
func credentialsVolume(member string) object {
	return object{"name": "nats", "projected": object{"sources": []object{{"secret": object{"name": member + "-nats"}}, {"secret": object{"name": member + "-password"}}}}}
}
func fleetPod(name, member, role string) object {
	o := obj("v1", "Pod", name, ns)
	o["metadata"].(object)["labels"] = object{"internal-dns-role": role, "internal-dns-member": member}
	mounts := []object{mount("config", "/config", true), mount("nats", "/var/run/internal-dns/nats", true), mount("state", "/var/lib/internal-dns", false), mount("daemon-config", "/etc/internal-dns", false), mount("lease", "/run/internal-dns", false), mount("runtime", "/tmp", false), mount("rndc", "/etc/bind", true)}
	containers := make([]object, 0, 3)
	for _, process := range []string{"agent", "watchdog"} {
		containers = append(containers, object{"name": process, "image": image, "imagePullPolicy": "Never", "command": []string{"/bin/sh", "-ec", "while [ ! -s /config/config.json ]; do sleep 1; done; exec /internal-dns --role=" + process + " --config=/config/config.json"}, "securityContext": security(), "volumeMounts": mounts, "resources": object{"requests": object{"cpu": "50m", "memory": "64Mi"}, "limits": object{"memory": "512Mi"}}})
	}
	sec := security()
	sec["capabilities"] = object{"drop": []string{"ALL"}, "add": []string{"NET_BIND_SERVICE"}}
	containers = append(containers, object{"name": "serving", "image": image, "imagePullPolicy": "Never", "command": []string{"/usr/local/sbin/internal-dns-daemons", role}, "securityContext": sec, "volumeMounts": mounts, "resources": object{"requests": object{"cpu": "100m", "memory": "128Mi"}, "limits": object{"memory": "512Mi"}}})
	volumes := []object{{"name": "config", "configMap": object{"name": name + "-config", "optional": true}}, credentialsVolume(member), {"name": "state", "persistentVolumeClaim": object{"claimName": name + "-state"}}, {"name": "daemon-config", "persistentVolumeClaim": object{"claimName": name + "-config"}}, {"name": "lease", "emptyDir": object{"medium": "Memory"}}, {"name": "runtime", "emptyDir": object{}}, {"name": "rndc", "secret": object{"secretName": "internal-dns-rndc"}}}
	// The aliases model the service-side destination contract only. The separate
	// probe has no ability to write routes or send trusted PROXY traffic.
	init := []object{}
	if role == "node" || role == "regional-dnsdist" {
		prefix := "fd53::/64"
		if role != "node" {
			prefix = "fd54::/64"
		}
		init = append(init, object{"name": "destination-fixture", "image": image, "imagePullPolicy": "Never", "command": []string{"/bin/sh", "-ec", "ip -6 route replace local " + prefix + " dev lo"}, "securityContext": object{"runAsUser": 0, "capabilities": object{"drop": []string{"ALL"}, "add": []string{"NET_ADMIN"}}}})
	}
	o["spec"] = object{"serviceAccountName": "internal-dns-fleet", "terminationGracePeriodSeconds": 2, "automountServiceAccountToken": false, "shareProcessNamespace": true, "securityContext": object{"fsGroup": 65532, "fsGroupChangePolicy": "OnRootMismatch", "sysctls": []object{{"name": "net.ipv4.ip_unprivileged_port_start", "value": "0"}}}, "containers": containers, "initContainers": init, "volumes": volumes}
	return o
}
func controller(name string) object {
	o := obj("apps/v1", "Deployment", name, ns)
	labels := object{"app": name, "internal-dns-role": "control-plane"}
	mounts := []object{mount("config", "/config", true), mount("nats", "/var/run/internal-dns/nats", true), mount("admission", "/var/run/internal-dns/admission", true), mount("sources", "/sources", true)}
	o["spec"] = object{"replicas": 1, "selector": object{"matchLabels": object{"app": name}}, "template": object{"metadata": object{"labels": labels}, "spec": object{"terminationGracePeriodSeconds": 2, "serviceAccountName": "internal-dns-control-plane", "containers": []object{{"name": "control-plane", "image": image, "imagePullPolicy": "Never", "command": []string{"/bin/sh", "-ec", "while [ ! -s /config/config.json ] || [ ! -s /sources/project-a.json ]; do sleep 1; done; exec /internal-dns --role=control-plane --config=/config/config.json"}, "securityContext": security(), "volumeMounts": mounts, "ports": []object{{"name": "admission", "containerPort": 9443}}, "readinessProbe": object{"tcpSocket": object{"port": 9443}}, "resources": object{"requests": object{"cpu": "100m", "memory": "128Mi"}, "limits": object{"memory": "768Mi"}}}}, "volumes": []object{{"name": "config", "configMap": object{"name": name + "-config", "optional": true}}, credentialsVolume(name), {"name": "admission", "secret": object{"secretName": "internal-dns-admission-tls"}}, {"name": "sources", "secret": object{"secretName": "internal-dns-sources", "optional": true}}}}}}
	return o
}
func sourceObjects(project string) []object {
	items := []object{obj("v1", "Namespace", ns, ""), obj("v1", "Namespace", projectNS, ""), obj("v1", "Namespace", "compute-system", ""), obj("v1", "Namespace", "network-system", "")}
	items = appendShippedRBAC(items)
	filtered := items[:0]
	for _, item := range items {
		if item["kind"] == "ClusterRoleBinding" && item["metadata"].(map[string]any)["name"] == "internal-dns-control-plane" {
			continue
		}
		filtered = append(filtered, item)
	}
	items = filtered
	worker := obj("rbac.authorization.k8s.io/v1", "RoleBinding", "internal-dns-source-worker", projectNS)
	worker["roleRef"] = object{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "internal-dns-control-plane"}
	worker["subjects"] = []object{{"kind": "ServiceAccount", "name": "internal-dns-control-plane", "namespace": ns}}
	sar := obj("rbac.authorization.k8s.io/v1", "ClusterRole", "internal-dns-source-authorization", "")
	sar["rules"] = []object{{"apiGroups": []string{"authorization.k8s.io"}, "resources": []string{"subjectaccessreviews"}, "verbs": []string{"create"}}}
	sarBinding := obj("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "internal-dns-source-authorization", "")
	sarBinding["roleRef"] = object{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "internal-dns-source-authorization"}
	sarBinding["subjects"] = worker["subjects"]
	items = append(items, worker, sar, sarBinding)
	accounts := []struct{ name, namespace, role string }{{"dns-publisher", "compute-system", "internal-dns-product-writer"}, {"dns-grant-issuer", "compute-system", "internal-dns-grant-issuer"}, {"dns-vpc-integration", "network-system", "internal-dns-vpc-integration"}, {"internal-dns-control-plane", ns, ""}}
	for _, account := range accounts {
		if account.role != "" {
			items = append(items, obj("v1", "ServiceAccount", account.name, account.namespace))
			rb := obj("rbac.authorization.k8s.io/v1", "RoleBinding", account.name, projectNS)
			rb["roleRef"] = object{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": account.role}
			rb["subjects"] = []object{{"kind": "ServiceAccount", "name": account.name, "namespace": account.namespace}}
			items = append(items, rb)
		}
		// Only the same service account and this fixed project parent can be
		// impersonated. This replaces Milo's trusted parent metadata in Kind.
		r := obj("rbac.authorization.k8s.io/v1", "ClusterRole", account.name+"-parent", "")
		r["rules"] = []object{{"apiGroups": []string{"authentication.k8s.io"}, "resources": []string{"userextras/iam.miloapis.com/parent-name"}, "resourceNames": []string{project}, "verbs": []string{"impersonate"}}}
		rb := obj("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", account.name+"-parent", "")
		rb["roleRef"] = object{"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": account.name + "-parent"}
		rb["subjects"] = []object{{"kind": "ServiceAccount", "name": account.name, "namespace": account.namespace}}
		items = append(items, r, rb)
		self := obj("rbac.authorization.k8s.io/v1", "Role", account.name+"-self", account.namespace)
		self["rules"] = []object{{"apiGroups": []string{""}, "resources": []string{"serviceaccounts"}, "resourceNames": []string{account.name}, "verbs": []string{"impersonate"}}}
		binding := obj("rbac.authorization.k8s.io/v1", "RoleBinding", account.name+"-self", account.namespace)
		binding["roleRef"] = object{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": account.name + "-self"}
		binding["subjects"] = rb["subjects"]
		items = append(items, self, binding)
	}
	c := obj("dns.networking.miloapis.com/v1alpha1", "DNSZoneClass", "private-bind", "")
	c["metadata"].(object)["labels"] = object{"internal-dns.datum.net/qualification": "true"}
	c["spec"] = object{"controllerName": "bind", "nameServerPolicy": object{"mode": "Static", "static": object{"servers": []string{"ns.internal."}}}}
	items = append(items, c)
	return items
}
func client(path string) *kubernetes.Clientset {
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	must(err)
	cl, err := kubernetes.NewForConfig(cfg)
	must(err)
	return cl
}
func ipv6(cl *kubernetes.Clientset, name string) string {
	p, err := cl.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
	must(err)
	for _, ip := range p.Status.PodIPs {
		addr, err := netip.ParseAddr(ip.IP)
		if err == nil && addr.Is6() {
			return ip.IP
		}
	}
	panic("pod has no IPv6 address: " + name)
}
func tokenConfig(admin, internal, project, account, namespace, name string) []byte {
	seconds := int64(7200)
	req, err := client(admin).CoreV1().ServiceAccounts(namespace).CreateToken(context.Background(), account, &authv1.TokenRequest{Spec: authv1.TokenRequestSpec{ExpirationSeconds: &seconds}}, metav1.CreateOptions{})
	must(err)
	cfg, err := clientcmd.LoadFromFile(admin)
	must(err)
	ctx := cfg.Contexts[cfg.CurrentContext]
	cluster := *cfg.Clusters[ctx.Cluster]
	subject := "system:serviceaccount:" + namespace + ":" + account
	auth := &clientcmdapi.AuthInfo{Token: req.Status.Token, Impersonate: subject, ImpersonateUserExtra: map[string][]string{"iam.miloapis.com/parent-name": {project}}}
	result := clientcmdapi.Config{Clusters: map[string]*clientcmdapi.Cluster{"source": &cluster}, Contexts: map[string]*clientcmdapi.Context{"source": {Cluster: "source", AuthInfo: "scoped", Namespace: projectNS}}, CurrentContext: "source", AuthInfos: map[string]*clientcmdapi.AuthInfo{"scoped": auth}}
	host, err := clientcmd.Write(result)
	must(err)
	must(os.WriteFile(filepath.Join(out, project+"-"+name+".kubeconfig"), host, 0600))
	if internal != "" {
		raw, err := clientcmd.LoadFromFile(internal)
		must(err)
		ic := raw.Contexts[raw.CurrentContext]
		result.Clusters["source"] = raw.Clusters[ic.Cluster]
	}
	data, err := clientcmd.Write(result)
	must(err)
	return data
}
func exampleConfigs(filename string) map[string]runtime.Config {
	data, err := os.ReadFile("config/internal-dns/" + filename)
	must(err)
	dec := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	result := map[string]runtime.Config{}
	for {
		var o object
		err = dec.Decode(&o)
		if err == io.EOF {
			break
		}
		must(err)
		if o["kind"] != "ConfigMap" {
			continue
		}
		m := o["metadata"].(map[string]any)
		d := o["data"].(map[string]any)
		raw, ok := d["config.json"].(string)
		if !ok {
			continue
		}
		var cfg runtime.Config
		must(json.Unmarshal([]byte(raw), &cfg))
		result[m["name"].(string)] = cfg
	}
	return result
}
func configMap(name string, cfg runtime.Config) object {
	raw, err := json.Marshal(cfg)
	must(err)
	o := obj("v1", "ConfigMap", name, ns)
	o["data"] = object{"config.json": string(raw)}
	return o
}
func configure() {
	cl := client(platform)
	nodeIP := ipv6(cl, podNames[0])
	frontIP := ipv6(cl, podNames[1])
	bindIP := []string{ipv6(cl, podNames[2]), ipv6(cl, podNames[3])}
	sources := obj("v1", "Secret", "internal-dns-sources", ns)
	sourceData := object{}
	projectRows := make([]object, 0, 2)
	for i, project := range []string{"project-a", "project-b"} {
		admin := []string{a, b}[i]
		internal := []string{ai, bi}[i]
		sourceData[project+".json"] = string(tokenConfig(admin, internal, project, "internal-dns-control-plane", ns, "worker"))
		tokenConfig(admin, "", project, "dns-publisher", "compute-system", "product")
		tokenConfig(admin, "", project, "dns-grant-issuer", "compute-system", "issuer")
		tokenConfig(admin, "", project, "dns-vpc-integration", "network-system", "integration")
		projectRows = append(projectRows, object{"name": project, "projectUID": project + "-uid", "sourceClusterUID": []string{"source-cluster-a-uid", "source-cluster-b-uid"}[i], "namespace": projectNS, "adminKubeconfig": admin, "productKubeconfig": filepath.Join(out, project+"-product.kubeconfig"), "issuerKubeconfig": filepath.Join(out, project+"-issuer.kubeconfig"), "integrationKubeconfig": filepath.Join(out, project+"-integration.kubeconfig"), "publisherSubject": "system:serviceaccount:compute-system:dns-publisher", "consumerAddress": []string{"fd53::a", "fd53::b"}[i]})
	}
	sources["stringData"] = sourceData
	items := make([]object, 0, 8)
	items = append(items, sources)
	cfg := exampleConfigs("control-plane.example.yaml")["internal-dns-control-plane-config"]
	cfg.ConsumerPrefix = "fd53::/64"
	cfg.ClusterPrefix = "fd54::/64"
	cfg.ManagedDomainSuffix = "managed.internal"
	cfg.OwnershipLeaseSeconds = 10
	cfg.Projects = []runtime.ProjectConfig{{Name: "project-a", ProjectUID: "project-a-uid", SourceClusterUID: "source-cluster-a-uid", Namespace: projectNS, Kubeconfig: "/sources/project-a.json"}, {Name: "project-b", ProjectUID: "project-b-uid", SourceClusterUID: "source-cluster-b-uid", Namespace: projectNS, Kubeconfig: "/sources/project-b.json"}}
	cfg.NodeBackends = []model.Backend{{MemberID: "node-bind-0", Address: "127.0.0.1", Port: 5300}}
	cfg.ClusterBackends = []model.Backend{{MemberID: memberNames[2], Address: bindIP[0], Port: 5300}, {MemberID: memberNames[3], Address: bindIP[1], Port: 5300}}
	cfg.Admission.PlatformSubjects = []string{"system:serviceaccount:" + ns + ":internal-dns-control-plane", "kubernetes-admin"}
	cfg.Admission.IntegrationSubjects = []string{"system:serviceaccount:network-system:dns-vpc-integration"}
	cfg.Admission.MaxAccessLeaseSeconds = 600
	for _, name := range []string{"control-a", "control-b"} {
		c := cfg
		c.NATS.Name = name
		c.NATS.Username = name
		must(c.Validate("control-plane"))
		items = append(items, configMap(name+"-config", c))
	}
	configs := exampleConfigs("fleet.example.yaml")
	for i, key := range []string{"internal-dns-node-0-config", "internal-dns-regional-front-0-config", "internal-dns-regional-bind-0-config", "internal-dns-regional-bind-1-config"} {
		c := configs[key]
		c.Watchdog.StartupGraceSeconds = 30
		if i == 0 {
			c.Agent.Render.NodeDNSDist.Backends = cfg.NodeBackends
			c.Agent.Render.NodeDNSDist.ACLs = []string{"fd00::/8", "::1/128"}
			c.Agent.Render.NodeBIND.ListenAddress = "127.0.0.1"
			c.Agent.Render.NodeBIND.ProxyPeers = []string{"127.0.0.1"}
		}
		if i == 1 {
			c.Agent.Render.ClusterDNSDist.Backends = cfg.ClusterBackends
			c.Agent.Render.ClusterDNSDist.ACLs = []string{nodeIP + "/128", frontIP + "/128", "::1/128"}
		}
		if i >= 2 {
			c.Agent.Render.ClusterBIND.ListenAddress = bindIP[i-2]
			c.Agent.Render.ClusterBIND.ProxyPeers = []string{frontIP, bindIP[i-2]}
			c.Agent.PublicationDNSProbe.Server = "[" + bindIP[i-2] + "]:5300"
		}
		must(c.Validate("agent"))
		must(c.Validate("watchdog"))
		items = append(items, configMap(podNames[i]+"-config", c))
	}
	routes := obj("v1", "Pod", "internal-dns-network-fixture", ns)
	// Kind's default masquerading would erase the pod peer identity for service
	// destinations outside its PodCIDR. Exempt only these two fixture prefixes;
	// never widen the protected BIND/front ACLs to the host's shared address.
	routeCommand := fmt.Sprintf("ip -6 route replace fd53::/64 via %s; ip -6 route replace fd54::/64 via %s; for prefix in fd53::/64 fd54::/64; do while ip6tables -t nat -C POSTROUTING -d \"$prefix\" -j RETURN 2>/dev/null; do ip6tables -t nat -D POSTROUTING -d \"$prefix\" -j RETURN; done; ip6tables -t nat -I POSTROUTING 1 -d \"$prefix\" -j RETURN; done", nodeIP, frontIP)
	routes["spec"] = object{"hostNetwork": true, "restartPolicy": "Never", "terminationGracePeriodSeconds": 2, "automountServiceAccountToken": false, "containers": []object{{"name": "routes", "image": image, "imagePullPolicy": "Never", "command": []string{"/bin/sh", "-ec", routeCommand}, "securityContext": object{"runAsUser": 0, "capabilities": object{"drop": []string{"ALL"}, "add": []string{"NET_ADMIN"}}}}}}
	items = append(items, routes)
	list("platform-configure.json", items)
	// No tokens or certificate contents are included in the public contract.
	write("environment.json", object{"platformKubeconfig": platform, "namespace": ns, "region": "us-central1", "shard": "shared-0", "probePod": "internal-dns-probe", "probeContainer": "probe", "probeBinary": "/dns-qualify", "controllerDeployments": []string{"control-a", "control-b"}, "brokerStatefulSet": "internal-dns-nats", "projects": projectRows, "regionalMembers": memberNames[2:], "members": memberNames, "limitations": []string{"Service-side route fixture, not Galactic VPC authorization", "Independent Kind project APIs, not Milo project discovery", "One host and one region, not independent regional failure"}})
}
