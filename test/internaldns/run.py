#!/usr/bin/env python3
"""Run the internal DNS Kubernetes -> JetStream -> serving fleet qualification."""

from __future__ import annotations

import argparse
import base64
from datetime import datetime, timedelta, timezone
import hashlib
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import time


ROOT = Path(__file__).resolve().parents[2]
HERE = Path(__file__).resolve().parent
DEV = ROOT / "dev/internal-dns"
COMPOSE = DEV / "compose.yaml"
CLUSTER = "datum-internal-dns-e2e"
PROJECT_NS = "project-e2e"
PLATFORM_NS = "internal-dns-system"
RESULTS: list[dict[str, object]] = []
COLIMA_PROFILE = "internal-dns-e2e"
ALPINE_IMAGE = "alpine@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8"


def require_suite_docker_host() -> None:
    """Reject accidental use of the user's default Docker/Colima context."""
    profile = os.environ.get("INTERNAL_DNS_COLIMA_PROFILE", COLIMA_PROFILE)
    expected = f"unix://{Path.home()}/.colima/{profile}/docker.sock"
    actual = os.environ.get("DOCKER_HOST", "")
    if actual != expected:
        raise RuntimeError(
            "internal DNS qualification requires its suite-owned Colima profile; "
            f"set DOCKER_HOST={expected} after starting "
            f"`colima --profile {profile} start --activate=false` (got {actual or '<unset>'})"
        )


