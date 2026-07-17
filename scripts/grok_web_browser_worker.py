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
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import quote, urlparse, urlunparse

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


def classify_worker_error(error: Exception) -> str:
    message = str(error).lower()
    if "err_proxy_connection_failed" in message or "proxy connection" in message:
        return "proxy_unavailable"
    if "cloudflare challenge" in message or any(marker in message for marker in CHALLENGE_MARKERS):
        return "anti_bot"
    return "browser_unavailable"


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


def validate_request(value: object, allowed_paths: set[str] | None = None) -> dict:
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
    if (
        parsed_endpoint.scheme != "https"
        or parsed_endpoint.hostname != "grok.com"
        or parsed_endpoint.path not in allowed_paths
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
        self.statsig_cache: dict[str, tuple[float, str]] = {}

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

    def _ensure_driver(self, value: dict):
        proxy_url = str(value.get("proxyURL", "")).strip()
        user_agent = str(value.get("userAgent", "")).strip()
        fingerprint = hashlib.sha256((proxy_url + "\0" + user_agent).encode()).hexdigest()
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

        should_navigate = account_changed or not str(driver.current_url).startswith(value["baseURL"]) or time.monotonic() - self.last_navigation > 600
        if should_navigate:
            driver.get(value["baseURL"] + "/")
            self.last_navigation = time.monotonic()
            self._wait_for_challenge(driver, 45)

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

    def _statsig_id(self, driver, value: dict) -> str:
        meta = driver.execute_script(
            """
            const nodes = Array.from(document.querySelectorAll('meta[name]'));
            const node = nodes.find((item) => String(item.name || '').toLowerCase()
              .replace(/[\u2010-\u2015\u2212]/g, '-') === 'grok-site-verification');
            return node ? String(node.content || '') : '';
            """
        )
        if not meta:
            raise RuntimeError("Grok page is missing the Statsig verification meta")
        signer_url = str(value.get("statsigSignerURL", "")).strip()
        parsed = urlparse(signer_url)
        if parsed.scheme not in {"http", "https"} or not parsed.hostname:
            raise RuntimeError("Statsig signer URL is invalid")
        request_path = urlparse(str(value.get("endpoint", ""))).path
        cache_key = hashlib.sha256((signer_url + "\0" + meta + "\0" + request_path).encode()).hexdigest()
        cached = self.statsig_cache.get(cache_key)
        if cached and time.monotonic() < cached[0]:
            return cached[1]

        original_handle = driver.current_window_handle
        driver.switch_to.new_window("tab")
        try:
            driver.get(signer_url)
            self._wait_for_challenge(driver, 45)
            driver.set_script_timeout(30)
            result = driver.execute_async_script(
                """
                const endpoint = arguments[0];
                const meta = arguments[1];
                const done = arguments[arguments.length - 1];
                fetch(endpoint, {
                  method: 'POST',
                  credentials: 'include',
                  headers: {'Content-Type': 'application/json'},
                  body: JSON.stringify({
                    method: 'POST',
                    path: arguments[2],
                    environment: {metaContent: meta}
                  })
                }).then(async (response) => done({status: response.status, text: await response.text()}))
                  .catch((error) => done({error: String(error && error.message || error)}));
                """,
                signer_url,
                meta,
                request_path,
            )
        finally:
            driver.close()
            driver.switch_to.window(original_handle)
        if not isinstance(result, dict) or result.get("error") or not 200 <= int(result.get("status", 0)) < 300:
            raise RuntimeError("Statsig browser signing failed")
        try:
            result = json.loads(str(result.get("text", "")))
        except json.JSONDecodeError as exc:
            raise RuntimeError("Statsig signer returned invalid JSON") from exc
        statsig_id = str(result.get("x-statsig-id", "")).strip()
        if not statsig_id:
            raise RuntimeError("Statsig signer returned an empty signature")
        self.statsig_cache[cache_key] = (time.monotonic() + 3600, statsig_id)
        return statsig_id

    @staticmethod
    def _fetch(driver, value: dict, statsig_id: str) -> dict:
        driver.set_script_timeout(value["timeoutSeconds"] + 15)
        result = driver.execute_async_script(
            """
            const endpoint = arguments[0];
            const payload = arguments[1];
            const statsig = arguments[2];
            const requestId = arguments[3];
            const done = arguments[arguments.length - 1];
            fetch(endpoint, {
              method: 'POST',
              credentials: 'include',
              cache: 'no-store',
              headers: {
                'Accept': '*/*',
                'Content-Type': 'application/json',
                'x-statsig-id': statsig,
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
            statsig_id,
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

    def request(self, value: dict) -> dict:
        with self.lock:
            driver = self._ensure_driver(value)
            try:
                self._prepare_page(driver, value)
                statsig_id = self._statsig_id(driver, value)
                result = self._fetch(driver, value, statsig_id)
                if result.get("statusCode") == HTTPStatus.FORBIDDEN:
                    driver.get(value["baseURL"] + "/")
                    self.last_navigation = time.monotonic()
                    self._wait_for_challenge(driver, 45)
                    statsig_id = self._statsig_id(driver, value)
                    result = self._fetch(driver, value, statsig_id)
                return result
            except Exception:
                logging.exception("browser request failed")
                self._close_unlocked()
                raise

    def warm(self, value: dict) -> None:
        with self.lock:
            driver = self._ensure_driver(value)
            try:
                self._prepare_page(driver, value)
                self._statsig_id(driver, value)
            except Exception:
                logging.exception("browser warmup failed")
                self._close_unlocked()
                raise


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
        if self.path not in {"/v1/grok/fast-image", "/v1/grok/quota", "/v1/grok/warm"}:
            self._json(HTTPStatus.NOT_FOUND, {"error": "not found"})
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if length <= 0 or length > MAX_REQUEST_BYTES:
                raise ValueError("request body size is invalid")
            allowed_paths = {"/rest/rate-limits"} if self.path == "/v1/grok/quota" else {"/rest/app-chat/conversations/new"}
            value = validate_request(json.loads(self.rfile.read(length)), allowed_paths)
            if self.path == "/v1/grok/warm":
                SESSION.warm(value)
                self._json(HTTPStatus.OK, {"ok": True})
                return
            started = time.monotonic()
            result = SESSION.request(value)
            status_code = int(result.get("statusCode", 0))
            if status_code < 200 or status_code >= 300:
                try:
                    preview = base64.b64decode(result.get("bodyBase64", ""), validate=True)[:500].decode(errors="replace")
                except Exception:
                    preview = "<invalid body>"
                logging.warning("grok fetch rejected (status=%s body=%s)", status_code, preview)
            logging.info(
                "grok fetch completed (status=%s bytes=%s duration_ms=%s)",
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
