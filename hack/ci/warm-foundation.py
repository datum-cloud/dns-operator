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
import shutil
import subprocess
import sys
import time
import uuid

ROOT = Path(__file__).resolve().parents[2]
CLUSTERS = ("dns-control", "dns-upstream", "dns-edge")
RECEIPT = "dns-ci-foundation"
NAMESPACE = "kube-system"
FORBIDDEN_NAMESPACES = {
    "dns-agent-system", "dns-replicator-system",
    "internal-dns-system", "project-e2e",
}

TOOLS = {
    "kustomize": ("KUSTOMIZE_VERSION", "sigs.k8s.io/kustomize/kustomize/v5"),
    "controller-gen": ("CONTROLLER_TOOLS_VERSION", "sigs.k8s.io/controller-tools"),
    "chainsaw": ("CHAINSAW_VERSION", "github.com/kyverno/chainsaw"),
}
RUSTFS_NAMESPACE = "rustfs-system"
RUSTFS_BUCKET = "lightningstream"
AWS_CLI_IMAGE = "amazon/aws-cli:2.17.62"
# Public, disposable fixture credentials from config/dependencies/rustfs.
# Never replace these with production or user credentials.
RUSTFS_ACCESS_KEY = "dns-federation"
RUSTFS_SECRET_KEY = "dns-federation-secret"
CLAIM_STAMP = "dev/warm-foundation-claimed.json"
BUNDLED_TOOLS = Path("/opt/dns-ci-tools")


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
                 "hack/ci/warm-foundation.py", "hack/install-go-tool.sh",
                 *sorted(str(p.relative_to(ROOT)) for p in
                         (ROOT / "config/dependencies/rustfs").glob("*.yaml"))):
        digest.update(path.encode() + b"\0" + (ROOT / path).read_bytes())
    digest.update(run("kind", "version").encode())
    return digest.hexdigest()


def verify_clean(cluster):
    namespaces = {item["metadata"]["name"] for item in get(cluster, "namespaces")["items"]}
    dirty = namespaces & FORBIDDEN_NAMESPACES
    if cluster != CLUSTERS[0] and RUSTFS_NAMESPACE in namespaces:
        dirty.add(RUSTFS_NAMESPACE)
    crds = [item["metadata"]["name"] for item in get(cluster, "crds")["items"]
            if item["spec"]["group"] == "dns.networking.miloapis.com"]
    if dirty or crds:
        raise ValueError(f"{cluster} contains DNS workload state: {sorted(dirty)}, {crds}")


def tool_pins():
    taskfile = (ROOT / "Taskfile.yaml").read_text()
    pins = {}
    for name, (variable, module) in TOOLS.items():
        match = re.search(rf"^  {variable}: ['\"]?(v[0-9]+\.[0-9]+\.[0-9]+)['\"]?\s*$",
                          taskfile, re.MULTILINE)
        if not match:
            raise ValueError(f"Missing immutable {variable} tool pin")
        pins[name] = (module, match[1])
    return pins


def matches_tool(binary, module, version):
    if not binary.is_file() or not os.access(binary, os.X_OK):
        return False
    metadata = run("go", "version", "-m", str(binary))
    return any(parts[:3] == ["mod", module, version]
               for parts in (line.split() for line in metadata.splitlines()))


def install_tools():
    """Use runner-bundled pinned binaries; compile only missing/mismatched tools."""
    fallback = []
    (ROOT / "bin").mkdir(exist_ok=True)
    for name, (module, version) in tool_pins().items():
        source = BUNDLED_TOOLS / name
        try:
            bundled = matches_tool(source, module, version)
        except subprocess.CalledProcessError:
            bundled = False
        if not bundled:
            fallback.append("tools:" + name)
            continue
        target = ROOT / "bin" / f"{name}-{version}"
        shutil.copy2(source, target)
        link = ROOT / "bin" / name
        link.unlink(missing_ok=True)
        link.symlink_to(target)
    if fallback:
        run("task", "--yes", *fallback, capture=False)


def tool_inventory():
    """Check actual Go module versions and bind the installed executable bytes."""
    inventory = {}
    for name, (module, version) in tool_pins().items():
        binary = ROOT / "bin" / name
        if not matches_tool(binary, module, version):
            raise ValueError(f"Prepared {name} does not match {module}@{version}")
        inventory[name] = {"version": version, "sha256": hashlib.sha256(binary.read_bytes()).hexdigest()}
    return inventory


def assert_empty_bucket(observation):
    buckets = observation["buckets"].get("Buckets", [])
    if [bucket["Name"] for bucket in buckets] != [RUSTFS_BUCKET]:
        raise ValueError("RustFS must contain only the expected lightningstream bucket")
    for section, fields in (("objects", ("Contents",)),
                            ("versions", ("Versions", "DeleteMarkers")),
                            ("uploads", ("Uploads",))):
        result = observation[section]
        if result.get("IsTruncated") or any(result.get(field) for field in fields):
            raise ValueError(f"RustFS bucket contains residual {section}; refusing to clear or reuse it")
    if observation["objects"].get("KeyCount", 0) != 0:
        raise ValueError("RustFS bucket is not empty")


