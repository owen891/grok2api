#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""协议注册 worker：HTTP 协议注册 + Build OAuth → spool xai-*.json。

与 Go Controller 约定对齐：
  python -u protocol_register_cli.py --config ... --state-dir ... --log-file ...
      [--count N] [--extra N] [--threads N] [--fast]

协议 HTTP 注册；Turnstile 默认走本地 HTTP 过盾（不弹浏览器）；邮箱默认 YYDS。
"""
from __future__ import annotations

import argparse
import base64
import json
import os
import secrets
import sys
import tempfile
import threading
import time
import traceback
import uuid
from concurrent.futures import FIRST_COMPLETED, Future, ThreadPoolExecutor, wait
from pathlib import Path
from typing import Any, Optional

ROOT = Path(__file__).resolve().parent
PROTOCOL_ROOT = ROOT / "protocol_auth"
if str(PROTOCOL_ROOT) not in sys.path:
    sys.path.insert(0, str(PROTOCOL_ROOT))
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

import yyds_mail as reg  # noqa: E402
from cpa_xai.schema import build_cpa_xai_auth, credential_file_name  # noqa: E402
from cpa_xai.writer import write_cpa_xai_auth  # noqa: E402

SIGNUP_URL = "https://accounts.x.ai/sign-up?redirect=grok-com"
SIGNIN_URL = "https://accounts.x.ai/sign-in?redirect=grok-com"
_log_lock = threading.Lock()
_stop = threading.Event()
_progress_lock = threading.Lock()
_done = 0
_attempted = 0
_failed = 0
_ok = 0

CHECKPOINT_VERSION = 1
RESUMABLE_STAGES = {
    "account_created",
    "sso_ready",
    "oauth_failed",
    "credential_ready",
    "credential_written",
}


def _now() -> str:
    return time.strftime("%Y-%m-%d %H:%M:%S")


def log(worker_id: int | str, msg: str, log_file: Path | None = None) -> None:
    line = f"[{_now()}] [protocol][w{worker_id}] {msg}"
    with _log_lock:
        # Go Controller 会捕获 stdout 写入 registration.log；
        # 这里只 print，避免与 --log-file 双写。
        print(line, flush=True)


def load_config(path: Path) -> dict[str, Any]:
    data = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(data, dict):
        raise ValueError("config must be a JSON object")
    return data


def resolve_yescaptcha_key(cfg: dict[str, Any]) -> str:
    for key in (
        "yescaptcha_api_key",
        "yes_captcha_key",
        "yes_captcha_api_key",
        "captcha_api_key",
        "YESCAPTCHA_API_KEY",
    ):
        val = str(cfg.get(key) or "").strip()
        # 防止把 YYDS 的 AC- 开头 key 误当成 YesCaptcha
        if val and not val.startswith("AC-"):
            return val
    env = str(os.environ.get("YESCAPTCHA_API_KEY") or "").strip()
    if env and not env.startswith("AC-"):
        return env
    return ""


def captcha_mode(cfg: dict[str, Any]) -> str:
    mode = str(cfg.get("captcha_solver") or cfg.get("turnstile_solver") or "local").strip().lower()
    if mode in {"yescaptcha", "yes", "remote"}:
        return "yescaptcha"
    return "local"


def solve_turnstile_token(
    cfg: dict[str, Any],
    *,
    proxy: str = "",
    website_url: str = SIGNUP_URL,
    log_file: Path | None = None,
    worker_id: int | str = "main",
) -> str:
    mode = captcha_mode(cfg)
    if mode == "yescaptcha":
        from xconsole_client import YesCaptchaSolver, config as C
        key = resolve_yescaptcha_key(cfg)
        if not key:
            raise RuntimeError("captcha_solver=yescaptcha 但未配置 yescaptcha_api_key")
        solver = YesCaptchaSolver(key)
        token = solver.solve_turnstile(
            website_url=website_url,
            website_key=C.TURNSTILE_SITEKEY,
            premium=True,
        )
        log(worker_id, f"YesCaptcha 过盾成功（{len(token)} 字符）", log_file)
        return token

    from local_turnstile import solve_turnstile_local
    from xconsole_client import config as C
    endpoint = str(
        cfg.get("captcha_endpoint")
        or cfg.get("local_captcha_endpoint")
        or cfg.get("yescaptcha_endpoint")
        or ""
    ).strip()
    client_key = str(
        cfg.get("captcha_client_key")
        or cfg.get("local_captcha_client_key")
        or cfg.get("yescaptcha_api_key")
        or ""
    ).strip()
    # 防止 YYDS 的 AC- key 被当成 captcha key
    if client_key.startswith("AC-"):
        client_key = "local"
    log(worker_id, f"本地过盾请求中：{endpoint or '（未配置 endpoint）'}...", log_file)
    token = solve_turnstile_local(
        website_url=website_url,
        website_key=getattr(C, "TURNSTILE_SITEKEY", "0x4AAAAAAAhrPj9_JwTyl4nM"),
        timeout=float(cfg.get("turnstile_timeout") or 120),
        headless=True,
        proxy=proxy or "",
        endpoint=endpoint,
        client_key=client_key or "local",
        task_type=str(cfg.get("captcha_task_type") or "TurnstileTaskProxyless"),
    )
    log(worker_id, f"本地过盾成功（{len(token)} 字符）", log_file)
    return token


def resolve_sso(
    client: Any,
    cfg: dict[str, Any],
    *,
    email: str,
    password: str,
    proxy: str,
    log_file: Path,
    worker_id: int,
    inspect_create_response: bool,
) -> str:
    if inspect_create_response:
        token = client.fetch_sso_token(email=email, password=password, save=False, retries=3) or ""
        if token:
            return token
        log(worker_id, "创建响应未携带 SSO，切换协议登录恢复", log_file)

    login_turnstile = solve_turnstile_token(
        cfg,
        proxy=proxy,
        website_url=SIGNIN_URL,
        log_file=log_file,
        worker_id=worker_id,
    )
    return client.obtain_session_via_password(
        email=email,
        password=password,
        turnstile_token=login_turnstile,
        referer=SIGNIN_URL,
        retries=3,
    ) or ""


class YydsCodeReceiver:
    """适配 grok_register_ttk 的 YYDS 邮箱为 wait_for_code 接口。"""

    def __init__(self, address: str, token: str):
        self.address = address
        self.token = token

    def wait_for_code(self, timeout: float = 120.0) -> str:
        deadline = time.time() + timeout
        seen_ids: set[str] = set()
        while time.time() < deadline:
            if _stop.is_set():
                raise RuntimeError("stopped")
            try:
                messages = reg.yyds_get_messages(self.address, token=self.token)
            except Exception:
                messages = []
            for item in messages or []:
                mid = str(item.get("id") or item.get("messageId") or "")
                if mid and mid in seen_ids:
                    continue
                if mid:
                    seen_ids.add(mid)
                subject = str(item.get("subject") or item.get("intro") or "")
                body = subject
                if mid:
                    try:
                        detail = reg.yyds_get_message_detail(mid, token=self.token)
                        if isinstance(detail, dict):
                            body = " ".join(
                                str(detail.get(k) or "")
                                for k in ("subject", "text", "html", "body", "intro", "content")
                            )
                    except Exception:
                        pass
                # 严格提取：禁止裸数字 6 位回退（会误抓 333333）
                try:
                    code = reg.extract_verification_code(body, subject)
                except Exception:
                    code = None
                if code and not str(code).isdigit():
                    return str(code)
            time.sleep(3)
        raise TimeoutError("YYDS 邮箱验证码超时（未找到有效 xAI 验证码）")

def make_email(cfg: dict[str, Any], backend: str):
    backend = (backend or "yyds").strip().lower()
    if backend in {"yyds", "mail_yyds"}:
        if not cfg.get("yyds_api_key") and not cfg.get("yyds_jwt"):
            raise RuntimeError("config 缺少 yyds_api_key / yyds_jwt")
        reg.config.update(
            {
                "yyds_api_key": cfg.get("yyds_api_key") or "",
                "yyds_jwt": cfg.get("yyds_jwt") or "",
            }
        )
        api_base = str(cfg.get("yyds_api_base") or "").strip().rstrip("/")
        if api_base:
            reg.YYDS_API_BASE = api_base
        address, token = reg.yyds_get_email_and_token(
            api_key=cfg.get("yyds_api_key") or None,
            jwt=cfg.get("yyds_jwt") or None,
        )
        return address, YydsCodeReceiver(address, token)

    if backend in {"tempmail", "tempmail_lol"}:
        api_key = str(cfg.get("tempmail_api_key") or os.environ.get("TEMPMAIL_API_KEY") or "").strip()
        from xconsole_client.tempmail_transport import TempmailInbox

        api_base = str(cfg.get("tempmail_api_base") or cfg.get("tempmail_lol_api_base") or "https://api.tempmail.lol").strip().rstrip("/")
        if api_base.endswith("/v2"):
            api_base = api_base[:-3]
        prefix = str(cfg.get("tempmail_lol_prefix") or "xai").strip()
        configured_domains = str(cfg.get("tempmail_lol_domain") or "").replace(",", "\n").splitlines()
        domains = [value.strip() for value in configured_domains if value.strip()]
        domain = secrets.choice(domains) if domains else ""
        inbox = TempmailInbox(api_key=api_key, prefix=prefix, domain=domain, base_url=api_base, debug=False)
        email = inbox.create()
        return email, inbox

    raise RuntimeError(f"不支持的邮箱后端: {backend}")


def write_state(state_dir: Path, **fields: Any) -> None:
    state_dir.mkdir(parents=True, exist_ok=True)
    path = state_dir / "state.json"
    current: dict[str, Any] = {}
    if path.exists():
        try:
            current = json.loads(path.read_text(encoding="utf-8"))
        except Exception:
            current = {}
    current.update(fields)
    current["updated_at"] = _now()
    _atomic_write_text(path, json.dumps(current, ensure_ascii=False, indent=2))


def _atomic_write_text(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    os.chmod(path.parent, 0o700)
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}-", suffix=".tmp", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary_name, 0o600)
        os.replace(temporary_name, path)
    finally:
        try:
            os.unlink(temporary_name)
        except FileNotFoundError:
            pass


def read_checkpoint(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}
    return value if isinstance(value, dict) else {}


def write_checkpoint(path: Path, **fields: Any) -> dict[str, Any]:
    current = read_checkpoint(path)
    current.update(fields)
    current["version"] = CHECKPOINT_VERSION
    current["updated_at"] = _now()
    if "created_at" not in current:
        current["created_at"] = current["updated_at"]
    _atomic_write_text(path, json.dumps(current, ensure_ascii=False, indent=2))
    return current


def resumable_checkpoints(
    state_dir: Path,
    account_type: str = "build",
    max_attempts: int | None = None,
) -> list[Path]:
    jobs_dir = state_dir / "jobs"
    if not jobs_dir.is_dir():
        return []
    result: list[tuple[str, Path]] = []
    for path in jobs_dir.glob("*.json"):
        value = read_checkpoint(path)
        if str(value.get("account_type") or "build") != account_type:
            continue
        if str(value.get("stage") or "") not in RESUMABLE_STAGES:
            continue
        if max_attempts is not None and int(value.get("attempts") or 0) >= max_attempts:
            continue
        if not value.get("email") or not value.get("password"):
            continue
        result.append((str(value.get("updated_at") or ""), path))
    result.sort(key=lambda item: item[0])
    return [path for _, path in result]


def new_checkpoint_path(state_dir: Path, index: int) -> Path:
    jobs_dir = state_dir / "jobs"
    jobs_dir.mkdir(parents=True, exist_ok=True)
    return jobs_dir / f"protocol-{int(time.time())}-{index:05d}-{uuid.uuid4().hex[:8]}.json"


def publish_protocol_credential(
    cfg: dict[str, Any],
    *,
    source: Path,
    spool_dir: Path,
) -> dict[str, Any]:
    from protocol_spool import await_hotload_result, stage_hotload_file

    submitted_at = time.time()
    destination = stage_hotload_file(source, spool_dir)
    await_result = bool(os.environ.get("REGISTRATION_CPA_HOTLOAD_DIR", "").strip()) or bool(
        cfg.get("cpa_hotload_await_result", False)
    )
    if not await_result:
        return {"ok": True, "status": "queued", "path": str(destination)}
    timeout = max(1.0, min(float(cfg.get("cpa_hotload_result_timeout_sec", 300) or 300), 600.0))
    result = await_hotload_result(
        spool_dir,
        source.stem,
        submitted_at=submitted_at,
        timeout=timeout,
    )
    result["path"] = str(destination)
    return result


def write_web_credential(directory: Path, *, email: str, sso: str) -> Path:
    directory.mkdir(parents=True, exist_ok=True)
    filename = "web-sso-" + credential_file_name(email).removeprefix("xai-")
    path = directory / filename
    payload = {
        "provider": "grok_web",
        "accounts": [{"name": email, "sso_token": sso, "tier": "auto"}],
    }
    _atomic_write_text(path, json.dumps(payload, ensure_ascii=False, indent=2) + "\n")
    return path


def preflight(cfg: dict[str, Any]) -> list[str]:
    errors: list[str] = []
    try:
        from xconsole_client import XConsoleAuthClient  # noqa: F401
        from xconsole_client.oauth_protocol import login_with_protocol  # noqa: F401
    except Exception as exc:
        errors.append(f"protocol imports: {exc}")
    return errors


def register_one(
    index: int,
    cfg: dict[str, Any],
    *,
    log_file: Path,
    spool_dir: Path,
    yescaptcha_key: str,
    email_backend: str,
    proxy: str,
    checkpoint_path: Path,
    account_type: str,
) -> dict[str, Any]:
    from xconsole_client import XConsoleAuthClient
    from xconsole_client.oauth_protocol import extract_cookies_from_auth_client
    from xconsole_client.oauth_protocol import login_with_protocol

    if _stop.is_set():
        return {"ok": False, "error": "stopped"}

    checkpoint = read_checkpoint(checkpoint_path)
    email = str(checkpoint.get("email") or "")
    password = str(checkpoint.get("password") or "")
    sso = str(checkpoint.get("sso") or "")
    cookies: Any = checkpoint.get("cookies") or {}
    stage = str(checkpoint.get("stage") or "")
    attempts = int(checkpoint.get("attempts") or 0) + 1
    write_checkpoint(checkpoint_path, attempts=attempts, status="running")
    try:
        if stage == "credential_ready":
            source = Path(str(checkpoint.get("credential_path") or ""))
            if not source.is_file():
                raise RuntimeError(f"checkpoint 凭据文件不存在: {source}")
            import_result = publish_protocol_credential(cfg, source=source, spool_dir=spool_dir)
            if not import_result.get("ok"):
                write_checkpoint(
                    checkpoint_path,
                    stage="credential_ready",
                    status="retryable",
                    last_error=f"spool_import_{import_result.get('status') or 'failed'}",
                )
                raise RuntimeError(f"spool 导入失败: {import_result}")
            write_checkpoint(checkpoint_path, stage="credential_written", status="awaiting_ledger", import_result=import_result)
            return {
                "ok": True,
                "email": email,
                "password": password,
                "spool": str(import_result.get("path") or ""),
                "engine": str(checkpoint.get("oauth_engine") or "protocol"),
            }
        if stage == "credential_written":
            stored_result = checkpoint.get("result") if isinstance(checkpoint.get("result"), dict) else {}
            return {
                "ok": True,
                "email": stored_result.get("email") or email,
                "password": stored_result.get("password") or password,
                "spool": str(checkpoint.get("credential_path") or stored_result.get("cpa_path") or stored_result.get("path") or ""),
                "engine": stored_result.get("engine") or checkpoint.get("oauth_engine") or "protocol",
            }
        if proxy:
            os.environ["HTTPS_PROXY"] = proxy
            os.environ["HTTP_PROXY"] = proxy

        client: Any = None
        if stage not in RESUMABLE_STAGES:
            client = XConsoleAuthClient(debug=False, signup_url=SIGNUP_URL)
            client.visit_home()
            client.load_signup_page()
            log(index, "页面 Cookie/信息抓取成功", log_file)

            email, receiver = make_email(cfg, email_backend)
            password = f"Pw{secrets.token_hex(6)}!aA1"
            write_checkpoint(checkpoint_path, stage="email_ready", email=email, password=password)
            log(index, f"邮箱={email}", log_file)

            client.create_email_validation_code(email)
            code = receiver.wait_for_code(timeout=120)
            if not code or str(code).isdigit():
                raise RuntimeError(f"邮箱验证码无效: {code!r}")
            log(index, "验证码获取成功", log_file)
            client.verify_email_validation_code(email, code)
            client.validate_password(email, password)

            try:
                turnstile = solve_turnstile_token(cfg, proxy=proxy, log_file=log_file, worker_id=index)
            except Exception as captcha_exc:
                log(index, f"本地过盾失败: {captcha_exc}", log_file)
                raise

            res = client.create_account(
                email=email,
                given_name="Grok",
                family_name="User",
                password=password,
                email_validation_code=code,
                turnstile_token=turnstile,
                castle_request_token="",
                conversion_id=str(uuid.uuid4()),
            )
            if not res.ok:
                raise RuntimeError(f"create_account HTTP {res.http_status}")
            write_checkpoint(checkpoint_path, stage="account_created", email=email, password=password)

            sso = resolve_sso(
                client,
                cfg,
                email=email,
                password=password,
                proxy=proxy,
                log_file=log_file,
                worker_id=index,
                inspect_create_response=True,
            )
            if not sso:
                raise RuntimeError("SSO 提取失败")
            cookies = extract_cookies_from_auth_client(client)
            write_checkpoint(checkpoint_path, stage="sso_ready", sso=sso, cookies=cookies)
            log(index, "SSO 获取成功，checkpoint 已保存", log_file)
        else:
            log(index, f"从 checkpoint 续跑 stage={stage} email={email}", log_file)

        if account_type == "web":
            if not sso:
                raise RuntimeError("checkpoint 缺少 Web SSO，无法导入 Web 账号")
            auth_dir = Path(str(cfg.get("cpa_auth_dir") or (checkpoint_path.parent.parent / "web_auths")))
            local_path = write_web_credential(auth_dir, email=email, sso=sso)
            write_checkpoint(
                checkpoint_path,
                stage="credential_ready",
                status="awaiting_import",
                credential_path=str(local_path),
                oauth_engine="protocol_web",
                account_type="web",
            )
            import_result = publish_protocol_credential(cfg, source=local_path, spool_dir=spool_dir)
            if not import_result.get("ok"):
                raise RuntimeError(f"Web SSO 导入失败: {import_result}")
            write_checkpoint(
                checkpoint_path,
                stage="credential_written",
                status="awaiting_ledger",
                import_result=import_result,
            )
            log(index, f"Web SSO 已导入并完成首次同步: {local_path.name}", log_file)
            return {
                "ok": True,
                "email": email,
                "password": password,
                "spool": str(import_result.get("path") or local_path),
                "engine": "protocol_web",
            }

        if stage == "account_created" or not sso:
            client = XConsoleAuthClient(debug=False, signup_url=SIGNUP_URL)
            sso = resolve_sso(
                client,
                cfg,
                email=email,
                password=password,
                proxy=proxy,
                log_file=log_file,
                worker_id=index,
                inspect_create_response=False,
            )
            if not sso:
                raise RuntimeError("checkpoint 缺少 SSO，协议恢复失败")
            cookies = extract_cookies_from_auth_client(client)
            write_checkpoint(checkpoint_path, stage="sso_ready", sso=sso, cookies=cookies)
            log(index, "SSO 协议恢复成功", log_file)

        try:
            oauth = login_with_protocol(
                email,
                password,
                yescaptcha_key=resolve_yescaptcha_key(cfg),
                proxy=proxy or "",
                debug=False,
                session_cookies=cookies,
                auth_client=client,
            )
        except Exception as protocol_error:
            write_checkpoint(checkpoint_path, stage="oauth_failed", status="retryable", last_error=str(protocol_error))
            log(index, f"协议 OAuth 失败: {protocol_error}", log_file)
            raise

        refresh = getattr(oauth, "refresh_token", None) or (oauth.get("refresh_token") if isinstance(oauth, dict) else None)
        access = getattr(oauth, "access_token", None) or (oauth.get("access_token") if isinstance(oauth, dict) else None)
        expires_in = getattr(oauth, "expires_in", None) or (oauth.get("expires_in") if isinstance(oauth, dict) else 0)
        if not refresh:
            raise RuntimeError(f"Build OAuth 失败: {oauth}")
        oauth = {
            "refresh_token": refresh,
            "access_token": access or "",
            "expires_in": expires_in or 0,
            "token_type": getattr(oauth, "token_type", None) or (oauth.get("token_type") if isinstance(oauth, dict) else None),
            "scope": getattr(oauth, "scope", None) or (oauth.get("scope") if isinstance(oauth, dict) else None),
        }

        log(index, "Build OAuth 成功", log_file)

        expires_in = int(oauth.get("expires_in") or 0)
        expired = ""
        if expires_in > 0:
            expired = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + expires_in))

        payload = build_cpa_xai_auth(
            email=email,
            access_token=str(oauth.get("access_token") or ""),
            refresh_token=str(oauth.get("refresh_token") or ""),
            expires_in=int(oauth.get("expires_in") or 0) or None,
            expired=expired or None,
            extra={
                "password": password,
                "sso": sso,
                "source": "protocol_register",
                "oauth_token_type": oauth.get("token_type"),
                "oauth_scope": oauth.get("scope"),
            },
        )
        filename = credential_file_name(email)
        auth_dir = Path(str(cfg.get("cpa_auth_dir") or (checkpoint_path.parent.parent / "cpa_auths")))
        local_path = write_cpa_xai_auth(auth_dir, payload, filename=filename)
        write_checkpoint(
            checkpoint_path,
            stage="credential_ready",
            status="awaiting_import",
            credential_path=str(local_path),
            oauth_engine="protocol",
        )
        import_result = publish_protocol_credential(cfg, source=local_path, spool_dir=spool_dir)
        if not import_result.get("ok"):
            raise RuntimeError(f"spool 导入失败: {import_result}")
        write_checkpoint(
            checkpoint_path,
            stage="credential_written",
            status="awaiting_ledger",
            import_result=import_result,
        )
        log(index, f"凭据已导入并完成首次同步: {local_path.name}", log_file)
        return {
            "ok": True,
            "email": email,
            "password": password,
            "spool": str(import_result.get("path") or local_path),
            "engine": "protocol",
            "refresh_token_prefix": str(oauth.get("refresh_token") or "")[:12],
        }
    except Exception as exc:
        current = read_checkpoint(checkpoint_path)
        current_stage = str(current.get("stage") or "")
        write_checkpoint(
            checkpoint_path,
            stage=current_stage if current_stage in RESUMABLE_STAGES else "failed",
            status="retryable" if current_stage in RESUMABLE_STAGES else "failed",
            last_error=str(exc),
        )
        log(index, f"失败: {exc}", log_file)
        return {
            "ok": False,
            "email": email,
            "password": password,
            "error": str(exc),
            "trace": traceback.format_exc(limit=6),
        }


def compute_target(count: int, extra: int, ledger_path: Path) -> int:
    """协议模式的 count = 本轮要新注册数量（不是历史账本绝对总量）。

    - count>0: 本轮注册 count 个
    - extra>0: 本轮追加 extra 个（与 count 语义相同，兼容 Go 传参）
    - 都为 0: 默认 1
    """
    del ledger_path  # 账本仅用于记录，不参与本轮目标扣减
    if count > 0:
        return count
    if extra > 0:
        return extra
    return 1


def append_ledger(ledger_path: Path, row: dict[str, Any]) -> None:
    ledger_path.parent.mkdir(parents=True, exist_ok=True)
    with ledger_path.open("a", encoding="utf-8", newline="\n") as handle:
        handle.write(json.dumps(row, ensure_ascii=False, separators=(",", ":")))
        handle.write("\n")
        handle.flush()
        os.fsync(handle.fileno())


def migrate_legacy_ledger(legacy_path: Path, ledger_path: Path) -> int:
    if ledger_path.exists() or not legacy_path.is_file():
        return 0
    try:
        rows = json.loads(legacy_path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return 0
    if not isinstance(rows, list):
        return 0
    temporary = ledger_path.with_name(f".{ledger_path.name}-{uuid.uuid4().hex}.tmp")
    try:
        with temporary.open("w", encoding="utf-8", newline="\n") as handle:
            for row in rows:
                if isinstance(row, dict):
                    handle.write(json.dumps(row, ensure_ascii=False, separators=(",", ":")) + "\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, 0o600)
        os.replace(temporary, ledger_path)
    finally:
        temporary.unlink(missing_ok=True)
    return sum(1 for row in rows if isinstance(row, dict))


def main() -> int:
    parser = argparse.ArgumentParser(description="Grok2API protocol registration worker")
    parser.add_argument("--config", required=True)
    parser.add_argument("--state-dir", required=True)
    parser.add_argument("--log-file", required=True)
    parser.add_argument("--count", type=int, default=0)
    parser.add_argument("--extra", type=int, default=0)
    parser.add_argument("--threads", type=int, default=1)
    parser.add_argument("--account-type", choices=("build", "web"), default="build")
    parser.add_argument("--fast", action="store_true")
    parser.add_argument("--preflight", action="store_true")
    args = parser.parse_args()

    config_path = Path(args.config)
    state_dir = Path(args.state_dir)
    log_file = Path(args.log_file)
    log_file.parent.mkdir(parents=True, exist_ok=True)
    state_dir.mkdir(parents=True, exist_ok=True)

    cfg = load_config(config_path)
    cfg["_config_path"] = str(config_path)
    # 让 reg.config 读到同一份配置
    reg.config.clear()
    reg.config.update(cfg)

    if args.preflight:
        errors = preflight(cfg)
        if errors:
            for error in errors:
                print(f"[preflight] FAIL {error}", flush=True)
            return 1
        print("[preflight] OK protocol dependencies", flush=True)
        return 0

    mode = captcha_mode(cfg)
    yescaptcha_key = resolve_yescaptcha_key(cfg)
    if mode == "yescaptcha" and not yescaptcha_key:
        log("main", "缺少 YesCaptcha Key（config.yescaptcha_api_key 或 YESCAPTCHA_API_KEY）", log_file)
        write_state(state_dir, status="failed", error="missing_yescaptcha_api_key")
        return 2
    log("main", f"过盾方式={mode}", log_file)

    configured_backends = [
        str(cfg.get("protocol_email_backend") or cfg.get("email_provider") or "yyds"),
        *[str(value) for value in (cfg.get("email_provider_fallbacks") or [])],
    ]
    email_backends = ["tempmail" if value == "tempmail_lol" else value for value in configured_backends if value]
    proxy = str(cfg.get("proxy") or cfg.get("browser_proxy") or "").strip()
    spool_dir = Path(str(cfg.get("spool_dir") or (ROOT / "cpa_auths" / "incoming")))
    spool_dir.mkdir(parents=True, exist_ok=True)

    legacy_ledger_path = state_dir / "protocol_accounts.json"
    ledger_path = state_dir / "protocol_accounts.jsonl"
    migrated = migrate_legacy_ledger(legacy_ledger_path, ledger_path)
    if migrated:
        log("main", f"已迁移旧协议账本 {migrated} 条到 JSONL", log_file)
    target = compute_target(int(args.count or 0), int(args.extra or 0), ledger_path)
    threads = max(1, min(int(args.threads or 1), 8))
    try:
        attempt_multiplier = max(1, min(int(cfg.get("protocol_attempt_multiplier", 3) or 3), 10))
    except (TypeError, ValueError):
        attempt_multiplier = 3
    try:
        stage_retry_limit = max(1, min(int(cfg.get("protocol_stage_retry_limit", 2) or 2), 5))
    except (TypeError, ValueError):
        stage_retry_limit = 2
    max_attempts = min(30_000, max(target, target * attempt_multiplier))
    resume_queue = resumable_checkpoints(state_dir, args.account_type, stage_retry_limit)

    log(
        "main",
        f"引擎=协议注册 目标可用账号={target} 最大尝试={max_attempts} 线程={threads} "
        f"待续跑={len(resume_queue)} 邮箱={','.join(email_backends)} 导入目录={spool_dir}",
        log_file,
    )
    write_state(
        state_dir,
        status="running",
        engine="protocol",
        target=target,
        done=0,
        attempted=0,
        ok=0,
        failed=0,
        resumable=len(resume_queue),
        started_at=_now(),
    )

    global _done, _attempted, _ok, _failed
    _done = _attempted = _ok = _failed = 0

    def _job(i: int, checkpoint_path: Path) -> dict[str, Any]:
        if _stop.is_set():
            return {"ok": False, "error": "stopped"}
        email_backend = email_backends[(i - 1) % len(email_backends)]
        result = register_one(
            i,
            cfg,
            log_file=log_file,
            spool_dir=spool_dir,
            yescaptcha_key=yescaptcha_key,
            email_backend=email_backend,
            proxy=proxy,
            checkpoint_path=checkpoint_path,
            account_type=args.account_type,
        )
        with _progress_lock:
            global _done, _attempted, _ok, _failed
            _attempted += 1
            if result.get("ok"):
                _ok += 1
                _done = _ok
                append_ledger(
                    ledger_path,
                    {
                        "email": result.get("email"),
                        "password": result.get("password"),
                        "spool": result.get("spool"),
                        "created_at": _now(),
                        "engine": result.get("engine") or "protocol",
                        "job_id": checkpoint_path.stem,
                    },
                )
                write_checkpoint(checkpoint_path, stage="completed", status="completed", ledgered_at=_now())
            else:
                _failed += 1
            write_state(
                state_dir,
                status="running",
                done=_done,
                attempted=_attempted,
                ok=_ok,
                failed=_failed,
                target=target,
                resumable=len(resumable_checkpoints(state_dir, args.account_type, stage_retry_limit)),
            )
        if not args.fast:
            time.sleep(1.5)
        return result

    try:
        with ThreadPoolExecutor(max_workers=threads) as pool:
            active: dict[Future[dict[str, Any]], tuple[int, Path]] = {}
            submitted = 0
            next_index = 1

            def submit_one() -> bool:
                nonlocal submitted, next_index
                if submitted >= max_attempts or _stop.is_set():
                    return False
                if resume_queue:
                    checkpoint_path = resume_queue.pop(0)
                    checkpoint = read_checkpoint(checkpoint_path)
                    index = int(checkpoint.get("index") or next_index)
                else:
                    index = next_index
                    checkpoint_path = new_checkpoint_path(state_dir, index)
                    write_checkpoint(checkpoint_path, index=index, stage="queued", status="queued", account_type=args.account_type)
                next_index = max(next_index + 1, index + 1)
                submitted += 1
                future = pool.submit(_job, index, checkpoint_path)
                active[future] = (index, checkpoint_path)
                return True

            while _ok < target and (_ok + len(active)) < target and len(active) < threads and submit_one():
                pass
            while active:
                finished, _ = wait(tuple(active), return_when=FIRST_COMPLETED)
                for future in finished:
                    index, checkpoint_path = active.pop(future)
                    try:
                        result = future.result()
                        checkpoint = read_checkpoint(checkpoint_path)
                        if (
                            not result.get("ok")
                            and str(checkpoint.get("stage") or "") in RESUMABLE_STAGES
                            and int(checkpoint.get("attempts") or 0) < stage_retry_limit
                        ):
                            resume_queue.append(checkpoint_path)
                    except Exception as exc:
                        log("main", f"工作线程异常 index={index}: {exc}", log_file)
                while _ok < target and (_ok + len(active)) < target and len(active) < threads and submit_one():
                    pass
    except KeyboardInterrupt:
        _stop.set()
        log("main", "任务被中断", log_file)

    status = "已完成" if _ok >= target else ("部分成功" if _ok else "失败")
    write_state(
        state_dir,
        status=status,
        done=_done,
        attempted=_attempted,
        ok=_ok,
        failed=_failed,
        target=target,
        resumable=len(resumable_checkpoints(state_dir, args.account_type, stage_retry_limit)),
        finished_at=_now(),
    )
    log("main", f"任务结束 状态={status} 成功={_ok}/{target} 失败尝试={_failed} 总尝试={_attempted}", log_file)
    if _ok >= target:
        return 0
    return 3 if _ok > 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
