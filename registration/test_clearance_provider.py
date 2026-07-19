from concurrent.futures import ThreadPoolExecutor
import sys
import threading
import time
import types
import unittest
from unittest import mock

from clearance_provider import DockerTokenProvider, YesCaptchaTokenProvider, create_clearance_provider


class ClearanceProviderTests(unittest.TestCase):
    def test_legacy_local_config_selects_docker_provider(self):
        provider = create_clearance_provider({"captcha_solver": "local", "captcha_endpoint": "docker://captcha:5072"})
        self.assertIsInstance(provider, DockerTokenProvider)

    def test_yescaptcha_requires_key(self):
        with self.assertRaisesRegex(RuntimeError, "no API key"):
            create_clearance_provider({"clearance_provider": "yescaptcha"})

    def test_docker_provider_maps_endpoint_and_proxy(self):
        calls = {}

        def solve_turnstile_local(**kwargs):
            calls.update(kwargs)
            return "token-docker"

        local_turnstile = types.SimpleNamespace(solve_turnstile_local=solve_turnstile_local)
        xconsole_client = types.SimpleNamespace(config=types.SimpleNamespace(TURNSTILE_SITEKEY="site-key"))
        with mock.patch.dict(sys.modules, {"local_turnstile": local_turnstile, "xconsole_client": xconsole_client}):
            result = DockerTokenProvider({"clearance_endpoint": "docker://captcha:5072", "clearance_timeout": 9}).solve(
                website_url="https://grok.com", proxy="http://proxy:8080"
            )
        self.assertEqual(result.token, "token-docker")
        self.assertEqual(calls["endpoint"], "docker://captcha:5072")
        self.assertEqual(calls["proxy"], "http://proxy:8080")
        self.assertEqual(calls["website_key"], "site-key")

    def test_docker_provider_round_robins_and_falls_back_endpoints(self):
        calls = []

        def solve_turnstile_local(**kwargs):
            calls.append(kwargs["endpoint"])
            if kwargs["endpoint"] == "http://solver-a:5072":
                raise TimeoutError("a timed out")
            return "token-pool"

        local_turnstile = types.SimpleNamespace(solve_turnstile_local=solve_turnstile_local)
        xconsole_client = types.SimpleNamespace(config=types.SimpleNamespace(TURNSTILE_SITEKEY="site-key"))
        with mock.patch.dict(sys.modules, {"local_turnstile": local_turnstile, "xconsole_client": xconsole_client}):
            provider = DockerTokenProvider({
                "captcha_endpoints": ["http://solver-a:5072", "http://solver-b:5072"],
                "clearance_timeout": 9,
            })
            result = provider.solve(website_url="https://grok.com")
        self.assertEqual(result.token, "token-pool")
        self.assertEqual(calls, ["http://solver-a:5072", "http://solver-b:5072"])

    def test_docker_provider_keeps_round_robin_state_and_forwards_live_sitekey(self):
        calls = []

        def solve_turnstile_local(**kwargs):
            calls.append((kwargs["endpoint"], kwargs["website_key"]))
            return "token-pool"

        local_turnstile = types.SimpleNamespace(solve_turnstile_local=solve_turnstile_local)
        xconsole_client = types.SimpleNamespace(config=types.SimpleNamespace(TURNSTILE_SITEKEY="stale-key"))
        with mock.patch.dict(sys.modules, {"local_turnstile": local_turnstile, "xconsole_client": xconsole_client}):
            provider = DockerTokenProvider({
                "captcha_endpoints": ["http://solver-a:5072", "http://solver-b:5072"],
            })
            provider.solve(website_url="https://grok.com", website_key="live-key")
            provider.solve(website_url="https://grok.com", website_key="live-key")

        self.assertEqual(
            calls,
            [
                ("http://solver-a:5072", "live-key"),
                ("http://solver-b:5072", "live-key"),
            ],
        )

    def test_single_endpoint_serializes_concurrent_tasks(self):
        state_lock = threading.Lock()
        active = 0
        peak = 0

        def solve_turnstile_local(**_kwargs):
            nonlocal active, peak
            with state_lock:
                active += 1
                peak = max(peak, active)
            time.sleep(0.03)
            with state_lock:
                active -= 1
            return "token-serialized"

        local_turnstile = types.SimpleNamespace(solve_turnstile_local=solve_turnstile_local)
        xconsole_client = types.SimpleNamespace(config=types.SimpleNamespace(TURNSTILE_SITEKEY="site-key"))
        with mock.patch.dict(sys.modules, {"local_turnstile": local_turnstile, "xconsole_client": xconsole_client}):
            provider = DockerTokenProvider({"captcha_endpoint": "http://solver:5072", "turnstile_timeout": 2})
            with ThreadPoolExecutor(max_workers=2) as pool:
                results = list(pool.map(lambda _: provider.solve(website_url="https://grok.com"), range(2)))

        self.assertEqual([result.token for result in results], ["token-serialized", "token-serialized"])
        self.assertEqual(peak, 1)

    def test_repeated_route_failures_open_circuit_without_creating_more_tasks(self):
        calls = 0

        def solve_turnstile_local(**_kwargs):
            nonlocal calls
            calls += 1
            raise RuntimeError("ERROR_CAPTCHA_UNSOLVABLE")

        local_turnstile = types.SimpleNamespace(solve_turnstile_local=solve_turnstile_local)
        xconsole_client = types.SimpleNamespace(config=types.SimpleNamespace(TURNSTILE_SITEKEY="site-key"))
        with mock.patch.dict(sys.modules, {"local_turnstile": local_turnstile, "xconsole_client": xconsole_client}):
            provider = DockerTokenProvider({
                "captcha_endpoint": "http://solver:5072",
                "captcha_failure_threshold": 2,
                "captcha_failure_cooldown": 60,
            })
            for _ in range(2):
                with self.assertRaisesRegex(RuntimeError, "ERROR_CAPTCHA_UNSOLVABLE"):
                    provider.solve(website_url="https://grok.com", proxy="http://proxy:8080")
            with self.assertRaisesRegex(RuntimeError, "熔断"):
                provider.solve(website_url="https://grok.com", proxy="http://proxy:8080")

        self.assertEqual(calls, 2)

    @mock.patch.dict("clearance_provider.os.environ", {"REGISTRATION_CAPTCHA_CONCURRENCY": "3"}, clear=False)
    def test_deployment_concurrency_matches_solver_workers(self):
        provider = DockerTokenProvider({
            "captcha_endpoint": "http://solver:5072",
            "captcha_endpoint_concurrency": 1,
        })
        self.assertEqual(provider.concurrency, 3)

    def test_yescaptcha_provider_returns_token(self):
        observed = {}

        class Solver:
            def __init__(self, key):
                self.key = key

            def solve_turnstile(self, **kwargs):
                self.kwargs = kwargs
                observed.update(kwargs)
                return "token-yescaptcha"

        xconsole_client = types.SimpleNamespace(
            YesCaptchaSolver=Solver,
            config=types.SimpleNamespace(TURNSTILE_SITEKEY="site-key"),
        )
        with mock.patch.dict(sys.modules, {"xconsole_client": xconsole_client}):
            result = YesCaptchaTokenProvider("api-key").solve(
                website_url="https://grok.com",
                website_key="live-key",
            )
        self.assertEqual(result.token, "token-yescaptcha")
        self.assertEqual(observed["website_key"], "live-key")


if __name__ == "__main__":
    unittest.main()
