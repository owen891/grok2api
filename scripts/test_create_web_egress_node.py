import contextlib
import io
import unittest
from unittest.mock import patch

import create_web_egress_node as script


class CreateWebEgressNodeTests(unittest.TestCase):
    def test_main_prints_only_public_node_fields(self):
        created = {
            "data": {
                "id": "7",
                "name": "web-cf-1",
                "scope": "grok_web",
                "enabled": True,
                "proxyConfigured": True,
                "cookieConfigured": True,
                "proxyURL": "http://user:secret@proxy.example:8080",
                "cloudflareCookies": "cf_clearance=secret-cookie",
            }
        }
        listed = {"data": {"items": [created["data"]], "accessToken": "secret-bearer"}}
        output = io.StringIO()

        with (
            patch.object(script, "PROXY_URL", "http://user:secret@proxy.example:8080"),
            patch.object(script, "CF_COOKIES", "cf_clearance=secret-cookie"),
            patch.object(script, "login", return_value="secret-bearer"),
            patch.object(script, "create_node", return_value=created),
            patch.object(script, "list_nodes", return_value=listed),
            contextlib.redirect_stdout(output),
        ):
            script.main()

        rendered = output.getvalue()
        self.assertIn('"proxyConfigured": true', rendered)
        self.assertNotIn("secret", rendered)
        self.assertNotIn("proxyURL", rendered)
        self.assertNotIn("cloudflareCookies", rendered)


if __name__ == "__main__":
    unittest.main()
