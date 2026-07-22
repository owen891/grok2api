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
# Accept usable low-resolution images, but never treat Grok's blurred
# subscription preview as a completed image.
MIN_USABLE_IMAGE_DIMENSION = 256
MIN_FINAL_IMAGE_DIMENSION = 768
TRUSTED_IMAGE_ASSET_HOSTS = {"assets.grok.com", "imagine-public.x.ai", "imgen.x.ai"}
ALLOWED_CF_COOKIES = {"cf_clearance", "__cf_bm", "_cfuvid"}
CHALLENGE_MARKERS = (
    "just a moment",
    "challenge-platform",
    "cf-browser-verification",
    "checking your browser",
    "enable javascript and cookies",
)
DRIVER_INITIALIZATION_LOCK = threading.Lock()


class WorkerBusyError(TimeoutError):
    """The caller's queue budget expired before Chromium became available."""


class ImageGenerationIncompleteError(RuntimeError):
    """Grok completed the UI flow without exposing a usable image asset."""


class ImageSubscriptionRequiredError(RuntimeError):
    """The selected Grok account cannot use the Imagine generation UI."""


def session_fingerprint(value: dict) -> str:
    """Restart Chromium when proxy, UA, or Cloudflare state changes."""
    proxy_url = str(value.get("proxyURL", "")).strip()
    user_agent = str(value.get("userAgent", "")).strip()
    cookies = parse_cookie_header(str(value.get("cloudflareCookies", "")))
    cookie_state = json.dumps(cookies, sort_keys=True, separators=(",", ":"))
    return hashlib.sha256((proxy_url + "\0" + user_agent + "\0" + cookie_state).encode()).hexdigest()


def classify_worker_error(error: Exception) -> str:
    if isinstance(error, ImageSubscriptionRequiredError):
        return "image_subscription_required"
    if isinstance(error, ImageGenerationIncompleteError):
        return "image_generation_incomplete"
    if isinstance(error, WorkerBusyError):
        return "worker_busy"
    message = str(error).lower()
    if any(
        marker in message
        for marker in (
            "err_proxy_connection_failed",
            "err_tunnel_connection_failed",
            "err_connection_closed",
            "err_connection_reset",
            "proxy connection",
        )
    ):
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


