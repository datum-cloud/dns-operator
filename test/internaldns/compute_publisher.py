#!/usr/bin/env python3
"""A deliberately unprivileged Compute DNS publisher used by the E2E suite."""

from __future__ import annotations

import argparse
from datetime import datetime, timedelta, timezone
import json
import subprocess
import sys
import time


GROUP_VERSION = "dns.networking.miloapis.com/v1alpha1"
SUBJECT = "system:serviceaccount:compute-system:dns-publisher"
CLUSTER_UID = "internal-dns-e2e-cluster"


class Publisher:
    def __init__(self, kubeconfig: str, namespace: str, source_cluster_uid: str = CLUSTER_UID,
                 status_timeout: float = 30):
        self.base = ["kubectl", "--kubeconfig", kubeconfig, "-n", namespace]
        self.source_cluster_uid = source_cluster_uid
        self.status_timeout = status_timeout

    def kubectl(self, *args: str, stdin: str | None = None, check: bool = True) -> subprocess.CompletedProcess[str]:
        process = subprocess.run([*self.base, *args], input=stdin, text=True, capture_output=True)
        if check and process.returncode:
            raise RuntimeError(f"kubectl {' '.join(args)} failed:\n{process.stdout}\n{process.stderr}")
        return process

    def apply(self, value: dict) -> dict:
        self.kubectl("apply", "-f", "-", stdin=json.dumps(value))
        return self.get(value["kind"], value["metadata"]["name"])

    def get(self, kind: str, name: str) -> dict:
        return json.loads(self.kubectl("get", kind, name, "-o", "json").stdout)

    def wait_status(self, kind: str, name: str, path: list[str], timeout: float | None = None) -> object:
        if timeout is None:
            timeout = self.status_timeout
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            obj = self.get(kind, name)
            value: object = obj
            for key in path:
                value = value.get(key) if isinstance(value, dict) else None
            if value:
                return value
            time.sleep(0.25)
        raise TimeoutError(f"{kind}/{name} did not populate {'.'.join(path)}")

    @staticmethod
    def record_sets(record_name: str, address: str, aaaa_address: str | None = None) -> list[dict]:
        values = [{"recordType": "A", "records": [{"name": record_name, "a": {"content": address}}]}]
        if aaaa_address:
            values.append({"recordType": "AAAA", "records": [{"name": record_name, "aaaa": {"content": aaaa_address}}]})
        return values

    def create(
        self,
        zone_ref: dict,
        prefix: str,
        record_name: str,
        address: str,
        lifetime: int,
        aaaa_address: str | None = None,
    ) -> dict:
        record_types = ["A", "AAAA"] if aaaa_address else ["A"]
        registration = self.apply(
            {
                "apiVersion": GROUP_VERSION,
                "kind": "DNSRegistration",
                "metadata": {"name": f"{prefix}-registration"},
                "spec": {
                    "dnsZoneRef": zone_ref,
                    "name": record_name,
                    "recordTypes": record_types,
                    "publicationPolicy": "EligibleContributions",
                    "ttlSeconds": 2,
                },
            }
        )
        reg_ref = {
            "name": registration["metadata"]["name"],
            "uid": registration["metadata"]["uid"],
            "generation": registration["metadata"]["generation"],
        }
        grant = self.apply(
            {
                "apiVersion": GROUP_VERSION,
                "kind": "DNSContributionGrant",
                "metadata": {"name": f"{prefix}-compute"},
                "spec": {
                    "registrationRef": reg_ref,
                    "producerID": "compute-e2e",
                    "principal": {"clusterUID": self.source_cluster_uid, "subject": SUBJECT},
                    "recordTypes": record_types,
                },
            }
        )
        writer_epoch = int(self.wait_status("dnscontributiongrant", grant["metadata"]["name"], ["status", "activeWriterEpoch"]))
        contribution = self.apply(
            {
                "apiVersion": GROUP_VERSION,
                "kind": "DNSRecordContribution",
                "metadata": {"name": f"{prefix}-endpoint"},
                "spec": {
                    "registrationRef": reg_ref,
                    "grantRef": {"name": grant["metadata"]["name"], "uid": grant["metadata"]["uid"]},
                    "recordSets": self.record_sets(record_name, address, aaaa_address),
                },
            }
        )
        bound_epoch = int(self.wait_status("dnsrecordcontribution", contribution["metadata"]["name"], ["status", "writerEpoch"]))
        if bound_epoch != writer_epoch:
            raise AssertionError(f"controller bound writer epoch {bound_epoch}, grant issued {writer_epoch}")
        self.observe(contribution["metadata"]["name"], sequence=1, eligible=True, lifetime=lifetime)
        return {
            "registration": reg_ref,
            "grant": {"name": grant["metadata"]["name"], "uid": grant["metadata"]["uid"], "writerEpoch": writer_epoch},
            "contribution": {"name": contribution["metadata"]["name"], "uid": contribution["metadata"]["uid"]},
        }

    def managed_namespace(self, context_uid: str, timeout: float = 45) -> dict:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            items = json.loads(self.kubectl("get", "dnsresolvercontexts", "-o", "json").stdout)["items"]
            for item in items:
                if item.get("metadata", {}).get("uid") != context_uid:
                    continue
                managed = item.get("status", {}).get("managedNamespace", {})
                zone_ref = managed.get("dnsZoneRef", {})
                if zone_ref.get("name") and zone_ref.get("uid") and managed.get("suffix"):
                    return item
            time.sleep(0.25)
        raise TimeoutError(f"managed DNS namespace for resolver context {context_uid} was not ready")

    def create_managed(
        self,
        context_uid: str,
        registration_class: str,
        allocated_name: str,
        prefix: str,
        address: str,
        lifetime: int,
        aaaa_address: str | None = None,
    ) -> dict:
        context = self.managed_namespace(context_uid)
        managed = context["status"]["managedNamespace"]
        name_prefix = "instances" if registration_class == "InstanceIdentity" else "services"
        label = "-".join(filter(None, (part.lower() for part in allocated_name.replace("_", "-").split("-"))))
        record_name = f"{label}.{name_prefix}"
        result = self.create(managed["dnsZoneRef"], prefix, record_name, address, lifetime, aaaa_address)
        result["managedNamespace"] = {
            "context": context["metadata"]["name"],
            "canonicalSuffix": managed["suffix"],
            "recordName": record_name,
            "fqdn": f'{record_name}.{managed["suffix"]}',
        }
        return result

    def observe(self, name: str, sequence: int, eligible: bool, lifetime: int) -> dict:
        contribution = self.get("dnsrecordcontribution", name)
        deadline = datetime.now(timezone.utc) + timedelta(seconds=lifetime)
        status = {
            "observedGeneration": contribution["metadata"]["generation"],
            "writerEpoch": contribution["status"]["writerEpoch"],
            "sequence": sequence,
            "eligible": eligible,
            "reason": "EndpointReady" if eligible else "EndpointNotReady",
            "validUntil": deadline.isoformat().replace("+00:00", "Z"),
        }
        patch = json.dumps({"status": status})
        self.kubectl("patch", "dnsrecordcontribution", name, "--subresource=status", "--type=merge", "-p", patch)
        return self.get("dnsrecordcontribution", name)

    def update(
        self,
        name: str,
        address: str,
        sequence: int,
        lifetime: int,
        record_name: str = "common",
        aaaa_address: str | None = None,
    ) -> dict:
        patch = json.dumps({"spec": {"recordSets": self.record_sets(record_name, address, aaaa_address)}})
        self.kubectl("patch", "dnsrecordcontribution", name, "--type=merge", "-p", patch)
        return self.observe(name, sequence=sequence, eligible=True, lifetime=lifetime)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--kubeconfig", required=True)
    parser.add_argument("--namespace", default="project-e2e")
    parser.add_argument("--source-cluster-uid", default=CLUSTER_UID)
    parser.add_argument("--status-timeout", type=float, default=30)
    commands = parser.add_subparsers(dest="command", required=True)
    create = commands.add_parser("create")
    create.add_argument("--zone", required=True)
    create.add_argument("--zone-uid", required=True)
    create.add_argument("--zone-generation", type=int, required=True)
    create.add_argument("--prefix", required=True)
    create.add_argument("--record-name", default="common")
    create.add_argument("--address", required=True)
    create.add_argument("--aaaa-address")
    create.add_argument("--lifetime", type=int, default=20)
    managed = commands.add_parser("create-managed")
    managed.add_argument("--context-uid", required=True)
    managed.add_argument("--registration-class", choices=("InstanceIdentity", "ServiceDiscovery", "ServiceVIP", "ServiceExport"), required=True)
    managed.add_argument("--allocated-name", required=True)
    managed.add_argument("--prefix", required=True)
    managed.add_argument("--address", required=True)
    managed.add_argument("--aaaa-address")
    managed.add_argument("--lifetime", type=int, default=20)
    observe = commands.add_parser("observe")
    observe.add_argument("--name", required=True)
    observe.add_argument("--sequence", type=int, required=True)
    observe.add_argument("--eligible", choices=("true", "false"), required=True)
    observe.add_argument("--lifetime", type=int, default=20)
    update = commands.add_parser("update")
    update.add_argument("--name", required=True)
    update.add_argument("--address", required=True)
    update.add_argument("--aaaa-address")
    update.add_argument("--record-name", default="common")
    update.add_argument("--sequence", type=int, required=True)
    update.add_argument("--lifetime", type=int, default=20)
    delete = commands.add_parser("delete")
    delete.add_argument("--name", required=True)
    args = parser.parse_args()

    publisher = Publisher(args.kubeconfig, args.namespace, args.source_cluster_uid, args.status_timeout)
    if args.command == "create":
        result = publisher.create(
            {"name": args.zone, "uid": args.zone_uid, "generation": args.zone_generation},
            args.prefix, args.record_name, args.address, args.lifetime, args.aaaa_address,
        )
    elif args.command == "create-managed":
        result = publisher.create_managed(
            args.context_uid, args.registration_class, args.allocated_name,
            args.prefix, args.address, args.lifetime, args.aaaa_address,
        )
    elif args.command == "observe":
        result = publisher.observe(args.name, args.sequence, args.eligible == "true", args.lifetime)
    elif args.command == "update":
        result = publisher.update(
            args.name, args.address, args.sequence, args.lifetime,
            args.record_name, args.aaaa_address,
        )
    else:
        publisher.kubectl("delete", "dnsrecordcontribution", args.name, "--wait=true")
        result = {"deleted": args.name}
    print(json.dumps(result, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
