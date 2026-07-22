import json
import tempfile
import time
import unittest
from pathlib import Path

from protocol_spool import await_hotload_result, stage_hotload_file


class ProtocolSpoolTests(unittest.TestCase):
    def test_stage_hotload_file_is_atomic(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "credential.json"
            incoming = root / "spool" / "incoming"
            source.write_text('{"ok":true}\n', encoding="utf-8")

            destination = stage_hotload_file(source, incoming)

            self.assertEqual(source.read_bytes(), destination.read_bytes())
            self.assertEqual([], list(incoming.glob("*.tmp")))

    def test_await_hotload_result_reads_processed_result(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "spool"
            incoming = root / "incoming"
            processed = root / "processed"
            incoming.mkdir(parents=True)
            processed.mkdir(parents=True)
            submitted_at = time.time()
            (processed / "credential.result.json").write_text(
                json.dumps({"status": "processed", "created": 1, "synced": 1}),
                encoding="utf-8",
            )

            result = await_hotload_result(
                incoming,
                "credential",
                submitted_at=submitted_at,
                timeout=0.2,
                poll_interval=0.05,
            )

            self.assertTrue(result["ok"])
            self.assertEqual(1, result["created"])

    def test_await_hotload_result_preserves_sync_errors(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "spool"
            incoming = root / "incoming"
            failed = root / "failed"
            incoming.mkdir(parents=True)
            failed.mkdir(parents=True)
            submitted_at = time.time()
            errors = [{"accountId": 42, "error": "browser worker queue wait timed out"}]
            (failed / "credential.result.json").write_text(
                json.dumps({"status": "sync_failed", "syncFailed": 1, "syncErrors": errors}),
                encoding="utf-8",
            )

            result = await_hotload_result(
                incoming,
                "credential",
                submitted_at=submitted_at,
                timeout=0.2,
                poll_interval=0.05,
            )

            self.assertFalse(result["ok"])
            self.assertEqual(errors, result["syncErrors"])


if __name__ == "__main__":
    unittest.main()
