"""Register-machine hook: mint CPA xai auth after successful registration.

OIDC package lives at ./cpa_xai (bundled with this project).
Optional override: config `api_reverse_tools` / env `API_REVERSE_TOOLS`
points at a directory that *contains* the `cpa_xai` package.
"""

from __future__ import annotations

import json
import os
import shutil
import sys
import tempfile
import time
from pathlib import Path
from typing import Any, Callable
from urllib.parse import urljoin

_REG_DIR = Path(__file__).resolve().parent
_DEFAULT_OUT = _REG_DIR / "cpa_auths"
_DEFAULT_CPA = Path("")  # empty = do not assume a machine-local CPA path


def _cpa_import_url(config: dict) -> str:
    """Resolve the Admin CPA import URL without logging credentials."""
    base = str(config.get("cpa_remote_base") or config.get("grok2api_remote_base") or "").strip().rstrip("/")
    path = str(config.get("cpa_remote_import_path") or "/admin/api/tokens/cpa/import").strip()
    if not base:
        return ""
    if base.endswith("/tokens/cpa/import"):
        return base
    if path.startswith("http://") or path.startswith("https://"):
        return path
    if base.endswith("/admin/api") and path.startswith("/admin/api/"):
        path = path[len("/admin/api"):]
    return urljoin(base + "/", path.lstrip("/"))


def upload_cpa_auth_file(
    path: str | Path,
    *,
    config: dict,
    log_callback: Callable[[str], None] | None = None,
) -> dict[str, Any]:
    """Upload one local CPA file with bounded retries.

    The caller owns the local file and it is deliberately never deleted or
    moved when the remote endpoint is unavailable.
    """
    log = log_callback or (lambda _message: None)
    if os.environ.get("REGISTRATION_DISABLE_REMOTE_IMPORT", "").strip().lower() in {"1", "true", "yes"}:
        return {"ok": False, "skipped": True, "reason": "disabled_by_environment"}
    if not bool(config.get("cpa_remote_import_enabled", True)):
        return {"ok": False, "skipped": True, "reason": "disabled"}
    url = _cpa_import_url(config)
    app_key = str(os.environ.get("GROK2API_APP_KEY") or config.get("cpa_remote_app_key") or config.get("grok2api_remote_app_key") or "").strip()
    local = Path(path)
    if not url or not app_key:
        return {"ok": False, "skipped": True, "reason": "remote_not_configured"}
    if not local.is_file():
        return {"ok": False, "error": "local_file_missing"}

    try:
        from curl_cffi import CurlMime, requests
    except Exception as exc:  # pragma: no cover - registration environment supplies curl_cffi
        return {"ok": False, "error": "http_client_unavailable", "error_type": type(exc).__name__}

    attempts = max(1, min(int(config.get("cpa_remote_import_retries", 3) or 3), 5))
    timeout = max(1.0, float(config.get("cpa_remote_import_timeout_sec", 15) or 15))
    transient = {408, 425, 429, 500, 502, 503, 504}
    for attempt in range(1, attempts + 1):
        try:
            multipart = CurlMime()
            try:
                multipart.addpart(
                    name="files",
                    filename=local.name,
                    content_type="application/json",
                    local_path=local,
                )
                response = requests.post(
                    url,
                    headers={"Authorization": f"Bearer {app_key}"},
                    multipart=multipart,
                    timeout=timeout,
                    proxies={},
                )
            finally:
                multipart.close()
            status = int(getattr(response, "status_code", 0) or 0)
            if 200 <= status < 300:
                try:
                    payload = response.json()
                except Exception:
                    payload = None
                results = payload.get("results") if isinstance(payload, dict) else None
                item = results[0] if isinstance(results, list) and len(results) == 1 else None
                result_status = str(item.get("status") or "") if isinstance(item, dict) else ""
                if result_status in {"imported", "updated", "skipped"}:
                    log(f"[cpa] uploaded {local.name} to grok2api: result={result_status}")
                    return {
                        "ok": True,
                        "status_code": status,
                        "result_status": result_status,
                    }
                reason = str(item.get("reason") or "invalid_import_response") if isinstance(item, dict) else "invalid_import_response"
                log(f"[cpa] upload rejected {local.name}: result={result_status or 'invalid'} reason={reason}")
                return {
                    "ok": False,
                    "status_code": status,
                    "error": "cpa_import_failed",
                    "result_status": result_status or "invalid",
                    "reason": reason,
                }
            if status not in transient:
                log(f"[cpa] upload rejected {local.name}: status={status}")
                return {"ok": False, "status_code": status, "error": "remote_rejected"}
            error = f"transient_status_{status}"
        except Exception:  # bounded retry for transport failures
            error = "transport_error"
            if attempt == attempts:
                log(f"[cpa] upload failed {local.name}: {error}")
        if attempt < attempts:
            time.sleep(min(2.0, 0.25 * (2 ** (attempt - 1))))
    return {"ok": False, "error": error, "attempts": attempts}


