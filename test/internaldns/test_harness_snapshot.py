"""Regression checks for VM-local E2E evidence extraction."""

import base64
import importlib.util
import io
import os
from pathlib import Path
import tarfile
import tempfile
import unittest


SPEC = importlib.util.spec_from_file_location("internal_dns_harness", Path(__file__).with_name("run.py"))
if SPEC is None or SPEC.loader is None:
    raise RuntimeError("cannot load the qualification harness")
harness = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(harness)


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


if __name__ == "__main__":
    unittest.main()
