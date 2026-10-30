"""Foundation handoff tests; no cluster or Docker daemon is contacted."""
import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("foundation", Path(__file__).with_name("warm-foundation.py"))
foundation = importlib.util.module_from_spec(spec)
spec.loader.exec_module(foundation)


def prepared():
    return {"metadata": {"resourceVersion": "17"}, "data": {
        "schema": "2", "state": "prepared", "fingerprint": "matching",
        "expires_unix": "200", "cluster_uids": json.dumps({"dns-control": "uid"}),
        "tools": "{}", "rustfs": json.dumps({"nodeport": 31234})}}


class ReceiptTests(unittest.TestCase):
    def test_valid_receipt(self):
        foundation.validate_receipt(prepared(), "matching", 100)

    def test_rejects_consumed_stale_and_incompatible_environments(self):
        for field, value in (("state", "claimed"), ("schema", "1"),
                             ("fingerprint", "different"), ("expires_unix", "99")):
            with self.subTest(field=field):
                receipt = prepared()
                receipt["data"][field] = value
                with self.assertRaises(ValueError):
                    foundation.validate_receipt(receipt, "matching", 100)

    def test_rejects_nonready_or_ipv4_only_nodes(self):
        node = {"metadata": {"name": "node"}, "spec": {"podCIDRs": ["10.0.0.0/24", "fd00::/64"]},
                "status": {"conditions": [{"type": "Ready", "status": "True"}]}}
        foundation.verify_nodes({"items": [node]})
        for bad in ({"items": []}, {"items": [dict(node, spec={"podCIDRs": ["10.0.0.0/24"]})]}):
            with self.assertRaises(ValueError):
                foundation.verify_nodes(bad)
        node["status"]["conditions"][0]["status"] = "False"
        with self.assertRaises(ValueError):
            foundation.verify_nodes({"items": [node]})

    @patch.object(foundation, "get")
    def test_rejects_dns_residue(self, get):
        for namespaces, crds in ((["dns-agent-system"], []),
                                 ([], [{"metadata": {"name": "dnszones"},
                                        "spec": {"group": "dns.networking.miloapis.com"}}])):
            get.side_effect = [{"items": [{"metadata": {"name": n}} for n in namespaces]},
                               {"items": crds}]
            with self.assertRaises(ValueError):
                foundation.verify_clean("dns-control")

    @patch.object(foundation, "run", return_value="dns-control\nother")
    def test_prepare_never_bootstraps_over_existing_clusters(self, run):
        with self.assertRaises(ValueError):
            foundation.prepare(100)
        run.assert_called_once_with("kind", "get", "clusters")

    @patch.object(foundation, "export_configs")
    @patch.object(foundation, "fingerprint", return_value="matching")
    @patch.object(foundation, "health_and_identity", side_effect=ValueError("unhealthy"))
    @patch.object(foundation, "kubectl")
    @patch.object(foundation, "run")
    def test_failed_preparation_never_advertises_ready_receipt(self, run, kubectl, *_):
        pin = foundation.re.search(r"TEST_INFRA_REPO_REF: '([0-9a-f]{40})'",
                                   (foundation.ROOT / "Taskfile.yaml").read_text()).group(1)
        run.side_effect = ["", None, pin, ""]
        with self.assertRaisesRegex(ValueError, "unhealthy"):
            foundation.prepare(100)
        kubectl.assert_not_called()

    @patch.object(foundation, "export_configs")
    @patch.object(foundation, "fingerprint", return_value="matching")
    @patch.object(foundation, "health_and_identity")
    @patch.object(foundation, "kubectl")
    @patch.object(foundation, "run", side_effect=["", None, "wrong revision"])
    def test_unpinned_checkout_cannot_be_published(self, run, kubectl, health, *_):
        with self.assertRaisesRegex(ValueError, "pinned"):
            foundation.prepare(100)
        kubectl.assert_not_called()
        health.assert_not_called()

    @patch.object(foundation, "export_configs")
    @patch.object(foundation, "fingerprint", return_value="matching")
    @patch.object(foundation.time, "time", return_value=100)
    @patch.object(foundation, "get", side_effect=lambda *args: copy.deepcopy(prepared()))
    @patch.object(foundation, "health_and_identity")
    @patch.object(foundation, "kubectl")
    def test_conflicting_claim_never_reaches_validation(self, kubectl, health, *_):
        kubectl.side_effect = subprocess.CalledProcessError(1, ["kubectl", "replace"])
        with self.assertRaises(subprocess.CalledProcessError):
            foundation.claim()
        health.assert_not_called()
        payload = json.loads(kubectl.call_args.kwargs["input"])
        self.assertEqual(payload["metadata"]["resourceVersion"], "17")
        self.assertEqual(payload["data"]["state"], "claimed")

    @patch.object(foundation, "export_configs")
    @patch.object(foundation, "fingerprint", return_value="matching")
    @patch.object(foundation.time, "time", return_value=100)
    @patch.object(foundation, "get", side_effect=lambda *args: copy.deepcopy(prepared()))
    @patch.object(foundation, "health_and_identity", return_value={"dns-control": "replacement"})
    @patch.object(foundation, "kubectl")
    @patch.object(foundation, "run")
    def test_replaced_cluster_burns_claim_and_does_not_export_deployment_config(self, run, kubectl, *_):
        with self.assertRaisesRegex(ValueError, "replaced"):
            foundation.claim()
        kubectl.assert_called_once()
        run.assert_not_called()

    @patch.object(foundation, "export_configs")
    @patch.object(foundation, "fingerprint", return_value="matching")
    @patch.object(foundation.time, "time", return_value=100)
    @patch.object(foundation, "get", side_effect=lambda *args: copy.deepcopy(prepared()))
    @patch.object(foundation, "health_and_identity", return_value={"dns-control": "uid"})
    @patch.object(foundation, "kubectl")
    @patch.object(foundation, "tool_inventory", return_value={})
    @patch.object(foundation, "rustfs_inventory", return_value={"nodeport": 31234})
    @patch.object(foundation, "run")
    def test_successful_claim_exports_only_named_cluster_configs(self, run, rustfs, tools, kubectl, *_):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "dev").mkdir()
            (root / "dev/kubeconfig").touch()
            with patch.object(foundation, "ROOT", root):
                foundation.claim()
                self.assertEqual((root / "dev/rustfs.nodeport").read_text(), "31234\n")
                self.assertEqual(json.loads((root / foundation.CLAIM_STAMP).read_text())["schema"], 2)
        self.assertEqual(run.call_count, 3)
        for call, cluster in zip(run.call_args_list, foundation.CLUSTERS):
            self.assertEqual(call.args[:5], ("kind", "export", "kubeconfig", "--name", cluster))
        kubectl.assert_called_once()