def _ensure_cpa_xai_on_path(tools_dir: str | Path | None = None) -> Path:
    """Put the parent of `cpa_xai` on sys.path. Default: this project root."""
    if tools_dir:
        tools = Path(tools_dir).expanduser().resolve()
    else:
        env = (os.environ.get("API_REVERSE_TOOLS") or "").strip()
        tools = Path(env).expanduser().resolve() if env else _REG_DIR
    # If user pointed at .../cpa_xai itself, use its parent
    if tools.name == "cpa_xai" and (tools / "__init__.py").is_file():
        tools = tools.parent
    if str(tools) not in sys.path:
        sys.path.insert(0, str(tools))
    return tools


def _should_reuse_mint_browser(config: dict) -> bool:
    if bool(config.get("cpa_close_browser_after_auth", True)):
        return False
    return bool(config.get("cpa_mint_browser_reuse", True))


def _stage_hotload_file(source: Path, incoming_dir: Path) -> Path:
    """Publish one credential atomically so the Go poller never sees a partial JSON file."""
    incoming_dir.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{source.stem}-",
        suffix=".tmp",
        dir=incoming_dir,
    )
    os.close(descriptor)
    temporary = Path(temporary_name)
    destination = incoming_dir / source.name
    try:
        shutil.copyfile(source, temporary)
        os.chmod(temporary, 0o600)
        os.replace(temporary, destination)
    finally:
        temporary.unlink(missing_ok=True)
    return destination


def _await_hotload_result(
    incoming_dir: Path,
    credential_stem: str,
    *,
    submitted_at: float,
    timeout: float,
    poll_interval: float = 0.5,
) -> dict[str, Any]:
    """Wait for the Go importer result without reading credential contents."""
    deadline = time.monotonic() + timeout
    spool_root = incoming_dir.parent
    while time.monotonic() < deadline:
        candidates: list[tuple[float, str, Path]] = []
        for bucket in ("processed", "failed"):
            directory = spool_root / bucket
            if not directory.is_dir():
                continue
            for path in directory.glob(f"{credential_stem}*.result.json"):
                try:
                    modified = path.stat().st_mtime
                except OSError:
                    continue
                if modified + 1 < submitted_at:
                    continue
                candidates.append((modified, bucket, path))
        for _, bucket, path in sorted(candidates, reverse=True):
            try:
                payload = json.loads(path.read_text(encoding="utf-8"))
            except (OSError, ValueError):
                continue
            if not isinstance(payload, dict):
                continue
            status = str(payload.get("status") or "")
            sync_failed = int(payload.get("syncFailed") or 0)
            return {
                "ok": bucket == "processed" and status == "processed" and sync_failed == 0,
                "bucket": bucket,
                "status": status,
                "created": int(payload.get("created") or 0),
                "updated": int(payload.get("updated") or 0),
                "synced": int(payload.get("synced") or 0),
                "syncFailed": sync_failed,
                "processedAt": str(payload.get("processedAt") or ""),
            }
        time.sleep(max(0.05, poll_interval))
    return {"ok": False, "status": "timeout"}