def verify_empty_bucket():
    """Read S3 state inside the cluster; never delete bucket data to make it pass."""
    pod_name = "foundation-s3-check-" + uuid.uuid4().hex[:12]
    script = """set -eu
aws() { command aws --endpoint-url http://rustfs.rustfs-system.svc.cluster.local:9000 --region us-east-1 --output json --no-paginate "$@"; }
aws s3api list-buckets > /tmp/buckets.json
aws s3api list-objects-v2 --bucket lightningstream --max-keys 1 > /tmp/objects.json
aws s3api list-object-versions --bucket lightningstream --max-keys 1 > /tmp/versions.json
aws s3api list-multipart-uploads --bucket lightningstream --max-uploads 1 > /tmp/uploads.json
printf '{"buckets":'; cat /tmp/buckets.json
printf ',"objects":'; cat /tmp/objects.json
printf ',"versions":'; cat /tmp/versions.json
printf ',"uploads":'; cat /tmp/uploads.json
printf '}\n'
"""
    pod = {"apiVersion": "v1", "kind": "Pod", "metadata": {
        "name": pod_name, "namespace": RUSTFS_NAMESPACE,
        "labels": {"dns-ci.datum.net/foundation-probe": "true"}}, "spec": {
        "restartPolicy": "Never", "automountServiceAccountToken": False,
        "containers": [{"name": "check", "image": AWS_CLI_IMAGE,
                        "command": ["sh", "-ec", script], "env": [
                            {"name": "AWS_ACCESS_KEY_ID", "value": RUSTFS_ACCESS_KEY},
                            {"name": "AWS_SECRET_ACCESS_KEY", "value": RUSTFS_SECRET_KEY},
                            {"name": "AWS_EC2_METADATA_DISABLED", "value": "true"},
                            {"name": "AWS_PAGER", "value": ""}]}]}}
    kubectl(CLUSTERS[0], "create", "-f", "-", input=json.dumps(pod))
    # A failed probe is intentionally retained for diagnostics; fail closed.
    kubectl(CLUSTERS[0], "-n", RUSTFS_NAMESPACE, "wait", f"pod/{pod_name}",
            "--for=jsonpath={.status.phase}=Succeeded", "--timeout=90s")
    observation = json.loads(kubectl(CLUSTERS[0], "-n", RUSTFS_NAMESPACE, "logs", pod_name))
    assert_empty_bucket(observation)
    kubectl(CLUSTERS[0], "-n", RUSTFS_NAMESPACE, "delete", "pod", pod_name,
            "--wait=true", "--timeout=30s")


def rustfs_inventory():
    cluster = CLUSTERS[0]
    kubectl(cluster, "-n", RUSTFS_NAMESPACE, "wait", "helmrelease/rustfs",
            "--for=condition=Ready", "--timeout=120s")
    release = get(cluster, "helmrelease", "rustfs", "-n", RUSTFS_NAMESPACE)
    auth = release["spec"]["values"]["auth"]
    if auth != {"accessKey": RUSTFS_ACCESS_KEY, "secretKey": RUSTFS_SECRET_KEY}:
        raise ValueError("RustFS fixture credential configuration differs from the dedicated CI defaults")
    history = release["status"].get("history", [])
    if not history or not history[0].get("chartVersion"):
        raise ValueError("RustFS resolved Helm chart revision is unavailable")
    resources = get(cluster, "deployments,statefulsets,services,persistentvolumeclaims",
                    "-n", RUSTFS_NAMESPACE)["items"]
    inventory = {}
    service = None
    for resource in resources:
        metadata = resource["metadata"]
        if metadata.get("labels", {}).get("app.kubernetes.io/instance") != "rustfs":
            raise ValueError(f"Unexpected resource in RustFS namespace: {resource['kind']}/{metadata['name']}")
        if resource["kind"] == "Service" and metadata["name"] == "rustfs":
            service = resource
        inventory[f"{resource['kind']}/{metadata['name']}"] = {
            "uid": metadata["uid"],
            "spec_sha256": hashlib.sha256(json.dumps(resource["spec"], sort_keys=True).encode()).hexdigest(),
        }
    if service is None:
        raise ValueError("RustFS service is missing")
    ports = [p for p in service["spec"]["ports"] if p["port"] == 9000]
    if len(ports) != 1 or not ports[0].get("nodePort"):
        raise ValueError("RustFS S3 NodePort is unavailable")
    pods = get(cluster, "pods", "-n", RUSTFS_NAMESPACE)["items"]
    for pod in pods:
        metadata = pod["metadata"]
        if metadata.get("labels", {}).get("app.kubernetes.io/instance") == "rustfs":
            continue
        if (metadata["name"] == "rustfs-mb" and pod["status"].get("phase") == "Succeeded"
                and len(pod["spec"]["containers"]) == 1
                and pod["spec"]["containers"][0]["image"] == AWS_CLI_IMAGE):
            continue
        raise ValueError(f"Unexpected pod in RustFS namespace: {metadata['name']}")
    verify_empty_bucket()
    return {"release_uid": release["metadata"]["uid"],
            "release_spec_sha256": hashlib.sha256(json.dumps(release["spec"], sort_keys=True).encode()).hexdigest(),
            "chart_version": history[0]["chartVersion"],
            "chart_digest": history[0].get("digest", ""),
            "resources": inventory, "nodeport": ports[0]["nodePort"]}


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
    if data["schema"] != "2" or data["state"] != "prepared":
        raise ValueError("Foundation is not prepared or has already been claimed")
    if data["fingerprint"] != expected_fingerprint:
        raise ValueError("Foundation inputs changed; prepare a fresh environment")
    if now >= float(data["expires_unix"]):
        raise ValueError("Foundation expired; prepare a fresh environment")


