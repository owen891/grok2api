import sys
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

    def test_yescaptcha_provider_returns_token(self):
        class Solver:
            def __init__(self, key):
                self.key = key

            def solve_turnstile(self, **kwargs):
                self.kwargs = kwargs
                return "token-yescaptcha"

        xconsole_client = types.SimpleNamespace(
            YesCaptchaSolver=Solver,
            config=types.SimpleNamespace(TURNSTILE_SITEKEY="site-key"),
        )
        with mock.patch.dict(sys.modules, {"xconsole_client": xconsole_client}):
            result = YesCaptchaTokenProvider("api-key").solve(website_url="https://grok.com")
        self.assertEqual(result.token, "token-yescaptcha")


if __name__ == "__main__":
    unittest.main()
