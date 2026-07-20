import unittest
from itertools import chain, repeat
from unittest.mock import patch

from local_turnstile import (
    _dockerize_loopback_proxy,
    _ensure_docker_container_running,
    _http_solve_with_retries,
    _pin_http_endpoint,
    check_turnstile_endpoint,
    solve_turnstile_local,
)


class DockerProxyMappingTests(unittest.TestCase):
    def test_maps_host_loopback_for_docker_desktop(self):
        self.assertEqual(
            _dockerize_loopback_proxy("http://127.0.0.1:7897"),
            "http://host.docker.internal:7897",
        )

    def test_preserves_remote_proxy(self):
        self.assertEqual(
            _dockerize_loopback_proxy("socks5://user:pass@proxy.example:1080"),
            "socks5://user:pass@proxy.example:1080",
        )

    @patch("local_turnstile.socket.getaddrinfo")
    def test_pins_compose_dns_for_task_polling(self, getaddrinfo):
        getaddrinfo.return_value = [
            (2, 1, 6, "", ("172.28.0.4", 5072)),
            (2, 1, 6, "", ("172.28.0.5", 5072)),
        ]
        pinned = _pin_http_endpoint("http://grok-turnstile-solver:5072")
        self.assertRegex(pinned, r"^http://172\.28\.0\.[45]:5072$")


class DockerContainerRecoveryTests(unittest.TestCase):
    @patch("local_turnstile.time.sleep")
    @patch("local_turnstile.subprocess.run")
    def test_stopped_container_is_started_and_waited_for(self, run, sleep):
        run.side_effect = [
            type("Result", (), {"returncode": 0, "stdout": "exited|137|false\n", "stderr": ""})(),
            type("Result", (), {"returncode": 0, "stdout": "turnstile-solver\n", "stderr": ""})(),
            type("Result", (), {"returncode": 0, "stdout": "running|0|false\n", "stderr": ""})(),
        ]
        _ensure_docker_container_running("turnstile-solver")
        self.assertEqual(run.call_args_list[1].args[0][:3], ["docker", "start", "turnstile-solver"])
        sleep.assert_called_once_with(0.5)

    @patch.dict("local_turnstile.os.environ", {"LOCAL_CAPTCHA_DOCKER_AUTOSTART": "0"}, clear=False)
    @patch("local_turnstile.subprocess.run")
    def test_autostart_can_be_disabled_with_diagnostics(self, run):
        run.return_value = type(
            "Result", (), {"returncode": 0, "stdout": "exited|137|true\n", "stderr": ""}
        )()
        with self.assertRaisesRegex(RuntimeError, r"exited .*exit=137.*oom=true"):
            _ensure_docker_container_running("turnstile-solver")
        self.assertEqual(run.call_count, 1)


class SolverHealthTests(unittest.TestCase):
    @staticmethod
    def response(payload, *, status_code=200, text=""):
        return type(
            "Response",
            (),
            {
                "status_code": status_code,
                "text": text,
                "json": lambda self: payload,
            },
        )()

    @patch("local_turnstile.requests.get")
    def test_healthy_http_solver_is_ready(self, get):
        get.return_value = self.response(
            {"ok": True, "lazy": False, "pool_ready": True, "thread": 2}
        )

        ready, detail = check_turnstile_endpoint("https://solver.example:5072", timeout=2)

        self.assertTrue(ready)
        self.assertIn('"pool_ready":true', detail)
        get.assert_called_once_with("https://solver.example:5072/health", timeout=2.0)

    @patch("local_turnstile.requests.get")
    def test_eager_solver_requires_browser_pool(self, get):
        get.return_value = self.response(
            {"ok": True, "lazy": False, "pool_ready": False, "thread": 1}
        )

        ready, detail = check_turnstile_endpoint("https://solver.example:5072")

        self.assertFalse(ready)
        self.assertIn("browser pool not ready", detail)

    @patch("local_turnstile.requests.get")
    def test_lazy_solver_may_be_idle_during_preflight(self, get):
        get.return_value = self.response(
            {"ok": True, "lazy": True, "pool_ready": False, "thread": 1}
        )

        ready, detail = check_turnstile_endpoint("https://solver.example:5072")

        self.assertTrue(ready)
        self.assertIn('"lazy":true', detail)

    @patch("local_turnstile.requests.get")
    def test_http_error_is_not_ready(self, get):
        get.return_value = self.response({}, status_code=503)

        self.assertEqual(
            check_turnstile_endpoint("https://solver.example:5072"),
            (False, "health HTTP 503"),
        )

    @patch("local_turnstile.requests.get")
    def test_invalid_health_json_is_not_ready(self, get):
        response = self.response({}, text="not-json")
        response.json = lambda: (_ for _ in ()).throw(ValueError("invalid JSON"))
        get.return_value = response

        self.assertEqual(
            check_turnstile_endpoint("https://solver.example:5072"),
            (False, "invalid health JSON: not-json"),
        )


