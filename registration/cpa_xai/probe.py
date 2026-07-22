"""Probe free Grok 4.5 via cli-chat-proxy with a CPA access_token."""

from __future__ import annotations

import json
import time
import urllib.error
import urllib.request
from typing import Any

from .proxyutil import resolve_proxy
from .schema import DEFAULT_BASE_URL, DEFAULT_CLIENT_HEADERS, normalize_cpa_base_url


def _opener(proxy: str | None = None) -> urllib.request.OpenerDirector:
    p = resolve_proxy(proxy)
    handlers: list[Any] = []
    if p:
        handlers.append(urllib.request.ProxyHandler({"http": p, "https": p}))
    return urllib.request.build_opener(*handlers) if handlers else urllib.request.build_opener()


def _probe_error_code(status: int, body: str) -> str:
    text = body.lower()
    if status == 403 and (
        "permission-denied" in text
        or "permission_denied" in text
        or "chat endpoint is denied" in text
    ):
        return "permission_denied"
    if status == 429:
        return "rate_limited"
    return f"http_{status}"


def probe_models(
    access_token: str,
    *,
    base_url: str = DEFAULT_BASE_URL,
    timeout: float = 30.0,
    proxy: str | None = None,
) -> dict[str, Any]:
    base = normalize_cpa_base_url(base_url)
    url = f"{base}/models"
    headers = {
        "Authorization": f"Bearer {access_token}",
        "Accept": "application/json",
        **DEFAULT_CLIENT_HEADERS,
    }
    opener = _opener(proxy)
    req = urllib.request.Request(url, headers=headers, method="GET")
    try:
        with opener.open(req, timeout=timeout) as resp:
            body = json.loads(resp.read().decode("utf-8"))
            ids = [x.get("id") for x in body.get("data") or [] if isinstance(x, dict)]
            return {
                "ok": True,
                "status": getattr(resp, "status", 200),
                "model_ids": ids,
                "has_grok_45": any(i == "grok-4.5" for i in ids),
            }
    except urllib.error.HTTPError as e:
        return {
            "ok": False,
            "status": e.code,
            "error": e.read().decode("utf-8", errors="replace")[:500],
            "model_ids": [],
            "has_grok_45": False,
        }
    except Exception as e:  # noqa: BLE001
        return {
            "ok": False,
            "status": 0,
            "error": str(e),
            "model_ids": [],
            "has_grok_45": False,
        }


def probe_mini_response(
    access_token: str,
    *,
    base_url: str = DEFAULT_BASE_URL,
    timeout: float = 60.0,
    proxy: str | None = None,
    attempts: int = 3,
    retry_delay: float = 1.0,
) -> dict[str, Any]:
    base = normalize_cpa_base_url(base_url)
    url = f"{base}/responses"
    payload = {
        "model": "grok-4.5",
        "stream": False,
        "input": "Reply with exactly MINT_OK",
        "reasoning": {"effort": "low"},
    }
    headers = {
        "Authorization": f"Bearer {access_token}",
        "Content-Type": "application/json",
        "Accept": "application/json",
        **DEFAULT_CLIENT_HEADERS,
    }
    attempts = max(1, min(int(attempts), 5))
    result: dict[str, Any] = {"ok": False, "status": 0, "error": "probe_not_attempted"}
    for attempt in range(1, attempts + 1):
        opener = _opener(proxy)
        req = urllib.request.Request(
            url,
            data=json.dumps(payload).encode("utf-8"),
            headers=headers,
            method="POST",
        )
        try:
            with opener.open(req, timeout=timeout) as resp:
                body = json.loads(resp.read().decode("utf-8"))
                texts: list[str] = [str(body.get("output_text") or "")]
                for item in body.get("output") or []:
                    if item.get("type") == "message":
                        for content in item.get("content") or []:
                            if content.get("type") == "output_text":
                                texts.append(content.get("text") or "")
                text = "\n".join(value for value in texts if value).strip()
                if "MINT_OK" in text:
                    return {
                        "ok": True,
                        "status": getattr(resp, "status", 200),
                        "model": body.get("model"),
                        "text": text,
                        "usage": body.get("usage"),
                        "attempts": attempt,
                    }
                result = {
                    "ok": False,
                    "status": getattr(resp, "status", 200),
                    "model": body.get("model"),
                    "error": "missing_expected_output",
                    "attempts": attempt,
                }
        except urllib.error.HTTPError as error:
            body = error.read().decode("utf-8", errors="replace")[:800]
            result = {
                "ok": False,
                "status": error.code,
                "code": _probe_error_code(error.code, body),
                "error": body,
                "attempts": attempt,
            }
        except Exception as error:  # noqa: BLE001
            result = {"ok": False, "status": 0, "error": str(error), "attempts": attempt}
        if attempt < attempts:
            time.sleep(max(0.0, retry_delay) * attempt)
    return result
