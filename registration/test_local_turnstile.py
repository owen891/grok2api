import unittest
from itertools import chain, repeat
from unittest.mock import patch

from local_turnstile import _dockerize_loopback_proxy, _ensure_docker_container_running, _pin_http_endpoint, solve_turnstile_local


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
            type("Result", (), {"returncode": 0, "stdout": "grokcli-2api\n", "stderr": ""})(),
            type("Result", (), {"returncode": 0, "stdout": "running|0|false\n", "stderr": ""})(),
        ]
        _ensure_docker_container_running("grokcli-2api")
        self.assertEqual(run.call_args_list[1].args[0][:3], ["docker", "start", "grokcli-2api"])
        sleep.assert_called_once_with(0.5)

    @patch.dict("local_turnstile.os.environ", {"LOCAL_CAPTCHA_DOCKER_AUTOSTART": "0"}, clear=False)
    @patch("local_turnstile.subprocess.run")
    def test_autostart_can_be_disabled_with_diagnostics(self, run):
        run.return_value = type(
            "Result", (), {"returncode": 0, "stdout": "exited|137|true\n", "stderr": ""}
        )()
        with self.assertRaisesRegex(RuntimeError, r"exited .*exit=137.*oom=true"):
            _ensure_docker_container_running("grokcli-2api")
        self.assertEqual(run.call_count, 1)


class DockerFallbackTests(unittest.TestCase):
    @patch("local_turnstile._http_solve", side_effect=TimeoutError("solver timed out"))
    @patch("local_turnstile._docker_cli_available", return_value=False)
    @patch.dict("local_turnstile.os.environ", {"LOCAL_CAPTCHA_RETRIES": "0"}, clear=False)
    def test_http_timeout_does_not_invoke_missing_docker(self, docker_available, http_solve):
        with self.assertRaisesRegex(RuntimeError, r"HTTP .*未找到 docker 命令"):
            solve_turnstile_local(
                endpoint="http://127.0.0.1:5072",
                website_url="https://grok.com",
                website_key="site-key",
                timeout=30,
            )
        docker_available.assert_called_once()
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
            with patch("local_turnstile.time.monotonic", side_effect=chain([0.0, 0.0, 0.0], repeat(31.0))), patch(
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