def export_cookies_from_page(page: Any) -> list[dict]:
    """Best-effort export of cookies from a DrissionPage tab/browser."""
    if page is None:
        return []
    cookies = None
    for getter in (
        lambda: page.cookies(all_domains=True, all_info=True),
        lambda: page.cookies(all_domains=True),
        lambda: page.cookies(),
    ):
        try:
            cookies = getter()
            if cookies:
                break
        except TypeError:
            continue
        except Exception:
            continue
    if not cookies:
        try:
            browser = getattr(page, "browser", None)
            if browser is not None:
                cookies = browser.cookies()
        except Exception:
            cookies = None
    if isinstance(cookies, list):
        return [c for c in cookies if isinstance(c, dict)]
    return []


def export_cpa_xai_for_account(
    email: str,
    password: str,
    *,
    page: Any | None = None,
    cookies: Any | None = None,
    sso: str | None = None,
    config: dict | None = None,
    log_callback: Callable[[str], None] | None = None,
) -> dict:
    """Mint OIDC + write xai-<email>.json under register cpa_auths (and optional CPA auth-dir)."""
    cfg = config or {}
    log = log_callback or (lambda m: print(m, flush=True))

    if not cfg.get("cpa_export_enabled", True):
        log("[cpa] export disabled")
        return {"ok": False, "skipped": True, "reason": "disabled"}

    tools_dir = cfg.get("api_reverse_tools") or cfg.get("cpa_xai_parent") or None
    _ensure_cpa_xai_on_path(tools_dir)

    try:
        from cpa_xai import mint_and_export  # type: ignore
    except Exception as e:  # noqa: BLE001
        log(f"[cpa] import cpa_xai failed: {e}")
        return {"ok": False, "error": f"import: {e}"}

    out_dir = Path(os.environ.get("REGISTRATION_CPA_EXPORT_DIR") or cfg.get("cpa_auth_dir") or _DEFAULT_OUT).expanduser()
    if not out_dir.is_absolute():
        out_dir = (_REG_DIR / out_dir).resolve()

    environment_hotload = os.environ.get("REGISTRATION_CPA_HOTLOAD_DIR", "").strip()
    hotload_raw = environment_hotload or str(cfg.get("cpa_hotload_dir") or "").strip()
    cpa_dir = Path(hotload_raw).expanduser() if hotload_raw else None
    if cpa_dir and not cpa_dir.is_absolute():
        cpa_dir = (_REG_DIR / cpa_dir).resolve()

    # Priority: cpa_proxy > proxy > env. Config must beat shell https_proxy.
    proxy = (cfg.get("cpa_proxy") or cfg.get("proxy") or "").strip()
    if not proxy:
        proxy = (
            os.environ.get("https_proxy")
            or os.environ.get("HTTPS_PROXY")
            or os.environ.get("http_proxy")
            or ""
        ).strip()
    # Default headed: headless is frequently Cloudflare-blocked on accounts.x.ai
    headless = bool(cfg.get("cpa_headless", False))
    probe = bool(cfg.get("cpa_probe_after_write", True))
    probe_chat = bool(cfg.get("cpa_probe_chat", True))
    timeout = float(cfg.get("cpa_mint_timeout_sec", 240))
    base_url = cfg.get("cpa_base_url") or "https://cli-chat-proxy.grok.com/v1"
    force_standalone = bool(cfg.get("cpa_force_standalone", True))
    cookie_inject = bool(cfg.get("cpa_mint_cookie_inject", True))
    reuse_browser = _should_reuse_mint_browser(cfg)
    recycle_every = int(cfg.get("cpa_mint_browser_recycle_every", 15) or 0)

    # cookies: explicit arg > page export > none
    use_cookies = cookies
    if use_cookies is None and cookie_inject and page is not None:
        use_cookies = export_cookies_from_page(page)
    if not cookie_inject:
        use_cookies = None
    else:
        # Always attach SSO cookie clones — register cookies alone often miss accounts.x.ai host
        sso_val = (sso or "").strip()
        if not sso_val and isinstance(use_cookies, list):
            for c in use_cookies:
                if isinstance(c, dict) and c.get("name") in ("sso", "sso-rw") and c.get("value"):
                    sso_val = str(c.get("value"))
                    break
        if sso_val:
            base = list(use_cookies) if isinstance(use_cookies, list) else []
            for name in ("sso", "sso-rw"):
                for dom in (".x.ai", "accounts.x.ai", ".accounts.x.ai", "auth.x.ai", "grok.com", ".grok.com"):
                    base.append({
                        "name": name,
                        "value": sso_val,
                        "domain": dom,
                        "path": "/",
                        "secure": True,
                        "httpOnly": True,
                    })
            use_cookies = base

    out_dir.mkdir(parents=True, exist_ok=True)
    log(
        f"[cpa] mint OIDC for {email} -> {out_dir} proxy={proxy or '(none)'} "
        f"cookies={len(use_cookies) if isinstance(use_cookies, list) else (1 if use_cookies else 0)} "
        f"reuse={reuse_browser}"
    )

    def _log(msg: str) -> None:
        log(f"[cpa] {msg}")

    result = mint_and_export(
        email=email,
        password=password,
        auth_dir=out_dir,
        page=None if force_standalone else page,
        proxy=proxy or None,
        headless=headless,
        base_url=base_url,
        probe=probe,
        probe_chat=probe_chat,
        browser_timeout_sec=timeout,
        force_standalone=force_standalone,
        cookies=use_cookies,
        reuse_browser=reuse_browser,
        recycle_every=recycle_every,
        log=_log,
    )

    if result.get("ok") and result.get("path"):
        remote = upload_cpa_auth_file(result["path"], config=cfg, log_callback=_log)
        result["remote_upload"] = remote
        if not remote.get("ok") and not remote.get("skipped"):
            # Registration remains successful; the local file is retained for retry.
            result["remote_upload_error"] = remote.get("error") or "remote_upload_failed"

    copy_to_hotload = bool(environment_hotload) or bool(cfg.get("cpa_copy_to_hotload", False))
    if result.get("ok") and result.get("path") and copy_to_hotload and cpa_dir:
        try:
            src = Path(result["path"])
            submitted_at = time.time()
            dst = _stage_hotload_file(src, cpa_dir)
            result["cpa_path"] = str(dst)
            log(f"[cpa] hotload copy -> {dst}")
            await_result = bool(environment_hotload) or bool(cfg.get("cpa_hotload_await_result", False))
            if await_result:
                result_timeout = max(1.0, min(float(cfg.get("cpa_hotload_result_timeout_sec", 300) or 300), 600.0))
                import_result = _await_hotload_result(
                    cpa_dir,
                    src.stem,
                    submitted_at=submitted_at,
                    timeout=result_timeout,
                )
                result["hotload_import"] = import_result
                if import_result.get("ok"):
                    log(
                        f"[cpa] spool import success: created={import_result.get('created', 0)} "
                        f"updated={import_result.get('updated', 0)} synced={import_result.get('synced', 0)}"
                    )
                else:
                    status = import_result.get("status") or "failed"
                    result["ok"] = False
                    result["error"] = f"spool_import_{status}"
                    log(f"[cpa] spool import failed: status={status}")
        except Exception as e:  # noqa: BLE001
            log(f"[cpa] hotload copy failed: {e}")
            result["cpa_copy_error"] = str(e)
            result["ok"] = False
            result["error"] = "spool_copy_failed"

    # failure log under register dir
    if not result.get("ok"):
        fail_path = out_dir / "cpa_auth_failed.txt"
        with open(fail_path, "a", encoding="utf-8") as f:
            f.write(f"{email}----{result.get('error') or 'unknown'}----{int(time.time())}\n")
        if cfg.get("cpa_mint_required", False):
            raise RuntimeError(f"CPA mint required but failed: {result.get('error')}")

    return result
