import json
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

from cpa_export import _await_hotload_result, _stage_hotload_file, upload_cpa_auth_file


class SpoolExportTest(unittest.TestCase):
    def test_environment_disables_legacy_remote_import(self):
        with patch.dict("os.environ", {"REGISTRATION_DISABLE_REMOTE_IMPORT": "1"}):
            result = upload_cpa_auth_file("missing.json", config={"cpa_remote_import_enabled": True})

        self.assertTrue(result["skipped"])
        self.assertEqual("disabled_by_environment", result["reason"])

    def test_stage_hotload_file_publishes_complete_json(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "source.json"
            source.write_text('{"refresh_token":"synthetic"}', encoding="utf-8")

            destination = _stage_hotload_file(source, root / "spool" / "incoming")

            self.assertEqual(source.read_bytes(), destination.read_bytes())
            self.assertEqual([], list(destination.parent.glob("*.tmp")))

    def test_await_hotload_result_reports_initial_sync_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "spool"
            incoming = root / "incoming"
            failed = root / "failed"
            incoming.mkdir(parents=True)
            failed.mkdir(parents=True)
            submitted_at = time.time()
            payload = {
                "status": "sync_failed",
                "created": 1,
                "updated": 0,
                "synced": 0,
                "syncFailed": 1,
                "processedAt": "2026-07-14T14:00:00Z",
            }
            (failed / "account.result.json").write_text(json.dumps(payload), encoding="utf-8")

            result = _await_hotload_result(
                incoming,
                "account",
                submitted_at=submitted_at,
                timeout=0.2,
                poll_interval=0.05,
            )

            self.assertFalse(result["ok"])
            self.assertEqual("sync_failed", result["status"])
            self.assertEqual(1, result["syncFailed"])


if __name__ == "__main__":
    unittest.main()
