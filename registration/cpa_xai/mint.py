"""High-level: mint CPA xai-*.json for one free registered account."""

from __future__ import annotations

from pathlib import Path
from typing import Any, Callable

from .browser_confirm import mint_with_browser
from .probe import probe_mini_response, probe_models
from .proxyutil import proxy_log_label, resolve_proxy, set_runtime_proxy
from .schema import DEFAULT_BASE_URL, build_cpa_xai_auth
from .writer import write_cpa_xai_auth

LogFn = Callable[[str], None]


def _noop(_: str) -> None:
    return None


def mint_and_export(
    *,
    email: str,
    password: str,
    auth_dir: str | Path,
    page: Any | None = None,
    proxy: str | None = None,
    headless: bool = False,
    base_url: str = DEFAULT_BASE_URL,
    probe: bool = True,
    probe_chat: bool = True,
    browser_timeout_sec: float = 240.0,
    force_standalone: bool = True,
    cookies: Any | None = None,
    reuse_browser: bool = True,
    recycle_every: int = 15,
    log: LogFn | None = None,
    cancel: Callable[[], bool] | None = None,
) -> dict[str, Any]:
    """Full pipeline: device-auth → write CPA file → optional probe.

    Returns dict with keys: ok, path, email, probe results, warnings?, error?
    """
    log = log or _noop
    email = (email or "").strip()
    if not email or not password:
        return {"ok": False, "email": email, "error": "missing email/password"}

    # Config/explicit proxy wins over shell https_proxy (common 7890 trap).
    # Thread-local pin — safe under concurrent mint workers.
    resolved = resolve_proxy(proxy)
    set_runtime_proxy(resolved or None)
    log(f"开始生成 CPA 凭据：{email}，代理={proxy_log_label(resolved) or '直连'}")
    try:
        tokens = mint_with_browser(
            email=email,
            password=password,
            page=None if force_standalone else page,
            proxy=resolved or None,
            headless=headless,
            browser_timeout_sec=browser_timeout_sec,
            force_standalone=force_standalone,
            cookies=cookies,
            reuse_browser=reuse_browser,
            recycle_every=recycle_every,
            poll_log=log,
            cancel=cancel,
        )
    except Exception as e:  # noqa: BLE001
        log(f"生成 CPA 凭据失败：{e}")
        return {"ok": False, "email": email, "error": str(e)}

    payload = build_cpa_xai_auth(
        email=email,
        access_token=tokens["access_token"],
        refresh_token=tokens["refresh_token"],
        id_token=tokens.get("id_token"),
        expires_in=tokens.get("expires_in"),
        base_url=base_url,
    )
    path = write_cpa_xai_auth(auth_dir, payload)
    log(f"CPA 凭据已写入：{path}")

    result: dict[str, Any] = {
        "ok": True,
        "importable": True,
        "email": email,
        "path": str(path),
        "user_code": tokens.get("user_code"),
        "base_url": base_url,
        "proxy": proxy_log_label(resolved),
    }

    if probe:
        pr = probe_models(tokens["access_token"], base_url=base_url, proxy=resolved or None)
        result["probe_models"] = pr
        log(f"模型探测：成功={pr.get('ok')}，包含 grok-4.5={pr.get('has_grok_45')}，模型={pr.get('model_ids')}")
        if not pr.get("has_grok_45"):
            result["ok"] = False
            result["error"] = "token ok but grok-4.5 not listed"
        if probe_chat and pr.get("has_grok_45"):
            ch = probe_mini_response(
                tokens["access_token"], base_url=base_url, proxy=resolved or None
            )
            result["probe_chat"] = ch
            log(f"对话探测：成功={ch.get('ok')}，模型={ch.get('model')}，结果={ch.get('text')!r}")
            if not ch.get("ok"):
                detail = ch.get("error") or ch.get("status")
                message = f"CPA chat probe unavailable: {detail}"
                result.setdefault("warnings", []).append(
                    {"code": "cpa_chat_probe_failed", "message": message}
                )
                log(f"chat probe warning: {detail}")
                detail_text = str(detail).lower()
                denied = ch.get("code") == "permission_denied" or (
                    ch.get("status") == 403
                    and (
                        "permission-denied" in detail_text
                        or "permission_denied" in detail_text
                        or "chat endpoint is denied" in detail_text
                    )
                )
                if denied:
                    result["importable"] = False
                    result["import_block_reason"] = "cpa_chat_permission_denied"
                    log("chat probe denied access; credential retained but automatic import is blocked")
    return result
