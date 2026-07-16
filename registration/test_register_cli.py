import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import register_cli as worker
from register_cli import _run_web_import_job, registration_exit_code, resolve_mint_workers


class ResolveMintWorkersTests(unittest.TestCase):
    def test_auto_matches_registration_threads(self):
        config = {"cpa_export_enabled": True, "cpa_mint_workers": -1}

        for threads in (1, 5, 10):
            with self.subTest(threads=threads):
                self.assertEqual(
                    resolve_mint_workers(
                        cli_value=-1,
                        threads=threads,
                        config=config,
                        inline_mint=False,
                    ),
                    threads,
                )

    def test_explicit_worker_count_still_overrides_auto(self):
        self.assertEqual(
            resolve_mint_workers(
                cli_value=2,
                threads=5,
                config={"cpa_export_enabled": True, "cpa_mint_workers": -1},
                inline_mint=False,
            ),
            2,
        )

    def test_protocol_worker_dispatches_before_browser_cli_parsing(self):
        result = subprocess.run(
            [sys.executable, "register_cli.py", "--protocol-worker", "--help"],
            cwd=Path(__file__).resolve().parent,
            check=False,
            capture_output=True,
            text=True,
            timeout=30,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Grok2API protocol registration worker", result.stdout)

    def test_exit_code_requires_the_requested_number_of_usable_accounts(self):
        partial = {"reg_success": 1, "mint_success": 1}
        self.assertEqual(
            1,
            registration_exit_code(partial, cpa_export_enabled=True, expected_successes=2),
        )

        missing_mint = {"reg_success": 2, "mint_success": 1}
        self.assertEqual(
            2,
            registration_exit_code(missing_mint, cpa_export_enabled=True, expected_successes=2),
        )

        complete = {"reg_success": 2, "mint_success": 2}
        self.assertEqual(
            0,
            registration_exit_code(complete, cpa_export_enabled=True, expected_successes=2),
        )

    def test_browser_progress_counts_only_minted_accounts_as_usable(self):
        with tempfile.TemporaryDirectory() as directory:
            path = str(Path(directory) / "browser_state.json")
            original = dict(worker._stats)
            try:
                with (
                    patch.object(worker, "_progress_state_path", path),
                    patch.object(worker, "_progress_target", 4),
                    patch.object(worker, "_progress_cpa_required", True),
                ):
                    with worker._stats_lock:
                        worker._stats.update(
                            reg_success=3,
                            reg_fail=1,
                            mint_success=2,
                            mint_fail=1,
                            mint_skip=0,
                        )
                        worker._write_progress_state_locked()
                state = json.loads(Path(path).read_text(encoding="utf-8"))
            finally:
                with worker._stats_lock:
                    worker._stats.clear()
                    worker._stats.update(original)

            self.assertEqual(2, state["done"])
            self.assertEqual(4, state["attempted"])
            self.assertEqual(2, state["failed"])

    def test_web_import_job_publishes_sso_without_mint(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "web.json"
            source.write_text("{}", encoding="utf-8")
            with (
                patch("protocol_register_cli.write_web_credential", return_value=source) as write_web,
                patch("protocol_register_cli.publish_protocol_credential", return_value={"ok": True}) as publish,
                patch.dict(os.environ, {"REGISTRATION_DATA_DIR": str(root), "REGISTRATION_CPA_HOTLOAD_DIR": str(root / "spool")}),
            ):
                result = _run_web_import_job(
                    "test",
                    {"email": "web@example.invalid", "sso": "synthetic-sso"},
                    {},
                )

            self.assertTrue(result["ok"])
            write_web.assert_called_once()
            publish.assert_called_once()


if __name__ == "__main__":
    unittest.main()
