#!/usr/bin/env python3
"""Qualify two active control planes over two independent source APIs."""

from __future__ import annotations

import argparse
import base64
from datetime import datetime, timedelta, timezone
import importlib.util
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import time
import urllib.request


HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
RUN_SPEC = importlib.util.spec_from_file_location("internal_dns_single_e2e", HERE / "run.py")
if RUN_SPEC is None or RUN_SPEC.loader is None:
    raise RuntimeError("cannot load the single-control-plane E2E harness")
single = importlib.util.module_from_spec(RUN_SPEC)
RUN_SPEC.loader.exec_module(single)

PLATFORM_CLUSTER = "datum-internal-dns-platform-e2e"
SOURCE_CLUSTERS = {
    "project-a": "datum-internal-dns-source-a-e2e",
    "project-b": "datum-internal-dns-source-b-e2e",
}
PROJECT_NAMESPACE = "project-e2e"
PLATFORM_NAMESPACE = "internal-dns-system"
SUBJECT = "system:serviceaccount:compute-system:dns-publisher"
PROJECTS = {
    "project-a": {"projectUID": "project-a-uid", "sourceClusterUID": "source-cluster-a-uid"},
    "project-b": {"projectUID": "project-b-uid", "sourceClusterUID": "source-cluster-b-uid"},
}
IMAGES = {
    "nats": "nats@sha256:e4bf19f15fd3218814a4e3c9e0064e1334bd8aa20d5984b9f1a0afd084f8cc00",
    "dnsdist": "powerdns/dnsdist-20@sha256:4ed9af56729c7021795dc478421237ab9cf6c7ceec8734677405e7617f06d52f",
    "bind": "internetsystemsconsortium/bind9@sha256:071465f88068854d0ceadd9b985fb1acae4071e80754e545cd811fde57541ad0",
    "runner": "docker@sha256:b1805116a6a86cc591b5d5f60a910a0715cdcc9d18d866ad68b1457ead25c35c",
}


