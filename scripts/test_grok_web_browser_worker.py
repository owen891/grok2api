import base64
import json
import sys
import threading
import time
import unittest
from types import SimpleNamespace
from unittest.mock import Mock, patch

from scripts.grok_web_browser_worker import (
    BrowserSession,
    ImageGenerationIncompleteError,
    QUOTA_SESSION,
    SESSION,
    WorkerBusyError,
    classify_worker_error,
    is_generated_image_source,
    is_antibot_result,
    is_worker_ready,
    parse_cookie_header,
    session_fingerprint,
    session_for_path,
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
            classify_worker_error(RuntimeError("unknown error: net::ERR_CONNECTION_CLOSED")),
            "proxy_unavailable",
        )
        self.assertEqual(
            classify_worker_error(RuntimeError("Cloudflare challenge did not clear in Chromium")),
            "anti_bot",
        )
        self.assertEqual(classify_worker_error(WorkerBusyError("busy")), "worker_busy")
        self.assertEqual(
            classify_worker_error(ImageGenerationIncompleteError("missing final image")),
            "image_generation_incomplete",
        )
        self.assertEqual(classify_worker_error(RuntimeError("Chrome crashed")), "browser_unavailable")

    def test_invalid_worker_input_is_rejected_as_value_error(self):
        common = {
            "baseURL": "https://grok.com",
            "endpoint": "https://grok.com/rest/app-chat/conversations/new",
            "ssoToken": "token",
            "payload": {},
        }
        for field, value in (
            ("timeoutSeconds", float("inf")),
            ("proxyURL", "http://proxy.example:99999"),
            ("proxyURL", "http://[invalid"),
        ):
            with self.subTest(field=field, value=value), self.assertRaises(ValueError):
                validate_request({**common, field: value})

    def test_generated_image_accepts_new_loaded_low_resolution_image(self):
        source, final_resolution = BrowserSession._new_generated_image(
            [
                {"src": "https://assets.grok.com/old.jpg", "complete": True, "width": 1024, "height": 1024},
                {"src": "https://assets.grok.com/new.jpg", "complete": True, "width": 512, "height": 512},
            ],
            {"https://assets.grok.com/old.jpg"},
            1,
        )
        self.assertEqual(source, "https://assets.grok.com/new.jpg")
        self.assertFalse(final_resolution)

    def test_generated_image_rejects_unloaded_and_tiny_candidates(self):
        source, final_resolution = BrowserSession._new_generated_image(
            [
                {"src": "https://assets.grok.com/loading.jpg", "complete": False, "width": 1024, "height": 1024},
                {"src": "https://assets.grok.com/tiny.jpg", "complete": True, "width": 128, "height": 128},
            ],
            set(),
            0,
        )
        self.assertEqual(source, "")
        self.assertFalse(final_resolution)

    def test_generated_image_sources_are_restricted_to_grok_assets(self):
        for source in (
            "https://assets.grok.com/generated/image.jpg",
            "https://imagine-public.x.ai/imagine-public/images/image.jpg",
            "https://imgen.x.ai/image.jpg",
            "blob:https://grok.com/id",
            "data:image/png;base64,AA==",
        ):
            self.assertTrue(is_generated_image_source(source), source)
        for source in (
            "https://grok.com/imagine/post/id",
            "https://example.com/image.jpg",
            "data:text/html;base64,AA==",
        ):
            self.assertFalse(is_generated_image_source(source), source)

    def test_generated_image_rejects_resource_entry_without_dom_dimensions(self):
        source, final_resolution = BrowserSession._new_generated_image(
            [{
                "src": "https://assets.grok.com/users/test/generated/image.jpg",
                "complete": True,
                "width": 0,
                "height": 0,
                "resource": True,
            }],
            set(),
            0,
        )
        self.assertEqual(source, "")
        self.assertFalse(final_resolution)

    def test_generated_image_rejects_untrusted_candidate(self):
        source, final_resolution = BrowserSession._new_generated_image(
            [{"src": "https://grok.com/imagine/post/id", "complete": True, "width": 1024, "height": 1024}],
            set(),
            0,
        )
        self.assertEqual(source, "")
        self.assertFalse(final_resolution)

    def test_quota_uses_a_dedicated_browser_session(self):
        self.assertIs(session_for_path("/v1/grok/quota"), QUOTA_SESSION)
        self.assertIs(session_for_path("/v1/grok/fast-image"), SESSION)

    def test_readyz_requires_the_primary_browser_session(self):
        with patch.object(SESSION, "driver", None), patch.object(QUOTA_SESSION, "driver", Mock()):
            self.assertFalse(is_worker_ready())
        with patch.object(SESSION, "driver", Mock()), patch.object(QUOTA_SESSION, "driver", None):
            self.assertTrue(is_worker_ready())

    def test_busy_session_drops_request_after_queue_budget(self):
        session = BrowserSession()
        session.lock.acquire()
        try:
            started = time.monotonic()
            with patch.object(session, "_ensure_driver") as ensure_driver, self.assertRaises(WorkerBusyError):
                session.request({"timeoutSeconds": 0.01})
            ensure_driver.assert_not_called()
            self.assertLess(time.monotonic() - started, 0.5)
        finally:
            session.lock.release()

    def test_queue_wait_consumes_the_request_budget(self):
        session = BrowserSession()
        session.lock.acquire()
        release_thread = threading.Thread(target=lambda: (time.sleep(0.08), session.lock.release()))
        release_thread.start()
        driver = Mock()
        seen = []

        def fetch(_driver, value):
            seen.append(value)
            return {"statusCode": 200}

        try:
            with (
                patch.object(session, "_ensure_driver", return_value=driver),
                patch.object(session, "_prepare_page"),
                patch.object(session, "_fetch", side_effect=fetch),
                patch.object(session, "_cloudflare_state", return_value={"cloudflareCookies": "", "userAgent": ""}),
            ):
                result = session.request({"timeoutSeconds": 0.3})
        finally:
            release_thread.join(timeout=1)
            if session.lock.locked():
                session.lock.release()

        self.assertEqual(result["statusCode"], 200)
        self.assertEqual(len(seen), 1)
        remaining_at_start = seen[0]["_deadline"] - time.monotonic()
        self.assertLess(remaining_at_start, 0.24)
        self.assertGreater(remaining_at_start, 0)

    def test_only_antibot_forbidden_response_is_retried(self):
        def encoded(value):
            return base64.b64encode(value.encode()).decode()
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
        ensure_driver.assert_called_once()
        work_value = ensure_driver.call_args.args[0]
        self.assertEqual(work_value["baseURL"], value["baseURL"])
        self.assertIs(prepare_page.call_args.args[0], driver)
        self.assertIs(prepare_page.call_args.args[1], work_value)
        probe_signed_fetch.assert_called_once_with(driver, work_value["_deadline"])

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
        self.assertIsNone(utils.USER_AGENT)

    def test_driver_initialization_serializes_process_global_user_agent(self):
        active = 0
        peak = 0
        seen = {}
        state_lock = threading.Lock()

        utils = SimpleNamespace(USER_AGENT="original")

        def get_webdriver(_proxy):
            nonlocal active, peak
            with state_lock:
                active += 1
                peak = max(peak, active)
                seen[threading.get_ident()] = utils.USER_AGENT
            try:
                time.sleep(0.03)
                return Mock()
            finally:
                with state_lock:
                    active -= 1

        utils.get_webdriver = get_webdriver
        sessions = [BrowserSession(), BrowserSession()]
        values = [
            {"userAgent": "Chrome/A", "cloudflareCookies": ""},
            {"userAgent": "Chrome/B", "cloudflareCookies": ""},
        ]
        barrier = threading.Barrier(3)
        errors = []

        def initialize(session, value):
            try:
                barrier.wait()
                session._ensure_driver(value)
            except Exception as error:
                errors.append(error)

        threads = [threading.Thread(target=initialize, args=item) for item in zip(sessions, values)]
        with patch.dict(sys.modules, {"utils": utils}):
            for thread in threads:
                thread.start()
            barrier.wait()
            for thread in threads:
                thread.join(timeout=1)

        self.assertEqual(errors, [])
        self.assertFalse(any(thread.is_alive() for thread in threads))
        self.assertEqual(peak, 1)
        self.assertEqual(set(seen.values()), {"Chrome/A", "Chrome/B"})
        self.assertEqual(utils.USER_AGENT, "original")

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
        dismiss.assert_called_once()
        self.assertIs(dismiss.call_args.args[0], driver)

    def test_composer_response_preserves_conversation_contract(self):
        raw = BrowserSession._synthetic_composer_response("conversation-1", "response-1", "hello")
        frames = [json.loads(line) for line in raw.decode().splitlines()]
        self.assertEqual(frames[0]["result"]["conversation"]["conversationId"], "conversation-1")
        self.assertEqual(frames[1]["result"]["response"]["userResponse"]["responseId"], "response-1")
        self.assertEqual(frames[1]["result"]["response"]["token"], "hello")

    def test_composer_submit_retries_intercepted_native_click(self):
        button = Mock()
        button.is_displayed.return_value = True
        button.is_enabled.return_value = True
        button.click.side_effect = [RuntimeError("element click intercepted by overlay"), None]
        driver = Mock()
        driver.find_elements.return_value = [button]
        driver.execute_script.side_effect = [None, True, None, True]
        BrowserSession._click_composer_submit(driver, 1e12)
        self.assertEqual(button.click.call_count, 2)
        self.assertNotIn("arguments[0].click();", str(driver.execute_script.call_args_list))

    def test_composer_submit_uses_dom_click_after_overlay_timeout(self):
        button = Mock()
        button.is_displayed.return_value = True
        button.is_enabled.return_value = True
        button.click.side_effect = RuntimeError("intercepted")
        driver = Mock()
        driver.find_elements.return_value = [button]
        driver.execute_script.side_effect = [None, True, None]
        with patch("scripts.grok_web_browser_worker.time.monotonic", side_effect=[0.0, 0.0, 1.0]):
            BrowserSession._click_composer_submit(driver, 2.0)
        self.assertTrue(any("arguments[0].click();" in str(call) for call in driver.execute_script.call_args_list))


if __name__ == "__main__":
    unittest.main()
