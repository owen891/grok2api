import io
import json
import unittest
import urllib.error
from unittest.mock import patch

from cpa_xai.probe import probe_mini_response
from cpa_xai.schema import DEFAULT_CLIENT_HEADERS


class _Response:
    status = 200

    def __init__(self, payload):
        self._body = json.dumps(payload).encode("utf-8")

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        return False

    def read(self):
        return self._body


class _Opener:
    def __init__(self, outcomes):
        self.outcomes = outcomes
        self.requests = []

    def open(self, request, timeout):
        self.requests.append((request, timeout))
        outcome = self.outcomes.pop(0)
        if isinstance(outcome, Exception):
            raise outcome
        return _Response(outcome)


def _permission_denied():
    body = json.dumps({"code": "permission-denied", "error": "not ready"}).encode("utf-8")
    return urllib.error.HTTPError("https://example.test", 403, "Forbidden", {}, io.BytesIO(body))


def _success():
    return {
        "model": "grok-4.5-build-free",
        "output": [
            {"type": "message", "content": [{"type": "output_text", "text": "MINT_OK"}]}
        ],
    }


class CPAProbeTests(unittest.TestCase):
    def test_client_headers_match_backend_build_baseline(self):
        self.assertEqual("0.2.106", DEFAULT_CLIENT_HEADERS["x-grok-client-version"])
        self.assertIn("0.2.106", DEFAULT_CLIENT_HEADERS["User-Agent"])

    def test_chat_probe_retries_eventual_permission(self):
        opener = _Opener([_permission_denied(), _success()])
        with patch("cpa_xai.probe._opener", return_value=opener), patch("cpa_xai.probe.time.sleep"):
            result = probe_mini_response("token", attempts=3, retry_delay=0)

        self.assertTrue(result["ok"])
        self.assertEqual(2, result["attempts"])
        self.assertEqual(2, len(opener.requests))

    def test_chat_probe_preserves_terminal_failure_details(self):
        opener = _Opener([_permission_denied(), _permission_denied(), _permission_denied()])
        with patch("cpa_xai.probe._opener", return_value=opener), patch("cpa_xai.probe.time.sleep"):
            result = probe_mini_response("token", attempts=3, retry_delay=0)

        self.assertFalse(result["ok"])
        self.assertEqual(403, result["status"])
        self.assertEqual("permission_denied", result["code"])
        self.assertEqual(3, result["attempts"])
        self.assertIn("permission-denied", result["error"])


if __name__ == "__main__":
    unittest.main()