class MultiControlPlaneHarness(single.Harness):
    def __init__(self, keep: bool, results: Path):
        super().__init__(keep, results)
        self.platform_kubeconfig = self.work / "platform.kubeconfig"
        self.kubeconfig = self.platform_kubeconfig
        self.source_kubeconfigs = {name: self.work / f"{name}-admin.kubeconfig" for name in PROJECTS}
        self.compute_kubeconfigs = {name: self.work / f"{name}-compute.kubeconfig" for name in PROJECTS}
        self.control_configs: dict[str, Path] = {}
        self.topology: dict[str, object] = {}
        self.owner_evidence: dict[str, object] = {}
        self.paused_containers: set[str] = set()
        single.RESULTS.clear()

    def query(self, server: str, name: str, tcp: bool = False) -> tuple[str, list[str]]:
        """Read status and answers from one DNS transaction."""
        flags = ["+tcp"] if tcp else []
        output = self.compose(
            "exec", "-T", "probe", "dig", f"@{server}", name, "A",
            "+noall", "+comments", "+answer", *flags,
        ).stdout
        status = next(
            (line.split("status:", 1)[1].split(",", 1)[0].strip() for line in output.splitlines() if "status:" in line),
            "UNKNOWN",
        )
        answers = []
        for line in output.splitlines():
            fields = line.split()
            if line.startswith(";") or len(fields) < 5 or fields[-2].upper() != "A":
                continue
            answers.append(fields[-1])
        return status, answers

    def source_kubectl(
        self,
        project: str,
        *args: str,
        compute: bool = False,
        check: bool = True,
        stdin: str | None = None,
        timeout: float | None = 30,
    ) -> subprocess.CompletedProcess[str]:
        config = self.compute_kubeconfigs[project] if compute else self.source_kubeconfigs[project]
        command = ["kubectl", "--kubeconfig", str(config)]
        if not compute:
            # Multi-project admission deliberately rejects an authenticated
            # caller whose project parent is absent or ambiguous. Exercise the
            # same parent-scoped identity contract for fixture administration;
            # the native Kind admin credential remains the impersonator used
            # by the control-plane clients themselves.
            command.extend([
                "--as=kubernetes-admin",
                "--as-group=system:masters",
                f"--as-user-extra=iam.miloapis.com/parent-name={project}",
            ])
        if timeout is not None:
            command.append(f"--request-timeout={int(timeout)}s")
        return self.run(*command, *args, check=check, stdin=stdin, timeout=timeout + 5 if timeout else None)

    def publisher(self, project: str, *args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
        return self.run(
            str(HERE / "compute_publisher.py"),
            "--kubeconfig", str(self.compute_kubeconfigs[project]),
            "--namespace", PROJECT_NAMESPACE,
            "--source-cluster-uid", PROJECTS[project]["sourceClusterUID"],
            *args,
            check=check,
        )

    def setup(self) -> None:
        single.require_suite_docker_host()
        for command in ("docker", "kind", "kubectl", "go", "openssl"):
            if shutil.which(command) is None:
                raise RuntimeError(f"missing prerequisite: {command}")
        self.capture_source()
        for name in ("node-0", "regional-front", "regional-0", "regional-1", "node-0-watchdog", "regional-front-watchdog", "regional-0-watchdog", "regional-1-watchdog"):
            self.run("docker", "rm", "-f", f"datum-internal-dns-{name}", check=False)
        cluster_configs = [(PLATFORM_CLUSTER, self.platform_kubeconfig)] + [
            (SOURCE_CLUSTERS[name], self.source_kubeconfigs[name]) for name in PROJECTS
        ]
        for cluster, config in cluster_configs:
            self.run("kind", "delete", "cluster", "--name", cluster, "--kubeconfig", str(config), check=False)
        self.run("kind", "delete", "cluster", "--name", single.CLUSTER, check=False)
        self.compose("down", "--volumes", "--remove-orphans", check=False)
        self.seed_runtime()
        self.compose("up", "-d", "--wait")

        cluster_configs = [(PLATFORM_CLUSTER, self.platform_kubeconfig)] + [
            (SOURCE_CLUSTERS[name], self.source_kubeconfigs[name]) for name in PROJECTS
        ]
        for cluster, config in cluster_configs:
            self.run("kind", "create", "cluster", "--name", cluster, "--kubeconfig", str(config), "--wait", "90s", timeout=120)
            lookup = self.run("docker", "exec", f"{cluster}-control-plane", "getent", "hosts", "host.docker.internal", check=False)
            if lookup.returncode:
                raise RuntimeError(f"Kind node {cluster} cannot resolve host.docker.internal")

        crd_dir = self.work / "crds"
        crd_dir.mkdir()
        self.run(
            str(ROOT / "bin/controller-gen"), "crd", "paths=./api/v1alpha1",
            f"output:crd:artifacts:config={crd_dir}", cwd=self.source,
        )
        self.kubectl("apply", "-f", str(crd_dir))
        self.kubectl("create", "namespace", PLATFORM_NAMESPACE)
        for project in PROJECTS:
            self.source_kubectl(project, "apply", "-f", str(crd_dir))
            self.install_source_bootstrap(project)

        self.run(
            "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
            "-keyout", str(self.certs / "tls.key"), "-out", str(self.certs / "tls.crt"),
            "-subj", "/CN=host.docker.internal", "-addext", "subjectAltName=DNS:host.docker.internal",
        )
        # The parent freezes runtime source changes before invoking the runner.
        # Both binaries are built from this per-run immutable capture.
        self.run("go", "build", "-o", str(self.binary), "./cmd/internal-dns", cwd=self.source)
        linux_env = os.environ.copy()
        linux_env.update({"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": "arm64"})
        process = subprocess.run(
            ["go", "build", "-o", str(self.linux_binary), "./cmd/internal-dns"],
            cwd=self.source, env=linux_env, text=True, capture_output=True,
        )
        if process.returncode:
            raise RuntimeError(f"linux agent build failed:\n{process.stdout}\n{process.stderr}")

        self.topology = {
            "platformAPI": {"kindCluster": PLATFORM_CLUSTER, "namespace": PLATFORM_NAMESPACE},
            "sourceAPIs": [
                {
                    "project": name,
                    "kindCluster": SOURCE_CLUSTERS[name],
                    "namespace": PROJECT_NAMESPACE,
                    **PROJECTS[name],
                }
                for name in PROJECTS
            ],
            "controlPlanes": ["control-a", "control-b"],
            "sharedFleet": ["node-0", "regional-front", "regional-0", "regional-1"],
            "admission": {
                "fixtureFrontDoor": "https://host.docker.internal:19443",
                "verifiedBackends": ["https://host.docker.internal:9443", "https://host.docker.internal:9444"],
                "productionLoadBalancerQualified": False,
            },
            "transport": {"kind": "single development NATS JetStream", "distributedRegions": False},
        }
        self.pass_check("three independent Kind API servers created", clusters=[PLATFORM_CLUSTER, *SOURCE_CLUSTERS.values()])

    def install_source_bootstrap(self, project: str) -> None:
        parent = project
        items = [
            {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": PROJECT_NAMESPACE}},
            {"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "compute-system"}},
            {"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": "dns-publisher", "namespace": "compute-system"}},
            {
                "apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
                "metadata": {"name": "compute-dns-publisher", "namespace": PROJECT_NAMESPACE},
                "rules": [
                    {"apiGroups": ["dns.networking.miloapis.com"], "resources": ["dnsmanagednamespaces", "dnsresolvercontexts"], "verbs": ["get", "list", "watch"]},
                    {"apiGroups": ["dns.networking.miloapis.com"], "resources": ["dnsregistrations", "dnscontributiongrants", "dnsrecordcontributions"], "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"]},
                    {"apiGroups": ["dns.networking.miloapis.com"], "resources": ["dnsrecordcontributions/status"], "verbs": ["get", "update", "patch"]},
                ],
            },
            {
                "apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
                "metadata": {"name": "compute-dns-publisher", "namespace": PROJECT_NAMESPACE},
                "subjects": [{"kind": "ServiceAccount", "name": "dns-publisher", "namespace": "compute-system"}],
                "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": "compute-dns-publisher"},
            },
            {
                "apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
                "metadata": {"name": "compute-dns-parent-impersonation"},
                "rules": [
                    {"apiGroups": [""], "resources": ["serviceaccounts"], "resourceNames": ["dns-publisher"], "verbs": ["impersonate"]},
                    {
                        "apiGroups": ["authentication.k8s.io"],
                        "resources": ["userextras/iam.miloapis.com/parent-name"],
                        "resourceNames": [parent], "verbs": ["impersonate"],
                    },
                ],
            },
            {
                "apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
                "metadata": {"name": "compute-dns-parent-impersonation"},
                "subjects": [{"kind": "ServiceAccount", "name": "dns-publisher", "namespace": "compute-system"}],
                "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "ClusterRole", "name": "compute-dns-parent-impersonation"},
            },
        ]
        self.source_kubectl(project, "apply", "-f", "-", stdin=json.dumps({"apiVersion": "v1", "kind": "List", "items": items}))
        token = self.source_kubectl(project, "-n", "compute-system", "create", "token", "dns-publisher", "--duration=1h").stdout.strip()
        admin = json.loads(self.source_kubectl(project, "config", "view", "--raw", "-o", "json").stdout)
        context = next(item["context"] for item in admin["contexts"] if item["name"] == admin["current-context"])
        cluster = next(item["cluster"] for item in admin["clusters"] if item["name"] == context["cluster"])
        config = {
            "apiVersion": "v1", "kind": "Config", "current-context": "compute",
            "clusters": [{"name": "source", "cluster": cluster}],
            "contexts": [{"name": "compute", "context": {"cluster": "source", "user": "compute", "namespace": PROJECT_NAMESPACE}}],
            "users": [{"name": "compute", "user": {
                "token": token, "as": SUBJECT,
                "as-user-extra": {"iam.miloapis.com/parent-name": [parent]},
            }}],
        }
        self.compute_kubeconfigs[project].write_text(json.dumps(config))

    def write_configs(self) -> dict[str, Path]:
        paths = super().write_configs()
        base = json.loads(paths["control"].read_text())
        base["projects"] = [
            {
                "name": name,
                **PROJECTS[name],
                "namespace": PROJECT_NAMESPACE,
                "kubeconfig": str(self.source_kubeconfigs[name]),
                # Accelerate only the blackholed source. The healthy source
                # keeps the production default bound under concurrent load.
                "kubeAPI": {"requestTimeoutSeconds": 1 if name == "project-a" else 10},
            }
            for name in PROJECTS
        ]
        base["admission"]["port"] = 9443
        control_a = json.loads(json.dumps(base))
        control_b = json.loads(json.dumps(base))
        control_b["admission"]["port"] = 9444
        for name, value in (("control-a", control_a), ("control-b", control_b)):
            path = self.work / f"{name}.json"
            path.write_text(json.dumps(value, indent=2))
            paths[name] = path
            self.control_configs[name] = path
        return paths

    def install_webhooks(self) -> None:
        if not getattr(self, "vm_admission_frontdoor", False):
            self.wait("admission fixture front door", lambda: socket.create_connection(("127.0.0.1", 19443), timeout=0.5), timeout=30)
        ca = base64.b64encode((self.certs / "tls.crt").read_bytes()).decode()
        template = (HERE / "fixtures/validating-webhook.yaml.tmpl").read_text().replace("${CA_BUNDLE}", ca).replace(":9443/", ":19443/")
        if getattr(self, "vm_admission_frontdoor", False):
            template = template.replace("host.docker.internal:19443", f"{self.admission_frontdoor_address}:19443")
        for project in PROJECTS:
            self.source_kubectl(project, "apply", "-f", "-", stdin=template)
            allowed = self.source_kubectl(project, "auth", "can-i", "create", "dnsrecordcontributions", compute=True).stdout.strip()
            secrets_result = self.source_kubectl(project, "auth", "can-i", "get", "secrets", compute=True, check=False)
            platform_result = self.source_kubectl(project, "auth", "can-i", "create", "dnspublicationmanifests", compute=True, check=False)
            secrets = secrets_result.stdout.strip()
            platform = platform_result.stdout.strip()
            if allowed != "yes" or secrets_result.returncode != 1 or secrets != "no" or platform_result.returncode != 1 or platform != "no":
                raise AssertionError(f"unexpected scoped publisher permissions for {project}: contribution={allowed}, secrets={secrets}/{secrets_result.returncode}, platform={platform}/{platform_result.returncode}")
        self.pass_check("scoped publishers have project DNS API access without platform or secret access")

    def create_source_objects(self, project: str) -> tuple[dict, dict]:
        destination = "fd53::a" if project == "project-a" else "fd53::b"
        value = {
            "apiVersion": "v1", "kind": "List", "items": [
                {
                    "apiVersion": "dns.networking.miloapis.com/v1alpha1", "kind": "DNSResolverContext",
                    "metadata": {"name": "vpc", "namespace": PROJECT_NAMESPACE},
                    "spec": {"consumerID": f"{project}/vpc", "managedNamespace": {"enabled": True}},
                },
                {
                    "apiVersion": "dns.networking.miloapis.com/v1alpha1", "kind": "DNSZone",
                    "metadata": {"name": "prod", "namespace": PROJECT_NAMESPACE},
                    "spec": {"domainName": "prod.internal", "dnsZoneClassName": "private-bind", "visibility": "Private"},
                },
            ],
        }
        self.source_kubectl(project, "apply", "-f", "-", stdin=json.dumps(value))
        zone = json.loads(self.source_kubectl(project, "-n", PROJECT_NAMESPACE, "get", "dnszone", "prod", "-o", "json").stdout)
        def issued():
            context = json.loads(self.source_kubectl(project, "-n", PROJECT_NAMESPACE, "get", "dnsresolvercontext", "vpc", "-o", "json").stdout)
            ready = next((row for row in context.get("status", {}).get("conditions", [])
                          if row.get("type") == "Ready"), None)
            return context if (
                int(context.get("status", {}).get("accessWriterEpoch", 0)) > 0
                and ready and ready.get("status") == "True"
                and int(ready.get("observedGeneration", 0)) == int(context["metadata"]["generation"])
            ) else None

        context = self.wait(f"{project} resolver context epoch", issued, timeout=60)
        access = {
            "apiVersion": "dns.networking.miloapis.com/v1alpha1", "kind": "DNSResolverAccessBinding",
            "metadata": {"name": "vpc-e2e", "namespace": PROJECT_NAMESPACE},
            "spec": {
                "contextRef": {"name": "vpc", "uid": context["metadata"]["uid"]},
                "region": "e2e", "queryIdentity": {"type": "DestinationAddress", "value": destination},
                "port": 53, "transports": ["UDP", "TCP"],
                "authorization": {
                    "writerEpoch": context["status"]["accessWriterEpoch"], "sequence": 1,
                    "validUntil": (datetime.now(timezone.utc) + timedelta(seconds=600)).isoformat().replace("+00:00", "Z"),
                },
            },
        }
        self.source_kubectl(project, "apply", "-f", "-", stdin=json.dumps(access))
        self.wait(f"{project} access accepted", lambda: self.source_condition(project, "dnsresolveraccessbinding", "vpc-e2e", "Accepted"), timeout=60)
        assoc = {
            "apiVersion": "dns.networking.miloapis.com/v1alpha1", "kind": "DNSZoneAssociation",
            "metadata": {"name": "prod-vpc", "namespace": PROJECT_NAMESPACE},
            "spec": {
                "dnsZoneRef": {"name": "prod", "uid": zone["metadata"]["uid"], "generation": zone["metadata"]["generation"]},
                "resolverContextRef": {"name": "vpc", "uid": context["metadata"]["uid"], "generation": context["metadata"]["generation"]},
            },
        }
        self.source_kubectl(project, "apply", "-f", "-", stdin=json.dumps(assoc))
        self.wait(
            f"{project} association accepted",
            lambda: self.source_condition(project, "dnszoneassociation", "prod-vpc", "Accepted"),
            timeout=60,
        )
        return zone, context

    def source_condition(self, project: str, kind: str, name: str, condition: str) -> bool:
        obj = json.loads(self.source_kubectl(project, "-n", PROJECT_NAMESPACE, "get", kind, name, "-o", "json").stdout)
        return any(row["type"] == condition and row["status"] == "True" for row in obj.get("status", {}).get("conditions", []))

    def publish(self, project: str, zone: dict, prefix: str, address: str, lifetime: int = 90) -> dict:
        result = self.publisher(
            project, "create", "--zone", zone["metadata"]["name"],
            "--zone-uid", zone["metadata"]["uid"], "--zone-generation", str(zone["metadata"]["generation"]),
            "--prefix", prefix, "--record-name", "common", "--address", address, "--lifetime", str(lifetime),
        )
        return json.loads(result.stdout)

    def sync_addresses(self) -> dict[str, dict]:
        def bindings():
            items = json.loads(self.kubectl("-n", PLATFORM_NAMESPACE, "get", "dnsresolverbindings", "-o", "json").stdout)["items"]
            return items if len(items) == 2 else None

        items = self.wait("two cross-cluster resolver bindings", bindings, timeout=90)
        result: dict[str, dict] = {}
        for item in items:
            spec = item["spec"]
            listeners = spec["configuration"]["listeners"]
            result[spec["source"]["projectUID"]] = item
            self.compose("exec", "-T", "node-vips", "ip", "-6", "address", "replace", f'{listeners["node"]["address"]}/128', "dev", "eth0")
            self.compose("exec", "-T", "regional-vips", "ip", "-6", "address", "replace", f'{listeners["regional"]["address"]}/128', "dev", "eth0")
        time.sleep(1.5)
        return result

    def wait_publication(self, contribution_uid: str, sequence: int, *, label: str) -> dict:
        required = {"regional-0", "regional-1"}

        def verified():
            items = json.loads(self.kubectl("-n", PLATFORM_NAMESPACE, "get", "dnspublicationmanifests", "-o", "json").stdout)["items"]
            candidates = [
                item for item in items
                if any(f.get("uid") == contribution_uid and int(f.get("sequence", 0)) == sequence for f in item.get("spec", {}).get("contributionFences", []))
            ]
            if not candidates:
                return None
            item = max(candidates, key=lambda value: int(value["spec"]["revision"]))
            now = datetime.now(timezone.utc) + timedelta(seconds=1)
            members = {
                ack["memberID"] for ack in item.get("status", {}).get("replicaAcknowledgements", [])
                if ack.get("phase") == "Verified"
                and int(ack.get("writerEpoch", 0)) == int(item["spec"]["writerEpoch"])
                and int(ack.get("revision", 0)) == int(item["spec"]["revision"])
                and datetime.fromisoformat(ack["validUntil"].replace("Z", "+00:00")) > now
            }
            if required <= members:
                return {"manifest": item["metadata"]["name"], "writerEpoch": item["spec"]["writerEpoch"], "revision": item["spec"]["revision"]}
            return None

        try:
            evidence = self.wait(label, verified, timeout=90)
        except TimeoutError as exc:
            diagnostics = self.publication_diagnostics(contribution_uid, sequence)
            path = self.work / f"publication-timeout-seq-{sequence}.json"
            path.write_text(json.dumps(diagnostics, indent=2) + "\n")
            summary = {
                "candidates": [item["metadata"]["name"] for item in diagnostics["candidates"]],
                "owners": [item["spec"] for item in diagnostics["owners"]],
                "outboxCount": len(diagnostics["outboxes"]),
            }
            raise TimeoutError(f"{exc}; diagnostics={path}; summary={json.dumps(summary, separators=(',', ':'))}") from exc
        self.pass_check(label, sequence=sequence, **evidence)
        return evidence

    def publication_diagnostics(self, contribution_uid: str, sequence: int) -> dict:
        manifests = json.loads(self.kubectl("-n", PLATFORM_NAMESPACE, "get", "dnspublicationmanifests", "-o", "json").stdout)["items"]
        candidates = [
            item for item in manifests
            if any(fence.get("uid") == contribution_uid and int(fence.get("sequence", 0)) == sequence for fence in item.get("spec", {}).get("contributionFences", []))
        ]
        outboxes = json.loads(self.kubectl("-n", PLATFORM_NAMESPACE, "get", "dnstransportoutboxes", "-o", "json").stdout)["items"]
        owners = json.loads(self.kubectl("-n", PLATFORM_NAMESPACE, "get", "dnspublicationownerships", "-o", "json").stdout)["items"]
        manifest_names = {item["metadata"]["name"] for item in candidates}
        related_outboxes = [
            item for item in outboxes
            if item.get("spec", {}).get("manifestRef", {}).get("name") in manifest_names
            or item.get("metadata", {}).get("name") in {dep for candidate in candidates for dep in candidate.get("spec", {}).get("chunks", []) if isinstance(dep, str)}
        ]
        nats: object
        try:
            with urllib.request.urlopen("http://127.0.0.1:18222/jsz?consumers=true&config=true", timeout=5) as response:
                nats = json.load(response)
        except BaseException as error:
            nats = {"error": str(error)}
        return {
            "contributionUID": contribution_uid,
            "sequence": sequence,
            "candidates": candidates,
            "owners": [owner for owner in owners if any(owner["spec"]["zoneUID"] == candidate["spec"]["zoneRef"]["uid"] for candidate in candidates)],
            "outboxes": related_outboxes,
            "nats": nats,
        }

    def wait_binding_serving(self, project_uid: str, *, label: str) -> dict:
        def fresh():
            items = json.loads(self.kubectl("-n", PLATFORM_NAMESPACE, "get", "dnsresolverbindings", "-o", "json").stdout)["items"]
            item = next((row for row in items if row["spec"]["source"]["projectUID"] == project_uid), None)
            if (
                item is None
                or item.get("status", {}).get("phase") != "Serving"
                or int(item["status"].get("observedConfigurationRevision", 0)) != int(item["spec"]["configuration"]["revision"])
            ):
                return None
            _, envelope, _ = self.active_snapshot()
            now = datetime.now(timezone.utc) + timedelta(seconds=1)
            acks = {
                row["memberID"]: row for row in item["status"].get("memberAcknowledgements", [])
                if row.get("phase") == "Verified" and row.get("validUntil")
                and datetime.fromisoformat(row["validUntil"].replace("Z", "+00:00")) > now
                and int(row.get("revision", 0)) == int(item["spec"]["configuration"]["revision"])
                and int(row.get("writerEpoch", 0)) == int(envelope["epoch"])
                and int(row.get("snapshotRevision", 0)) == int(envelope["revision"])
                and int(row.get("authorizationIssuerEpoch", 0)) == int(item["spec"]["authorization"]["writerEpoch"])
                and int(row.get("authorizationRevision", 0)) == int(item["spec"]["authorization"]["sequence"])
            }
            if {"node-0", "regional-front", "regional-0", "regional-1"} <= set(acks):
                return {
                    "binding": item["metadata"]["name"],
                    "configurationRevision": item["spec"]["configuration"]["revision"],
                    "snapshotEpoch": envelope["epoch"],
                    "snapshotRevision": envelope["revision"],
                    "ackMembers": sorted(acks),
                    "authorizationValidUntil": item["spec"]["authorization"]["validUntil"],
                }
            return None

        evidence = self.wait(label, fresh, timeout=90)
        self.pass_check(label, **evidence)
        return evidence

    def wait_source_publication_status(self, project: str, publication: dict, expected: dict, *, label: str) -> dict:
        def projected():
            zone = json.loads(self.source_kubectl(project, "-n", PROJECT_NAMESPACE, "get", "dnszone", "prod", "-o", "json").stdout)
            registration = json.loads(self.source_kubectl(project, "-n", PROJECT_NAMESPACE, "get", "dnsregistration", publication["registration"]["name"], "-o", "json").stdout)
            zone_conditions = {row["type"]: row["status"] for row in zone.get("status", {}).get("conditions", [])}
            registration_conditions = {row["type"]: row["status"] for row in registration.get("status", {}).get("conditions", [])}
            if (
                zone_conditions.get("Published") == "True"
                and registration_conditions.get("Published") == "True"
                and registration_conditions.get("Available") == "True"
                and int(registration["status"].get("publicationWriterEpoch", 0)) == int(expected["writerEpoch"])
                and int(registration["status"].get("publicationRevision", 0)) == int(expected["revision"])
            ):
                return {
                    "zoneUID": zone["metadata"]["uid"],
                    "registrationUID": registration["metadata"]["uid"],
                    "publicationWriterEpoch": registration["status"].get("publicationWriterEpoch"),
                    "publicationRevision": registration["status"].get("publicationRevision"),
                }
            return None

        evidence = self.wait(label, projected, timeout=60)
        self.pass_check(label, **evidence)
        return evidence

    def shard_state(self) -> tuple[dict, dict]:
        items = json.loads(self.kubectl("-n", PLATFORM_NAMESPACE, "get", "configmap", "-o", "json").stdout)["items"]
        owner = next((item for item in items if item["metadata"]["name"].startswith("dns-shard-owner-")), None)
        if owner is None:
            return {}, {}
        return owner, json.loads(owner["data"]["state"])

    def zone_owner(self, zone_uid: str) -> dict:
        items = json.loads(self.kubectl("-n", PLATFORM_NAMESPACE, "get", "dnspublicationownerships", "-o", "json").stdout)["items"]
        return next((item for item in items if item["spec"]["zoneUID"] == zone_uid), {})

    @staticmethod
    def decode_payload(value: str) -> bytes:
        return base64.b64decode(value)

    def active_snapshot(self) -> tuple[dict, dict, dict]:
        _, state = self.shard_state()
        out = json.loads(self.kubectl("-n", PLATFORM_NAMESPACE, "get", "dnstransportoutbox", state["activeOutbox"], "-o", "json").stdout)
        envelope = json.loads(self.decode_payload(out["spec"]["payload"]))
        snapshot = envelope["payload"]
        if isinstance(snapshot, str):
            snapshot = json.loads(base64.b64decode(snapshot))
        return out, envelope, snapshot

    def capture_old_envelopes(self, zone_uid: str) -> list[dict[str, object]]:
        owner = self.zone_owner(zone_uid)
        outboxes = json.loads(self.kubectl("-n", PLATFORM_NAMESPACE, "get", "dnstransportoutboxes", "-o", "json").stdout)["items"]
        publication = next(
            item for item in outboxes
            if item.get("spec", {}).get("activation")
            and item.get("spec", {}).get("manifestRef", {}).get("name") == owner["spec"]["activeManifestName"]
        )
        snapshot, _, _ = self.active_snapshot()
        return [publication, snapshot]

    def replay(self, outboxes: list[dict[str, object]]) -> None:
        for index, outbox in enumerate(outboxes):
            spec = outbox["spec"]
            payload = self.decode_payload(spec["payload"])
            path = self.work / f"replay-{index}.json"
            path.write_bytes(payload)
            self.run(
                "go", "run", str(HERE / "replay_event.go"),
                "--url", "nats://127.0.0.1:14222", "--subject", spec["subject"], "--payload", str(path),
                timeout=60,
            )

    def cross_project_denials(self, a_publication: dict, b_publication: dict) -> None:
        # A token is signed by source A and is not an identity in source B.
        a_compute = json.loads(self.compute_kubeconfigs["project-a"].read_text())
        b_admin = json.loads(self.source_kubectl("project-b", "config", "view", "--raw", "-o", "json").stdout)
        b_context = next(row["context"] for row in b_admin["contexts"] if row["name"] == b_admin["current-context"])
        b_cluster = next(row["cluster"] for row in b_admin["clusters"] if row["name"] == b_context["cluster"])
        foreign = {
            "apiVersion": "v1", "kind": "Config", "current-context": "foreign",
            "clusters": [{"name": "b", "cluster": b_cluster}],
            "contexts": [{"name": "foreign", "context": {"cluster": "b", "user": "a", "namespace": PROJECT_NAMESPACE}}],
            "users": [{"name": "a", "user": a_compute["users"][0]["user"]}],
        }
        foreign_path = self.work / "foreign-a-to-b.kubeconfig"
        foreign_path.write_text(json.dumps(foreign))
        denied = self.run("kubectl", "--kubeconfig", str(foreign_path), "get", "dnsregistrations", check=False, timeout=15)
        if denied.returncode == 0:
            raise AssertionError("source A service-account credential authenticated to source B")

        forged = json.loads(json.dumps(a_compute))
        forged["users"][0]["user"]["as-user-extra"]["iam.miloapis.com/parent-name"] = ["project-b"]
        forged_path = self.work / "forged-project-parent.kubeconfig"
        forged_path.write_text(json.dumps(forged))
        forged_result = self.run("kubectl", "--kubeconfig", str(forged_path), "get", "dnsregistrations", check=False, timeout=15)
        if forged_result.returncode == 0 or "forbidden" not in (forged_result.stdout + forged_result.stderr).lower():
            raise AssertionError(f"source A credential forged project B parent context: {forged_result.stdout} {forged_result.stderr}")

        # A source-B scoped publisher cannot turn a grant for source A's
        # configured producer identity into a contribution.
        reg = b_publication["registration"]
        wrong_grant = {
            "apiVersion": "dns.networking.miloapis.com/v1alpha1", "kind": "DNSContributionGrant",
            "metadata": {"name": "cross-project-wrong-principal", "namespace": PROJECT_NAMESPACE},
            "spec": {
                "registrationRef": reg,
                "producerID": "cross-project-attempt",
                "principal": {"clusterUID": PROJECTS["project-a"]["sourceClusterUID"], "subject": SUBJECT},
                "recordTypes": ["A"],
            },
        }
        self.source_kubectl("project-b", "apply", "-f", "-", compute=True, stdin=json.dumps(wrong_grant))

        def issued():
            grant = json.loads(self.source_kubectl("project-b", "-n", PROJECT_NAMESPACE, "get", "dnscontributiongrant", "cross-project-wrong-principal", "-o", "json").stdout)
            return grant if grant.get("status", {}).get("activeWriterEpoch") else None

        grant = self.wait("wrong-principal grant reconciled", issued, timeout=60)
        contribution = {
            "apiVersion": "dns.networking.miloapis.com/v1alpha1", "kind": "DNSRecordContribution",
            "metadata": {"name": "cross-project-attempt", "namespace": PROJECT_NAMESPACE},
            "spec": {
                "registrationRef": reg,
                "grantRef": {"name": grant["metadata"]["name"], "uid": grant["metadata"]["uid"]},
                "recordSets": [{"recordType": "A", "records": [{"name": "common", "a": {"content": "10.99.0.1"}}]}],
            },
        }
        result = self.source_kubectl("project-b", "apply", "-f", "-", compute=True, check=False, stdin=json.dumps(contribution))
        if result.returncode == 0 or "principal" not in (result.stdout + result.stderr).lower():
            raise AssertionError(f"cross-project wrong-principal contribution was not rejected: {result.stdout} {result.stderr}")
        self.pass_check(
            "cross-project publishers are denied by API identity and grant principal",
            foreignCredential="authentication denied", forgedParentExtra="impersonation denied", wrongPrincipal="admission denied",
            sourceAContributionUID=a_publication["contribution"]["uid"],
        )

    def run_suite(self) -> None:
        self.setup()
        configs = self.write_configs()
        self.start("control-a", *self.command("control-plane", configs["control-a"]))
        self.wait("control A admission backend", lambda: socket.create_connection(("127.0.0.1", 9443), timeout=0.5), timeout=30)
        self.start(
            "admission-proxy", sys.executable, str(HERE / "admission_proxy.py"),
            "--listen", "0.0.0.0:19443",
            "--cert", str(self.certs / "tls.crt"), "--key", str(self.certs / "tls.key"), "--ca", str(self.certs / "tls.crt"),
            "--backend", "127.0.0.1:9443", "--backend", "127.0.0.1:9444",
        )
        self.install_webhooks()
        source: dict[str, dict] = {}
        for project in PROJECTS:
            zone, context = self.create_source_objects(project)
            source[project] = {"zone": zone, "context": context}

        # Start the second process after the first has acquired initial leases;
        # both remain active and configured for both source APIs thereafter.
        initial_owner = self.wait(
            "control A initial zone ownership",
            lambda: self.zone_owner(source["project-b"]["zone"]["metadata"]["uid"]),
            timeout=60,
        )
        initial_shard_obj, initial_shard = self.wait(
            "control A initial shard ownership",
            lambda: self.shard_state() if self.shard_state()[1].get("holder") else None,
            timeout=60,
        )
        owner_a = initial_owner["spec"]["holderIdentity"]
        if not initial_shard["holder"].startswith(owner_a + "-"):
            raise AssertionError(f"zone and shard were not owned by one process: {owner_a}, {initial_shard['holder']}")
        self.start("control-b", *self.command("control-plane", configs["control-b"]))
        self.wait("control B admission backend", lambda: socket.create_connection(("127.0.0.1", 9444), timeout=0.5), timeout=30)
        if any(process.poll() is not None for process in self.processes.values()):
            raise RuntimeError("a control-plane process exited during concurrent startup")
        self.pass_check("two native control planes concurrently watch both source APIs", initialHolder=owner_a)

        publications = {
            "project-a": self.publish("project-a", source["project-a"]["zone"], "a-common", "10.10.0.10"),
            "project-b": self.publish("project-b", source["project-b"]["zone"], "b-common", "10.20.0.20"),
        }
        bindings = self.sync_addresses()
        vip = {name: bindings[PROJECTS[name]["projectUID"]]["spec"]["configuration"]["listeners"]["node"]["address"] for name in PROJECTS}
        self.wait_for_bootstrap()
        for name in ("regional-0", "regional-1", "regional-front", "node-0"):
            self.start_container(name, configs[name], network="datum-internal-dns_dns")
        self.wait_for_agent_leases(("node-0", "regional-front", "regional-0", "regional-1"))
        for name in ("regional-0-watchdog", "regional-1-watchdog", "regional-front-watchdog", "node-0-watchdog"):
            self.start_container(name, configs[name], network="none", watchdog=True)
        for project in PROJECTS:
            self.publisher(project, "observe", "--name", publications[project]["contribution"]["name"], "--sequence", "2", "--eligible", "true", "--lifetime", "90")
            self.wait_publication(publications[project]["contribution"]["uid"], 2, label=f"{project} publication verified by both regional BIND members")

        for tcp in (False, True):
            self.expect_query(f"project A overlap {'TCP' if tcp else 'UDP'}", vip["project-a"], "common.prod.internal", "NOERROR", ["10.10.0.10"], tcp)
            self.expect_query(f"project B overlap {'TCP' if tcp else 'UDP'}", vip["project-b"], "common.prod.internal", "NOERROR", ["10.20.0.20"], tcp)

        _, snapshot_env, snapshot = self.active_snapshot()
        project_uids = sorted(binding["projectUID"] for binding in snapshot["bindings"])
        if project_uids != sorted(value["projectUID"] for value in PROJECTS.values()):
            raise AssertionError(f"shared shard snapshot is incomplete: {project_uids}")
        self.pass_check(
            "complete shared shard snapshot contains both projects",
            configurationEpoch=snapshot["configurationEpoch"],
            configurationRevision=snapshot["configurationRevision"],
            projectUIDs=project_uids,
            eventID=snapshot_env["eventID"],
        )
        provenance: dict[str, object] = {}
        manifests = json.loads(self.kubectl("-n", PLATFORM_NAMESPACE, "get", "dnspublicationmanifests", "-o", "json").stdout)["items"]
        for project in PROJECTS:
            grant = json.loads(self.source_kubectl(project, "-n", PROJECT_NAMESPACE, "get", "dnscontributiongrant", publications[project]["grant"]["name"], "-o", "json").stdout)
            binding = bindings[PROJECTS[project]["projectUID"]]
            manifest = max(
                (item for item in manifests if any(fence.get("uid") == publications[project]["contribution"]["uid"] for fence in item["spec"].get("contributionFences", []))),
                key=lambda item: int(item["spec"]["revision"]),
            )
            if grant["spec"]["principal"]["clusterUID"] != PROJECTS[project]["sourceClusterUID"]:
                raise AssertionError(f"{project} grant lost configured source-cluster provenance")
            if (
                binding["spec"]["source"]["projectUID"] != PROJECTS[project]["projectUID"]
                or binding["spec"]["source"]["resolverContextRef"]["uid"] != source[project]["context"]["metadata"]["uid"]
                or source[project]["zone"]["metadata"]["uid"] not in [ref["uid"] for ref in binding["spec"]["configuration"]["zoneRefs"]]
            ):
                raise AssertionError(f"{project} platform binding lost source UID provenance")
            if not any(fence.get("uid") == publications[project]["registration"]["uid"] and int(fence.get("generation", 0)) == publications[project]["registration"]["generation"] for fence in manifest["spec"].get("registrationFences", [])):
                raise AssertionError(f"{project} manifest lost registration UID/generation provenance")
            provenance[project] = {
                "projectUID": PROJECTS[project]["projectUID"],
                "sourceClusterUID": PROJECTS[project]["sourceClusterUID"],
                "resolverContextUID": source[project]["context"]["metadata"]["uid"],
                "zoneUID": source[project]["zone"]["metadata"]["uid"],
                "registrationUID": publications[project]["registration"]["uid"],
                "grantUID": publications[project]["grant"]["uid"],
                "contributionUID": publications[project]["contribution"]["uid"],
                "manifest": manifest["metadata"]["name"],
            }
        mirrored_zones = json.loads(self.kubectl("get", "dnszones", "--all-namespaces", "-o", "json").stdout)["items"]
        mirrored_registrations = json.loads(self.kubectl("get", "dnsregistrations", "--all-namespaces", "-o", "json").stdout)["items"]
        if mirrored_zones or mirrored_registrations:
            raise AssertionError("source zones or registrations were mirrored into the platform API")
        self.pass_check("source project, cluster, VPC, zone, registration, grant, and contribution UID provenance is preserved", sources=provenance, platformSourceCopies=0)
        self.cross_project_denials(publications["project-a"], publications["project-b"])

        # One project's update and deletion cannot change the other view.
        a_name = publications["project-a"]["contribution"]["name"]
        self.publisher("project-a", "update", "--name", a_name, "--address", "10.10.0.11", "--sequence", "3", "--lifetime", "90")
        self.expect_query("project A update reaches its view", vip["project-a"], "common.prod.internal", "NOERROR", ["10.10.0.11"])
        self.expect_query("project B survives project A update", vip["project-b"], "common.prod.internal", "NOERROR", ["10.20.0.20"])
        self.publisher("project-a", "delete", "--name", a_name)
        self.expect_query("project A deletion removes only its answer", vip["project-a"], "common.prod.internal", "NOERROR", [])
        self.expect_query("project B survives project A deletion", vip["project-b"], "common.prod.internal", "NOERROR", ["10.20.0.20"])
        # Reuse the existing registration and grant. A registration continues
        # to reserve its name after its contribution is deleted.
        publications["project-a"] = self.publish("project-a", source["project-a"]["zone"], "a-common", "10.10.0.12")
        self.expect_query("project A can republish after isolated deletion", vip["project-a"], "common.prod.internal", "NOERROR", ["10.10.0.12"])

        # Freeze the current owner. The second real process must acquire both
        # independent leases, use higher epochs, and program a new revision.
        b_zone_uid = source["project-b"]["zone"]["metadata"]["uid"]
        old_envelopes = self.capture_old_envelopes(b_zone_uid)
        before_zone = self.zone_owner(b_zone_uid)
        _, before_shard = self.shard_state()
        if before_zone["spec"]["holderIdentity"] != owner_a or not before_shard["holder"].startswith(owner_a + "-"):
            raise AssertionError("control A did not retain both current leases before SIGSTOP")
        self.processes["control-a"].send_signal(signal.SIGSTOP)
        zone_after = self.wait(
            "survivor zone ownership takeover",
            lambda: (lambda value: value if value["spec"]["holderIdentity"] != owner_a else None)(self.zone_owner(b_zone_uid)),
            timeout=35,
        )
        shard_after_obj, shard_after = self.wait(
            "survivor shard ownership takeover",
            lambda: (lambda value: value if value[1]["holder"] != before_shard["holder"] else None)(self.shard_state()),
            timeout=35,
        )
        owner_b = zone_after["spec"]["holderIdentity"]
        if int(zone_after["spec"]["writerEpoch"]) <= int(before_zone["spec"]["writerEpoch"]):
            raise AssertionError("zone takeover did not advance writer epoch")
        if int(shard_after["epoch"]) <= int(before_shard["epoch"]):
            raise AssertionError("shard takeover did not advance configuration epoch")
        if not shard_after["holder"].startswith(owner_b + "-"):
            raise AssertionError("surviving process did not own both zone and shard leases")
        self.pass_check(
            "SIGSTOP owner causes real survivor takeover at higher zone and shard epochs",
            oldIdentity=owner_a, newIdentity=owner_b,
            zoneEpochBefore=before_zone["spec"]["writerEpoch"], zoneEpochAfter=zone_after["spec"]["writerEpoch"],
            shardEpochBefore=before_shard["epoch"], shardEpochAfter=shard_after["epoch"],
        )
        self.owner_evidence = {
            "initial": {"identity": owner_a, "zone": before_zone["spec"], "shard": before_shard},
            "takeover": {"identity": owner_b, "zone": zone_after["spec"], "shard": shard_after},
        }
        b_name = publications["project-b"]["contribution"]["name"]
        self.publisher("project-b", "update", "--name", b_name, "--address", "10.20.0.21", "--sequence", "3", "--lifetime", "90")
        takeover_publication = self.wait_publication(publications["project-b"]["contribution"]["uid"], 3, label="survivor publishes fresh record at higher ownership fence")
        self.expect_query("survivor programs new record after takeover", vip["project-b"], "common.prod.internal", "NOERROR", ["10.20.0.21"])
        self.processes["control-a"].send_signal(signal.SIGCONT)
        self.replay(old_envelopes)
        time.sleep(12)
        stable_zone = self.zone_owner(b_zone_uid)
        _, stable_shard = self.shard_state()
        self.owner_evidence["afterResumeAndReplay"] = {"zone": stable_zone.get("spec", {}), "shard": stable_shard}
        if stable_zone["spec"]["holderIdentity"] != owner_b or int(stable_zone["spec"]["writerEpoch"]) < int(zone_after["spec"]["writerEpoch"]):
            raise AssertionError("resumed old zone owner regressed the ownership fence")
        if stable_shard["holder"] != shard_after["holder"] or int(stable_shard["epoch"]) < int(shard_after["epoch"]):
            raise AssertionError("resumed old shard owner regressed the ownership fence")
        self.expect_query("resumed old owner and exact stale-envelope replay cannot overwrite fresh record", vip["project-b"], "common.prod.internal", "NOERROR", ["10.20.0.21"])
        self.pass_check(
            "higher ownership fence stays stable beyond a lease window",
            writerEpoch=stable_zone["spec"]["writerEpoch"], publicationRevision=takeover_publication["revision"],
            configurationEpoch=stable_shard["epoch"], configurationRevision=stable_shard["nextRevision"] - 1,
        )

        # Blackhole source A long enough for both the original observation and
        # binding authorization to expire. Source B must continue through the
        # same controllers, planner, NATS stream, and serving fleet.
        a_name = publications["project-a"]["contribution"]["name"]
        short = json.loads(self.publisher("project-a", "observe", "--name", a_name, "--sequence", "2", "--eligible", "true", "--lifetime", "15").stdout)
        self.wait_publication(publications["project-a"]["contribution"]["uid"], 2, label="source A short original deadline reaches both replicas")
        self.expect_query("source A record live before API blackhole", vip["project-a"], "common.prod.internal", "NOERROR", ["10.10.0.12"])
        access_a = json.loads(self.source_kubectl("project-a", "-n", PROJECT_NAMESPACE, "get", "dnsresolveraccessbinding", "vpc-e2e", "-o", "json").stdout)
        short_access_deadline = datetime.now(timezone.utc) + timedelta(seconds=30)
        access_auth = access_a["spec"]["authorization"]
        access_patch = {"spec": {"authorization": {**access_auth, "sequence": int(access_auth["sequence"]) + 1, "validUntil": short_access_deadline.isoformat().replace("+00:00", "Z")}}}
        self.source_kubectl("project-a", "-n", PROJECT_NAMESPACE, "patch", "dnsresolveraccessbinding", "vpc-e2e", "--type=merge", "-p", json.dumps(access_patch))
        auth_deadline = short_access_deadline
        expected_access_sequence = int(access_auth["sequence"]) + 1
        platform_a_binding = self.wait(
            "source A short access deadline reaches platform binding",
            lambda: next((item for item in json.loads(self.kubectl("-n", PLATFORM_NAMESPACE, "get", "dnsresolverbindings", "-o", "json").stdout)["items"] if item["spec"]["source"]["projectUID"] == PROJECTS["project-a"]["projectUID"] and int(item["spec"]["authorization"]["sequence"]) == expected_access_sequence), None),
            timeout=60,
        )
        local_authorization = self.wait_for_local_authorization(
            platform_a_binding["metadata"]["uid"], expected_access_sequence, auth_deadline,
        )
        self.pass_check("source A short access deadline installed by every serving role", **local_authorization)
        record_deadline = datetime.fromisoformat(short["status"]["validUntil"].replace("Z", "+00:00"))
        source_a_container = f"{SOURCE_CLUSTERS['project-a']}-control-plane"
        self.run("docker", "pause", source_a_container)
        self.paused_containers.add(source_a_container)

        self.publisher("project-b", "update", "--name", b_name, "--address", "10.20.0.22", "--sequence", "4", "--lifetime", "90")
        b_partition_publication = self.wait_publication(publications["project-b"]["contribution"]["uid"], 4, label="source B remains platform Published while source A API is blackholed")
        self.wait_binding_serving(PROJECTS["project-b"]["projectUID"], label="source B Serving ACK projection stays fresh during source A blackhole")
        self.wait_source_publication_status("project-b", publications["project-b"], b_partition_publication, label="source B project Published and Available status remains current during source A blackhole")
        self.expect_query("source B update serves during source A blackhole", vip["project-b"], "common.prod.internal", "NOERROR", ["10.20.0.22"])
        time.sleep(max(0, (record_deadline - datetime.now(timezone.utc)).total_seconds()) + 2)
        self.expect_query("source A original record deadline expires locally before authorization", vip["project-a"], "common.prod.internal", "NOERROR", [], timeout=12)
        self.publisher("project-b", "update", "--name", b_name, "--address", "10.20.0.23", "--sequence", "5", "--lifetime", "90")
        self.wait_publication(publications["project-b"]["contribution"]["uid"], 5, label="source B publishes again while source A remains unavailable")
        time.sleep(max(0, (auth_deadline - datetime.now(timezone.utc)).total_seconds()) + 2)
        self.expect_query("source A authorization expiry fails closed after API blackhole", vip["project-a"], "common.prod.internal", "REFUSED", [], timeout=15)
        self.expect_query("source B remains available after source A authorization expiry", vip["project-b"], "common.prod.internal", "NOERROR", ["10.20.0.23"])

        self.run("docker", "unpause", source_a_container)
        self.paused_containers.discard(source_a_container)
        recovery_deadline = time.monotonic() + 5
        recovery_gated = False
        recovery_b_probes = 0
        recovery_b_transient_failures = 0
        recovery_b_max_delay = 0.0

        def require_source_b() -> None:
            nonlocal recovery_b_probes, recovery_b_transient_failures, recovery_b_max_delay
            started = time.monotonic()
            deadline = time.monotonic() + 1.5
            while True:
                recovery_b_probes += 1
                try:
                    actual = self.query(vip["project-b"], "common.prod.internal")
                    if actual != ("NOERROR", ["10.20.0.23"]):
                        raise AssertionError(f"source B returned an unexpected recovery response: {actual}")
                    elapsed = time.monotonic() - started
                    recovery_b_max_delay = max(recovery_b_max_delay, elapsed)
                    if elapsed > 1.5:
                        raise AssertionError(f"source B DNS recovery took {elapsed:.3f}s, exceeding the 1.5s bound")
                    return
                except RuntimeError as error:
                    expected = f"communications error to {vip['project-b']}#53: connection refused".lower()
                    if expected not in str(error).lower():
                        raise
                    recovery_b_transient_failures += 1
                if time.monotonic() >= deadline:
                    raise AssertionError("source B connection refusal exceeded the 1.5s dnsdist reload bound")
                time.sleep(0.1)

        while time.monotonic() < recovery_deadline:
            require_source_b()
            try:
                status, answers = self.query(vip["project-a"], "common.prod.internal")
            except RuntimeError as error:
                # The destination gate may reject the expired VPC at its exact
                # VIP. The shared dnsdist must remain live for source B above.
                expected = f"communications error to {vip['project-a']}#53: connection refused".lower()
                if expected in str(error).lower():
                    recovery_gated = True
                    require_source_b()
                    if not (self.work / "recovery-gate-state.json").exists():
                        (self.work / "recovery-gate-state.json").write_text(json.dumps({
                            "capturedAt": datetime.now(timezone.utc).isoformat(),
                            "sourceAVIP": vip["project-a"],
                            "sourceBVIP": vip["project-b"],
                            "port": 53,
                            "sourceAProbeError": str(error),
                            "sourceBProbe": {"status": "NOERROR", "answers": ["10.20.0.23"]},
                            "composeProcesses": self.compose("ps", "--all", "--format", "json", check=False).stdout,
                            "nodeDNSDistConfig": (self.runtime / "node-dnsdist.conf").read_text(),
                            "nodeBINDConfig": (self.runtime / "node-bind.conf").read_text(),
                        }, indent=2) + "\n")
                    time.sleep(0.5)
                    continue
                raise
            if answers:
                raise AssertionError(f"source A returned answers before a fresh observation: {answers}")
            if status != "REFUSED":
                raise AssertionError(f"unexpected source A recovery response: {status} {answers}")
            time.sleep(0.5)
        self.pass_check(
            "source API recovery does not resurrect expired observation while the other VPC stays live",
            sourceAVIP=vip["project-a"],
            sourceBVIP=vip["project-b"],
            port=53,
            sourceBProbes=recovery_b_probes,
            sourceBTransientReloadFailures=recovery_b_transient_failures,
            sourceBProbeRecoveryBoundSeconds=1.5,
            sourceBMaxProbeDelaySeconds=round(recovery_b_max_delay, 3),
            transportGated=recovery_gated,
        )
        # Both the observation and the independently fenced resolver access
        # expired during the outage. A fresh record alone must not bypass the
        # expired access fence, so renew access first and wait until every
        # serving role has installed that exact authorization revision.
        recovered_access_deadline = datetime.now(timezone.utc) + timedelta(seconds=600)
        recovered_access_sequence = expected_access_sequence + 1
        recovered_access_patch = {
            "spec": {
                "authorization": {
                    **access_auth,
                    "sequence": recovered_access_sequence,
                    "validUntil": recovered_access_deadline.isoformat().replace("+00:00", "Z"),
                },
            },
        }
        self.source_kubectl(
            "project-a", "-n", PROJECT_NAMESPACE, "patch", "dnsresolveraccessbinding", "vpc-e2e",
            "--type=merge", "-p", json.dumps(recovered_access_patch),
        )
        recovered_platform_binding = self.wait(
            "source A recovered access reaches platform binding",
            lambda: next((
                item for item in json.loads(self.kubectl(
                    "-n", PLATFORM_NAMESPACE, "get", "dnsresolverbindings", "-o", "json",
                ).stdout)["items"]
                if item["spec"]["source"]["projectUID"] == PROJECTS["project-a"]["projectUID"]
                and int(item["spec"]["authorization"]["sequence"]) == recovered_access_sequence
                and not item["spec"].get("tombstone", False)
            ), None),
            timeout=60,
        )
        self.wait_for_local_authorization(
            recovered_platform_binding["metadata"]["uid"], recovered_access_sequence, recovered_access_deadline,
        )
        self.publisher("project-a", "observe", "--name", a_name, "--sequence", "3", "--eligible", "true", "--lifetime", "90")
        self.wait_publication(publications["project-a"]["contribution"]["uid"], 3, label="fresh source A observation republishes after recovery")
        self.expect_query("fresh source A observation restores answer", vip["project-a"], "common.prod.internal", "NOERROR", ["10.10.0.12"])
        self.expect_query("source B remains live after source A recovery", vip["project-b"], "common.prod.internal", "NOERROR", ["10.20.0.23"])

    def redact(self, value: object) -> object:
        if isinstance(value, dict):
            return {key: ("<redacted>" if key.lower() in {"apikey", "token", "password", "passwordfile", "credentialsfile", "keyfile"} else self.redact(item)) for key, item in value.items()}
        if isinstance(value, list):
            return [self.redact(item) for item in value]
        return value

    def freeze_evidence(self) -> Path:
        for container in list(self.paused_containers):
            self.run("docker", "unpause", container, check=False)
            self.paused_containers.discard(container)
        for name in list(self.processes):
            process = self.processes[name]
            if process.poll() is None:
                process.send_signal(signal.SIGCONT)
            self.stop(name)
        for container in sorted(self.containers):
            logs = self.run("docker", "logs", container, check=False)
            (self.logs / f"{container}.log").write_text(logs.stdout + logs.stderr)

        artifact = self.results.with_name(self.results.stem + "-artifacts")
        if artifact.exists():
            shutil.rmtree(artifact)
        (artifact / "logs").mkdir(parents=True)
        (artifact / "runtime").mkdir()
        (artifact / "configs").mkdir()
        (artifact / "diagnostics").mkdir()
        for path in self.logs.glob("*.log"):
            shutil.copy2(path, artifact / "logs" / path.name)
        self.capture_runtime(artifact / "runtime")
        self.capture_state(artifact / "state")
        for name, path in {**self.control_configs, **{key: self.work / f"{key}.json" for key in ("node-0", "regional-front", "regional-0", "regional-1")}}.items():
            if path.exists():
                (artifact / "configs" / f"{name}.json").write_text(json.dumps(self.redact(json.loads(path.read_text())), indent=2) + "\n")
        for path in self.work.glob("publication-timeout-*.json"):
            shutil.copy2(path, artifact / "diagnostics" / path.name)
        for path in self.work.glob("recovery-gate-state.json"):
            shutil.copy2(path, artifact / "diagnostics" / path.name)
        (artifact / "source.sha256").write_text(self.source_hash + "\n")
        (artifact / "metadata.json").write_text(json.dumps({
            "capturedAt": datetime.now(timezone.utc).isoformat(),
            "sourceSHA256": self.source_hash,
            "topology": self.topology,
            "ownership": self.owner_evidence,
            "images": IMAGES,
            "credentialsIncluded": False,
        }, indent=2) + "\n")
        return artifact

    def cleanup(self) -> None:
        for container in list(self.paused_containers):
            self.run("docker", "unpause", container, check=False)
            self.paused_containers.discard(container)
        for name in list(self.containers):
            self.run("docker", "rm", "-f", name, check=False)
            self.containers.discard(name)
        for name in list(self.processes):
            self.stop(name)
        if self.keep:
            print(f"kept multi-control-plane E2E state at {self.work}")
            return
        cluster_configs = [(PLATFORM_CLUSTER, self.platform_kubeconfig)] + [
            (SOURCE_CLUSTERS[name], self.source_kubeconfigs[name]) for name in PROJECTS
        ]
        for cluster, config in cluster_configs:
            self.run("kind", "delete", "cluster", "--name", cluster, "--kubeconfig", str(config), check=False)
        self.compose("down", "--volumes", "--remove-orphans", check=False)
        self.cleanup_runtime_mount()
        shutil.rmtree(self.work, ignore_errors=True)

    def write_results(self, error: BaseException | None, artifact: Path | None) -> None:
        payload = {
            "suite": "internal-dns-multi-control-plane",
            "passed": error is None,
            "error": None if error is None else str(error),
            "checks": single.RESULTS,
            "sourceSHA256": self.source_hash,
            "topology": self.topology,
            "ownership": self.owner_evidence,
            "images": IMAGES,
            "artifactDirectory": None if artifact is None else str(artifact),
            "limitations": [
                "One development NATS/JetStream and one serving fleet are shared; this does not qualify physically distributed regional brokers.",
                "Kind API servers and native control-plane processes run on one host.",
                "The development fleet reloads one dnsdist process with docker restart; bounded DNS retry is qualified, not zero-packet-loss dataplane reloads.",
            ],
        }
        self.results.parent.mkdir(parents=True, exist_ok=True)
        self.results.write_text(json.dumps(payload, indent=2) + "\n")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--keep", action="store_true", default=os.environ.get("INTERNAL_DNS_KEEP") == "1")
    parser.add_argument("--results", type=Path, default=HERE / "results/bind-multi-control-plane-latest.json")
    args = parser.parse_args()
    harness = MultiControlPlaneHarness(args.keep, args.results)
    error: BaseException | None = None
    artifact: Path | None = None
    try:
        harness.run_suite()
    except BaseException as exc:
        error = exc
        print(f"FAIL {exc}", file=sys.stderr)
    finally:
        try:
            artifact = harness.freeze_evidence()
        except BaseException as evidence_error:
            print(f"evidence capture failed: {evidence_error}", file=sys.stderr)
            if error is None:
                error = evidence_error
        harness.write_results(error, artifact)
        harness.cleanup()
    if error is not None:
        raise error
    print(f"qualified {len(single.RESULTS)} multi-control-plane full-path checks")
    return 0


if __name__ == "__main__":
    sys.exit(main())
