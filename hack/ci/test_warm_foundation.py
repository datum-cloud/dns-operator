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
        "schema": "1", "state": "prepared", "fingerprint": "matching",
        "expires_unix": "200", "cluster_uids": json.dumps({"dns-control": "uid"})}}


class ReceiptTests(unittest.TestCase):
    def test_valid_receipt(self):
        foundation.validate_receipt(prepared(), "matching", 100)

    def test_rejects_consumed_stale_and_incompatible_environments(self):
        for field, value in (("state", "claimed"), ("schema", "2"),
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
    @patch.object(foundation, "run")
    def test_successful_claim_exports_only_named_cluster_configs(self, run, kubectl, *_):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "dev").mkdir()
            (root / "dev/kubeconfig").touch()
            with patch.object(foundation, "ROOT", root):
                foundation.claim()
        self.assertEqual(run.call_count, 3)
        for call, cluster in zip(run.call_args_list, foundation.CLUSTERS):
            self.assertEqual(call.args[:5], ("kind", "export", "kubeconfig", "--name", cluster))
        kubectl.assert_called_once()


if __name__ == "__main__":
    unittest.main()