class ExpandedFoundationTests(unittest.TestCase):
    def empty(self):
        return {"buckets": {"Buckets": [{"Name": "lightningstream"}]},
                "objects": {"KeyCount": 0, "IsTruncated": False},
                "versions": {"IsTruncated": False}, "uploads": {"IsTruncated": False}}

    def test_empty_bucket(self):
        foundation.assert_empty_bucket(self.empty())

    def test_rejects_objects_versions_delete_markers_and_uploads(self):
        for section, field in (("objects", "Contents"), ("versions", "Versions"),
                               ("versions", "DeleteMarkers"), ("uploads", "Uploads")):
            with self.subTest(field=field):
                observation = self.empty()
                observation[section][field] = [{"Key": "leftover"}]
                with self.assertRaises(ValueError):
                    foundation.assert_empty_bucket(observation)

    def test_rejects_other_buckets_and_truncated_results(self):
        observation = self.empty()
        observation["buckets"]["Buckets"].append({"Name": "other"})
        with self.assertRaises(ValueError):
            foundation.assert_empty_bucket(observation)
        for section in ("objects", "versions", "uploads"):
            observation = self.empty()
            observation[section]["IsTruncated"] = True
            with self.assertRaises(ValueError):
                foundation.assert_empty_bucket(observation)

    @patch.object(foundation, "get")
    def test_rustfs_only_allowed_on_control(self, get):
        state = [{"items": [{"metadata": {"name": "rustfs-system"}}]}, {"items": []}]
        get.side_effect = copy.deepcopy(state)
        foundation.verify_clean("dns-control")
        for cluster in ("dns-upstream", "dns-edge"):
            get.side_effect = copy.deepcopy(state)
            with self.assertRaises(ValueError):
                foundation.verify_clean(cluster)

    @patch.object(foundation, "run")
    def test_tools_require_actual_pinned_modules_and_bind_bytes(self, run):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "bin").mkdir()
            (root / "Taskfile.yaml").write_text("\n".join(
                f"  {variable}: v1.2.3" for variable, module in foundation.TOOLS.values()))
            for name in foundation.TOOLS:
                binary = root / "bin" / name
                binary.write_bytes(b"binary")
                binary.chmod(0o755)
            run.side_effect = [f"\tmod\t{module}\tv1.2.3\th1:hash"
                               for variable, module in foundation.TOOLS.values()]
            with patch.object(foundation, "ROOT", root):
                inventory = foundation.tool_inventory()
                self.assertEqual(len(inventory), 3)
                self.assertEqual(inventory["chainsaw"]["version"], "v1.2.3")
                run.side_effect = None
                run.return_value = "mod wrong/module v1.2.3"
                with self.assertRaises(ValueError):
                    foundation.tool_inventory()

    @patch.object(foundation, "run")
    def test_bundled_tools_copy_only_matching_pins(self, run):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            bundle = root / "bundle"
            bundle.mkdir()
            (root / "Taskfile.yaml").write_text("\n".join(
                f"  {variable}: v1.2.3" for variable, module in foundation.TOOLS.values()))
            binary = bundle / "kustomize"
            binary.write_bytes(b"bundled")
            binary.chmod(0o755)
            run.return_value = "mod sigs.k8s.io/kustomize/kustomize/v5 v1.2.3"
            with patch.object(foundation, "ROOT", root), patch.object(foundation, "BUNDLED_TOOLS", bundle):
                foundation.install_tools()
            self.assertEqual((root / "bin/kustomize").read_bytes(), b"bundled")
            self.assertTrue((root / "bin/kustomize").is_symlink())
            run.assert_called_with("task", "--yes", "tools:controller-gen", "tools:chainsaw", capture=False)

    @patch.object(foundation, "export_configs")
    @patch.object(foundation, "fingerprint", return_value="matching")
    @patch.object(foundation.time, "time", return_value=100)
    @patch.object(foundation, "get", side_effect=lambda *args: copy.deepcopy(prepared()))
    @patch.object(foundation, "health_and_identity", return_value={"dns-control": "uid"})
    @patch.object(foundation, "kubectl")
    @patch.object(foundation, "tool_inventory", return_value={"chainsaw": "changed"})
    def test_changed_tools_burn_claim_and_remove_stale_stamp(self, *_):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "dev").mkdir()
            (root / foundation.CLAIM_STAMP).write_text("stale")
            with patch.object(foundation, "ROOT", root):
                with self.assertRaisesRegex(ValueError, "tools changed"):
                    foundation.claim()
            self.assertFalse((root / foundation.CLAIM_STAMP).exists())

    @patch.object(foundation, "export_configs")
    @patch.object(foundation, "fingerprint", return_value="matching")
    @patch.object(foundation.time, "time", return_value=100)
    @patch.object(foundation, "get")
    def test_second_claim_rejection_preserves_first_success_stamp(self, get, *_):
        receipt = prepared()
        receipt["data"]["state"] = "claimed"
        get.return_value = receipt
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "dev").mkdir()
            stamp = root / foundation.CLAIM_STAMP
            stamp.write_text("first successful claim")
            with patch.object(foundation, "ROOT", root):
                with self.assertRaises(ValueError):
                    foundation.claim()
            self.assertEqual(stamp.read_text(), "first successful claim")

    @patch.object(foundation, "verify_empty_bucket")
    @patch.object(foundation, "kubectl")
    @patch.object(foundation, "get")
    def test_rustfs_requires_fixture_credentials_and_only_expected_resources(self, get, kubectl, probe):
        release = {"metadata": {"uid": "release"}, "spec": {"values": {"auth": {
            "accessKey": foundation.RUSTFS_ACCESS_KEY, "secretKey": foundation.RUSTFS_SECRET_KEY}}},
            "status": {"history": [{"chartVersion": "0.1.2", "digest": "sha256:chart"}]}}
        service = {"kind": "Service", "metadata": {"name": "rustfs", "uid": "service",
                   "labels": {"app.kubernetes.io/instance": "rustfs"}},
                   "spec": {"ports": [{"port": 9000, "nodePort": 31234}]}}
        get.side_effect = [release, {"items": [service]}, {"items": []}]
        inventory = foundation.rustfs_inventory()
        self.assertEqual(inventory["nodeport"], 31234)
        self.assertEqual(inventory["chart_version"], "0.1.2")
        probe.assert_called_once()
        probe.reset_mock()
        service["metadata"]["labels"] = {}
        get.side_effect = [release, {"items": [service]}]
        with self.assertRaisesRegex(ValueError, "Unexpected resource"):
            foundation.rustfs_inventory()
        probe.assert_not_called()
        release["spec"]["values"]["auth"]["secretKey"] = "unrecognized"
        get.side_effect = [release]
        with self.assertRaisesRegex(ValueError, "credential"):
            foundation.rustfs_inventory()


if __name__ == "__main__":
    unittest.main()
