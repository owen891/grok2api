import base64
import json
import sys
import unittest
from types import SimpleNamespace
from unittest.mock import Mock, patch

from scripts.grok_web_browser_worker import (
    BrowserSession,
    classify_worker_error,
    is_antibot_result,
    parse_cookie_header,
    session_fingerprint,
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

    def test_only_antibot_forbidden_response_is_retried(self):
        encoded = lambda value: base64.b64encode(value.encode()).decode()
        self.assertTrue(is_antibot_result({"statusCode": 403, "bodyBase64": encoded('{"error":{"message":"Request rejected by anti-bot rules."}}')}))
        self.assertFalse(is_antibot_result({"statusCode": 403, "bodyBase64": encoded('{"error":{"message":"Model is not found"}}')}))

    def test_cookie_filter_keeps_only_cloudflare_state(self):
        self.assertEqual(
            parse_cookie_header("cf_clearance=ok; sso=secret; __cf_bm=bm; x-userid=user"),
            {"cf_clearance": "ok", "__cf_bm": "bm"},
        )

    def test_cloudflare_cookie_change_restarts_session(self):
        base = {"proxyURL": "http://proxy:8080", "userAgent": "Chrome", "cloudflareCookies": "cf_clearance=one"}
        reordered = {**base, "cloudflareCookies": "cf_clearance=one; __cf_bm=bm"}
        same_values = {**base, "cloudflareCookies": "cf_clearance=one"}
        self.assertEqual(session_fingerprint(base), session_fingerprint(same_values))
        self.assertNotEqual(session_fingerprint(base), session_fingerprint(reordered))

    def test_cloudflare_state_exports_only_allowed_cookies(self):
        driver = Mock()
        driver.get_cookies.return_value = [
            {"name": "cf_clearance", "value": "fresh"},
            {"name": "__cf_bm", "value": "bm"},
            {"name": "sso", "value": "secret"},
        ]
        driver.execute_script.return_value = "Chrome/Fresh"
        self.assertEqual(
            BrowserSession._cloudflare_state(driver),
            {"cloudflareCookies": "__cf_bm=bm; cf_clearance=fresh", "userAgent": "Chrome/Fresh"},
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

    def test_chat_endpoint_allows_only_single_conversation_response_path(self):
        value = {
            "baseURL": "https://grok.com",
            "endpoint": "https://grok.com/rest/app-chat/conversations/conv_123/responses",
            "ssoToken": "token",
            "payload": {"message": "continue"},
        }
        validated = validate_request(
            value,
            {"/rest/app-chat/conversations/new"},
            allow_conversation_responses=True,
        )
        self.assertEqual(validated["endpoint"], value["endpoint"])
        with self.assertRaises(ValueError):
            validate_request(
                {**value, "endpoint": "https://grok.com/rest/app-chat/conversations/a/b/responses"},
                {"/rest/app-chat/conversations/new"},
                allow_conversation_responses=True,
            )

    def test_loopback_proxy_is_translated_for_container(self):
        import os

        with patch.dict(os.environ, {"GROK_WORKER_LOOPBACK_PROXY_HOST": "host.docker.internal"}):
            self.assertEqual(
                translated_proxy_url("http://user:pass@127.0.0.1:7890"),
                "http://user:pass@host.docker.internal:7890",
            )

    def test_warm_prepares_page_and_probes_signed_fetch_without_generating(self):
        session = BrowserSession()
        driver = Mock()
        value = {"baseURL": "https://grok.com"}
        with (
            patch.object(session, "_ensure_driver", return_value=driver) as ensure_driver,
            patch.object(session, "_prepare_page") as prepare_page,
            patch.object(session, "_probe_signed_fetch") as probe_signed_fetch,
        ):
            session.warm(value)
        ensure_driver.assert_called_once_with(value)
        prepare_page.assert_called_once_with(driver, value)
        probe_signed_fetch.assert_called_once_with(driver)

    def test_antibot_request_reloads_page_before_retry(self):
        session = BrowserSession()
        driver = Mock()
        value = {"baseURL": "https://grok.com", "requestID": "request-1"}
        with (
            patch.object(session, "_ensure_driver", return_value=driver),
            patch.object(session, "_prepare_page"),
            patch.object(session, "_wait_for_challenge"),
            patch.object(
                session,
                "_fetch",
                side_effect=[
                    {"statusCode": 403, "bodyBase64": base64.b64encode(b"request rejected by anti-bot rules").decode()},
                    {"statusCode": 200},
                ],
            ) as fetch,
            patch.object(session, "_cloudflare_state", return_value={"cloudflareCookies": "", "userAgent": "Chrome"}),
        ):
            result = session.request(value)
        self.assertEqual(result["statusCode"], 200)
        self.assertEqual(fetch.call_count, 2)
        driver.get.assert_called_once_with("https://grok.com/")

    def test_fetch_uses_page_signer_without_external_statsig_header(self):
        driver = Mock()
        driver.execute_async_script.return_value = {"statusCode": 200, "text": "ok"}
        value = {
            "endpoint": "https://grok.com/rest/rate-limits",
            "payload": {"modelName": "fast"},
            "timeoutSeconds": 30,
            "requestID": "request-1",
        }
        result = BrowserSession._fetch(driver, value)
        script = driver.execute_async_script.call_args.args[0]
        self.assertNotIn("'x-statsig-id':", script)
        self.assertEqual(driver.execute_async_script.call_args.args[1:], (value["endpoint"], value["payload"], "request-1"))
        self.assertEqual(base64.b64decode(result["bodyBase64"]), b"ok")

    def test_ensure_driver_passes_proxy_to_webdriver(self):
        session = BrowserSession()
        driver = Mock()
        utils = SimpleNamespace(USER_AGENT=None, get_webdriver=Mock(return_value=driver))
        value = {
            "proxyURL": "http://user:pass@proxy.example:8080",
            "userAgent": "Chrome/Test",
            "cloudflareCookies": "",
        }
        with patch.dict(sys.modules, {"utils": utils}):
            session._ensure_driver(value)
        utils.get_webdriver.assert_called_once_with(
            {"url": "http://proxy.example:8080", "username": "user", "password": "pass"}
        )
        self.assertEqual(utils.USER_AGENT, "Chrome/Test")

    def test_signed_fetch_probe_rejects_unauthorized_session(self):
        driver = Mock()
        driver.execute_async_script.return_value = {"status": 401, "text": "unauthorized"}
        with self.assertRaisesRegex(RuntimeError, "status=401"):
            BrowserSession._probe_signed_fetch(driver)

    def test_prepare_page_acknowledges_tos_gate_after_navigation(self):
        session = BrowserSession()
        driver = Mock()
        driver.current_url = "https://grok.com/tos-gate"
        value = {"baseURL": "https://grok.com", "ssoToken": "token", "cloudflareCookies": ""}
        with (
            patch.object(session, "_wait_for_challenge"),
            patch.object(session, "_dismiss_tos_gate") as dismiss,
        ):
            session._prepare_page(driver, value)
        dismiss.assert_called_once_with(driver)

    def test_composer_response_preserves_conversation_contract(self):
        raw = BrowserSession._synthetic_composer_response("conversation-1", "response-1", "hello")
        frames = [json.loads(line) for line in raw.decode().splitlines()]
        self.assertEqual(frames[0]["result"]["conversation"]["conversationId"], "conversation-1")
        self.assertEqual(frames[1]["result"]["response"]["userResponse"]["responseId"], "response-1")
        self.assertEqual(frames[1]["result"]["response"]["token"], "hello")


if __name__ == "__main__":
    unittest.main()