def prepare(ttl):
    existing = set(run("kind", "get", "clusters").splitlines()) & set(CLUSTERS)
    if existing:
        raise ValueError(f"Refusing to prepare over existing clusters: {sorted(existing)}")
    # Reuse the exact CI foundation topology, including dual-stack and audit mounts.
    run("task", "--yes", "env:foundation-up", capture=False)
    expected_ref = re.search(r"TEST_INFRA_REPO_REF: '([0-9a-f]{40})'",
                             (ROOT / "Taskfile.yaml").read_text()).group(1)
    actual_ref = run("git", "-C", str(ROOT / ".test-infra"), "rev-parse", "HEAD")
    if actual_ref != expected_ref:
        raise ValueError("test-infra checkout does not match the pinned foundation revision")
    run("git", "-C", str(ROOT / ".test-infra"), "diff", "--exit-code", "HEAD", "--")
    # Bootstrap may upgrade kind; fingerprint the tools that actually prepared
    # the clusters so a subsequent claim sees the same version.
    key = fingerprint()
    export_configs()
    identities = health_and_identity()
    install_tools()
    tools = tool_inventory()
    run("task", "--yes", "env:rustfs-up", capture=False)
    rustfs = rustfs_inventory()
    now = time.time()
    receipt = {
        "apiVersion": "v1", "kind": "ConfigMap",
        "metadata": {"name": RECEIPT, "namespace": NAMESPACE},
        "data": {"schema": "2", "state": "prepared", "fingerprint": key,
                 "prepared_at": datetime.now(timezone.utc).isoformat(),
                 "expires_unix": str(now + ttl),
                 "cluster_uids": json.dumps(identities, sort_keys=True),
                 "tools": json.dumps(tools, sort_keys=True),
                 "rustfs": json.dumps(rustfs, sort_keys=True)},
    }
    kubectl(CLUSTERS[0], "create", "-f", "-", input=json.dumps(receipt))
    print("Foundation prepared. It may be claimed once; DNS has not been deployed.")


def claim():
    export_configs()
    receipt = get(CLUSTERS[0], "configmap", RECEIPT, "-n", NAMESPACE)
    validate_receipt(receipt, fingerprint(), time.time())
    (ROOT / CLAIM_STAMP).unlink(missing_ok=True)
    # Kubernetes resourceVersion makes two concurrent claims mutually exclusive.
    # Claim BEFORE readiness/clean checks: a failed check burns this environment.
    receipt["data"]["state"] = "claimed"
    receipt["data"]["claim_id"] = str(uuid.uuid4())
    receipt["data"]["claimed_at"] = datetime.now(timezone.utc).isoformat()
    kubectl(CLUSTERS[0], "replace", "-f", "-", input=json.dumps(receipt))
    identities = health_and_identity()
    if identities != json.loads(receipt["data"]["cluster_uids"]):
        raise ValueError("A foundation cluster was replaced after preparation")
    if tool_inventory() != json.loads(receipt["data"]["tools"]):
        raise ValueError("Prepared tools changed after preparation")
    rustfs = rustfs_inventory()
    if rustfs != json.loads(receipt["data"]["rustfs"]):
        raise ValueError("RustFS changed after preparation")
    # Do not merge anything into the operator's default kubeconfig.
    (ROOT / "dev").mkdir(exist_ok=True)
    for cluster in CLUSTERS:
        run("kind", "export", "kubeconfig", "--name", cluster,
            "--kubeconfig", str(ROOT / "dev/kubeconfig"))
    (ROOT / "dev/kubeconfig").chmod(0o600)
    (ROOT / "dev/rustfs.nodeport").write_text(str(rustfs["nodeport"]) + "\n")
    (ROOT / CLAIM_STAMP).write_text(json.dumps({"schema": 2, "claim_id": receipt["data"]["claim_id"],
                                              "fingerprint": receipt["data"]["fingerprint"]}) + "\n")
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
