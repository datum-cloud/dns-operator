#!/usr/bin/env python3
"""Single-use warm foundation experiment on a dedicated Docker-capable executor.

This is a prepared-environment handoff, not a pool scheduler or a VM snapshotter.
The selected Docker daemon must belong exclusively to this environment's owner.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import uuid

ROOT = Path(__file__).resolve().parents[2]
CLUSTERS = ("dns-control", "dns-upstream", "dns-edge")
RECEIPT = "dns-ci-foundation"
NAMESPACE = "kube-system"
FORBIDDEN_NAMESPACES = {
    "dns-agent-system", "dns-replicator-system", "rustfs-system",
    "internal-dns-system", "project-e2e",
}


def run(*args, capture=True, input=None):
    result = subprocess.run(args, cwd=ROOT, text=True, input=input, check=True,
                            stdout=subprocess.PIPE if capture else None)
    return result.stdout.strip() if capture else None


def config(cluster):
    return ROOT / ".internal-dns-e2e" / "foundation" / f"{cluster}.kubeconfig"


def export_configs():
    directory = config(CLUSTERS[0]).parent
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    for cluster in CLUSTERS:
        run("kind", "export", "kubeconfig", "--name", cluster,
            "--kubeconfig", str(config(cluster)))
        config(cluster).chmod(0o600)


def kubectl(cluster, *args, input=None):
    return run("kubectl", "--kubeconfig", str(config(cluster)),
               "--request-timeout=30s", *args, input=input)


def get(cluster, *args):
    return json.loads(kubectl(cluster, "get", *args, "-o", "json"))


def fingerprint():
    # Conservative invalidation: any Taskfile change rebuilds the foundation.
    # The immutable include revision pins the shared cluster/add-on definition.
    taskfile = (ROOT / "Taskfile.yaml").read_text()
    match = re.search(r"TEST_INFRA_REPO_REF: '([0-9a-f]{40})'", taskfile)
    if not match:
        raise ValueError("Foundation experiment requires an immutable test-infra commit")
    digest = hashlib.sha256()
    for path in ("Taskfile.yaml", "test/internaldns/kubernetes/prepare-kind.sh",
                 "hack/ci/warm-foundation.py"):
        digest.update(path.encode() + b"\0" + (ROOT / path).read_bytes())
    digest.update(run("kind", "version").encode())
    return digest.hexdigest()


def verify_clean(cluster):
    namespaces = {item["metadata"]["name"] for item in get(cluster, "namespaces")["items"]}
    dirty = namespaces & FORBIDDEN_NAMESPACES
    crds = [item["metadata"]["name"] for item in get(cluster, "crds")["items"]
            if item["spec"]["group"] == "dns.networking.miloapis.com"]
    if dirty or crds:
        raise ValueError(f"{cluster} contains DNS workload state: {sorted(dirty)}, {crds}")


def verify_nodes(nodes):
    if not nodes["items"]:
        raise ValueError("Foundation has no nodes")
    for node in nodes["items"]:
        conditions = {c["type"]: c["status"] for c in node["status"]["conditions"]}
        cidrs = node["spec"].get("podCIDRs", [])
        if conditions.get("Ready") != "True" or not any(":" in c for c in cidrs) or not any("." in c for c in cidrs):
            raise ValueError(f"Node {node['metadata']['name']} must be Ready and dual-stack")


def health_and_identity():
    identities = {}
    for cluster in CLUSTERS:
        kubectl(cluster, "get", "--raw=/readyz")
        verify_nodes(get(cluster, "nodes"))
        verify_clean(cluster)
        for namespace, resources in (
            ("flux-system", [f"deployment/{name}-controller" for name in
                             ("source", "helm", "kustomize", "notification")]),
            ("kyverno", [f"deployment/kyverno-{name}-controller" for name in
                         ("admission", "background", "cleanup", "reports")]),
        ):
            kubectl(cluster, "-n", namespace, "wait", *resources,
                    "--for=condition=Available", "--timeout=120s")
        kubectl(cluster, "-n", "cert-manager", "wait",
                "helmrelease/cert-manager", "helmrelease/cert-manager-csi-driver",
                "--for=condition=Ready", "--timeout=120s")
        kubectl(cluster, "-n", "flux-system", "wait", "helmrelease/envoy-gateway",
                "--for=condition=Ready", "--timeout=120s")
        kubectl(cluster, "-n", "envoy-gateway-system", "wait", "gateway/default-gateway",
                "--for=condition=Programmed", "--timeout=120s")
        identities[cluster] = get(cluster, "namespace", "kube-system")["metadata"]["uid"]
    return identities


def validate_receipt(receipt, expected_fingerprint, now):
    data = receipt["data"]
    if data["schema"] != "1" or data["state"] != "prepared":
        raise ValueError("Foundation is not prepared or has already been claimed")
    if data["fingerprint"] != expected_fingerprint:
        raise ValueError("Foundation inputs changed; prepare a fresh environment")
    if now >= float(data["expires_unix"]):
        raise ValueError("Foundation expired; prepare a fresh environment")


def prepare(ttl):
    existing = set(run("kind", "get", "clusters").splitlines()) & set(CLUSTERS)
    if existing:
        raise ValueError(f"Refusing to prepare over existing clusters: {sorted(existing)}")
    key = fingerprint()
    # Reuse the exact CI foundation topology, including dual-stack and audit mounts.
    run("task", "--yes", "env:foundation-up", capture=False)
    expected_ref = re.search(r"TEST_INFRA_REPO_REF: '([0-9a-f]{40})'",
                             (ROOT / "Taskfile.yaml").read_text()).group(1)
    actual_ref = run("git", "-C", str(ROOT / ".test-infra"), "rev-parse", "HEAD")
    if actual_ref != expected_ref:
        raise ValueError("test-infra checkout does not match the pinned foundation revision")
    run("git", "-C", str(ROOT / ".test-infra"), "diff", "--exit-code", "HEAD", "--")
    export_configs()
    identities = health_and_identity()
    now = time.time()
    receipt = {
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": RECEIPT, "namespace": NAMESPACE},
        "data": {"schema": "1", "state": "prepared", "fingerprint": key,
                 "prepared_at": datetime.now(timezone.utc).isoformat(),
                 "expires_unix": str(now + ttl),
                 "cluster_uids": json.dumps(identities, sort_keys=True)},
    }
    kubectl(CLUSTERS[0], "create", "-f", "-", input=json.dumps(receipt))
    print("Foundation prepared. It may be claimed once; DNS has not been deployed.")


def claim():
    export_configs()
    receipt = get(CLUSTERS[0], "configmap", RECEIPT, "-n", NAMESPACE)
    validate_receipt(receipt, fingerprint(), time.time())
    # Kubernetes resourceVersion makes two concurrent claims mutually exclusive.
    # Claim BEFORE readiness/clean checks: a failed check burns this environment.
    receipt["data"]["state"] = "claimed"
    receipt["data"]["claim_id"] = str(uuid.uuid4())
    receipt["data"]["claimed_at"] = datetime.now(timezone.utc).isoformat()
    kubectl(CLUSTERS[0], "replace", "-f", "-", input=json.dumps(receipt))
    identities = health_and_identity()
    if identities != json.loads(receipt["data"]["cluster_uids"]):
        raise ValueError("A foundation cluster was replaced after preparation")
    # Do not merge anything into the operator's default kubeconfig.
    (ROOT / "dev").mkdir(exist_ok=True)
    for cluster in CLUSTERS:
        run("kind", "export", "kubeconfig", "--name", cluster,
            "--kubeconfig", str(ROOT / "dev/kubeconfig"))
    (ROOT / "dev/kubeconfig").chmod(0o600)
    print("Foundation claimed and checked. Deploy DNS, then destroy this environment.")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("prepare", "claim"))
    parser.add_argument("--ttl-seconds", type=int, default=3600,
                        help="prepared foundation lifetime, used only by prepare")
    args = parser.parse_args()
    if args.ttl_seconds <= 0:
        parser.error("--ttl-seconds must be positive")
    os.umask(0o077)
    try:
        if args.action == "prepare":
            prepare(args.ttl_seconds)
        else:
            claim()
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError) as error:
        print(f"Foundation handoff failed: {error}. Do not deploy DNS or reuse this environment.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