def is_generated_image_source(source: str) -> bool:
    value = str(source or "").strip()
    if value.startswith("blob:") or value.startswith("data:image/"):
        return True
    parsed = urlparse(value)
    return parsed.scheme == "https" and (parsed.hostname or "").lower() in TRUSTED_IMAGE_ASSET_HOSTS


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
    try:
        parsed_base = urlparse(base_url)
        parsed_endpoint = urlparse(endpoint)
    except ValueError as exc:
        raise ValueError("baseURL or endpoint is invalid") from exc
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
    try:
        timeout = int(value.get("timeoutSeconds", 180))
    except (TypeError, ValueError, OverflowError) as exc:
        raise ValueError("timeoutSeconds is invalid") from exc
    if timeout < 5 or timeout > 1800:
        raise ValueError("timeoutSeconds is invalid")
    proxy_url = str(value.get("proxyURL", "")).strip()
    if proxy_url:
        try:
            parsed_proxy = urlparse(proxy_url)
            proxy_port = parsed_proxy.port
        except ValueError as exc:
            raise ValueError("proxyURL is invalid") from exc
        if (
            parsed_proxy.scheme.lower()
            not in {"http", "https", "socks4", "socks4a", "socks5", "socks5h"}
            or not parsed_proxy.hostname
            or proxy_port is not None
            and not 1 <= proxy_port <= 65535
        ):
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

    def _acquire(self, value: dict) -> float:
        try:
            timeout = float(value.get("timeoutSeconds", 180))
        except (TypeError, ValueError):
            timeout = 180.0
        deadline = time.monotonic() + max(0.0, timeout)
        remaining = deadline - time.monotonic()
        if remaining <= 0 or not self.lock.acquire(timeout=remaining):
            raise WorkerBusyError("browser worker queue wait timed out")
        if time.monotonic() >= deadline:
            self.lock.release()
            raise WorkerBusyError("browser worker request budget expired while waiting for Chromium")
        return deadline

    @staticmethod
    def _request_deadline(value: dict) -> float:
        deadline = value.get("_deadline")
        if isinstance(deadline, (int, float)):
            return float(deadline)
        try:
            timeout = float(value.get("timeoutSeconds", 180))
        except (TypeError, ValueError):
            timeout = 180.0
        return time.monotonic() + max(0.0, timeout)

    @staticmethod
    def _remaining_seconds(value: dict, maximum: float | None = None) -> float:
        remaining = BrowserSession._request_deadline(value) - time.monotonic()
        if remaining <= 0.05:
            raise WorkerBusyError("browser worker request budget expired")
        if maximum is not None:
            remaining = min(remaining, float(maximum))
        return remaining

    @staticmethod
    def _sleep_with_deadline(seconds: float, deadline: float | None = None) -> None:
        if deadline is None:
            time.sleep(seconds)
            return
        remaining = deadline - time.monotonic()
        if remaining <= 0.05:
            raise WorkerBusyError("browser worker request budget expired")
        time.sleep(min(seconds, remaining))
        if time.monotonic() >= deadline:
            raise WorkerBusyError("browser worker request budget expired")

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

        driver = None
        try:
            # FlareSolverr stores the requested UA in a process-global. Keep
            # the two independent sessions from observing each other's value.
            with DRIVER_INITIALIZATION_LOCK:
                previous_user_agent = utils.USER_AGENT
                try:
                    utils.USER_AGENT = user_agent or None
                    driver = utils.get_webdriver(proxy_config(proxy_url))
                finally:
                    utils.USER_AGENT = previous_user_agent
            driver.set_page_load_timeout(self._remaining_seconds(value, 90))
        except Exception:
            if driver is not None:
                try:
                    driver.quit()
                except Exception:
                    pass
            raise
        self.driver = driver
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
        deadline = self._request_deadline(value)
        driver.set_page_load_timeout(self._remaining_seconds(value, 90))
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
            self._remaining_seconds(value)
            driver.set_page_load_timeout(self._remaining_seconds(value, 90))
            driver.get(value["baseURL"] + "/")
            self.last_navigation = time.monotonic()
            self._wait_for_challenge(driver, 45, deadline)
            self._dismiss_tos_gate(driver, deadline)

    @staticmethod
    def _dismiss_tos_gate(driver, deadline: float | None = None) -> None:
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
        gate_deadline = time.monotonic() + 20
        if deadline is not None:
            observe_until = min(observe_until, deadline)
            gate_deadline = min(gate_deadline, deadline)
        clicked = False
        while True:
            if deadline is not None and time.monotonic() >= deadline:
                raise WorkerBusyError("browser worker request budget expired")
            on_gate = "/tos-gate" in str(driver.current_url or "")
            if not on_gate:
                if clicked:
                    return
                if time.monotonic() >= observe_until:
                    return
                BrowserSession._sleep_with_deadline(0.2, deadline)
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
            if time.monotonic() >= gate_deadline:
                state = "after acknowledgement" if clicked else "without acknowledgement control"
                raise RuntimeError(f"Grok TOS gate did not clear {state}")
            BrowserSession._sleep_with_deadline(0.25, deadline)

    @staticmethod
    def _challenge_visible(driver) -> bool:
        title = str(driver.title or "").lower()
        source = str(driver.page_source or "")[:500_000].lower()
        return any(marker in title or marker in source for marker in CHALLENGE_MARKERS)

    def _wait_for_challenge(self, driver, timeout: int, request_deadline: float | None = None) -> None:
        deadline = time.monotonic() + timeout
        if request_deadline is not None:
            deadline = min(deadline, request_deadline)
        clicked_at = 0.0
        while self._challenge_visible(driver):
            if time.monotonic() >= deadline:
                if request_deadline is not None and time.monotonic() >= request_deadline:
                    raise WorkerBusyError("browser worker request budget expired")
                raise RuntimeError("Cloudflare challenge did not clear in Chromium")
            if time.monotonic() - clicked_at >= 6:
                try:
                    from flaresolverr_service import click_verify

                    click_verify(driver)
                except Exception:
                    pass
                clicked_at = time.monotonic()
            self._sleep_with_deadline(
                min(1.0, max(0.0, deadline - time.monotonic())),
                request_deadline,
            )

    @staticmethod
    def _fetch(driver, value: dict) -> dict:
        driver.set_script_timeout(BrowserSession._remaining_seconds(value))
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
        deadline = self._request_deadline(value)
        endpoint = urlparse(str(value["endpoint"]))
        parts = [part for part in endpoint.path.split("/") if part]
        # /rest/app-chat/conversations/new starts a clean, temporary chat.
        if parts[-1] == "new":
            # The shell may still have a full-page hydration layer over the
            # Home/New Chat link. Selenium's native click then raises
            # ElementClickInterceptedException and blocks the whole worker
            # request until the Go-side probe timeout. Navigating to the
            # canonical root is equivalent for a new temporary conversation
            # and lets the page finish its own routing without a fragile DOM
            # click.
            root = str(value["baseURL"]).rstrip("/") + "/"
            if str(driver.current_url or "") != root:
                driver.set_page_load_timeout(self._remaining_seconds(value, 90))
                driver.get(root)
                self._wait_for_challenge(driver, 45, deadline)
            self._dismiss_tos_gate(driver, deadline)
            self._sleep_with_deadline(0.6, deadline)
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
                driver.set_page_load_timeout(self._remaining_seconds(value, 90))
                driver.get(target)
                self._wait_for_challenge(driver, 45, deadline)
                self._dismiss_tos_gate(driver, deadline)
                self._sleep_with_deadline(0.6, deadline)

    def _select_composer_model(self, driver, mode: str, deadline: float | None = None) -> None:
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
            self._sleep_with_deadline(0.35, deadline)
            from selenium.webdriver.common.by import By

            for item in driver.find_elements(By.CSS_SELECTOR, "[role='menuitem'], [role='menuitemradio'], [role='option'], button, a, li, div, span"):
                try:
                    if not item.is_displayed():
                        continue
                    text = str(item.text or "").strip()
                    if len(text) > 80 or text.split("\n", 1)[0].strip().lower() != mode:
                        continue
                    item.click()
                    self._sleep_with_deadline(0.35, deadline)
                    return
                except WorkerBusyError:
                    raise
                except Exception:
                    continue
            # Close a menu when this account cannot select the requested tier.
            selector.click()
        except WorkerBusyError:
            raise
        except Exception:
            return

    def _enable_private_composer_chat(self, driver, deadline: float | None = None) -> None:
        if self.private_chat_enabled:
            return
        control = self._first_visible(driver, "[aria-label*='Switch to Private Chat' i], [aria-label*='Private Chat' i]")
        if control is None:
            return
        try:
            control.click()
            self.private_chat_enabled = True
            self._sleep_with_deadline(0.25, deadline)
        except WorkerBusyError:
            raise
        except Exception:
            return

    @staticmethod
    def _click_composer_submit(driver, deadline: float) -> None:
        """Click the composer submit control across hydration/overlay races."""
        selector = "button[data-testid='chat-submit'], button[type='submit'], button[aria-label*='Send' i], button[aria-label*='Submit' i]"
        last_intercepted = None
        js_fallback_at = time.monotonic() + 0.75
        while time.monotonic() < deadline:
            for candidate in driver.find_elements("css selector", selector):
                try:
                    if not candidate.is_displayed() or not candidate.is_enabled():
                        continue
                    driver.execute_script(
                        "arguments[0].scrollIntoView({block:'center', inline:'center'});",
                        candidate,
                    )
                    unobstructed = driver.execute_script(
                        """
                        const el = arguments[0];
                        const rect = el.getBoundingClientRect();
                        if (!rect.width || !rect.height) return false;
                        const top = document.elementFromPoint(
                          rect.left + rect.width / 2,
                          rect.top + rect.height / 2
                        );
                        return top === el || el.contains(top);
                        """,
                        candidate,
                    )
                    if unobstructed:
                        try:
                            candidate.click()
                            return
                        except Exception as error:
                            # Chromium can report a transient not-interactable
                            # state while the React shell finishes hydration.
                            last_intercepted = error
                    if time.monotonic() >= js_fallback_at:
                        driver.execute_script("arguments[0].click();", candidate)
                        return
                except Exception as error:
                    if "stale" in error.__class__.__name__.lower():
                        continue
                    last_intercepted = error
                    continue
            BrowserSession._sleep_with_deadline(0.1, deadline)
        BrowserSession._assert_deadline(deadline)
        if last_intercepted is not None:
            raise RuntimeError("Grok composer submit control remained blocked") from last_intercepted
        raise RuntimeError("Grok composer submit control was not clickable")

    @staticmethod
    def _assert_deadline(deadline: float | None) -> None:
        if deadline is not None and time.monotonic() >= deadline:
            raise WorkerBusyError("browser worker request budget expired")

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

    @staticmethod
    def _synthetic_image_response(conversation_id: str, image_url: str) -> bytes:
        frames = [
            {"result": {"conversation": {"conversationId": conversation_id}}},
            {"result": {"response": {
                "streamingImageGenerationResponse": {
                    "imageUrl": image_url,
                    "progress": 100,
                    "isFinal": True,
                },
                "messageTag": "final",
            }}},
        ]
        body = ("\n".join(json.dumps(frame, ensure_ascii=False, separators=(",", ":")) for frame in frames) + "\n").encode()
        if len(body) > MAX_RESPONSE_BYTES:
            raise RuntimeError("Generated image response exceeds 20 MiB")
        return body

    @staticmethod
    def _image_sources(driver) -> list[dict]:
        values = driver.execute_script(
            """
            const resolveAssetURL = (value) => {
              const source = String(value || '').trim();
              if (!source) return '';
              const trustedHosts = new Set(['assets.grok.com', 'imagine-public.x.ai', 'imgen.x.ai']);
              if (source.startsWith('blob:') || source.startsWith('data:image/')) return source;
              try {
                const parsed = new URL(source, window.location.href);
                const original = parsed.searchParams.get('url');
                if (original) {
                  const originalURL = new URL(original, window.location.href);
                  if (originalURL.protocol === 'https:' && trustedHosts.has(originalURL.hostname.toLowerCase())) {
                    return originalURL.href;
                  }
                }
                if (parsed.protocol === 'https:' && trustedHosts.has(parsed.hostname.toLowerCase())) {
                  return parsed.href;
                }
              } catch (_) {}
              return '';
            };
            const bestSource = (image) => {
              const anchor = image.closest('a[href]');
              const candidates = [
                anchor && anchor.href,
                image.getAttribute('data-original'),
                image.getAttribute('data-full-src'),
                image.getAttribute('data-image-url'),
                image.getAttribute('data-src'),
                image.currentSrc,
                image.src
              ];
              const srcset = String(image.srcset || '').split(',').map((entry) => entry.trim().split(/\\s+/, 1)[0]);
              candidates.push(...srcset.reverse());
              for (const attribute of Array.from(image.attributes)) {
                if (/(src|url|image)/i.test(attribute.name)) candidates.push(attribute.value);
              }
              for (const candidate of candidates) {
                const resolved = resolveAssetURL(candidate);
                if (resolved) return resolved;
              }
              return '';
            };
            const selector = [
              'img[alt="Generated image"]',
              'img[src*="imagine-public"]',
              'img[src*="assets.grok.com"]',
              'img[src*="imgen.x.ai"]',
              '[data-testid*="generated"] img',
              '[data-testid*="image"] img'
            ].join(',');
            const images = Array.from(document.querySelectorAll(selector)).map((image) => {
              return {
                src: bestSource(image),
                complete: Boolean(image.complete),
                width: Number(image.naturalWidth || 0),
                height: Number(image.naturalHeight || 0)
              };
            });
            const known = new Set(images.map((item) => item.src).filter(Boolean));
            for (const entry of performance.getEntriesByType('resource')) {
              const source = resolveAssetURL(entry.name);
              if (!source || known.has(source) || !/(\\/generated\\/|\\/imagine-public\\/images\\/)/i.test(source)) continue;
              known.add(source);
              images.push({src: source, complete: true, width: 0, height: 0, resource: true});
            }
            return images;
            """
        )
        return values if isinstance(values, list) else []

    @staticmethod
    def _new_generated_image(
        images: list[dict], before_sources: set[str], before_count: int
    ) -> tuple[str, bool]:
        """Return the latest loaded generated image and whether it is full-size."""
        for index in range(len(images) - 1, -1, -1):
            item = images[index]
            if not isinstance(item, dict):
                continue
            source = str(item.get("src", "")).strip()
            try:
                width = int(item.get("width", 0))
                height = int(item.get("height", 0))
            except (TypeError, ValueError):
                continue
            if (
                not source
                or not is_generated_image_source(source)
                or not item.get("complete")
                or min(width, height) < MIN_USABLE_IMAGE_DIMENSION
                or (index < before_count and source in before_sources)
            ):
                continue
            is_final = min(width, height) >= MIN_FINAL_IMAGE_DIMENSION
            return source, is_final
        return "", False

    @staticmethod
    def _subscription_preview_visible(driver) -> bool:
        current_url = str(driver.current_url or "").lower()
        if "#subscribe" in current_url or "/subscribe" in current_url:
            return True
        return bool(
            driver.execute_script(
                r"""
                const selector = [
                  'img[alt="Generated image"]',
                  'img[src*="imagine-public"]',
                  'img[src*="assets.grok.com"]',
                  'img[src*="imgen.x.ai"]',
                  '[data-testid*="generated"] img',
                  '[data-testid*="image"] img'
                ].join(',');
                const subscriptionHref = /(?:#|\/)subscribe(?:$|[/?#])/i;
                const hasBlur = (node) => {
                  const style = getComputedStyle(node);
                  const filter = `${style.filter || ''} ${style.backdropFilter || ''}`;
                  return /blur\((?!0(?:px|rem|em)?\))/i.test(filter);
                };
                for (const image of document.querySelectorAll(selector)) {
                  const link = image.closest('a[href]');
                  if (link && subscriptionHref.test(String(link.getAttribute('href') || ''))) return true;
                  let node = image;
                  for (let depth = 0; node && depth < 6; depth += 1, node = node.parentElement) {
                    if (hasBlur(node)) return true;
                  }
                }
                return false;
                """
            )
        )

    @staticmethod
    def _materialize_image_url(driver, source: str, deadline: float | None = None) -> str:
        source = str(source or "").strip()
        if not is_generated_image_source(source):
            raise RuntimeError("Grok Imagine returned an unsupported image URL")
        if source.startswith("data:image/") or source.startswith("https://"):
            return source
        BrowserSession._assert_deadline(deadline)
        remaining = 30.0 if deadline is None else min(30.0, deadline - time.monotonic())
        if remaining <= 0.05:
            raise WorkerBusyError("browser worker request budget expired")
        driver.set_script_timeout(remaining)
        result = driver.execute_async_script(
            """
            const source = arguments[0];
            const done = arguments[arguments.length - 1];
            fetch(source).then((response) => {
              if (!response.ok) throw new Error(`image fetch returned ${response.status}`);
              return response.blob();
            }).then((blob) => {
              const reader = new FileReader();
              reader.onload = () => done({value: String(reader.result || '')});
              reader.onerror = () => done({error: 'image blob could not be read'});
              reader.readAsDataURL(blob);
            }).catch((error) => done({error: String(error && error.message || error)}));
            """,
            source,
        )
        if not isinstance(result, dict) or result.get("error") or not str(result.get("value", "")).startswith("data:"):
            raise RuntimeError("Grok Imagine image blob could not be materialized")
        return str(result["value"])

    def _select_image_aspect_ratio(self, driver, ratio: str, deadline: float | None = None) -> None:
        control = self._first_visible(driver, "button[aria-label='Aspect Ratio'], [aria-label='Aspect Ratio']")
        if control is None:
            raise RuntimeError("Grok Imagine aspect-ratio control was not found")
        if str(control.text or "").strip().split("\n", 1)[0] == ratio:
            return
        control.click()
        self._sleep_with_deadline(0.35, deadline)
        from selenium.webdriver.common.by import By

        for item in driver.find_elements(
            By.CSS_SELECTOR,
            "[role='menuitem'], [role='menuitemradio'], [role='option'], button, li",
        ):
            try:
                if not item.is_displayed():
                    continue
                text = str(item.text or "").strip().split("\n", 1)[0]
                if text != ratio:
                    continue
                item.click()
                self._sleep_with_deadline(0.35, deadline)
                break
            except WorkerBusyError:
                raise
            except Exception:
                continue
        if str(control.text or "").strip().split("\n", 1)[0] != ratio:
            raise RuntimeError(f"Grok Imagine aspect ratio could not be set to {ratio}")

    def _composer_image_fetch(self, driver, value: dict) -> dict:
        """Generate through Grok's native Imagine UI so the app signs the request."""
        deadline = min(
            self._request_deadline(value),
            time.monotonic() + min(float(value.get("timeoutSeconds", 180)), 1800.0),
        )
        payload = value["payload"]
        message = str(payload.get("message", "")).strip()
        attachments = payload.get("fileAttachments") or payload.get("imageAttachments")
        if not message:
            raise RuntimeError("Grok Imagine request is missing a prompt")
        if attachments:
            raise RuntimeError("Grok Imagine worker does not support attachments")

        target = str(value["baseURL"]).rstrip("/") + "/imagine"
        self._assert_deadline(deadline)
        driver.set_page_load_timeout(min(90.0, deadline - time.monotonic()))
        driver.get(target)
        self.last_navigation = time.monotonic()
        self._wait_for_challenge(driver, 45, deadline)
        self._dismiss_tos_gate(driver, deadline)
        if self._subscription_preview_visible(driver):
            raise ImageSubscriptionRequiredError(
                "Grok Imagine requires an account with image-generation access"
            )

        input_box = None
        while input_box is None and time.monotonic() < deadline:
            input_box = self._first_visible(
                driver,
                "[aria-label='Ask Grok anything'][contenteditable='true'], "
                "textarea[aria-label='Ask Grok anything'], [aria-label='Ask Grok anything'], "
                "textarea[placeholder], div[contenteditable='true'], [role='textbox']",
            )
            if input_box is None:
                self._sleep_with_deadline(0.2, deadline)
        if input_box is None:
            self._assert_deadline(deadline)
            raise RuntimeError("Grok Imagine prompt input was not found")

        self._select_image_aspect_ratio(driver, "1:1", deadline)
        driver.execute_script("if (window.performance && performance.clearResourceTimings) performance.clearResourceTimings();")
        before_images = self._image_sources(driver)
        before_sources = {str(item.get("src", "")) for item in before_images if isinstance(item, dict)}
        before_count = len(before_images)
        try:
            input_box.clear()
        except Exception:
            pass
        driver.execute_script(
            """
            const input = arguments[0];
            const value = arguments[1];
            input.focus();
            if ('value' in input) {
              const setter = Object.getOwnPropertyDescriptor(
                input.tagName === 'TEXTAREA' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype,
                'value'
              );
              if (setter && setter.set) setter.set.call(input, value);
              else input.value = value;
            } else {
              input.textContent = value;
            }
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
        self._click_composer_submit(driver, deadline)

        candidate = ""
        final_resolution = False
        stable_since = 0.0
        while time.monotonic() < deadline:
            if self._subscription_preview_visible(driver):
                raise ImageSubscriptionRequiredError(
                    "Grok Imagine returned a subscription preview instead of a generated image"
                )
            images = self._image_sources(driver)
            selected, selected_is_final = self._new_generated_image(images, before_sources, before_count)
            if selected:
                if selected != candidate:
                    candidate = selected
                    final_resolution = selected_is_final
                    stable_since = time.monotonic()
                elif time.monotonic() - stable_since >= 2.0:
                    break
            self._sleep_with_deadline(0.25, deadline)
        if not candidate:
            raise ImageGenerationIncompleteError("Grok Imagine did not expose a usable generated image")
        if self._subscription_preview_visible(driver):
            raise ImageSubscriptionRequiredError(
                "Grok Imagine returned a subscription preview instead of a generated image"
            )

        image_url = self._materialize_image_url(driver, candidate, deadline)
        logging.info(
            "Grok Imagine image captured (source=%s final_resolution=%s)",
            image_url.split(":", 1)[0],
            final_resolution,
        )
        body = self._synthetic_image_response("composer_image_" + uuid.uuid4().hex, image_url)
        return {
            "statusCode": HTTPStatus.OK,
            "status": "200 OK",
            "headers": {"content-type": "application/x-ndjson"},
            "bodyBase64": base64.b64encode(body).decode(),
        }

    def _composer_fetch(self, driver, value: dict) -> dict:
        """Send the prompt through Grok's own UI and return its response stream.

        Grok's x-statsig-id is tied to the exact request body.  Calling fetch
        from Selenium creates an unsigned body and receives Code 7, whereas a
        real edit + submit lets Grok's application generate the body and signer.
        """
        from selenium.webdriver.common.by import By

        deadline = min(
            self._request_deadline(value),
            time.monotonic() + min(float(value.get("timeoutSeconds", 180)), 180.0),
        )
        payload = value["payload"]
        message = str(payload.get("message", "")).strip()
        attachments = payload.get("fileAttachments") or payload.get("imageAttachments")
        if not message:
            raise RuntimeError("Grok composer request is missing a message")
        if attachments:
            raise RuntimeError("Grok composer worker does not support attachments")
        self._start_composer_chat(driver, value)
        self._enable_private_composer_chat(driver, deadline)
        self._select_composer_model(driver, str(payload.get("modeId", "")), deadline)
        capture_image = bool(value.get("captureImage"))
        before_images = []
        before_sources: set[str] = set()
        before_image_count = 0
        if capture_image:
            driver.execute_script("if (window.performance && performance.clearResourceTimings) performance.clearResourceTimings();")
            before_images = self._image_sources(driver)
            before_sources = {str(item.get("src", "")) for item in before_images if isinstance(item, dict)}
            before_image_count = len(before_images)

        input_box = None
        while input_box is None and time.monotonic() < deadline:
            input_box = self._first_visible(driver, "[aria-label='Ask Grok anything'][contenteditable='true'], textarea[placeholder], textarea, div[contenteditable='true'], [role='textbox']")
            if input_box is None:
                self._sleep_with_deadline(0.2, deadline)
        if input_box is None:
            self._assert_deadline(deadline)
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
                self._sleep_with_deadline(0.15, deadline)
        if submit is None:
            self._assert_deadline(deadline)
            raise RuntimeError("Grok composer submit control was not available")
        self._click_composer_submit(driver, deadline)

        if capture_image:
            candidate = ""
            stable_since = 0.0
            while time.monotonic() < deadline:
                images = self._image_sources(driver)
                selected, _ = self._new_generated_image(images, before_sources, before_image_count)
                if selected:
                    if selected != candidate:
                        candidate = selected
                        stable_since = time.monotonic()
                    elif time.monotonic() - stable_since >= 2.0:
                        break
                self._sleep_with_deadline(0.25, deadline)
            if not candidate:
                raise ImageGenerationIncompleteError(
                    "Grok Web composer did not expose a usable generated image"
                )
            image_url = self._materialize_image_url(driver, candidate, deadline)
            body = self._synthetic_image_response("composer_image_" + uuid.uuid4().hex, image_url)
            return {
                "statusCode": HTTPStatus.OK,
                "status": "200 OK",
                "headers": {"content-type": "application/x-ndjson"},
                "bodyBase64": base64.b64encode(body).decode(),
            }

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
            self._sleep_with_deadline(0.25, deadline)
        if latest is None or not latest_text:
            self._assert_deadline(deadline)
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
        deadline = self._acquire(value)
        work_value = dict(value)
        work_value["_deadline"] = deadline
        try:
            driver = self._ensure_driver(work_value)
            try:
                self._remaining_seconds(work_value)
                self._prepare_page(driver, work_value)
                if work_value.get("imageMode"):
                    result = self._composer_image_fetch(driver, work_value)
                elif work_value.get("useComposer"):
                    result = self._composer_fetch(driver, work_value)
                else:
                    result = self._fetch(driver, work_value)
                if is_antibot_result(result):
                    driver.set_page_load_timeout(self._remaining_seconds(work_value, 90))
                    driver.get(work_value["baseURL"] + "/")
                    self.last_navigation = time.monotonic()
                    self._wait_for_challenge(driver, 45, deadline)
                    if work_value.get("imageMode"):
                        result = self._composer_image_fetch(driver, work_value)
                    elif work_value.get("useComposer"):
                        result = self._composer_fetch(driver, work_value)
                    else:
                        result = self._fetch(driver, work_value)
                self._remaining_seconds(work_value)
                state = self._cloudflare_state(driver)
                result.update(state)
                self._remember_cloudflare_state(work_value, state)
                return result
            except Exception as error:
                logging.exception(
                    "browser request failed (trace_id=%s upstream_request_id=%s)",
                    work_value.get("traceID", ""),
                    work_value.get("requestID", ""),
                )
                if not isinstance(error, (ImageSubscriptionRequiredError, ImageGenerationIncompleteError)):
                    self._close_unlocked()
                raise
        finally:
            self.lock.release()

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
        deadline = self._acquire(value)
        work_value = dict(value)
        work_value["_deadline"] = deadline
        try:
            driver = self._ensure_driver(work_value)
            try:
                self._prepare_page(driver, work_value)
                self._probe_signed_fetch(driver, deadline)
                self._remaining_seconds(work_value)
                state = self._cloudflare_state(driver)
                self._remember_cloudflare_state(work_value, state)
                return state
            except Exception:
                logging.exception("browser warmup failed")
                self._close_unlocked()
                raise
        finally:
            self.lock.release()

    @staticmethod
    def _probe_signed_fetch(driver, deadline: float | None = None) -> None:
        remaining = 30.0 if deadline is None else min(30.0, deadline - time.monotonic())
        if remaining <= 0.05:
            raise WorkerBusyError("browser worker request budget expired")
        driver.set_script_timeout(remaining)
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
QUOTA_SESSION = BrowserSession()


def is_worker_ready() -> bool:
    return SESSION.driver is not None


def session_for_path(path: str) -> BrowserSession:
    # Quota refreshes must not sit behind a multi-minute image generation.
    # Separate Chromium instances also keep account cookies isolated.
    if path == "/v1/grok/quota":
        return QUOTA_SESSION
    return SESSION


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
            self._json(
                HTTPStatus.OK,
                {
                    "ok": True,
                    "browserReady": SESSION.driver is not None,
                    "quotaBrowserReady": QUOTA_SESSION.driver is not None,
                },
            )
            return
        if self.path == "/readyz":
            ready = is_worker_ready()
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
            value["imageMode"] = bool(value.get("imageMode", False)) and self.path == "/v1/grok/fast-image"
            value["captureImage"] = self.path == "/v1/grok/fast-image"
            value["useComposer"] = self.path in {"/v1/grok/chat", "/v1/grok/fast-image"}
            if self.path == "/v1/grok/warm":
                state = SESSION.warm(value)
                self._json(HTTPStatus.OK, {"ok": True, **state})
                return
            started = time.monotonic()
            result = session_for_path(self.path).request(value)
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
        QUOTA_SESSION.close()
        server.server_close()


if __name__ == "__main__":
    main()
