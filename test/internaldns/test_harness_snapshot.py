"""Regression checks for VM-local E2E evidence extraction."""

import base64
from datetime import datetime, timedelta, timezone
import importlib.util
import io
import os
import sys
from pathlib import Path
import tarfile
import tempfile
import unittest


SPEC = importlib.util.spec_from_file_location("internal_dns_harness", Path(__file__).with_name("run.py"))
if SPEC is None or SPEC.loader is None:
    raise RuntimeError("cannot load the qualification harness")
harness = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(harness)

MULTI_SPEC = importlib.util.spec_from_file_location("internal_dns_multi_harness", Path(__file__).with_name("multi_control_plane.py"))
if MULTI_SPEC is None or MULTI_SPEC.loader is None:
    raise RuntimeError("cannot load the distributed qualification harness")
multi = importlib.util.module_from_spec(MULTI_SPEC)
MULTI_SPEC.loader.exec_module(multi)


def archive_entry(name: str, contents: bytes = b"checkpoint", kind: bytes = tarfile.REGTYPE) -> str:
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode="w:gz") as archive:
        member = tarfile.TarInfo(name)
        member.uid = 12345
        member.gid = 12345
        member.mode = 0
        member.type = kind
        if kind == tarfile.SYMTYPE:
            member.linkname = "/outside"
        else:
            member.size = len(contents)
        archive.addfile(member, io.BytesIO(contents) if kind == tarfile.REGTYPE else None)
    return base64.b64encode(buffer.getvalue()).decode()


class EvidenceSnapshotTests(unittest.TestCase):
    def test_checkpoint_bytes_survive_without_container_ownership(self):
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory)
            payload = b'{"format":3,"authorizationDeadline":"2026-10-09T12:00:00Z"}'
            harness.extract_evidence_snapshot(archive_entry("./node/checkpoint.json", payload), target)
            checkpoint = target / "node/checkpoint.json"
            self.assertEqual(checkpoint.read_bytes(), payload)
            self.assertEqual(checkpoint.stat().st_uid, os.getuid())

    def test_archive_cannot_escape_artifact_directory(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(RuntimeError, "unsafe evidence archive path"):
                harness.extract_evidence_snapshot(archive_entry("../escape"), Path(directory) / "artifact")
            self.assertFalse((Path(directory) / "escape").exists())

    def test_archive_cannot_import_container_symlinks(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(RuntimeError, "unsupported evidence archive entry"):
                harness.extract_evidence_snapshot(archive_entry("checkpoint", kind=tarfile.SYMTYPE), Path(directory))


class TakeoverPreparationTests(unittest.TestCase):
    def test_known_owner_requires_both_fresh_leases(self):
        now = datetime(2026, 10, 9, 12, tzinfo=timezone.utc)
        zone = {"spec": {"holderIdentity": "owner-a", "leaseUntil": (now + timedelta(seconds=9)).isoformat()}}
        shard = {"holder": "owner-a-e2e-shared", "leaseUntil": (now + timedelta(seconds=8)).isoformat()}
        self.assertTrue(multi.live_owner_pair(zone, shard, "owner-a", now, 3))
        shard["holder"] = "owner-b-e2e-shared"
        self.assertFalse(multi.live_owner_pair(zone, shard, "owner-a", now, 3))
        shard["holder"] = "owner-a-e2e-shared"
        shard["leaseUntil"] = (now + timedelta(seconds=2)).isoformat()
        self.assertFalse(multi.live_owner_pair(zone, shard, "owner-a", now, 3))
        shard["leaseUntil"] = (now + timedelta(seconds=8)).isoformat()
        zone["spec"]["leaseUntil"] = (now - timedelta(seconds=1)).isoformat()
        self.assertFalse(multi.live_owner_pair(zone, shard, "owner-a", now, 3))

    def test_restarted_standby_preserves_both_process_logs(self):
        with tempfile.TemporaryDirectory() as directory:
            fixture = harness.Harness.__new__(harness.Harness)
            fixture.logs = Path(directory)
            fixture.processes = {}
            for message in ("original-standby", "restarted-standby"):
                fixture.start("control-b", sys.executable, "-c", f"print({message!r})")
                fixture.processes["control-b"].wait(timeout=3)
                fixture.stop("control-b")
            captured = (fixture.logs / "control-b.log").read_text()
            self.assertIn("original-standby", captured)
            self.assertIn("restarted-standby", captured)
            self.assertEqual(captured.count("harness-process-start"), 2)


if __name__ == "__main__":
    unittest.main()
