#!/usr/bin/env python3
"""Persistent Chromium relay for selected Grok Web requests.

The relay is intentionally narrow: it accepts an explicit endpoint allowlist, keeps
Cloudflare and Grok browser state in Chromium, and serializes requests so SSO
cookies are never shared concurrently between accounts.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import logging
import os
import signal
import sys
import threading
import time
import uuid
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, quote, urlparse, urlunparse

MAX_REQUEST_BYTES = 2 << 20
MAX_RESPONSE_BYTES = 20 << 20
ALLOWED_CF_COOKIES = {"cf_clearance", "__cf_bm", "_cfuvid"}
CHALLENGE_MARKERS = (
    "just a moment",
    "challenge-platform",
    "cf-browser-verification",
    "checking your browser",
    "enable javascript and cookies",
)


def session_fingerprint(value: dict) -> str:
    """Restart Chromium when proxy, UA, or Cloudflare state changes."""
    proxy_url = str(value.get("proxyURL", "")).strip()
    user_agent = str(value.get("userAgent", "")).strip()
    cookies = parse_cookie_header(str(value.get("cloudflareCookies", "")))
    cookie_state = json.dumps(cookies, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256((proxy_url + "\0" + user_agent + "\0" + cookie_state).encode()).hexdigest()


def classify_worker_error(error: Exception) -> str:
    message = str(error).lower()
    if "err_proxy_connection_failed" in message or "proxy connection" in message:
        return "proxy_unavailable"
    if "cloudflare challenge" in message or any(marker in message for marker in CHALLENGE_MARKERS):
        return "anti_bot"
    return "browser_unavailable"


def is_antibot_result(result: dict) -> bool:
    if int(result.get("statusCode", 0)) != HTTPStatus.FORBIDDEN:
        return False
    try:
        body = base64.b64decode(str(result.get("bodyBase64", "")), validate=True).decode(errors="replace").lower()
    except Exception:
        return False
    return "request rejected by anti-bot" in body or "cloudflare" in body or any(marker in body for marker in CHALLENGE_MARKERS)


def parse_cookie_header(value: str) -> dict[str, str]:
    result: dict[str, str] = {}
    for part in value.split(";"):
        name, separator, cookie_value = part.strip().partition("=")
        lower = name.strip().lower()
        if not separator or not cookie_value.strip():
            continue
        if lower in ALLOWED_CF_COOKIES or lower.startswith("cf_chl_"):
            result[lower] = cookie_value.strip()
    return result


def validate_request(
    value: object,
    allowed_paths: set[str] | None = None,
    *,
    allow_conversation_responses: bool = False,
) -> dict:
    if not isinstance(value, dict):
        raise ValueError("request must be an object")
    base_url = str(value.get("baseURL", "")).rstrip("/")
    endpoint = str(value.get("endpoint", ""))
    parsed_base = urlparse(base_url)
    parsed_endpoint = urlparse(endpoint)
    if parsed_base.scheme != "https" or parsed_base.hostname != "grok.com":
        raise ValueError("baseURL must be https://grok.com")
    if allowed_paths is None:
        allowed_paths = {"/rest/app-chat/conversations/new"}
    conversation_response = (
        allow_conversation_responses
        and parsed_endpoint.path.startswith("/rest/app-chat/conversations/")
        and parsed_endpoint.path.endswith("/responses")
        and "/" not in parsed_endpoint.path[len("/rest/app-chat/conversations/") : -len("/responses")]
        and len(parsed_endpoint.path) > len("/rest/app-chat/conversations//responses")
    )
    if (
        parsed_endpoint.scheme != "https"
        or parsed_endpoint.hostname != "grok.com"
        or (parsed_endpoint.path not in allowed_paths and not conversation_response)
        or parsed_endpoint.query
        or parsed_endpoint.fragment
    ):
        raise ValueError("endpoint is not allowed")
    if not isinstance(value.get("payload"), dict):
        raise ValueError("payload must be an object")
    if not str(value.get("ssoToken", "")).strip():
        raise ValueError("ssoToken is required")
    timeout = int(value.get("timeoutSeconds", 180))
    if timeout < 5 or timeout > 1800:
        raise ValueError("timeoutSeconds is invalid")
    proxy_url = str(value.get("proxyURL", "")).strip()
    if proxy_url:
        parsed_proxy = urlparse(proxy_url)
        if parsed_proxy.scheme.lower() not in {"http", "https", "socks4", "socks4a", "socks5", "socks5h"} or not parsed_proxy.hostname:
            raise ValueError("proxyURL is invalid")
    value["baseURL"] = base_url
    value["timeoutSeconds"] = timeout
    return value


def translated_proxy_url(proxy_url: str) -> str:
    if not proxy_url:
        return ""
    parsed = urlparse(proxy_url)
    host = parsed.hostname or ""
    # A loopback proxy belongs to the Go host, not to this worker container.
    if host in {"127.0.0.1", "localhost", "::1"}:
        host = os.getenv("GROK_WORKER_LOOPBACK_PROXY_HOST", host).strip() or host
    port = parsed.port or (443 if parsed.scheme == "https" else 80)
    credentials = ""
    if parsed.username is not None:
        credentials = quote(parsed.username, safe="") + ":" + quote(parsed.password or "", safe="") + "@"
    return urlunparse((parsed.scheme, f"{credentials}{host}:{port}", "", "", "", ""))


def proxy_config(proxy_url: str) -> dict | None:
    clean_url = translated_proxy_url(proxy_url)
    if not clean_url:
        return None
    parsed = urlparse(clean_url)
    host = parsed.hostname or ""
    port = parsed.port or (443 if parsed.scheme == "https" else 80)
    browser_url = f"{parsed.scheme}://{host}:{port}"
    if parsed.username is None:
        return {"url": browser_url}
    from urllib.parse import unquote

    return {
        "url": browser_url,
        "username": unquote(parsed.username),
        "password": unquote(parsed.password or ""),
    }


class BrowserSession:
    def __init__(self) -> None:
        self.lock = threading.Lock()
        self.driver = None
        self.fingerprint = ""
        self.account_digest = ""
        self.last_navigation = 0.0
        self.cloudflare_seeded = False
        self.private_chat_enabled = False
        self.composer_conversations: set[str] = set()

    def close(self) -> None:
        with self.lock:
            self._close_unlocked()

    def _close_unlocked(self) -> None:
        if self.driver is not None:
            try:
                self.driver.quit()
            except Exception:
                pass
        self.driver = None
        self.fingerprint = ""
        self.account_digest = ""
        self.last_navigation = 0.0
        self.cloudflare_seeded = False
        self.private_chat_enabled = False
        self.composer_conversations.clear()

    def _ensure_driver(self, value: dict):
        proxy_url = str(value.get("proxyURL", "")).strip()
        user_agent = str(value.get("userAgent", "")).strip()
        fingerprint = session_fingerprint(value)
        if self.driver is not None and fingerprint == self.fingerprint:
            return self.driver
        self._close_unlocked()
        if "/app" not in sys.path:
            sys.path.insert(0, "/app")
        import utils  # FlareSolverr image runtime

        utils.USER_AGENT = user_agent or None
        self.driver = utils.get_webdriver(proxy_config(proxy_url))
        self.driver.set_page_load_timeout(90)
        self.fingerprint = fingerprint
        logging.info("browser session started (proxy=%s)", bool(proxy_url))
        return self.driver

    @staticmethod
    def _set_cookie(driver, name: str, value: str, *, http_only: bool = False) -> None:
        driver.execute_cdp_cmd(
            "Network.setCookie",
            {
                "name": name,
                "value": value,
                "domain": ".grok.com",
                "path": "/",
                "secure": True,
                "httpOnly": http_only,
                "sameSite": "None" if name.startswith("cf_") or name == "__cf_bm" else "Lax",
            },
        )

    @staticmethod
    def _delete_cookie(driver, name: str) -> None:
        for domain in (".grok.com", "grok.com"):
            try:
                driver.execute_cdp_cmd("Network.deleteCookies", {"name": name, "domain": domain, "path": "/"})
            except Exception:
                pass

    def _prepare_page(self, driver, value: dict) -> None:
        driver.execute_cdp_cmd("Network.enable", {})
        if not self.cloudflare_seeded:
            for name, cookie_value in parse_cookie_header(str(value.get("cloudflareCookies", ""))).items():
                self._set_cookie(driver, name, cookie_value, http_only=True)
            self.cloudflare_seeded = True

        token = str(value["ssoToken"]).strip()
        digest = hashlib.sha256(token.encode()).hexdigest()
        account_changed = digest != self.account_digest
        if account_changed:
            for name in ("sso", "sso-rw", "x-userid"):
                self._delete_cookie(driver, name)
            self._set_cookie(driver, "sso", token, http_only=True)
            self._set_cookie(driver, "sso-rw", token, http_only=True)
            self.account_digest = digest
            self.composer_conversations.clear()

        should_navigate = account_changed or not str(driver.current_url).startswith(value["baseURL"]) or time.monotonic() - self.last_navigation > 600
        if should_navigate:
            driver.get(value["baseURL"] + "/")
            self.last_navigation = time.monotonic()
            self._wait_for_challenge(driver, 45)
            self._dismiss_tos_gate(driver)

    @staticmethod
    def _dismiss_tos_gate(driver) -> None:
        """Acknowledge Grok's per-account TOS gate before API work starts.

        New SSO sessions are redirected to /tos-gate.  Leaving that page in
        place makes otherwise valid signed API calls fail as an unauthenticated
        application session.  The button only acknowledges the already-visible
        gate; it does not accept any other dialog or change account settings.
        """
        from selenium.webdriver.common.by import By

        # Grok may redirect to /tos-gate a moment *after* driver.get('/') has
        # returned, while the React shell hydrates.  Observe that hand-off
        # before deciding there is no gate.
        observe_until = time.monotonic() + 5
        deadline = time.monotonic() + 20
        clicked = False
        while True:
            on_gate = "/tos-gate" in str(driver.current_url or "")
            if not on_gate:
                if clicked:
                    return
                if time.monotonic() >= observe_until:
                    return
                time.sleep(0.2)
                continue
            buttons = driver.find_elements(
                By.XPATH,
                "//button[normalize-space() = 'Got it' or normalize-space() = 'GOT IT']",
            )
            for button in buttons:
                try:
                    if not button.is_displayed() or not button.is_enabled():
                        continue
                    # Grok binds the acknowledgement through React's native
                    # pointer/click path.  A JavaScript .click() is visible in
                    # the DOM but does not always commit the gate state.
                    button.click()
                    clicked = True
                    logging.info("acknowledged Grok TOS gate")
                    break
                except Exception:
                    continue
            if not on_gate:
                return
            if time.monotonic() >= deadline:
                state = "after acknowledgement" if clicked else "without acknowledgement control"
                raise RuntimeError(f"Grok TOS gate did not clear {state}")
            time.sleep(0.25)

    @staticmethod
    def _challenge_visible(driver) -> bool:
        title = str(driver.title or "").lower()
        source = str(driver.page_source or "")[:500_000].lower()
        return any(marker in title or marker in source for marker in CHALLENGE_MARKERS)

    def _wait_for_challenge(self, driver, timeout: int) -> None:
        deadline = time.monotonic() + timeout
        clicked_at = 0.0
        while self._challenge_visible(driver):
            if time.monotonic() >= deadline:
                raise RuntimeError("Cloudflare challenge did not clear in Chromium")
            if time.monotonic() - clicked_at >= 6:
                try:
                    from flaresolverr_service import click_verify

                    click_verify(driver)
                except Exception:
                    pass
                clicked_at = time.monotonic()
            time.sleep(1)

    @staticmethod
    def _fetch(driver, value: dict) -> dict:
        driver.set_script_timeout(value["timeoutSeconds"] + 15)
        result = driver.execute_async_script(
            """
            const endpoint = arguments[0];
            const payload = arguments[1];
            const requestId = arguments[2];
            const done = arguments[arguments.length - 1];
            // Use Grok's current page-level fetch wrapper. It generates a
            // body-bound x-statsig-id for this exact request in the real DOM.
            fetch(endpoint, {
              method: 'POST',
              credentials: 'include',
              cache: 'no-store',
              headers: {
                'Accept': '*/*',
                'Content-Type': 'application/json',
                'x-xai-request-id': requestId
              },
              body: JSON.stringify(payload)
            }).then(async (response) => {
              const text = await response.text();
              const headers = {};
              for (const name of ['content-type', 'cf-ray', 'retry-after']) {
                const item = response.headers.get(name);
                if (item) headers[name] = item;
              }
              done({statusCode: response.status, status: `${response.status} ${response.statusText}`, headers, text});
            }).catch((error) => done({error: String(error && error.message || error)}));
            """,
            value["endpoint"],
            value["payload"],
            str(value.get("requestID", "")),
        )
        if not isinstance(result, dict):
            raise RuntimeError("Chromium fetch returned an invalid result")
        if result.get("error"):
            raise RuntimeError("Chromium fetch failed: " + str(result["error"]))
        text = str(result.pop("text", ""))
        raw = text.encode()
        if len(raw) > MAX_RESPONSE_BYTES:
            raise RuntimeError("Grok response exceeds 20 MiB")
        result["bodyBase64"] = base64.b64encode(raw).decode()
        return result

    @staticmethod
    def _first_visible(driver, selector: str):
        from selenium.webdriver.common.by import By

        for value in driver.find_elements(By.CSS_SELECTOR, selector):
            try:
                if value.is_displayed():
                    return value
            except Exception:
                continue
        return None

    def _start_composer_chat(self, driver, value: dict) -> None:
        """Open the right page for a native Grok composer send."""
        endpoint = urlparse(str(value["endpoint"]))
        parts = [part for part in endpoint.path.split("/") if part]
        # /rest/app-chat/conversations/new starts a clean, temporary chat.
        if parts[-1] == "new":
            new_chat = self._first_visible(
                driver,
                "a[data-testid='new-chat'], a[href='/'], a[href='/chat'], "
                "[aria-label*='New chat' i], [aria-label*='New conversation' i]",
            )
            if new_chat is not None:
                new_chat.click()
                time.sleep(0.6)
            return
        # A response resource names the upstream conversation directly.  Grok's
        # normal conversation route preserves the visible context before typing.
        if len(parts) >= 2 and parts[-1] == "responses":
            conversation_id = parts[-2]
            # Temporary/private chats intentionally keep the browser on '/'
            # and do not expose a Grok conversation route. Keep their current
            # UI context instead of navigating to a made-up upstream path.
            if conversation_id in self.composer_conversations:
                return
            response_id = str(value.get("payload", {}).get("responseId", "")).strip()
            target = str(value["baseURL"]).rstrip("/") + "/c/" + quote(conversation_id, safe="")
            if response_id:
                target += "?rid=" + quote(response_id, safe="")
            if str(driver.current_url or "") != target:
                driver.get(target)
                self._wait_for_challenge(driver, 45)
                self._dismiss_tos_gate(driver)
                time.sleep(0.6)

    def _select_composer_model(self, driver, mode: str) -> None:
        mode = str(mode or "").strip().lower()
        if mode not in {"fast", "auto", "expert", "heavy"}:
            return
        selector = self._first_visible(driver, "[aria-label='Model select'], [data-testid*='model' i]")
        if selector is None:
            return
        try:
            if str(selector.text or "").strip().split("\n", 1)[0].strip().lower() == mode:
                return
            selector.click()
            time.sleep(0.35)
            from selenium.webdriver.common.by import By

            for item in driver.find_elements(By.CSS_SELECTOR, "[role='menuitem'], [role='menuitemradio'], [role='option'], button, a, li, div, span"):
                try:
                    if not item.is_displayed():
                        continue
                    text = str(item.text or "").strip()
                    if len(text) > 80 or text.split("\n", 1)[0].strip().lower() != mode:
                        continue
                    item.click()
                    time.sleep(0.35)
                    return
                except Exception:
                    continue
            # Close a menu when this account cannot select the requested tier.
            selector.click()
        except Exception:
            return

    def _enable_private_composer_chat(self, driver) -> None:
        if self.private_chat_enabled:
            return
        control = self._first_visible(driver, "[aria-label*='Switch to Private Chat' i], [aria-label*='Private Chat' i]")
        if control is None:
            return
        try:
            control.click()
            self.private_chat_enabled = True
            time.sleep(0.25)
        except Exception:
            return

    @staticmethod
    def _synthetic_composer_response(conversation_id: str, parent_id: str, message: str) -> bytes:
        """Adapt an already-completed native composer turn to Grok's NDJSON shape.

        The Go adapter consumes the upstream response as a sequence of JSON
        objects.  Keeping that contract means the existing OpenAI conversion and
        response-state persistence continue to work without exposing browser DOM
        details to callers.
        """
        frames = [
            {"result": {"conversation": {"conversationId": conversation_id}}},
            {"result": {"response": {
                "userResponse": {"responseId": parent_id},
                "token": message,
                "isThinking": False,
                "messageTag": "final",
            }}},
        ]
        return ("\n".join(json.dumps(frame, ensure_ascii=False, separators=(",", ":")) for frame in frames) + "\n").encode()

    def _composer_fetch(self, driver, value: dict) -> dict:
        """Send the prompt through Grok's own UI and return its response stream.

        Grok's x-statsig-id is tied to the exact request body.  Calling fetch
        from Selenium creates an unsigned body and receives Code 7, whereas a
        real edit + submit lets Grok's application generate the body and signer.
        """
        from selenium.webdriver.common.by import By

        payload = value["payload"]
        message = str(payload.get("message", "")).strip()
        attachments = payload.get("fileAttachments") or payload.get("imageAttachments")
        if not message:
            raise RuntimeError("Grok composer request is missing a message")
        if attachments:
            raise RuntimeError("Grok composer worker does not support attachments")
        self._start_composer_chat(driver, value)
        self._enable_private_composer_chat(driver)
        self._select_composer_model(driver, str(payload.get("modeId", "")))

        deadline = time.monotonic() + min(int(value["timeoutSeconds"]), 180)
        input_box = None
        while input_box is None and time.monotonic() < deadline:
            input_box = self._first_visible(driver, "[aria-label='Ask Grok anything'][contenteditable='true'], textarea[placeholder], textarea, div[contenteditable='true'], [role='textbox']")
            if input_box is None:
                time.sleep(0.2)
        if input_box is None:
            raise RuntimeError("Grok composer input was not found")
        before_count = len(driver.find_elements(By.CSS_SELECTOR, "[data-testid='assistant-message']"))
        try:
            input_box.clear()
        except Exception:
            driver.execute_script("arguments[0].textContent = ''; arguments[0].dispatchEvent(new Event('input', {bubbles:true}))", input_box)
        # React owns the composer state.  Selenium send_keys updates the DOM
        # cursor but can leave that state empty in headless Chromium, so Grok
        # keeps chat-submit disabled.  This is the same native InputEvent path
        # used by the app's editable composer.
        driver.execute_script(
            """
            const input = arguments[0];
            const value = arguments[1];
            input.focus();
            input.textContent = value;
            try {
              input.dispatchEvent(new InputEvent('input', {
                bubbles: true, data: value, inputType: 'insertText'
              }));
            } catch (_) {
              input.dispatchEvent(new Event('input', {bubbles: true}));
            }
            input.dispatchEvent(new Event('change', {bubbles: true}));
            """,
            input_box,
            message,
        )

        submit = None
        while submit is None and time.monotonic() < deadline:
            for candidate in driver.find_elements(By.CSS_SELECTOR, "button[data-testid='chat-submit'], button[type='submit'], button[aria-label*='Send' i], button[aria-label*='Submit' i]"):
                try:
                    if candidate.is_displayed() and candidate.is_enabled():
                        submit = candidate
                        break
                except Exception:
                    continue
            if submit is None:
                time.sleep(0.15)
        if submit is None:
            raise RuntimeError("Grok composer submit control was not available")
        submit.click()

        latest = None
        latest_text = ""
        stable_since = 0.0
        while time.monotonic() < deadline:
            messages = driver.find_elements(By.CSS_SELECTOR, "[data-testid='assistant-message']")
            if len(messages) > before_count:
                candidate = messages[-1]
                try:
                    text = str(candidate.text or "").strip()
                except Exception:
                    text = ""
                if text:
                    if text == latest_text:
                        if stable_since == 0.0:
                            stable_since = time.monotonic()
                        if time.monotonic() - stable_since >= 1.0:
                            latest = candidate
                            break
                    else:
                        latest_text = text
                        stable_since = time.monotonic()
            time.sleep(0.25)
        if latest is None or not latest_text:
            raise RuntimeError("Grok composer did not return an assistant response")

        parsed = urlparse(str(driver.current_url or ""))
        path_parts = [part for part in parsed.path.split("/") if part]
        conversation_id = path_parts[-1] if len(path_parts) >= 2 and path_parts[-2] == "c" else ""
        parent_id = (parse_qs(parsed.query).get("rid") or [""])[0]
        if not conversation_id:
            conversation_id = "composer_" + uuid.uuid4().hex
            self.composer_conversations.add(conversation_id)
        if not parent_id:
            parent_id = "resp_" + uuid.uuid4().hex
        body = self._synthetic_composer_response(conversation_id, parent_id, latest_text)
        return {
            "statusCode": HTTPStatus.OK,
            "status": "200 OK",
            "headers": {"content-type": "application/x-ndjson"},
            "bodyBase64": base64.b64encode(body).decode(),
        }

    def request(self, value: dict) -> dict:
        with self.lock:
            driver = self._ensure_driver(value)
            try:
                self._prepare_page(driver, value)
                result = self._composer_fetch(driver, value) if value.get("useComposer") else self._fetch(driver, value)
                if is_antibot_result(result):
                    driver.get(value["baseURL"] + "/")
                    self.last_navigation = time.monotonic()
                    self._wait_for_challenge(driver, 45)
                    result = self._fetch(driver, value)
                state = self._cloudflare_state(driver)
                result.update(state)
                self._remember_cloudflare_state(value, state)
                return result
            except Exception:
                logging.exception(
                    "browser request failed (trace_id=%s upstream_request_id=%s)",
                    value.get("traceID", ""),
                    value.get("requestID", ""),
                )
                self._close_unlocked()
                raise

    @staticmethod
    def _cloudflare_state(driver) -> dict[str, str]:
        cookies: dict[str, str] = {}
        try:
            for cookie in driver.get_cookies() or []:
                name = str(cookie.get("name", "")).strip().lower()
                value = str(cookie.get("value", "")).strip()
                if not value or not (name in ALLOWED_CF_COOKIES or name.startswith("cf_chl_")):
                    continue
                cookies[name] = value
        except Exception:
            cookies = {}
        try:
            user_agent = str(driver.execute_script("return navigator.userAgent") or "").strip()
        except Exception:
            user_agent = ""
        return {
            "cloudflareCookies": "; ".join(f"{name}={cookies[name]}" for name in sorted(cookies)),
            "userAgent": user_agent,
        }

    def _remember_cloudflare_state(self, value: dict, state: dict[str, str]) -> None:
        current = dict(value)
        current["cloudflareCookies"] = state.get("cloudflareCookies", "")
        current["userAgent"] = state.get("userAgent", "")
        self.fingerprint = session_fingerprint(current)

    def warm(self, value: dict) -> dict[str, str]:
        with self.lock:
            driver = self._ensure_driver(value)
            try:
                self._prepare_page(driver, value)
                self._probe_signed_fetch(driver)
                state = self._cloudflare_state(driver)
                self._remember_cloudflare_state(value, state)
                return state
            except Exception:
                logging.exception("browser warmup failed")
                self._close_unlocked()
                raise

    @staticmethod
    def _probe_signed_fetch(driver) -> None:
        driver.set_script_timeout(30)
        result = driver.execute_async_script(
            """
            const done = arguments[arguments.length - 1];
            fetch('/rest/app-chat/conversations?pageSize=1', {
              method: 'GET',
              credentials: 'include',
              cache: 'no-store',
              headers: {'Accept': '*/*'}
            }).then(async (response) => done({status: response.status, text: await response.text()}))
              .catch((error) => done({error: String(error && error.message || error)}));
            """
        )
        if not isinstance(result, dict):
            raise RuntimeError("Grok page fetch probe returned an invalid result")
        status = int(result.get("status", 0))
        if result.get("error") or not 200 <= status < 300:
            preview = str(result.get("text", result.get("error", "")))[:200].replace("\n", " ")
            raise RuntimeError(f"Grok page signed fetch probe failed (status={status}, body={preview})")


SESSION = BrowserSession()


class Handler(BaseHTTPRequestHandler):
    server_version = "grok-web-browser-worker/1"

    def log_message(self, fmt: str, *args) -> None:
        logging.info("http " + fmt, *args)

    def _json(self, status: int, value: dict) -> None:
        data = json.dumps(value, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self) -> None:  # noqa: N802
        if self.path == "/healthz":
            self._json(HTTPStatus.OK, {"ok": True, "browserReady": SESSION.driver is not None})
            return
        if self.path == "/readyz":
            ready = SESSION.driver is not None
            self._json(HTTPStatus.OK if ready else HTTPStatus.SERVICE_UNAVAILABLE, {"ready": ready})
            return
        if self.path not in {"/healthz", "/readyz"}:
            self._json(HTTPStatus.NOT_FOUND, {"error": "not found"})
            return

    def do_POST(self) -> None:  # noqa: N802
        if self.path not in {"/v1/grok/chat", "/v1/grok/fast-image", "/v1/grok/quota", "/v1/grok/warm"}:
            self._json(HTTPStatus.NOT_FOUND, {"error": "not found"})
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if length <= 0 or length > MAX_REQUEST_BYTES:
                raise ValueError("request body size is invalid")
            allowed_paths = {"/rest/rate-limits"} if self.path == "/v1/grok/quota" else {"/rest/app-chat/conversations/new"}
            value = validate_request(
                json.loads(self.rfile.read(length)),
                allowed_paths,
                allow_conversation_responses=self.path == "/v1/grok/chat",
            )
            value["useComposer"] = self.path == "/v1/grok/chat"
            if self.path == "/v1/grok/warm":
                state = SESSION.warm(value)
                self._json(HTTPStatus.OK, {"ok": True, **state})
                return
            started = time.monotonic()
            result = SESSION.request(value)
            status_code = int(result.get("statusCode", 0))
            if status_code < 200 or status_code >= 300:
                try:
                    preview = base64.b64decode(result.get("bodyBase64", ""), validate=True)[:500].decode(errors="replace")
                except Exception:
                    preview = "<invalid body>"
                logging.warning(
                    "grok fetch rejected (trace_id=%s upstream_request_id=%s status=%s body=%s)",
                    value.get("traceID", ""),
                    value.get("requestID", ""),
                    status_code,
                    preview,
                )
            logging.info(
                "grok fetch completed (trace_id=%s upstream_request_id=%s status=%s bytes=%s duration_ms=%s)",
                value.get("traceID", ""),
                value.get("requestID", ""),
                status_code,
                len(result.get("bodyBase64", "")) * 3 // 4,
                int((time.monotonic() - started) * 1000),
            )
            self._json(HTTPStatus.OK, result)
        except ValueError as exc:
            self._json(HTTPStatus.BAD_REQUEST, {"error": str(exc), "code": "invalid_request"})
        except Exception as exc:
            self._json(
                HTTPStatus.BAD_GATEWAY,
                {"error": str(exc)[:500], "code": classify_worker_error(exc)},
            )


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--listen", default="127.0.0.1:8192")
    args = parser.parse_args()
    host, separator, port_text = args.listen.rpartition(":")
    if not separator:
        raise SystemExit("--listen must be HOST:PORT")
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    server = ThreadingHTTPServer((host, int(port_text)), Handler)

    def shutdown(_signum, _frame) -> None:
        threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    try:
        logging.info("worker listening on %s", args.listen)
        server.serve_forever()
    finally:
        SESSION.close()
        server.server_close()


if __name__ == "__main__":
    main()
