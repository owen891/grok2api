import unittest
from unittest.mock import Mock, patch

from scripts.grok_web_browser_worker import (
    BrowserSession,
    classify_worker_error,
    parse_cookie_header,
    translated_proxy_url,
    validate_request,
)


class BrowserWorkerValidationTests(unittest.TestCase):
    def test_worker_error_classification(self):
        self.assertEqual(
            classify_worker_error(RuntimeError("unknown error: net::ERR_PROXY_CONNECTION_FAILED")),
            "proxy_unavailable",
        )
        self.assertEqual(
            classify_worker_error(RuntimeError("Cloudflare challenge did not clear in Chromium")),
            "anti_bot",
        )
        self.assertEqual(classify_worker_error(RuntimeError("Chrome crashed")), "browser_unavailable")

    def test_cookie_filter_keeps_only_cloudflare_state(self):
        self.assertEqual(
            parse_cookie_header("cf_clearance=ok; sso=secret; __cf_bm=bm; x-userid=user"),
            {"cf_clearance": "ok", "__cf_bm": "bm"},
        )

    def test_request_is_limited_to_fast_image_endpoint(self):
        value = validate_request(
            {
                "baseURL": "https://grok.com",
                "endpoint": "https://grok.com/rest/app-chat/conversations/new",
                "ssoToken": "token",
                "payload": {"message": "Drawing: test"},
                "timeoutSeconds": 180,
            }
        )
        self.assertEqual(value["timeoutSeconds"], 180)

        with self.assertRaises(ValueError):
            validate_request(
                {
                    "baseURL": "https://grok.com",
                    "endpoint": "https://example.com/collect",
                    "ssoToken": "token",
                    "payload": {},
                }
            )

    def test_quota_endpoint_has_a_separate_allowlist(self):
        value = validate_request(
            {
                "baseURL": "https://grok.com",
                "endpoint": "https://grok.com/rest/rate-limits",
                "ssoToken": "token",
                "payload": {"modelName": "fast"},
            },
            {"/rest/rate-limits"},
        )
        self.assertEqual(value["endpoint"], "https://grok.com/rest/rate-limits")
        with self.assertRaises(ValueError):
            validate_request(value, {"/rest/app-chat/conversations/new"})

    def test_loopback_proxy_is_translated_for_container(self):
        import os

        with patch.dict(os.environ, {"GROK_WORKER_LOOPBACK_PROXY_HOST": "host.docker.internal"}):
            self.assertEqual(
                translated_proxy_url("http://user:pass@127.0.0.1:7890"),
                "http://user:pass@host.docker.internal:7890",
            )

    def test_warm_prepares_page_and_statsig_without_generating(self):
        session = BrowserSession()
        driver = Mock()
        value = {"baseURL": "https://grok.com"}
        with (
            patch.object(session, "_ensure_driver", return_value=driver) as ensure_driver,
            patch.object(session, "_prepare_page") as prepare_page,
            patch.object(session, "_statsig_id", return_value="signed") as statsig_id,
        ):
            session.warm(value)
        ensure_driver.assert_called_once_with(value)
        prepare_page.assert_called_once_with(driver, value)
        statsig_id.assert_called_once_with(driver, value)


if __name__ == "__main__":
    unittest.main()