class Harness:
    def __init__(self, keep: bool, results: Path):
        self.keep = keep
        self.results = results
        docker_shared = ROOT / ".internal-dns-e2e"
        docker_shared.mkdir(exist_ok=True)
        self.work = Path(tempfile.mkdtemp(prefix="run-", dir=docker_shared))
        self.kubeconfig = self.work / "admin.kubeconfig"
        self.compute_kubeconfig = self.work / "compute.kubeconfig"
        self.runtime = self.work / "runtime"
        self.runtime_mount = f"/tmp/datum-internal-dns-{self.work.name}-runtime"
        self.state_mount = f"/tmp/datum-internal-dns-{self.work.name}-state"
        self.state = self.work / "state"
        self.logs = self.work / "logs"
        self.certs = self.work / "certs"
        self.binary = self.work / "internal-dns"
        self.linux_binary = self.work / "internal-dns-linux-arm64"
        self.source = self.work / "source"
        self.source_hash = ""
        self.qualification_hash = ""
        self.processes: dict[str, subprocess.Popen[str]] = {}
        self.issuer_kubeconfig = self.work / "grant-issuer.kubeconfig"
        self.containers: set[str] = set()
        for directory in (self.runtime, self.state, self.logs, self.certs):
            directory.mkdir(parents=True, exist_ok=True)
        shutil.copytree(DEV / "runtime", self.runtime, dirs_exist_ok=True)
        os.environ["INTERNAL_DNS_RUNTIME"] = self.runtime_mount
        os.environ["GOCACHE"] = "/private/tmp/internal-dns-go-cache"
        os.environ["GOMODCACHE"] = "/private/tmp/internal-dns-go-mod-cache"

    def run(self, *args: str, check: bool = True, stdin: str | None = None, timeout: float | None = None, cwd: Path = ROOT) -> subprocess.CompletedProcess[str]:
        process = subprocess.run(args, cwd=cwd, env=os.environ, input=stdin, text=True, capture_output=True, timeout=timeout)
        if check and process.returncode:
            raise RuntimeError(f"command failed ({process.returncode}): {' '.join(args)}\nstdout:\n{process.stdout}\nstderr:\n{process.stderr}")
        return process

    def compose(self, *args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
        return self.run("docker", "compose", "-f", str(COMPOSE), *args, check=check)

    def seed_runtime(self) -> None:
        self.run(
            "docker", "run", "--rm",
            "-v", f"{self.runtime}:/seed:ro", "-v", f"{self.runtime_mount}:/runtime",
            ALPINE_IMAGE, "sh", "-ec",
            "find /runtime -mindepth 1 -maxdepth 1 -exec rm -rf {} +; cp -a /seed/. /runtime/",
        )

    def capture_runtime(self, target: Path) -> None:
        target.mkdir(parents=True, exist_ok=True)
        self.run(
            "docker", "run", "--rm",
            "-v", f"{self.runtime_mount}:/runtime:ro", "-v", f"{target.resolve()}:/evidence",
            ALPINE_IMAGE, "sh", "-ec", "cp -a /runtime/. /evidence/",
        )

    def capture_state(self, target: Path) -> None:
        target.mkdir(parents=True, exist_ok=True)
        self.run(
            "docker", "run", "--rm",
            "-v", f"{self.state_mount}:/state:ro", "-v", f"{target.resolve()}:/evidence",
            ALPINE_IMAGE, "sh", "-ec", "cp -a /state/. /evidence/",
        )

    def cleanup_runtime_mount(self) -> None:
        names = [Path(self.runtime_mount).name, Path(self.state_mount).name]
        if any(not name.startswith("datum-internal-dns-run-") or
               not name.endswith(("-runtime", "-state")) for name in names):
            raise RuntimeError(f"refusing to clean unexpected VM paths {self.runtime_mount}, {self.state_mount}")
        self.run("docker", "run", "--rm", "-v", "/tmp:/vm-tmp", ALPINE_IMAGE,
                 "rm", "-rf", *(f"/vm-tmp/{name}" for name in names), check=False)

    def kubectl(self, *args: str, compute: bool = False, check: bool = True, stdin: str | None = None) -> subprocess.CompletedProcess[str]:
        config = self.compute_kubeconfig if compute else self.kubeconfig
        return self.run("kubectl", "--kubeconfig", str(config), *args, check=check, stdin=stdin)

    def start(self, name: str, *args: str) -> None:
        log = (self.logs / f"{name}.log").open("w")
        self.processes[name] = subprocess.Popen(args, cwd=ROOT, env=os.environ, text=True, stdout=log, stderr=subprocess.STDOUT)

    def stop(self, name: str) -> None:
        process = self.processes.pop(name, None)
        if process is None or process.poll() is not None:
            return
        process.send_signal(signal.SIGTERM)
        try:
            process.wait(timeout=8)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=3)

    def start_container(self, name: str, config: Path, *, network: str, watchdog: bool = False) -> None:
        container = f"datum-internal-dns-{name}"
        self.run("docker", "rm", "-f", container, check=False)
        args = [
            "docker", "run", "-d", "--name", container, "--network", network,
            "-v", f"{self.linux_binary}:/internal-dns:ro",
            "-v", f"{config}:/config.json:ro",
            "-v", f"{self.state_mount}:/state",
        ]
        # Resolver transactions and every watchdog use the Docker API to
        # validate, reload, or gate the real serving daemons.
        args.extend(["-v", "/var/run/docker.sock:/var/run/docker.sock"])
        member_ips = {
            "regional-0": "10.253.0.70",
            "regional-1": "10.253.0.71",
            "node-0": "10.253.0.72",
            "regional-front": "10.253.0.73",
        }
        if name in member_ips:
            args.extend(["--ip", member_ips[name]])
            args.extend(["-v", f"{self.runtime_mount}:/runtime"])
        args.extend([
            "docker@sha256:b1805116a6a86cc591b5d5f60a910a0715cdcc9d18d866ad68b1457ead25c35c",
            "/internal-dns", "--role", "watchdog" if watchdog else "agent", "--config", "/config.json",
        ])
        self.run(*args)
        self.containers.add(container)

    def stop_container(self, name: str) -> None:
        container = f"datum-internal-dns-{name}"
        self.run("docker", "rm", "-f", container, check=False)
        self.containers.discard(container)

    def wait(self, description: str, callback, timeout: float = 60, interval: float = 0.25):
        deadline = time.monotonic() + timeout
        last: object = None
        while time.monotonic() < deadline:
            for name, process in self.processes.items():
                if process.poll() not in (None, 0):
                    tail = (self.logs / f"{name}.log").read_text()[-6000:]
                    raise RuntimeError(f"{name} exited while waiting for {description}:\n{tail}")
            for container in self.containers:
                state = self.run("docker", "inspect", "-f", "{{.State.Status}} {{.State.ExitCode}}", container, check=False).stdout.strip()
                if not state.startswith("running "):
                    logs = self.run("docker", "logs", "--tail", "200", container, check=False)
                    tail = logs.stdout + logs.stderr
                    raise RuntimeError(f"{container} exited while waiting for {description} ({state}):\n{tail}")
            try:
                last = callback()
                if last:
                    return last
            except (RuntimeError, OSError, json.JSONDecodeError, KeyError, IndexError):
                pass
            time.sleep(interval)
        raise TimeoutError(f"timed out waiting for {description}; last value: {last!r}")

    def pass_check(self, label: str, **evidence: object) -> None:
        RESULTS.append({"label": label, "passed": True, **evidence})
        print(f"PASS {label}", flush=True)

    def capture_source(self) -> None:
        self.source.mkdir()
        for filename in ("go.mod", "go.sum"):
            shutil.copy2(ROOT / filename, self.source / filename)
        for directory in ("api", "internal"):
            shutil.copytree(ROOT / directory, self.source / directory)
        (self.source / "cmd").mkdir()
        shutil.copytree(ROOT / "cmd/internal-dns", self.source / "cmd/internal-dns")
        digest = hashlib.sha256()
        for path in sorted(p for p in self.source.rglob("*") if p.is_file()):
            digest.update(str(path.relative_to(self.source)).encode() + b"\0")
            digest.update(path.read_bytes())
        self.source_hash = digest.hexdigest()
        (self.work / "source.sha256").write_text(self.source_hash + "\n")

    def setup(self) -> None:
        require_suite_docker_host()
        for command in ("docker", "kind", "kubectl", "go", "openssl"):
            if shutil.which(command) is None:
                raise RuntimeError(f"missing prerequisite: {command}")
        self.capture_source()
        # A prior --keep run can leave standalone agents/watchdogs connected to
        # the shared Compose network. Remove those before starting a new broker
        # so stale consumers cannot program or gate the fresh environment.
        for name in ("node-0", "regional-front", "regional-0", "regional-1", "node-0-watchdog", "regional-front-watchdog", "regional-0-watchdog", "regional-1-watchdog"):
            self.run("docker", "rm", "-f", f"datum-internal-dns-{name}", check=False)
        for cluster in (
            CLUSTER,
            "datum-internal-dns-platform-e2e",
            "datum-internal-dns-source-a-e2e",
            "datum-internal-dns-source-b-e2e",
        ):
            self.run("kind", "delete", "cluster", "--name", cluster, check=False)
        self.compose("down", "--volumes", "--remove-orphans", check=False)
        self.seed_runtime()
        self.compose("up", "-d", "--wait")

        self.run("kind", "create", "cluster", "--name", CLUSTER, "--kubeconfig", str(self.kubeconfig), "--wait", "90s", timeout=120)
        host_lookup = self.run("docker", "exec", f"{CLUSTER}-control-plane", "getent", "hosts", "host.docker.internal", check=False)
        if host_lookup.returncode:
            raise RuntimeError("Kind node cannot resolve host.docker.internal, required for the host webhook")

        crd_dir = self.work / "crds"
        crd_dir.mkdir()
        self.run(str(ROOT / "bin/controller-gen"), "crd", "paths=./api/v1alpha1", f"output:crd:artifacts:config={crd_dir}", cwd=self.source)
        self.kubectl("apply", "-f", str(crd_dir))
        self.kubectl("apply", "-f", str(HERE / "fixtures/bootstrap.yaml"))

        self.run("openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-keyout", str(self.certs / "tls.key"), "-out", str(self.certs / "tls.crt"), "-subj", "/CN=host.docker.internal", "-addext", "subjectAltName=DNS:host.docker.internal")
        self.run("go", "build", "-o", str(self.binary), "./cmd/internal-dns", cwd=self.source)
        linux_env = os.environ.copy()
        linux_env.update({"CGO_ENABLED": "0", "GOOS": "linux", "GOARCH": "arm64"})
        process = subprocess.run(["go", "build", "-o", str(self.linux_binary), "./cmd/internal-dns"], cwd=self.source, env=linux_env, text=True, capture_output=True)
        if process.returncode:
            raise RuntimeError(f"linux agent build failed:\n{process.stdout}\n{process.stderr}")
        self.make_compute_kubeconfig()

    def make_compute_kubeconfig(self) -> None:
        token = self.kubectl("-n", "compute-system", "create", "token", "dns-publisher", "--duration=1h").stdout.strip()
        admin = json.loads(self.kubectl("config", "view", "--raw", "-o", "json").stdout)
        current = admin["current-context"]
        context = next(item["context"] for item in admin["contexts"] if item["name"] == current)
        cluster = next(item["cluster"] for item in admin["clusters"] if item["name"] == context["cluster"])
        config = {
            "apiVersion": "v1",
            "kind": "Config",
            "current-context": "compute",
            "clusters": [{"name": "e2e", "cluster": cluster}],
            "contexts": [{"name": "compute", "context": {"cluster": "e2e", "user": "compute", "namespace": PROJECT_NS}}],
            "users": [{"name": "compute", "user": {"token": token}}],
        }
        self.compute_kubeconfig.write_text(json.dumps(config))
        issuer_token = self.kubectl("-n", "compute-system", "create", "token", "dns-grant-issuer", "--duration=1h").stdout.strip()
        issuer = json.loads(json.dumps(config))
        issuer["users"][0]["user"] = {"token": issuer_token}
        self.issuer_kubeconfig.write_text(json.dumps(issuer))

    def command(self, role: str, config: Path) -> tuple[str, ...]:
        return (str(self.binary), "--role", role, "--config", str(config), "--kubeconfig", str(self.kubeconfig))

    def common_config(self, *, container: bool = False) -> dict:
        return {
            "region": "e2e",
            "shard": "shared-01",
            "platformNamespace": PLATFORM_NS,
            "nats": {"URL": "nats://10.253.0.5:4222" if container else "nats://127.0.0.1:14222", "Name": "internal-dns-e2e", "AllowInsecure": True},
            "stream": {"Name": "INTERNAL_DNS_E2E", "Subjects": ["dns.private.>"], "Replicas": 1, "SnapshotsBucket": "INTERNAL_DNS_E2E_SNAPSHOTS"},
        }

    @staticmethod
    def cmd(*args: str) -> dict:
        return {"path": args[0], "args": list(args[1:])}

    def write_configs(self) -> dict[str, Path]:
        common = self.common_config()
        agent_common = self.common_config(container=True)
        regional_backends = [
            {"memberID": "regional-0", "address": "10.253.0.20", "port": 5300},
            {"memberID": "regional-1", "address": "10.253.0.21", "port": 5300},
        ]
        control = {
            **common,
            "projects": [{"projectUID": "project-e2e-uid", "sourceClusterUID": "internal-dns-e2e-cluster", "namespace": PROJECT_NS}],
            "nodeBackends": [{"memberID": "node-bind-0", "address": "10.253.0.30", "port": 5300}],
            "clusterBackends": regional_backends,
            "members": [
                {"memberID": "node-0", "replicaID": "node-0", "role": "node"},
                {"memberID": "regional-front", "replicaID": "regional-front", "role": "resolver"},
                {"memberID": "regional-0", "replicaID": "regional-0", "role": "regional"},
                {"memberID": "regional-1", "replicaID": "regional-1", "role": "regional"},
            ],
            "consumerPrefix": "fd53::/64",
            "clusterPrefix": "fd54::/64",
            "ensureStream": True,
            "ownershipLeaseSeconds": 10,
            "managedDomainSuffix": "managed.internal",
            "privateZoneClassName": "private-bind",
            "admission": {
                "enabled": True,
                "port": 9443,
                "certDir": str(self.certs),
                "platformSubjects": ["kubernetes-admin"],
                "integrationSubjects": ["kubernetes-admin"],
                "maxContributionLeaseSeconds": 90,
                "maxAccessLeaseSeconds": 600,
            },
        }
        docker = "docker"
        configs: dict[str, dict] = {"control": control}
        node_path = "/runtime/node"
        node_files = [f"{node_path}/node-dnsdist.conf", f"{node_path}/node-bind.conf"]
        configs["node-0"] = {
            **agent_common,
            "agent": {
                "region": "e2e", "shard": "shared-01", "memberID": "node-0", "replicaID": "node-0", "mode": "resolver",
                "stateDir": "/state/node-0",
                "render": {
                    "role": "node",
                    "nodeDNSDist": {"path": node_files[0], "listenAddress": "[::]:53", "ACLs": ["fd00::/8"], "poolName": "node-shared", "backends": [{"memberID": "node-bind-0", "address": "10.253.0.30", "port": 5300}]},
                    "nodeBIND": {"path": node_files[1], "listenAddress": "10.253.0.30", "port": 5300, "proxyPeers": ["10.253.0.50"], "defaultCacheSize": "16M", "maxNegativeCacheTTLSeconds": 5, "RNDCIncludePath": "/etc/bind/rndc.key", "controlAddress": "127.0.0.1", "controlPort": 9953, "controlKeyName": "internal-dns-e2e"},
                },
                "transaction": {
                    "stageDir": node_path, "commandStageDir": node_path,
                    "validate": [
                        {**self.cmd(docker, "exec", "-u", "0", "datum-internal-dns-node-dnsdist-1", "dnsdist", "--check-config", "-C", "{stageDir}/node-dnsdist.conf"), "whenChanged": [node_files[0]]},
                        {**self.cmd(docker, "exec", "-u", "0", "datum-internal-dns-node-bind-1", "named-checkconf", "{stageDir}/node-bind.conf"), "whenChanged": [node_files[1]]},
                    ],
                    "reload": [
                        {**self.cmd(docker, "exec", "datum-internal-dns-node-bind-1", "rndc", "-s", "127.0.0.1", "-p", "9953", "-k", "/etc/bind/rndc.key", "reconfig"), "whenChanged": [node_files[1]]},
                        {**self.cmd(docker, "restart", "-t", "0", "datum-internal-dns-node-dnsdist-1"), "whenChanged": [node_files[0]]},
                    ],
                },
                "cacheFlush": [
                    self.cmd(docker, "exec", "datum-internal-dns-node-bind-1", "rndc", "-s", "127.0.0.1", "-p", "9953", "-k", "/etc/bind/rndc.key", "flushtree", "{name}"),
                ],
                "expiryInterval": 250000000, "retryDelay": 250000000, "ackInterval": 5000000000,
                "ackLease": 20000000000, "localServingLease": 8000000000,
                "watchdogLeasePath": "/state/node-0/watchdog.json", "watchdogLease": 5000000000,
                "externalWatchdog": True, "resolverDNSProbe": {"timeout": 3000000000}, "resolverVerifyConcurrency": 16,
            },
        }

        front_path = "/runtime/regional-front"
        front_file = f"{front_path}/cluster-dnsdist.conf"
        configs["regional-front"] = {
            **agent_common,
            "agent": {
                "region": "e2e", "shard": "shared-01", "memberID": "regional-front", "replicaID": "regional-front", "mode": "resolver",
                "stateDir": "/state/regional-front",
                "render": {"role": "regional-dnsdist", "clusterDNSDist": {"path": front_file, "listenAddress": "[::]:53", "ACLs": ["fd00::/8"], "poolName": "regional-shared", "backends": regional_backends}},
                "transaction": {
                    "stageDir": front_path, "commandStageDir": front_path,
                    "validate": [{**self.cmd(docker, "exec", "-u", "0", "datum-internal-dns-regional-dnsdist-1", "dnsdist", "--check-config", "-C", "{stageDir}/cluster-dnsdist.conf"), "whenChanged": [front_file]}],
                    "reload": [{**self.cmd(docker, "restart", "-t", "0", "datum-internal-dns-regional-dnsdist-1"), "whenChanged": [front_file]}],
                },
                "expiryInterval": 250000000, "retryDelay": 250000000, "ackInterval": 5000000000,
                "ackLease": 20000000000, "localServingLease": 8000000000,
                "watchdogLeasePath": "/state/regional-front/watchdog.json", "watchdogLease": 5000000000,
                "externalWatchdog": True, "resolverDNSProbe": {"timeout": 3000000000}, "resolverVerifyConcurrency": 16,
            },
        }

        for index in (0, 1):
            member = f"regional-{index}"
            daemon = f"datum-internal-dns-{member}-bind-1"
            runtime_path = f"/runtime/{member}"
            bind_file = f"{runtime_path}/cluster-bind.conf"
            bind_address = f"10.253.0.{20 + index}"
            configs[member] = {
                **agent_common,
                "agent": {
                    "region": "e2e", "shard": "shared-01", "memberID": member, "replicaID": member, "mode": "combined",
                    "stateDir": f"/state/{member}",
                    "render": {"role": "regional-bind", "clusterBIND": {"path": bind_file, "listenAddress": bind_address, "port": 5300, "proxyPeers": ["10.253.0.40", f"10.253.0.{70 + index}"], "defaultCacheSize": "16M", "maxNegativeCacheTTLSeconds": 5, "RNDCIncludePath": "/etc/bind/rndc.key", "controlAddress": "127.0.0.1", "controlPort": 9953, "controlKeyName": "internal-dns-e2e"}},
                    "transaction": {
                        "stageDir": runtime_path, "commandStageDir": runtime_path,
                        "validate": [{**self.cmd(docker, "exec", "-u", "0", daemon, "named-checkconf", "-z", "{stageDir}/cluster-bind.conf"), "whenChanged": [bind_file]}],
                        # Regional members host primary zones. `rndc reconfig`
                        # only guarantees loading configuration and new zones;
                        # use `reload` so an existing zone adopts its newly
                        # rendered immutable file before publication proof.
                        "reload": [{**self.cmd(docker, "exec", daemon, "rndc", "-s", "127.0.0.1", "-p", "9953", "-k", "/etc/bind/rndc.key", "reload"), "whenChanged": [bind_file]}],
                    },
                    "cacheFlush": [
                        self.cmd(docker, "exec", daemon, "rndc", "-s", "127.0.0.1", "-p", "9953", "-k", "/etc/bind/rndc.key", "flushtree", "{name}"),
                    ],
                    "expiryInterval": 250000000, "retryDelay": 250000000, "ackInterval": 5000000000,
                    "ackLease": 20000000000, "watchdogLeasePath": f"/state/{member}/watchdog.json",
                    "watchdogLease": 5000000000, "externalWatchdog": True,
                    "publicationDNSProbe": {"server": f"{bind_address}:5300", "timeout": 3000000000},
                },
            }

        watchdog_targets = {
            "node-0": "datum-internal-dns-node-dnsdist-1",
            "regional-front": "datum-internal-dns-regional-dnsdist-1",
            "regional-0": "datum-internal-dns-regional-0-bind-1",
            "regional-1": "datum-internal-dns-regional-1-bind-1",
        }
        for member, target in watchdog_targets.items():
            configs[f"{member}-watchdog"] = {"watchdog": {
                "leasePath": f"/state/{member}/watchdog.json", "memberID": member, "replicaID": member,
                "maxLeaseSeconds": 5, "startupGraceSeconds": 30, "intervalMillis": 250,
                "failClosed": [self.cmd(docker, "stop", target)],
            }}
        paths: dict[str, Path] = {}
        for name, value in configs.items():
            path = self.work / f"{name}.json"
            path.write_text(json.dumps(value, indent=2))
            paths[name] = path
        return paths

    def install_webhook(self) -> None:
        self.wait("admission webhook", lambda: socket.create_connection(("127.0.0.1", 9443), timeout=0.5), timeout=30)
        ca = base64.b64encode((self.certs / "tls.crt").read_bytes()).decode()
        template = (HERE / "fixtures/validating-webhook.yaml.tmpl").read_text().replace("${CA_BUNDLE}", ca)
        self.kubectl("apply", "-f", "-", stdin=template)
        denied = self.kubectl("auth", "can-i", "create", "dnscontributiongrants", "-n", PROJECT_NS, compute=True, check=False)
        discovery = self.kubectl("auth", "can-i", "list", "dnsresolvercontexts", "-n", PROJECT_NS, compute=True).stdout.strip()
        if denied.returncode != 1 or denied.stdout.strip() != "no" or discovery != "yes":
            raise AssertionError("product publisher must discover contexts but cannot issue grants")
        self.pass_check("product discovery and separate grant issuance permissions are enforced")

    def create_access(self, context_name: str, address: str, *, lifetime: int = 300) -> dict:
        def issued():
            context = json.loads(self.kubectl("-n", PROJECT_NS, "get", "dnsresolvercontext", context_name, "-o", "json").stdout)
            ready = next((row for row in context.get("status", {}).get("conditions", [])
                          if row.get("type") == "Ready"), None)
            return context if (
                int(context.get("status", {}).get("accessWriterEpoch", 0)) > 0
                and ready and ready.get("status") == "True"
                and int(ready.get("observedGeneration", 0)) == int(context["metadata"]["generation"])
            ) else None

        context = self.wait(f"resolver context {context_name} access epoch", issued, timeout=60)
        access = {
            "apiVersion": "dns.networking.miloapis.com/v1alpha1",
            "kind": "DNSResolverAccessBinding",
            "metadata": {"name": f"{context_name}-e2e", "namespace": PROJECT_NS},
            "spec": {
                "contextRef": {"name": context_name, "uid": context["metadata"]["uid"]},
                "region": "e2e",
                "queryIdentity": {"type": "DestinationAddress", "value": address},
                "port": 53,
                "transports": ["UDP", "TCP"],
                "authorization": {
                    "writerEpoch": context["status"]["accessWriterEpoch"],
                    "sequence": 1,
                    "validUntil": (datetime.now(timezone.utc) + timedelta(seconds=lifetime)).isoformat().replace("+00:00", "Z"),
                },
            },
        }
        self.kubectl("apply", "-f", "-", stdin=json.dumps(access))
        self.wait(
            f"resolver access {context_name} accepted",
            lambda: self.condition("dnsresolveraccessbinding", f"{context_name}-e2e", "Accepted", PROJECT_NS),
            timeout=60,
        )
        return context

    def create_association(self, name: str, zone_name: str, context_name: str) -> None:
        zone = json.loads(self.kubectl("-n", PROJECT_NS, "get", "dnszone", zone_name, "-o", "json").stdout)
        context = json.loads(self.kubectl("-n", PROJECT_NS, "get", "dnsresolvercontext", context_name, "-o", "json").stdout)
        value = {
            "apiVersion": "dns.networking.miloapis.com/v1alpha1", "kind": "DNSZoneAssociation", "metadata": {"name": name, "namespace": PROJECT_NS},
            "spec": {
                "dnsZoneRef": {"name": zone_name, "uid": zone["metadata"]["uid"], "generation": zone["metadata"]["generation"]},
                "resolverContextRef": {"name": context_name, "uid": context["metadata"]["uid"], "generation": context["metadata"]["generation"]},
            },
        }
        self.kubectl("apply", "-f", "-", stdin=json.dumps(value))
        self.wait(f"association {name} accepted", lambda: self.condition("dnszoneassociation", name, "Accepted", PROJECT_NS))

    def condition(self, kind: str, name: str, condition: str, namespace: str) -> bool:
        obj = json.loads(self.kubectl("-n", namespace, "get", kind, name, "-o", "json").stdout)
        return any(c["type"] == condition and c["status"] == "True" for c in obj.get("status", {}).get("conditions", []))

    def wait_for_bootstrap(self) -> None:
        def acknowledged():
            raw = json.loads(self.kubectl("-n", PLATFORM_NS, "get", "dnstransportoutboxes", "-o", "json").stdout)
            items = raw["items"]
            return items if items and all(item.get("status", {}).get("state") in ("Acknowledged", "Superseded") for item in items) else None
        self.wait("durable bootstrap snapshots", acknowledged, timeout=60)

    def wait_for_replica_publication(self, contribution_uid: str, sequence: int) -> None:
        required = {"regional-0", "regional-1"}

        def verified():
            raw = json.loads(self.kubectl("-n", PLATFORM_NS, "get", "dnspublicationmanifests", "-o", "json").stdout)
            candidates = []
            for item in raw["items"]:
                fences = item.get("spec", {}).get("contributionFences", [])
                if any(fence.get("uid") == contribution_uid and fence.get("sequence") == sequence for fence in fences):
                    candidates.append(item)
            if not candidates:
                return None
            item = max(candidates, key=lambda value: int(value["spec"]["revision"]))
            epoch = item["spec"]["writerEpoch"]
            revision = item["spec"]["revision"]
            now = datetime.now(timezone.utc) + timedelta(seconds=1)
            members = {
                ack["memberID"]
                for ack in item.get("status", {}).get("replicaAcknowledgements", [])
                if ack.get("phase") == "Verified"
                and ack.get("writerEpoch") == epoch
                and ack.get("revision") == revision
                and datetime.fromisoformat(ack["validUntil"].replace("Z", "+00:00")) > now
            }
            return {"manifest": item["metadata"]["name"], "revision": revision} if required <= members else None

        evidence = self.wait(f"both regional BIND members verified contribution sequence {sequence}", verified, timeout=75)
        self.pass_check("publication sequence verified by both regional BIND members", sequence=sequence, **evidence)

    def wait_for_local_authorization(
        self,
        binding_uid: str,
        sequence: int,
        valid_until: datetime,
        members: tuple[str, ...] = ("node-0", "regional-front", "regional-0", "regional-1"),
    ) -> dict[str, object]:
        """Wait until every serving role has durably accepted the short access fence."""

        def applied():
            observed: dict[str, str] = {}
            for member in members:
                checkpoint = self.run(
                    "docker", "exec", f"datum-internal-dns-{member}",
                    "cat", f"/state/{member}/checkpoint.json", check=False,
                )
                if checkpoint.returncode != 0 or not checkpoint.stdout.strip():
                    return None
                state = json.loads(checkpoint.stdout)
                binding = next(
                    (item for item in (state.get("snapshot") or {}).get("bindings", []) if item.get("bindingUID") == binding_uid),
                    None,
                )
                if binding is None:
                    return None
                authorization = binding.get("authorization", {})
                installed_deadline = datetime.fromisoformat(authorization.get("validUntil", "").replace("Z", "+00:00"))
                if int(authorization.get("revision", 0)) != sequence or installed_deadline > valid_until + timedelta(seconds=1):
                    return None
                observed[member] = installed_deadline.isoformat()
            return {"bindingUID": binding_uid, "sequence": sequence, "validUntil": valid_until.isoformat(), "members": observed}

        return self.wait("short access authorization installed by every serving role", applied, timeout=60)

    def publisher(self, *args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
        command = (str(HERE / "compute_publisher.py"), "--kubeconfig", str(self.compute_kubeconfig))
        if args and args[0] in ("create", "create-managed"):
            declaration = self.run(*command, "--declare-only", *args, check=check)
            if declaration.returncode:
                return declaration
            self.issue_grant(self.issuer_kubeconfig, json.loads(declaration.stdout), "internal-dns-e2e-cluster")
        return self.run(*command, *args, check=check)

    def issue_grant(self, kubeconfig: Path, declaration: dict, cluster_uid: str) -> None:
        # This fixture issuer checks the API-assigned registration lifetime and
        # authorizes only the fixed Compute principal, separately from publishing.
        base = ("kubectl", "--kubeconfig", str(kubeconfig), "-n", PROJECT_NS)
        registration = json.loads(self.run(*base, "get", "dnsregistration", declaration["registration"]["name"], "-o", "json").stdout)
        if registration["metadata"]["uid"] != declaration["registration"]["uid"]:
            raise AssertionError("registration lifetime changed before grant issuance")
        grant = {"apiVersion": "dns.networking.miloapis.com/v1alpha1", "kind": "DNSContributionGrant", "metadata": {"name": declaration["grantName"]}, "spec": {"registrationRef": declaration["registration"], "producerID": "compute-e2e", "principal": {"clusterUID": cluster_uid, "subject": "system:serviceaccount:compute-system:dns-publisher"}, "recordTypes": registration["spec"]["recordTypes"]}}
        self.run(*base, "apply", "-f", "-", stdin=json.dumps(grant))

    def publish(self, zone: str, prefix: str, record_name: str, address: str, lifetime: int = 90) -> dict:
        obj = json.loads(self.kubectl("-n", PROJECT_NS, "get", "dnszone", zone, "-o", "json").stdout)
        result = self.publisher("create", "--zone", zone, "--zone-uid", obj["metadata"]["uid"], "--zone-generation", str(obj["metadata"]["generation"]), "--prefix", prefix, "--record-name", record_name, "--address", address, "--lifetime", str(lifetime))
        return json.loads(result.stdout)

    def sync_addresses(self, count: int) -> dict[str, dict]:
        def bindings():
            raw = json.loads(self.kubectl("-n", PLATFORM_NS, "get", "dnsresolverbindings", "-o", "json").stdout)
            return raw["items"] if len(raw["items"]) >= count else None
        items = self.wait(f"{count} resolver bindings", bindings, timeout=60)
        by_vpc: dict[str, dict] = {}
        for item in items:
            spec = item["spec"]
            source = spec["source"]
            listeners = spec["configuration"]["listeners"]
            by_vpc[source["resolverContextRef"]["uid"]] = item
            self.compose("exec", "-T", "node-vips", "ip", "-6", "address", "replace", f'{listeners["node"]["address"]}/128', "dev", "eth0")
            self.compose("exec", "-T", "regional-vips", "ip", "-6", "address", "replace", f'{listeners["regional"]["address"]}/128', "dev", "eth0")
        # Docker's bridge performs IPv6 duplicate-address detection after an alias.
        time.sleep(1.5)
        return by_vpc

    def wait_for_agent_leases(self, names: tuple[str, ...]) -> None:
        def fresh():
            for name in names:
                result = self.run(
                    "docker", "exec", f"datum-internal-dns-{name}",
                    "cat", f"/state/{name}/watchdog.json", check=False,
                )
                if result.returncode:
                    return None
                lease = json.loads(result.stdout)
                deadline = datetime.fromisoformat(lease["validUntil"].replace("Z", "+00:00"))
                if deadline <= datetime.now(timezone.utc) + timedelta(seconds=1):
                    return None
            return True
        self.wait("initial verified agent steps", fresh, timeout=75)

    def query(self, server: str, name: str, tcp: bool = False) -> tuple[str, list[str]]:
        flags = ["+tcp"] if tcp else []
        comments = self.compose("exec", "-T", "probe", "dig", f"@{server}", name, "A", "+noall", "+comments", *flags).stdout
        status = next((line.split("status:", 1)[1].split(",", 1)[0].strip() for line in comments.splitlines() if "status:" in line), "UNKNOWN")
        short = self.compose("exec", "-T", "probe", "dig", f"@{server}", name, "A", "+short", *flags).stdout
        return status, [line.strip() for line in short.splitlines() if line.strip()]

    def expect_query(self, label: str, server: str, name: str, status: str, answers: list[str], tcp: bool = False, timeout: float = 45) -> None:
        last_actual: tuple[str, list[str]] | None = None

        def matches():
            nonlocal last_actual
            value = self.query(server, name, tcp)
            last_actual = value
            return value if value == (status, answers) else None
        try:
            actual = self.wait(label, matches, timeout=timeout, interval=0.5)
        except TimeoutError as exc:
            raise TimeoutError(f"timed out waiting for {label}; last DNS response: {last_actual!r}") from exc
        self.pass_check(label, server=server, name=name, transport="TCP" if tcp else "UDP", status=actual[0], answers=actual[1])

    def container_ids(self) -> dict[str, str]:
        services = ("node-dnsdist", "node-bind", "regional-dnsdist", "regional-0-bind", "regional-1-bind")
        return {service: self.compose("ps", "--all", "-q", service).stdout.strip() for service in services}

    def wait_binding_serving_condition(self, binding_name: str, expected: str, label: str) -> dict:
        def condition():
            value = json.loads(self.kubectl(
                "-n", PLATFORM_NS, "get", "dnsresolverbinding", binding_name, "-o", "json",
            ).stdout)
            row = next((item for item in value.get("status", {}).get("conditions", [])
                        if item.get("type") == "Serving"), None)
            return value if row and row.get("status") == expected else None

        value = self.wait(label, condition, timeout=35)
        self.pass_check(label, binding=binding_name, expectedServing=expected,
                        phase=value.get("status", {}).get("phase"))
        return value

    def run_suite(self) -> None:
        self.setup()
        configs = self.write_configs()
        self.start("control", *self.command("control-plane", configs["control"]))
        self.install_webhook()

        self.kubectl("apply", "-f", str(HERE / "fixtures/vpc-a.yaml"))
        context_a = self.create_access("vpc-a", "fd53::a")
        self.create_association("prod-a-vpc-a", "prod-a", "vpc-a")
        self.create_association("apps-a-vpc-a", "apps-a", "vpc-a")
        processes_before = self.container_ids()
        a_common = self.publish("prod-a", "a-common", "common", "10.0.1.10")
        a_tool = self.publish("apps-a", "a-tool", "tool", "10.0.2.10")
        bindings = self.sync_addresses(1)
        vpc_a_uid = context_a["metadata"]["uid"]
        vip_a = bindings[vpc_a_uid]["spec"]["configuration"]["listeners"]["node"]["address"]
        self.wait_for_bootstrap()
        for name in ("regional-0", "regional-1", "regional-front", "node-0"):
            self.start_container(name, configs[name], network="datum-internal-dns_dns")
        self.wait_for_agent_leases(("node-0", "regional-front", "regional-0", "regional-1"))
        # The compute simulator refreshes observations after the deliberately
        # expensive cold bootstrap, as the real Compute publisher does.
        self.publisher("observe", "--name", a_common["contribution"]["name"], "--sequence", "2", "--eligible", "true", "--lifetime", "90")
        self.publisher("observe", "--name", a_tool["contribution"]["name"], "--sequence", "2", "--eligible", "true", "--lifetime", "90")
        for name in ("regional-0-watchdog", "regional-1-watchdog"):
            self.start_container(name, configs[name], network="none", watchdog=True)
        self.expect_query("VPC A initial UDP", vip_a, "common.prod.internal", "NOERROR", ["10.0.1.10"])
        self.expect_query("VPC A second zone", vip_a, "tool.apps.internal", "NOERROR", ["10.0.2.10"])
        self.wait_for_agent_leases(("node-0", "regional-front"))
        for name in ("node-0-watchdog", "regional-front-watchdog"):
            self.start_container(name, configs[name], network="none", watchdog=True)

        self.kubectl("apply", "-f", str(HERE / "fixtures/vpc-b.yaml"))
        context_b = self.create_access("vpc-b", "fd53::b")
        self.create_association("prod-b-vpc-b", "prod-b", "vpc-b")
        b_common = self.publish("prod-b", "b-common", "common", "10.0.1.20")
        self.publish("prod-b", "b-only", "only-b", "10.0.1.21")
        bindings = self.sync_addresses(2)
        vpc_b_uid = context_b["metadata"]["uid"]
        vip_b = bindings[vpc_b_uid]["spec"]["configuration"]["listeners"]["node"]["address"]
        if self.container_ids() != processes_before:
            raise AssertionError("adding VPC B changed shared serving container identities")
        self.pass_check("adding a VPC creates no serving process")
        self.publisher("observe", "--name", b_common["contribution"]["name"], "--sequence", "2", "--eligible", "true", "--lifetime", "90")
        self.publisher("observe", "--name", "b-only-endpoint", "--sequence", "2", "--eligible", "true", "--lifetime", "90")
        for tcp in (False, True):
            self.expect_query(f"overlap VPC A {'TCP' if tcp else 'UDP'}", vip_a, "common.prod.internal", "NOERROR", ["10.0.1.10"], tcp)
            self.expect_query(f"overlap VPC B {'TCP' if tcp else 'UDP'}", vip_b, "common.prod.internal", "NOERROR", ["10.0.1.20"], tcp)
        self.expect_query("VPC B positive before denial", vip_b, "only-b.prod.internal", "NOERROR", ["10.0.1.21"])
        self.expect_query("VPC A NXDOMAIN", vip_a, "only-b.prod.internal", "NXDOMAIN", [])
        self.expect_query("VPC B unaffected by VPC A negative cache", vip_b, "only-b.prod.internal", "NOERROR", ["10.0.1.21"], True)

        managed = json.loads(
            self.publisher(
                "create-managed", "--context-uid", vpc_a_uid,
                "--registration-class", "InstanceIdentity",
                "--allocated-name", "instance-a", "--prefix", "managed-instance-a",
                "--address", "10.0.1.30", "--lifetime", "90",
            ).stdout
        )
        self.publisher("observe", "--name", managed["contribution"]["name"], "--sequence", "2", "--eligible", "true", "--lifetime", "90")
        self.wait_for_replica_publication(managed["contribution"]["uid"], 2)
        self.expect_query(
            "Compute publishes through managed VPC namespace",
            vip_a,
            managed["managedNamespace"]["fqdn"],
            "NOERROR",
            ["10.0.1.30"],
        )

        contribution = a_common["contribution"]["name"]
        self.publisher("update", "--name", contribution, "--address", "10.0.1.11", "--sequence", "3", "--lifetime", "90")
        self.expect_query("record update reaches DNS", vip_a, "common.prod.internal", "NOERROR", ["10.0.1.11"], timeout=45)
        self.publisher("delete", "--name", contribution)
        self.expect_query("contribution deletion removes answer", vip_a, "common.prod.internal", "NOERROR", [], timeout=45)

        b_only = "b-only-endpoint"
        self.publisher("observe", "--name", b_only, "--sequence", "3", "--eligible", "true", "--lifetime", "30")
        self.expect_query("health record live before withdrawal", vip_b, "only-b.prod.internal", "NOERROR", ["10.0.1.21"], timeout=30)
        self.publisher("observe", "--name", b_only, "--sequence", "4", "--eligible", "false", "--lifetime", "20")
        self.expect_query("health withdrawal removes answer", vip_b, "only-b.prod.internal", "NOERROR", [], timeout=45)
        # Recovery is a positive-path assertion. Keep it live long enough for
        # the full managed-zone publication backlog; the later disconnected
        # broker case separately exercises an intentionally short deadline.
        self.publisher("observe", "--name", b_only, "--sequence", "5", "--eligible", "true", "--lifetime", "30")
        self.expect_query("health recovery republishes answer", vip_b, "only-b.prod.internal", "NOERROR", ["10.0.1.21"], timeout=45)

        # Renew the surviving test record before the two deliberate 20s+
        # regional-member outages. Each query is made after the stopped member's
        # ACK lease has expired, so this proves regional BIND failover
        # rather than cache or a still-valid stopped-member lease.
        binding_b_name = bindings[vpc_b_uid]["metadata"]["name"]
        for sequence, stopped in enumerate(("regional-0", "regional-1"), start=3):
            self.publisher("observe", "--name", b_common["contribution"]["name"], "--sequence", str(sequence), "--eligible", "true", "--lifetime", "90")
            self.wait_for_replica_publication(b_common["contribution"]["uid"], sequence)
            self.stop_container(f"{stopped}-watchdog")
            self.stop_container(stopped)
            self.compose("stop", f"{stopped}-bind")
            time.sleep(22)
            self.wait_binding_serving_condition(
                binding_b_name, "False",
                f"project readiness is degraded after {stopped} ACK expiry",
            )
            self.compose("exec", "-T", "node-bind", "rndc", "-s", "127.0.0.1", "-p", "9953", "-k", "/etc/bind/rndc.key", "flush")
            self.expect_query(f"surviving regional member serves after {stopped} ACK expiry", vip_b, "common.prod.internal", "NOERROR", ["10.0.1.20"], timeout=20)
            self.compose("start", f"{stopped}-bind")
            self.start_container(stopped, configs[stopped], network="datum-internal-dns_dns")
            self.wait_for_agent_leases((stopped,))
            self.wait_for_replica_publication(b_common["contribution"]["uid"], sequence)
            self.wait_binding_serving_condition(
                binding_b_name, "True",
                f"project readiness recovers after {stopped} rejoins",
            )
            self.start_container(f"{stopped}-watchdog", configs[f"{stopped}-watchdog"], network="none", watchdog=True)

        partition_observation = json.loads(
            self.publisher("observe", "--name", b_only, "--sequence", "6", "--eligible", "true", "--lifetime", "15").stdout
        )
        self.wait_for_replica_publication(b_common["contribution"]["uid"], 4)
        self.wait_for_replica_publication(
            partition_observation["metadata"]["uid"],
            partition_observation["status"]["sequence"],
        )
        self.expect_query("health record live before NATS disconnect", vip_b, "only-b.prod.internal", "NOERROR", ["10.0.1.21"], timeout=30)
        partition_deadline = datetime.fromisoformat(partition_observation["status"]["validUntil"].replace("Z", "+00:00"))
        self.compose("stop", "nats")
        time.sleep(max(0, (partition_deadline - datetime.now(timezone.utc)).total_seconds()) + 3)
        self.expect_query("freshness expiry during NATS disconnect", vip_b, "only-b.prod.internal", "NOERROR", [], timeout=12)
        self.stop_container("regional-1-watchdog")
        self.stop_container("regional-1")
        self.compose("start", "nats")
        self.start_container("regional-1", configs["regional-1"], network="datum-internal-dns_dns")
        self.wait_for_agent_leases(("regional-1",))
        self.start_container("regional-1-watchdog", configs["regional-1-watchdog"], network="none", watchdog=True)
        self.expect_query("restart does not extend expired freshness", vip_b, "only-b.prod.internal", "NOERROR", [], timeout=12)

        old_generation = a_common["registration"]["generation"]
        reg_name = a_common["registration"]["name"]
        self.kubectl("-n", PROJECT_NS, "patch", "dnsregistration", reg_name, "--type=merge", "-p", '{"spec":{"ttlSeconds":3}}', compute=True)
        stale_value = {
            "apiVersion": "dns.networking.miloapis.com/v1alpha1", "kind": "DNSRecordContribution",
            "metadata": {"name": "stale-registration-generation", "namespace": PROJECT_NS},
            "spec": {
                "registrationRef": {"name": reg_name, "uid": a_common["registration"]["uid"], "generation": old_generation},
                "grantRef": {"name": a_common["grant"]["name"], "uid": a_common["grant"]["uid"]},
                "recordSets": [{"recordType": "A", "records": [{"name": "common", "a": {"content": "10.0.9.9"}}]}],
            },
        }
        stale = self.kubectl("apply", "-f", "-", compute=True, check=False, stdin=json.dumps(stale_value))
        if stale.returncode == 0 or "generation" not in (stale.stdout + stale.stderr).lower():
            raise AssertionError(f"stale registration generation was not rejected: {stale.stdout} {stale.stderr}")
        self.pass_check("stale registration generation rejected")

        grant_name = b_common["grant"]["name"]
        old_epoch = b_common["grant"]["writerEpoch"]
        self.run("kubectl", "--kubeconfig", str(self.issuer_kubeconfig), "-n", PROJECT_NS, "patch", "dnscontributiongrant", grant_name, "--type=merge", "-p", '{"spec":{"nameScopes":["common"]}}')
        self.wait(
            "new grant epoch",
            lambda: (obj := json.loads(self.kubectl("-n", PROJECT_NS, "get", "dnscontributiongrant", grant_name, "-o", "json").stdout))["status"].get("activeWriterEpoch", 0) > old_epoch,
        )
        retired = self.publisher("observe", "--name", b_common["contribution"]["name"], "--sequence", "5", "--eligible", "true", "--lifetime", "20", check=False)
        if retired.returncode == 0 or "epoch" not in (retired.stdout + retired.stderr).lower():
            raise AssertionError(f"retired grant epoch was not rejected: {retired.stdout} {retired.stderr}")
        self.pass_check("retired grant writer epoch rejected")

        binding_name = bindings[vpc_b_uid]["metadata"]["name"]
        binding_b = json.loads(self.kubectl("-n", PLATFORM_NS, "get", "dnsresolverbinding", binding_name, "-o", "json").stdout)
        access_name = binding_b["spec"]["source"]["accessBindingRef"]["name"]
        access = json.loads(self.kubectl("-n", PROJECT_NS, "get", "dnsresolveraccessbinding", access_name, "-o", "json").stdout)
        authorization = access["spec"]["authorization"]
        deadline = datetime.now(timezone.utc) + timedelta(seconds=30)
        authorization_sequence = int(authorization["sequence"]) + 1
        patch = {"spec": {"authorization": {**authorization, "sequence": authorization_sequence, "validUntil": deadline.isoformat().replace("+00:00", "Z")}}}
        self.kubectl("-n", PROJECT_NS, "patch", "dnsresolveraccessbinding", access_name, "--type=merge", "-p", json.dumps(patch))
        self.wait(
            "short access deadline projected",
            lambda: datetime.fromisoformat(json.loads(self.kubectl("-n", PLATFORM_NS, "get", "dnsresolverbinding", binding_name, "-o", "json").stdout)["spec"]["authorization"]["validUntil"].replace("Z", "+00:00")) <= deadline + timedelta(seconds=1),
            timeout=30,
        )
        local_authorization = self.wait_for_local_authorization(
            binding_b["metadata"]["uid"], authorization_sequence, deadline,
        )
        self.pass_check("short access deadline installed by every serving role", **local_authorization)
        self.stop("control")
        self.compose("stop", "nats")
        time.sleep(max(0, (deadline - datetime.now(timezone.utc)).total_seconds()) + 2)
        self.expect_query("authorization expiry fails closed", vip_b, "common.prod.internal", "REFUSED", [], timeout=15)

    def cleanup(self) -> None:
        for name in list(self.containers):
            logs = self.run("docker", "logs", name, check=False)
            (self.logs / f"{name}.log").write_text(logs.stdout + logs.stderr)
            self.run("docker", "rm", "-f", name, check=False)
            self.containers.discard(name)
        for name in list(self.processes):
            self.stop(name)
        if self.keep:
            print(f"kept E2E state at {self.work}")
            return
        self.run("kind", "delete", "cluster", "--name", CLUSTER, check=False)
        self.compose("down", "--volumes", "--remove-orphans", check=False)
        self.cleanup_runtime_mount()
        shutil.rmtree(self.work, ignore_errors=True)

    def redact(self, value: object) -> object:
        if isinstance(value, dict):
            sensitive = {"apikey", "token", "password", "passwordfile", "credentialsfile", "keyfile"}
            return {
                key: ("<redacted>" if key.lower() in sensitive else self.redact(item))
                for key, item in value.items()
            }
        if isinstance(value, list):
            return [self.redact(item) for item in value]
        return value

    def freeze_evidence(self) -> Path:
        artifact = self.results.with_name(self.results.stem + "-artifacts")
        if artifact.exists():
            shutil.rmtree(artifact)
        for directory in ("logs", "runtime", "state", "configs", "diagnostics", "qualification-source"):
            (artifact / directory).mkdir(parents=True, exist_ok=True)

        resources = (
            "dnsresolvercontexts,dnsresolveraccessbindings,dnszones,dnszoneassociations,"
            "dnsregistrations,dnscontributiongrants,dnsrecordcontributions,"
            "dnsresolverbindings,dnspublicationmanifests,dnspublicationownerships,dnstransportoutboxes"
        )
        snapshot = self.kubectl("get", resources, "-A", "-o", "json", check=False)
        (artifact / "diagnostics" / "dns-resources.json").write_text(snapshot.stdout)
        compose_state = self.compose("ps", "--all", "--format", "json", check=False)
        (artifact / "diagnostics" / "compose.jsonl").write_text(compose_state.stdout)
        for service in ("node-bind", "node-dnsdist", "regional-0-bind", "regional-1-bind", "regional-dnsdist", "nats"):
            logs = self.compose("logs", "--no-color", service, check=False)
            (artifact / "logs" / f"compose-{service}.log").write_text(logs.stdout + logs.stderr)

        for container in sorted(self.containers):
            logs = self.run("docker", "logs", container, check=False)
            (self.logs / f"{container}.log").write_text(logs.stdout + logs.stderr)
        for name in list(self.processes):
            self.stop(name)
        for path in self.logs.glob("*.log"):
            shutil.copy2(path, artifact / "logs" / path.name)
        self.capture_runtime(artifact / "runtime")
        self.capture_state(artifact / "state")
        for name in ("control", "node-0", "regional-front", "regional-0", "regional-1"):
            path = self.work / f"{name}.json"
            if path.exists():
                value = self.redact(json.loads(path.read_text()))
                (artifact / "configs" / path.name).write_text(json.dumps(value, indent=2) + "\n")

        qualification = artifact / "qualification-source"
        for path in (Path(__file__), HERE / "compute_publisher.py", DEV / "compose.yaml"):
            if path.exists():
                target = qualification / path.name
                shutil.copy2(path, target)
        shutil.copytree(HERE / "fixtures", qualification / "fixtures", dirs_exist_ok=True)
        shutil.copytree(DEV / "runtime", qualification / "dev-runtime", dirs_exist_ok=True)
        digest = hashlib.sha256()
        for path in sorted(item for item in qualification.rglob("*") if item.is_file()):
            digest.update(str(path.relative_to(qualification)).encode() + b"\0")
            digest.update(path.read_bytes())
        self.qualification_hash = digest.hexdigest()
        (artifact / "source.sha256").write_text(self.source_hash + "\n")
        (artifact / "qualification-source.sha256").write_text(self.qualification_hash + "\n")
        (artifact / "metadata.json").write_text(json.dumps({
            "capturedAt": datetime.now(timezone.utc).isoformat(),
            "sourceSHA256": self.source_hash,
            "qualificationSourceSHA256": self.qualification_hash,
            "dockerHost": os.environ.get("DOCKER_HOST"),
            "credentialsIncluded": False,
            "topology": {"nodeMembers": 1, "regionalDNSDistFrontends": 1, "regionalBINDMembers": 2, "perVPCProcesses": 0},
        }, indent=2) + "\n")
        return artifact

    def write_results(self, error: BaseException | None, artifact: Path | None) -> None:
        payload = {
            "passed": error is None,
            "error": None if error is None else str(error),
            "checks": RESULTS,
            "sourceSHA256": self.source_hash,
            "qualificationSourceSHA256": self.qualification_hash,
            "images": {
                "dnsdist": "powerdns/dnsdist-20@sha256:4ed9af56729c7021795dc478421237ab9cf6c7ceec8734677405e7617f06d52f",
                "bind": "internetsystemsconsortium/bind9@sha256:071465f88068854d0ceadd9b985fb1acae4071e80754e545cd811fde57541ad0",
            },
            "topology": {"nodeMembers": 1, "regionalDNSDistFrontends": 1, "regionalBINDMembers": 2, "perVPCProcesses": 0},
            "dockerHost": os.environ.get("DOCKER_HOST"),
            "artifactDirectory": None if artifact is None else str(artifact),
        }
        self.results.parent.mkdir(parents=True, exist_ok=True)
        self.results.write_text(json.dumps(payload, indent=2) + "\n")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--keep", action="store_true", default=os.environ.get("INTERNAL_DNS_KEEP") == "1")
    parser.add_argument("--results", type=Path, default=HERE / "results/bind-e2e-latest.json")
    args = parser.parse_args()
    harness = Harness(args.keep, args.results)
    error: BaseException | None = None
    artifact: Path | None = None
    try:
        harness.run_suite()
    except BaseException as exc:
        error = exc
        print(f"FAIL {exc}", file=sys.stderr)
        for name, process in harness.processes.items():
            if process.poll() not in (None, 0):
                print(f"--- {name} ---\n{(harness.logs / f'{name}.log').read_text()[-6000:]}", file=sys.stderr)
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
    print(f"qualified {len(RESULTS)} full-path checks")
    return 0


if __name__ == "__main__":
    sys.exit(main())
