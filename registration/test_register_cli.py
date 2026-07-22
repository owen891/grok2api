import json
import inspect
import os
import queue
import stat
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

import register_cli as worker
from register_cli import _run_web_import_job, registration_exit_code, resolve_mint_workers


class PrivateAccountLedgerTests(unittest.TestCase):
    def test_private_account_ledger_is_created_with_private_permissions(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "accounts.txt"
            worker._append_private_account(str(path), "user@example.invalid----secret----session\n")

            self.assertEqual("user@example.invalid----secret----session\n", path.read_text(encoding="utf-8"))
            if os.name != "nt":
                self.assertEqual(0o600, stat.S_IMODE(path.stat().st_mode))


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

    def test_browser_cli_exposes_controller_runtime_arguments(self):
        result = subprocess.run(
            [sys.executable, "register_cli.py", "--help"],
            cwd=Path(__file__).resolve().parent,
            check=False,
            capture_output=True,
            text=True,
            timeout=30,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        for argument in ("--config", "--state-dir", "--log-file", "--proxy", "--preflight", "--inline-mint", "--auto-nsfw", "--resume-only"):
            self.assertIn(argument, result.stdout)

    def test_browser_factory_accepts_cpa_proxy_override(self):
        self.assertIn("proxy_override", inspect.signature(worker._patched_create_browser_options).parameters)

    def test_tempmail_lol_accepts_string_recipient(self):
        address = "user@example.test"
        messages = [{
            "date": 1,
            "to": address,
            "subject": "ABC-123 xAI verification code",
            "body": "Use ABC-123 to verify your account.",
        }]

        with patch.object(worker.reg, "tempmail_lol_get_messages", return_value=messages):
            code = worker.reg.tempmail_lol_get_oai_code("token", address, timeout=1, poll_interval=0)

        self.assertEqual("ABC-123", code)

    def test_verification_code_prefers_subject_over_html_css_token(self):
        code = worker.reg.extract_verification_code(
            ".helper-100 { padding: 0 } verification code: DQK-VOQ",
            "SpaceXAI confirmation code: DQK-VOQ",
        )

        self.assertEqual("DQK-VOQ", code)

    def test_duckmail_prefers_less_obvious_verified_domain(self):
        domains = [
            {"domain": "duckmail.sbs", "isVerified": True},
            {"domain": "baldur.edu.kg", "isVerified": True},
        ]

        with (
            patch.object(worker.reg, "get_domains", return_value=domains),
            patch.object(worker.reg.secrets, "choice", side_effect=lambda values: values[0]),
        ):
            domain = worker.reg.pick_domain()

        self.assertEqual("baldur.edu.kg", domain)

    def test_browser_preflight_checks_static_runtime_without_starting_chromium(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = root / "config.json"
            config.write_text("{}", encoding="utf-8")
            with (
                patch("browser_runtime.browser_path", return_value=sys.executable),
                patch.dict(os.environ, {"REGISTRATION_BROWSER_MODE": "headless"}),
                patch.object(worker, "browser_registration_page_ready", return_value=(True, "synthetic sign-up page")),
            ):
                result = worker.browser_preflight(
                    str(config),
                    str(root / "state"),
                    str(root / "logs" / "registration.log"),
                )
            self.assertEqual(0, result)

    def test_browser_registration_page_preflight_rejects_cloudflare_denial(self):
        class Page:
            url = "https://accounts.x.ai/sign-up"
            title = "Attention Required! | Cloudflare"
            html = "<title>Attention Required! | Cloudflare</title>"

            def get(self, _url, timeout=None):
                return True

        with (
            patch.object(worker.reg, "start_browser", return_value=(object(), Page())),
            patch.object(worker.reg, "stop_browser"),
            patch.object(worker.time, "sleep"),
        ):
            ok, detail = worker.browser_registration_page_ready({"signup_url": "https://accounts.x.ai/sign-up"})
        self.assertFalse(ok)
        self.assertIn("Cloudflare challenge", detail)

    def test_browser_registration_page_preflight_rejects_redirect_away_from_accounts(self):
        class Page:
            url = "https://example.invalid/block"
            title = "blocked"
            html = ""

            def get(self, _url, timeout=None):
                return True

        with (
            patch.object(worker.reg, "start_browser", return_value=(object(), Page())),
            patch.object(worker.reg, "stop_browser"),
            patch.object(worker.time, "sleep"),
        ):
            ok, detail = worker.browser_registration_page_ready({"signup_url": "https://accounts.x.ai/sign-up"})
        self.assertFalse(ok)
        self.assertIn("unexpected final URL", detail)

    def test_browser_registration_page_preflight_rejects_page_load_timeout(self):
        class Page:
            url = ""
            title = ""
            html = ""

            def get(self, _url, timeout=None):
                self.timeout = timeout
                return False

        page = Page()
        with (
            patch.object(worker.reg, "start_browser", return_value=(object(), page)),
            patch.object(worker.reg, "stop_browser"),
        ):
            ok, detail = worker.browser_registration_page_ready({"signup_url": "https://accounts.x.ai/sign-up"})
        self.assertFalse(ok)
        self.assertEqual(15, page.timeout)
        self.assertIn("timed out after 15s", detail)

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

    def test_browser_metrics_summarize_pipeline_without_credentials(self):
        with worker._metrics_lock:
            original_records = list(worker._metrics_records)
            original_started = worker._metrics_batch_started_monotonic
            original_memory = worker._metrics_peak_memory_mib
            original_browsers = worker._metrics_peak_browser_processes
            try:
                worker._metrics_records[:] = [
                    {
                        "id": "1-1", "index": 1, "attempt": 1, "worker": 1, "status": "succeeded",
                        "registrationSeconds": 10.0, "oauthSeconds": 20.0, "pipelineSeconds": 30.0,
                        "turnstileReuseCount": 1, "oauthOK": True, "importOK": True, "syncFailed": 0,
                        "_startedMonotonic": 1.0,
                    },
                    {
                        "id": "2-1", "index": 2, "attempt": 1, "worker": 2, "status": "succeeded",
                        "registrationSeconds": 20.0, "oauthSeconds": 30.0, "pipelineSeconds": 40.0,
                        "turnstileReuseCount": 0, "oauthOK": True, "importOK": True, "syncFailed": 0,
                        "_startedMonotonic": 2.0,
                    },
                    {
                        "id": "3-1", "index": 3, "attempt": 1, "worker": 3, "status": "failed",
                        "turnstileReuseCount": 2, "oauthOK": False, "importOK": False, "syncFailed": 1,
                        "_startedMonotonic": 3.0,
                    },
                ]
                worker._metrics_batch_started_monotonic = time.monotonic() - 50
                worker._metrics_peak_memory_mib = 512.25
                worker._metrics_peak_browser_processes = 6
                payload = worker._metrics_payload_locked()
            finally:
                worker._metrics_records[:] = original_records
                worker._metrics_batch_started_monotonic = original_started
                worker._metrics_peak_memory_mib = original_memory
                worker._metrics_peak_browser_processes = original_browsers

        summary = payload["summary"]
        self.assertEqual(2, summary["succeeded"])
        self.assertEqual(0.6667, summary["successRate"])
        self.assertEqual(3, summary["turnstileReuseCount"])
        self.assertEqual(20.0, summary["registrationSeconds"]["p95"])
        self.assertEqual(1, summary["oauthFailed"])
        self.assertEqual(1, summary["syncFailed"])
        self.assertEqual(6, summary["peakBrowserProcesses"])
        self.assertNotIn("_startedMonotonic", payload["attempts"][0])

    def _pending_fixture(self, directory):
        original = {
            "pending_path": worker._pending_state_path,
            "pending_jobs": dict(worker._pending_jobs),
            "stats": dict(worker._stats),
        }
        worker._pending_state_path = str(Path(directory) / "browser_pending_oauth.json")
        with worker._pending_lock:
            worker._pending_jobs.clear()
        with worker._stats_lock:
            for key in worker._stats:
                worker._stats[key] = 0
        return original

    def _restore_pending_fixture(self, original):
        with worker._pending_lock:
            worker._pending_jobs.clear()
            worker._pending_jobs.update(original["pending_jobs"])
        worker._pending_state_path = original["pending_path"]
        with worker._stats_lock:
            worker._stats.clear()
            worker._stats.update(original["stats"])

    def test_inline_mint_retries_same_account_and_counts_one_success(self):
        with tempfile.TemporaryDirectory() as directory:
            original = self._pending_fixture(directory)
            try:
                job = {"email": "retry@example.invalid", "password": "pw-secret", "sso": "sso-secret", "idx": 35}
                with (
                    patch.object(worker, "_run_mint_job", side_effect=[
                        {"ok": False, "error": "token ok but grok-4.5 not listed"},
                        {"ok": True, "path": "auth.json"},
                    ]) as mint,
                    patch.object(worker, "log"),
                    patch.object(worker.time, "sleep"),
                ):
                    result = worker._run_mint_with_retry("R1", job, {"cpa_mint_retry_attempts": 2, "cpa_mint_retry_delay_sec": 0})
                self.assertTrue(result["ok"])
                self.assertEqual(2, mint.call_count)
                self.assertEqual(1, worker._stats["mint_success"])
                self.assertEqual(0, worker._pending_count())
                self.assertFalse(Path(worker._pending_state_path).exists())
            finally:
                self._restore_pending_fixture(original)

    def test_exhausted_mint_retries_leave_a_private_resumable_job(self):
        with tempfile.TemporaryDirectory() as directory:
            original = self._pending_fixture(directory)
            try:
                job = {"email": "pending@example.invalid", "password": "pw-secret", "sso": "sso-secret", "idx": 35}
                with patch.object(worker, "_run_mint_job", return_value={"ok": False, "error": "temporary"}), patch.object(worker, "log"), patch.object(worker.time, "sleep"):
                    result = worker._run_mint_with_retry("R1", job, {"cpa_mint_retry_attempts": 2, "cpa_mint_retry_delay_sec": 0})
                self.assertFalse(result["ok"])
                self.assertEqual(1, worker._stats["mint_fail"])
                self.assertEqual(1, worker._pending_count())
                payload = json.loads(Path(worker._pending_state_path).read_text(encoding="utf-8"))
                self.assertEqual("pending@example.invalid", payload["jobs"][0]["email"])
                self.assertEqual("pw-secret", payload["jobs"][0]["password"])
                self.assertEqual(2, payload["jobs"][0]["mintAttempts"])
                if os.name != "nt":
                    self.assertEqual(0o600, Path(worker._pending_state_path).stat().st_mode & 0o777)
            finally:
                self._restore_pending_fixture(original)

    def test_permission_denied_mint_is_terminal_and_not_counted_as_success(self):
        with tempfile.TemporaryDirectory() as directory:
            original = self._pending_fixture(directory)
            try:
                job = {"email": "denied@example.invalid", "password": "pw-secret", "idx": 35}
                with (
                    patch.object(
                        worker,
                        "_run_mint_job",
                        return_value={
                            "ok": True,
                            "importable": False,
                            "import_block_reason": "cpa_chat_permission_denied",
                            "path": "auth.json",
                        },
                    ) as mint,
                    patch.object(worker, "log"),
                ):
                    result = worker._run_mint_with_retry(
                        "R1",
                        job,
                        {"cpa_mint_retry_attempts": 3, "cpa_mint_retry_delay_sec": 0},
                    )

                self.assertFalse(result["ok"])
                self.assertTrue(result["credential_ok"])
                self.assertEqual("cpa_chat_permission_denied", result["error"])
                self.assertEqual(1, mint.call_count)
                self.assertEqual(0, worker._stats["mint_success"])
                self.assertEqual(1, worker._stats["mint_fail"])
                self.assertEqual(1, worker._stats["mint_terminal_fail"])
                self.assertEqual(0, worker._pending_count())
            finally:
                self._restore_pending_fixture(original)

    def test_next_run_resumes_pending_job_without_inflating_batch_mint_success(self):
        with tempfile.TemporaryDirectory() as directory:
            original = self._pending_fixture(directory)
            try:
                job = {"email": "resume@example.invalid", "password": "pw-secret", "sso": "sso-secret", "idx": 35}
                worker._persist_pending_job(job)
                with patch.object(worker, "_run_mint_job", return_value={"ok": True, "path": "auth.json"}), patch.object(worker, "log"):
                    succeeded, failed = worker._resume_pending_mint_jobs({"cpa_mint_retry_attempts": 1})
                self.assertEqual((1, 0), (succeeded, failed))
                self.assertEqual(0, worker._stats["mint_success"])
                self.assertEqual(0, worker._pending_count())
                self.assertEqual(0, registration_exit_code({"reg_success": 0, "mint_success": 0}, cpa_export_enabled=True, expected_successes=0, resumable=0))
            finally:
                self._restore_pending_fixture(original)

    def test_web_import_retry_preserves_the_same_registered_account(self):
        with tempfile.TemporaryDirectory() as directory:
            original = self._pending_fixture(directory)
            job = {
                "email": "web-pending@example.invalid",
                "password": "pw-secret",
                "sso": "sso-secret",
                "accountType": "web",
                "autoNSFW": True,
            }
            try:
                with (
                    patch.object(worker, "_run_web_import_job", return_value={"ok": False, "error": "temporary"}),
                    patch.object(worker, "log"),
                ):
                    result = worker._run_web_import_with_retry("R1", job, {"cpa_mint_retry_attempts": 1})
                self.assertFalse(result["ok"])
                self.assertEqual(1, worker._pending_count("web"))
                pending = worker._pending_job_snapshot()[0]
                self.assertEqual("web", pending["accountType"])
                self.assertTrue(pending["autoNSFW"])

                with patch.object(worker, "_run_web_import_job", return_value={"ok": True}), patch.object(worker, "log"):
                    self.assertEqual((1, 0), worker._resume_pending_jobs({}, "web"))
                self.assertEqual(0, worker._pending_count("web"))
            finally:
                self._restore_pending_fixture(original)

    def test_resume_only_run_fails_while_pending_jobs_remain(self):
        self.assertEqual(
            2,
            registration_exit_code({"reg_success": 0, "mint_success": 0}, cpa_export_enabled=True, expected_successes=0, resumable=1),
        )

    def test_unbounded_run_with_no_registered_accounts_still_fails(self):
        self.assertEqual(
            1,
            registration_exit_code({"reg_success": 0, "mint_success": 0}, cpa_export_enabled=True),
        )

    def test_progress_reports_resumable_count_without_credentials(self):
        with tempfile.TemporaryDirectory() as directory:
            original = self._pending_fixture(directory)
            original_progress = (worker._progress_state_path, worker._progress_target, worker._progress_cpa_required)
            try:
                worker._progress_state_path = str(Path(directory) / "browser_state.json")
                worker._progress_target = 1
                worker._progress_cpa_required = True
                worker._persist_pending_job({"email": "metrics@example.invalid", "password": "pw-secret", "sso": "sso-secret"})
                with worker._stats_lock:
                    worker._stats["reg_success"] = 1
                    worker._stats["mint_success"] = 0
                    worker._write_progress_state_locked()
                state = json.loads(Path(worker._progress_state_path).read_text(encoding="utf-8"))
                self.assertEqual(1, state["resumable"])
                self.assertNotIn("pw-secret", json.dumps(state))
                self.assertNotIn("sso-secret", json.dumps(state))
            finally:
                worker._progress_state_path, worker._progress_target, worker._progress_cpa_required = original_progress
                self._restore_pending_fixture(original)

    def test_register_worker_recovers_after_browser_disconnect_and_cleans_up(self):
        tasks = queue.Queue()
        tasks.put(7)
        recovered = {"email": "recovered@example.invalid"}
        worker._stop_event.clear()
        with (
            patch.object(worker, "register_one", side_effect=[None, recovered]) as register,
            patch.object(worker.reg, "restart_browser") as restart,
            patch.object(worker.reg, "stop_browser") as stop,
            patch.object(worker, "log"),
        ):
            worker._register_worker(1, tasks, 1, "accounts.txt", None, False, True, "build")

        self.assertEqual(2, register.call_count)
        restart.assert_called_once()
        stop.assert_called_once()

    def test_register_worker_forwards_web_auto_nsfw(self):
        tasks = queue.Queue()
        tasks.put(7)
        worker._stop_event.clear()
        with (
            patch.object(worker, "register_one", return_value={"email": "web@example.invalid"}) as register,
            patch.object(worker.reg, "stop_browser"),
            patch.object(worker, "log"),
        ):
            worker._register_worker(1, tasks, 1, "accounts.txt", None, False, True, "web", True)

        self.assertEqual("web", register.call_args.kwargs["account_type"])
        self.assertTrue(register.call_args.kwargs["auto_nsfw"])

    def test_exhausted_registration_slot_is_replaced_until_batch_target_is_reached(self):
        tasks = queue.Queue()
        tasks.put(7)
        recovered = {"email": "replacement@example.invalid"}
        original_stats = dict(worker._stats)
        original_next = worker._next_idx[0]
        original_limit = worker._replacement_max_index[0]
        worker._stop_event.clear()
        try:
            with worker._stats_lock:
                for key in worker._stats:
                    worker._stats[key] = 0
            worker._next_idx[0] = 8
            worker._replacement_max_index[0] = 8
            with (
                patch.object(worker, "register_one", side_effect=[None, None, recovered]) as register,
                patch.object(worker.reg, "restart_browser"),
                patch.object(worker.reg, "stop_browser"),
                patch.object(worker, "log"),
            ):
                worker._register_worker(1, tasks, 1, "accounts.txt", None, False, True, "build")

            self.assertEqual([7, 7, 8], [call.args[1] for call in register.call_args_list])
            self.assertEqual(1, worker._stats["reg_fail"])
            self.assertEqual(0, tasks.qsize())
        finally:
            worker._next_idx[0] = original_next
            worker._replacement_max_index[0] = original_limit
            worker._stop_event.clear()
            with worker._stats_lock:
                worker._stats.clear()
                worker._stats.update(original_stats)

    def test_terminal_cpa_failures_queue_replacements_until_usable_target(self):
        tasks = queue.Queue()
        original_stats = dict(worker._stats)
        original_next = worker._next_idx[0]
        original_limit = worker._replacement_max_index[0]
        try:
            with worker._stats_lock:
                worker._stats.update(mint_success=62, mint_fail=38, mint_terminal_fail=38)
            worker._next_idx[0] = 101
            worker._replacement_max_index[0] = 300

            queued, usable, deficit = worker._enqueue_terminal_cpa_replacements(
                tasks,
                target=100,
                already_queued=0,
            )

            self.assertEqual((38, 62, 38), (queued, usable, deficit))
            self.assertEqual(list(range(101, 139)), [tasks.get_nowait() for _ in range(queued)])
            self.assertEqual(139, worker._next_idx[0])

            queued_again, _, _ = worker._enqueue_terminal_cpa_replacements(
                tasks,
                target=100,
                already_queued=38,
            )
            self.assertEqual(0, queued_again)

            with worker._stats_lock:
                worker._stats.update(mint_success=62, mint_fail=38, mint_terminal_fail=0)
            recoverable_queued, _, _ = worker._enqueue_terminal_cpa_replacements(
                tasks,
                target=100,
                already_queued=0,
            )
            self.assertEqual(0, recoverable_queued)
        finally:
            worker._next_idx[0] = original_next
            worker._replacement_max_index[0] = original_limit
            with worker._stats_lock:
                worker._stats.clear()
                worker._stats.update(original_stats)

    def test_stop_request_cancels_queued_registration_and_cleans_up(self):
        tasks = queue.Queue()
        tasks.put(9)
        try:
            worker._request_stop()
            with (
                patch.object(worker, "register_one") as register,
                patch.object(worker.reg, "stop_browser") as stop,
                patch.object(worker, "log"),
            ):
                worker._register_worker(1, tasks, 1, "accounts.txt", None, False, True, "build")

            register.assert_not_called()
            stop.assert_called_once()
            self.assertEqual(1, tasks.qsize())
        finally:
            worker._stop_event.clear()

    def test_stop_during_registration_does_not_restart_browser_or_count_failure(self):
        tasks = queue.Queue()
        tasks.put(11)
        original_stats = dict(worker._stats)

        def cancel_registration(*_args, **_kwargs):
            worker._request_stop()
            return None

        try:
            with worker._stats_lock:
                for key in worker._stats:
                    worker._stats[key] = 0
            with (
                patch.object(worker, "register_one", side_effect=cancel_registration) as register,
                patch.object(worker.reg, "restart_browser") as restart,
                patch.object(worker.reg, "stop_browser") as stop,
                patch.object(worker, "log"),
            ):
                worker._register_worker(1, tasks, 1, "accounts.txt", None, False, True, "build")

            register.assert_called_once()
            restart.assert_not_called()
            stop.assert_called_once()
            self.assertEqual(0, worker._stats["reg_fail"])
        finally:
            worker._stop_event.clear()
            with worker._stats_lock:
                worker._stats.clear()
                worker._stats.update(original_stats)

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
                    {"email": "web@example.invalid", "sso": "synthetic-sso", "autoNSFW": True},
                    {},
                )

            self.assertTrue(result["ok"])
            write_web.assert_called_once()
            self.assertTrue(write_web.call_args.kwargs["auto_nsfw"])
            publish.assert_called_once()


if __name__ == "__main__":
    unittest.main()