class DockerFallbackTests(unittest.TestCase):
    @patch("local_turnstile._http_solve", side_effect=RuntimeError("transient"))
    @patch.dict("local_turnstile.os.environ", {"LOCAL_CAPTCHA_RETRIES": "1"}, clear=False)
    def test_explicit_retries_share_one_total_timeout(self, http_solve):
        with patch("local_turnstile.time.monotonic", side_effect=[0.0, 0.0, 4.0, 6.0]), patch(
            "local_turnstile.time.sleep"
        ):
            with self.assertRaisesRegex(RuntimeError, "attempt 2/2"):
                _http_solve_with_retries("http://solver:5072", {}, 10)
        self.assertEqual([call.args[2] for call in http_solve.call_args_list], [10.0, 4.0])

    @patch("local_turnstile._http_solve", side_effect=TimeoutError("solver timed out"))
    @patch("local_turnstile._docker_cli_available")
    @patch.dict("local_turnstile.os.environ", {"LOCAL_CAPTCHA_RETRIES": "0"}, clear=False)
    def test_http_timeout_does_not_invoke_docker(self, docker_available, http_solve):
        with self.assertRaisesRegex(TimeoutError, "solver timed out"):
            solve_turnstile_local(
                endpoint="http://127.0.0.1:5072",
                website_url="https://grok.com",
                website_key="site-key",
                timeout=30,
            )
        docker_available.assert_not_called()
        http_solve.assert_called_once()

    @patch("local_turnstile.requests.get")
    @patch("local_turnstile.requests.post")
    @patch.dict("local_turnstile.os.environ", {"LOCAL_CAPTCHA_RETRIES": "0"}, clear=False)
    def test_http_timeout_includes_solver_health(self, post, get):
        post.side_effect = [
            type("Response", (), {"status_code": 200, "json": lambda self: {"errorId": 0, "taskId": "task-1"}, "text": ""})(),
            type("Response", (), {"status_code": 200, "json": lambda self: {"errorId": 0, "status": "processing"}, "text": ""})(),
        ]
        get.return_value = type(
            "Response", (), {"status_code": 200, "json": lambda self: {"ok": True, "pool_ready": False, "in_flight": 1}}
        )()
        with self.assertRaisesRegex(TimeoutError, r"taskId=task-1.*pool_ready.*in_flight"):
            # Patch the clock/sleep indirectly by using the minimum timeout and
            # make the poll response consume the deadline immediately.
            with patch("local_turnstile.time.monotonic", side_effect=chain(repeat(0.0, 8), repeat(31.0))), patch(
                "local_turnstile.time.sleep"
            ):
                solve_turnstile_local(
                    endpoint="https://solver.example:5072",
                    website_url="https://grok.com",
                    website_key="site-key",
                    timeout=30,
                )


if __name__ == "__main__":
    unittest.main()
