import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import protocol_register_cli as worker
from protocol_register_cli import (
    append_ledger,
    migrate_legacy_ledger,
    read_checkpoint,
    resumable_checkpoints,
    write_checkpoint,
    write_web_credential,
)


class ProtocolCheckpointTests(unittest.TestCase):
    def test_shared_email_provider_wins_over_legacy_protocol_backend(self):
        self.assertEqual(
            ["tempmail"],
            worker.configured_email_backends(
                {
                    "email_provider": "tempmail_lol",
                    "protocol_email_backend": "yyds",
                    "email_provider_fallbacks": [],
                    "yyds_api_key": "stale-key",
                }
            ),
        )

    def test_explicit_email_fallbacks_are_preserved(self):
        self.assertEqual(
            ["tempmail", "cloudmail"],
            worker.configured_email_backends(
                {
                    "email_provider": "tempmail_lol",
                    "protocol_email_backend": "yyds",
                    "email_provider_fallbacks": ["cloudmail"],
                }
            ),
        )

    def test_legacy_protocol_script_dispatches_browser_worker_flag(self):
        result = subprocess.run(
            [sys.executable, "protocol_register_cli.py", "--browser-worker", "--help"],
            cwd=Path(__file__).resolve().parent,
            check=False,
            capture_output=True,
            text=True,
            timeout=30,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("--inline-mint", result.stdout)

    @patch.object(worker, "check_turnstile_endpoint")
    def test_local_preflight_accepts_first_healthy_endpoint(self, check_endpoint):
        check_endpoint.side_effect = [
            (False, "connection refused"),
            (True, '{"ok":true,"pool_ready":true}'),
        ]

        errors = worker.preflight(
            {
                "captcha_solver": "local",
                "captcha_endpoints": [
                    "http://solver-a:5072",
                    "http://solver-b:5072",
                    "http://solver-c:5072",
                ],
                "captcha_preflight_timeout": 1.5,
            }
        )

        self.assertFalse(any("本地过盾" in error for error in errors), errors)
        self.assertEqual(
            [call.args[0] for call in check_endpoint.call_args_list],
            ["http://solver-a:5072", "http://solver-b:5072"],
        )
        self.assertTrue(
            all(call.kwargs["timeout"] == 1.5 for call in check_endpoint.call_args_list)
        )

    @patch.object(worker, "check_turnstile_endpoint")
    def test_local_preflight_rejects_when_all_endpoints_are_unhealthy(self, check_endpoint):
        check_endpoint.side_effect = [
            (False, "connection refused"),
            (False, "health HTTP 503"),
        ]

        errors = worker.preflight(
            {
                "captcha_solver": "local",
                "captcha_endpoints": "http://solver-a:5072,http://solver-b:5072",
            }
        )

        captcha_errors = [error for error in errors if "本地过盾服务不可用" in error]
        self.assertEqual(len(captcha_errors), 1)
        self.assertIn("solver-a", captcha_errors[0])
        self.assertIn("solver-b", captcha_errors[0])

    @patch.object(worker, "check_turnstile_endpoint")
    def test_invalid_local_endpoint_does_not_start_health_probe(self, check_endpoint):
        errors = worker.preflight(
            {
                "captcha_solver": "local",
                "captcha_endpoints": ["solver-without-scheme:5072"],
            }
        )

        self.assertTrue(any("本地过盾地址格式无效" in error for error in errors), errors)
        check_endpoint.assert_not_called()

    def test_solve_turnstile_reuses_provider_and_forwards_live_sitekey(self):
        class Provider:
            name = "docker"

            def solve(self, **kwargs):
                self.kwargs = kwargs
                return SimpleNamespace(token="fresh-token")

        provider = Provider()
        token = worker.solve_turnstile_token(
            {},
            proxy="http://proxy:8080",
            website_key="live-key",
            clearance_provider=provider,
        )

        self.assertEqual(token, "fresh-token")
        self.assertEqual(provider.kwargs["website_key"], "live-key")
        self.assertEqual(provider.kwargs["proxy"], "http://proxy:8080")

    def test_proxy_pool_deduplicates_and_appends_fallback(self):
        self.assertEqual(
            worker.proxy_pool({"proxy_pool": ["socks5://a:1", "socks5://a:1", "http://b:2"], "proxy": "http://fallback:3"}),
            ["socks5://a:1", "http://b:2", "http://fallback:3"],
        )

    def test_proxy_pool_uses_direct_when_empty(self):
        self.assertEqual(worker.proxy_pool({}), [])

    def test_checkpoint_contains_attempt_identity_and_email_fingerprint(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "job.json"
            value = write_checkpoint(path, stage="pending_email", email="User@Example.invalid", proxy_group_id="7")
            self.assertEqual(value["state_stage"], "pending_email")
            self.assertEqual(value["proxy_group_id"], "7")
            self.assertEqual(len(value["attempt_id"]), 32)
            self.assertEqual(value["email_hash"], worker.hashlib.sha256(b"user@example.invalid").hexdigest())

    def test_resolve_sso_falls_back_to_password_session_with_fresh_token(self):
        class FakeClient:
            turnstile_sitekey = "live-signin-key"

            def fetch_sso_token(self, **_kwargs):
                return ""

            def obtain_session_via_password(self, **kwargs):
                self.kwargs = kwargs
                return "synthetic-sso"

        client = FakeClient()
        with patch.object(worker, "solve_turnstile_token", return_value="fresh-turnstile") as solve:
            result = worker.resolve_sso(
                client,
                {},
                email="user@example.invalid",
                password="secret",
                proxy="http://127.0.0.1:7897",
                log_file=Path("registration.log"),
                worker_id=1,
                inspect_create_response=True,
            )

        self.assertEqual("synthetic-sso", result)
        self.assertEqual("fresh-turnstile", client.kwargs["turnstile_token"])
        self.assertEqual(worker.SIGNIN_URL, client.kwargs["referer"])
        self.assertEqual(worker.SIGNIN_URL, solve.call_args.kwargs["website_url"])
        self.assertEqual("live-signin-key", solve.call_args.kwargs["website_key"])

    def test_only_recoverable_jobs_are_resumed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            resumable = root / "jobs" / "resume.json"
            completed = root / "jobs" / "completed.json"
            write_checkpoint(
                resumable,
                index=1,
                stage="sso_ready",
                email="resume@example.invalid",
                password="secret",
                sso="synthetic",
            )
            write_checkpoint(
                completed,
                index=2,
                stage="completed",
                email="done@example.invalid",
                password="secret",
            )

            self.assertEqual([resumable], resumable_checkpoints(root))
            self.assertEqual("sso_ready", read_checkpoint(resumable)["stage"])

    def test_exhausted_recoverable_job_is_not_resumed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            exhausted = root / "jobs" / "exhausted.json"
            write_checkpoint(
                exhausted,
                stage="account_created",
                attempts=2,
                email="exhausted@example.invalid",
                password="secret",
            )

            self.assertEqual([], resumable_checkpoints(root, max_attempts=2))

    def test_submission_unknown_is_not_resumed_automatically(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "jobs" / "unknown.json"
            write_checkpoint(path, stage="submission_unknown", email="unknown@example.invalid", password="secret")
            self.assertEqual([], resumable_checkpoints(Path(directory)))

    def test_resumable_jobs_are_isolated_by_account_type(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            build = root / "jobs" / "build.json"
            web = root / "jobs" / "web.json"
            for path, account_type in ((build, "build"), (web, "web")):
                write_checkpoint(
                    path,
                    stage="sso_ready",
                    email=f"{account_type}@example.invalid",
                    password="secret",
                    sso="synthetic",
                    account_type=account_type,
                )

            self.assertEqual([web], resumable_checkpoints(root, "web"))
            self.assertEqual([build], resumable_checkpoints(root, "build"))

    def test_web_credential_uses_web_provider_envelope(self):
        with tempfile.TemporaryDirectory() as directory:
            path = write_web_credential(
                Path(directory),
                email="web@example.invalid",
                sso="synthetic-sso",
            )
            payload = json.loads(path.read_text(encoding="utf-8"))

            self.assertEqual("grok_web", payload["provider"])
            self.assertEqual("synthetic-sso", payload["accounts"][0]["sso_token"])

    def test_legacy_array_migrates_once_then_uses_append_only_jsonl(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            legacy = root / "protocol_accounts.json"
            ledger = root / "protocol_accounts.jsonl"
            legacy.write_text(json.dumps([{"email": "first@example.invalid"}]), encoding="utf-8")

            self.assertEqual(1, migrate_legacy_ledger(legacy, ledger))
            append_ledger(ledger, {"email": "second@example.invalid"})
            rows = [json.loads(line) for line in ledger.read_text(encoding="utf-8").splitlines()]

            self.assertEqual(
                ["first@example.invalid", "second@example.invalid"],
                [row["email"] for row in rows],
            )
            self.assertEqual(0, migrate_legacy_ledger(legacy, ledger))

    def test_main_retries_the_same_checkpoint_until_target_is_usable(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = root / "config.json"
            state_dir = root / "state"
            log_file = root / "registration.log"
            config.write_text(
                json.dumps(
                    {
                        "captcha_solver": "local",
                        "email_provider": "yyds",
                        "protocol_attempt_multiplier": 2,
                        "protocol_stage_retry_limit": 2,
                        "spool_dir": str(root / "spool" / "incoming"),
                    }
                ),
                encoding="utf-8",
            )
            calls: list[Path] = []
            providers = []
            started_states = []

            def fake_register(_index, _cfg, **kwargs):
                checkpoint_path = kwargs["checkpoint_path"]
                calls.append(checkpoint_path)
                providers.append(kwargs["clearance_provider"])
                started_states.append(json.loads((state_dir / "state.json").read_text(encoding="utf-8")))
                if len(calls) == 1:
                    write_checkpoint(
                        checkpoint_path,
                        stage="sso_ready",
                        attempts=1,
                        email="resume@example.invalid",
                        password="secret",
                        sso="synthetic",
                    )
                    return {"ok": False, "error": "synthetic oauth failure"}
                return {
                    "ok": True,
                    "email": "resume@example.invalid",
                    "password": "secret",
                    "spool": "synthetic.json",
                    "engine": "protocol",
                }

            argv = [
                "protocol_register_cli.py",
                "--config",
                str(config),
                "--state-dir",
                str(state_dir),
                "--log-file",
                str(log_file),
                "--count",
                "1",
                "--threads",
                "1",
                "--fast",
            ]
            worker._stop.clear()
            with patch.object(sys, "argv", argv), patch.object(worker, "register_one", side_effect=fake_register):
                exit_code = worker.main()

            self.assertEqual(0, exit_code)
            self.assertEqual(2, len(calls))
            self.assertEqual(calls[0], calls[1])
            self.assertIs(providers[0], providers[1])
            self.assertEqual(
                [(state["attempted"], state["active"]) for state in started_states],
                [(1, 1), (2, 1)],
            )
            self.assertEqual(1, len(list((state_dir / "jobs").glob("*.json"))))
            state = json.loads((state_dir / "state.json").read_text(encoding="utf-8"))
            self.assertEqual({"done": 1, "ok": 1, "attempted": 2, "failed": 1}, {key: state[key] for key in ("done", "ok", "attempted", "failed")})
            self.assertEqual(state["active"], 0)

    def test_main_runtime_proxy_overrides_persisted_proxy(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            config = root / "config.json"
            state_dir = root / "state"
            log_file = root / "registration.log"
            config.write_text(
                json.dumps({"captcha_solver": "local", "proxy": "http://127.0.0.1:7890"}),
                encoding="utf-8",
            )
            observed: list[str] = []

            def fake_register(_index, _cfg, **kwargs):
                observed.append(kwargs["proxy"])
                return {
                    "ok": True,
                    "email": "proxy@example.invalid",
                    "password": "secret",
                    "spool": "synthetic.json",
                    "engine": "protocol",
                }

            argv = [
                "protocol_register_cli.py",
                "--config", str(config),
                "--state-dir", str(state_dir),
                "--log-file", str(log_file),
                "--count", "1",
                "--threads", "1",
                "--proxy", "http://127.0.0.1:7897",
            ]
            worker._stop.clear()
            with patch.object(sys, "argv", argv), patch.object(worker, "register_one", side_effect=fake_register):
                exit_code = worker.main()

            self.assertEqual(0, exit_code)
            self.assertEqual(["http://127.0.0.1:7897"], observed)


if __name__ == "__main__":
    unittest.main()
