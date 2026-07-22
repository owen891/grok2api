import json
import os
import stat
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

from cpa_export import _append_private_text, _await_hotload_result, _stage_hotload_file, export_cpa_xai_for_account, upload_cpa_auth_file


class SpoolExportTest(unittest.TestCase):
    def test_failure_log_append_is_owner_only(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "private" / "cpa_auth_failed.txt"
            _append_private_text(path, "user@example.test----synthetic----1\n")

            self.assertEqual("user@example.test----synthetic----1\n", path.read_text(encoding="utf-8"))
            if os.name != "nt":
                self.assertEqual(0o700, stat.S_IMODE(path.parent.stat().st_mode))
                self.assertEqual(0o600, stat.S_IMODE(path.stat().st_mode))

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
                "syncErrors": [{"accountId": 7, "error": "quota timeout"}],
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
            self.assertEqual(payload["syncErrors"], result["syncErrors"])

    def test_permission_denied_credential_is_not_uploaded_or_hotloaded(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            credential = root / "xai-user@example.test.json"
            credential.write_text('{"refresh_token":"synthetic"}', encoding="utf-8")
            minted = {
                "ok": True,
                "path": str(credential),
                "importable": False,
                "import_block_reason": "cpa_chat_permission_denied",
            }
            with patch("cpa_xai.mint_and_export", return_value=minted), patch("cpa_export.upload_cpa_auth_file") as upload, patch("cpa_export._stage_hotload_file") as stage:
                result = export_cpa_xai_for_account(
                    "user@example.test",
                    "password",
                    config={
                        "cpa_auth_dir": str(root),
                        "cpa_copy_to_hotload": True,
                        "cpa_hotload_dir": str(root / "incoming"),
                    },
                    log_callback=lambda _message: None,
                )

            self.assertTrue(result["ok"])
            self.assertEqual("cpa_chat_permission_denied", result["import_skipped"])
            upload.assert_not_called()
            stage.assert_not_called()


if __name__ == "__main__":
    unittest.main()
