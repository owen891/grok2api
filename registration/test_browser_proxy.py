import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import browser_proxy


class FakeOptions:
    def __init__(self):
        self.arguments: list[str] = []
        self.extensions: list[str] = []

    def set_argument(self, value: str) -> None:
        self.arguments.append(value)

    def add_extension(self, value: str) -> None:
        self.extensions.append(value)


class BrowserProxyTests(unittest.TestCase):
    def tearDown(self):
        browser_proxy.cleanup_proxy_auth_extensions()

    def test_unauthenticated_proxy_sets_only_proxy_server(self):
        options = FakeOptions()
        server = browser_proxy.configure_chromium_proxy(options, "http://proxy.example:8080")
        self.assertEqual("http://proxy.example:8080", server)
        self.assertEqual(["--proxy-server=http://proxy.example:8080"], options.arguments)
        self.assertEqual([], options.extensions)

    def test_authenticated_http_proxy_adds_private_mv3_extension(self):
        options = FakeOptions()
        with tempfile.TemporaryDirectory() as temporary, patch("tempfile.tempdir", temporary):
            server = browser_proxy.configure_chromium_proxy(options, "http://user:p%40ss@proxy.example:3128")
            self.assertEqual("http://proxy.example:3128", server)
            self.assertEqual(1, len(options.extensions))
            extension = Path(options.extensions[0])
            manifest = json.loads((extension / "manifest.json").read_text(encoding="utf-8"))
            worker = (extension / "service_worker.js").read_text(encoding="utf-8")
            self.assertEqual(3, manifest["manifest_version"])
            self.assertIn("webRequestAuthProvider", manifest["permissions"])
            self.assertIn('"username": "user"', worker)
            self.assertIn('"password": "p@ss"', worker)
            self.assertIn("details.isProxy", worker)
            self.assertIn("asyncBlocking", worker)

    def test_authenticated_socks_proxy_requires_local_relay(self):
        options = FakeOptions()
        with self.assertRaisesRegex(ValueError, "local unauthenticated relay"):
            browser_proxy.configure_chromium_proxy(options, "socks5://user:secret@proxy.example:1080")


if __name__ == "__main__":
    unittest.main()
