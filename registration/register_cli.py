"""CLI wrapper for grok_register_ttk — multi-thread register + async CPA mint pipeline.

Architecture:
  Register workers (R)  →  accounts_cli + mint_queue
  Mint workers (M)      →  cpa_auths/xai-*.json + optional hotload

Browser lifecycle:
  - One Chromium per register worker, reused via TabPool.clear_session
  - Full recycle every N accounts or on error
  - Register browser released BEFORE mint (mint always standalone Chromium)
  - Peak browsers ≈ R + M (not 2×R)
"""
from __future__ import annotations

import argparse
import json
import os
import queue
import signal
import shutil
import sys
import tempfile
import threading
import time
import traceback
from pathlib import Path
from typing import Any

# 强制走本目录的 grok_register_ttk
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))


def _bootstrap_runtime_environment(arguments: list[str]) -> None:
    mappings = {
        "--config": "REGISTRATION_CONFIG_FILE",
        "--state-dir": "REGISTRATION_DATA_DIR",
    }
    for index, value in enumerate(arguments[:-1]):
        environment_key = mappings.get(value)
        if environment_key:
            os.environ[environment_key] = os.path.abspath(arguments[index + 1])


_bootstrap_runtime_environment(sys.argv[1:])

if __name__ == "__main__" and len(sys.argv) > 1 and sys.argv[1] == "--protocol-worker":
    sys.argv.pop(1)
    from protocol_register_cli import main as protocol_main

    raise SystemExit(protocol_main())

import grok_register_ttk as reg  # noqa: E402


# Linux 适配: DrissionPage 默认找 'chrome', 我们装的是 chromium
# 保留原版 slim flags + proxy，再补 chromium 路径与 turnstilePatch。
_orig_create_browser_options = reg.create_browser_options


def _patched_create_browser_options(proxy_override=None):
    # Prefer original factory (proxy + CHROMIUM_SLIM_FLAGS + extension)
    try:
        opts = _orig_create_browser_options(proxy_override=proxy_override)
    except Exception:
        from DrissionPage import ChromiumOptions

        opts = ChromiumOptions()
        opts.auto_port()
        opts.set_timeouts(base=1)
        for flag in getattr(reg, "CHROMIUM_SLIM_FLAGS", ()) or ():
            try:
                opts.set_argument(flag)
            except Exception:
                pass

    try:
        opts.auto_port()
    except Exception:
        pass
    try:
        opts.set_timeouts(base=1)
    except Exception:
        pass

    from browser_runtime import apply_browser_runtime

    apply_browser_runtime(opts)

    ext_path = os.path.join(os.path.dirname(os.path.abspath(reg.__file__)), "turnstilePatch")
    if os.path.isdir(ext_path):
        try:
            opts.add_extension(ext_path)
        except Exception:
            pass
    return opts


reg.create_browser_options = _patched_create_browser_options


# ── 线程安全日志 ──

_log_queue: queue.Queue = queue.Queue()


def _log_writer():
    while True:
        msg = _log_queue.get()
        if msg is None:
            break
        print(msg, flush=True)


def log(worker_id: int | str, msg: str) -> None:
    _log_queue.put(f"[{time.strftime('%H:%M:%S')}] [W{worker_id}] {msg}")


def browser_preflight(config_path: str, state_dir: str, log_file: str) -> int:
    from browser_runtime import browser_mode, browser_path

    checks: dict[str, dict[str, Any]] = {}

    def add(name: str, ok: bool, detail: str) -> None:
        checks[name] = {"ok": bool(ok), "detail": detail}

    add("config", os.path.isfile(config_path), config_path)
    add("grok_register_ttk", hasattr(reg, "create_browser_options"), reg.__file__)
    try:
        import DrissionPage  # noqa: F401

        add("DrissionPage", True, "importable")
    except Exception as exc:
        add("DrissionPage", False, str(exc))

    try:
        from browser_proxy import proxy_auth_supported

        for name, raw_proxy in (
            ("browserProxyAuth", str(reg.config.get("proxy") or "")),
            ("cpaBrowserProxyAuth", str(reg.config.get("cpa_proxy") or reg.config.get("proxy") or "")),
        ):
            supported, detail = proxy_auth_supported(raw_proxy)
            add(name, supported, detail)
    except Exception as exc:
        add("browserProxyAuth", False, str(exc))
        add("cpaBrowserProxyAuth", False, str(exc))

    executable = browser_path()
    add("chromium", bool(executable and os.path.isfile(executable)), executable or "not found")
    extension_dir = Path(reg.__file__).resolve().parent / "turnstilePatch"
    for filename in ("manifest.json", "content.js"):
        path = extension_dir / filename
        add("turnstilePatch/" + filename, path.is_file(), str(path))

    mode = browser_mode("headed")
    display_ok = sys.platform != "linux" or mode == "headless" or bool(os.getenv("DISPLAY")) or bool(shutil.which("Xvfb"))
    add("display", display_ok, f"mode={mode} DISPLAY={os.getenv('DISPLAY', '')!r}")
    signup_ok, signup_detail = browser_registration_page_ready(reg.config)
    add("registrationPage", signup_ok, signup_detail)
    for name, path in (("state_dir", state_dir), ("log_dir", os.path.dirname(log_file))):
        try:
            os.makedirs(path, mode=0o700, exist_ok=True)
            with tempfile.NamedTemporaryFile(prefix=".preflight-", dir=path, delete=True):
                pass
            add(name, True, path)
        except Exception as exc:
            add(name, False, f"{path}: {exc}")

    ok = all(item["ok"] for item in checks.values())
    print(json.dumps({"ok": ok, "checks": checks}, ensure_ascii=False), flush=True)
    return 0 if ok else 1


def browser_registration_page_ready(config: dict[str, Any]) -> tuple[bool, str]:
    """Verify that the selected egress reaches the actual sign-up UI.

    A TCP-successful Cloudflare denial is not a usable registration route. Keep
    this check in the standalone CLI too, because it is often invoked directly
    rather than through the Go controller's preflight endpoint.
    """
    target = str(config.get("signup_url") or "https://accounts.x.ai/sign-up?redirect=grok-com").strip()
    browser = None
    try:
        # Do not use requests/urllib here. Cloudflare can accept their HTTP
        # fingerprint while rejecting the actual Chromium registration tab.
        browser, page = reg.start_browser()
        page.get(target)
        time.sleep(0.8)
        current_url = str(getattr(page, "url", "") or "")
        title = str(getattr(page, "title", "") or "").lower()
        html = str(getattr(page, "html", "") or "")[:64 * 1024].lower()
    except Exception as exc:
        return False, f"{target}: {type(exc).__name__}: {str(exc)[:180]}"
    finally:
        try:
            if browser is not None:
                reg.stop_browser()
        except Exception:
            pass

    page_text = title + "\n" + html
    if "attention required! | cloudflare" in page_text or "/cdn-cgi/challenge-platform/" in page_text:
        return False, f"{target}: Cloudflare challenge ({current_url or 'no final URL'})"
    if current_url and "accounts.x.ai" not in current_url.lower():
        return False, f"{target}: unexpected final URL {current_url[:180]}"
    return True, f"{target}: Chromium loaded {current_url or target}"


# ── 统计 ──

_stats_lock = threading.Lock()
_accounts_file_lock = threading.Lock()
_stats = {
    "reg_success": 0,
    "reg_fail": 0,
    "mint_success": 0,
    "mint_fail": 0,
    "mint_skip": 0,
}
_progress_state_path = ""
_progress_target: int | None = None
_progress_cpa_required = True
_progress_account_type = "build"
_pending_state_path = ""
_pending_lock = threading.Lock()
_pending_jobs: dict[str, dict[str, Any]] = {}
_metrics_state_path = ""
_metrics_lock = threading.Lock()
_metrics_started_at = ""
_metrics_finished_at = ""
_metrics_batch_started_monotonic = 0.0
_metrics_records: list[dict[str, Any]] = []
_metrics_peak_memory_mib = 0.0
_metrics_peak_browser_processes = 0
_stop_event = threading.Event()


def _request_stop(_signum: int | None = None, _frame: Any | None = None) -> None:
    _stop_event.set()


def _write_progress_state_locked(*, status: str = "running", finished: bool = False) -> None:
    if not _progress_state_path:
        return
    registered = int(_stats.get("reg_success", 0))
    registration_failed = int(_stats.get("reg_fail", 0))
    mint_success = int(_stats.get("mint_success", 0))
    done = min(registered, mint_success) if _progress_cpa_required else registered
    attempted = registered + registration_failed
    payload = {
        "status": status,
        "target": _progress_target,
        "done": done,
        "attempted": attempted,
        "ok": done,
        "failed": max(0, attempted - done),
        "registered": registered,
        "registration_failed": registration_failed,
        "mint_success": mint_success,
        "mint_failed": int(_stats.get("mint_fail", 0)),
        "mint_skipped": int(_stats.get("mint_skip", 0)),
        "resumable": _pending_count(_progress_account_type),
        "updated_at": time.strftime("%Y-%m-%d %H:%M:%S"),
    }
    if finished:
        payload["finished_at"] = payload["updated_at"]
    path = os.path.abspath(_progress_state_path)
    os.makedirs(os.path.dirname(path), mode=0o700, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=".browser-state-", suffix=".tmp", dir=os.path.dirname(path))
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, ensure_ascii=False, indent=2)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, 0o600)
        os.replace(temporary, path)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


def _pending_count(account_type: str | None = None) -> int:
    with _pending_lock:
        if account_type is None:
            return len(_pending_jobs)
        selected = "web" if str(account_type).lower() == "web" else "build"
        return sum(
            1
            for job in _pending_jobs.values()
            if str(job.get("accountType") or "build").lower() == selected
        )


def _write_pending_jobs_locked() -> None:
    if not _pending_state_path:
        return
    path = os.path.abspath(_pending_state_path)
    os.makedirs(os.path.dirname(path), mode=0o700, exist_ok=True)
    if not _pending_jobs:
        try:
            os.unlink(path)
        except FileNotFoundError:
            pass
        return
    payload = {
        "version": 1,
        "jobs": list(_pending_jobs.values()),
        "updatedAt": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
    }
    descriptor, temporary = tempfile.mkstemp(prefix=".browser-pending-oauth-", suffix=".tmp", dir=os.path.dirname(path))
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, ensure_ascii=False, indent=2)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, 0o600)
        os.replace(temporary, path)
        os.chmod(path, 0o600)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


def _load_pending_jobs(path: str) -> int:
    global _pending_state_path
    resolved = os.path.abspath(path)
    loaded: dict[str, dict[str, Any]] = {}
    if os.path.exists(resolved):
        with open(resolved, encoding="utf-8") as handle:
            payload = json.load(handle)
        raw_jobs = payload.get("jobs", []) if isinstance(payload, dict) else []
        if not isinstance(raw_jobs, list):
            raise ValueError("pending OAuth state has an invalid jobs field")
        for raw in raw_jobs:
            if not isinstance(raw, dict):
                continue
            email = str(raw.get("email") or "").strip()
            password = str(raw.get("password") or "")
            if not email or not password:
                continue
            loaded[email] = {
                "email": email,
                "password": password,
                "sso": str(raw.get("sso") or ""),
                "cookies": raw.get("cookies") if isinstance(raw.get("cookies"), list) else [],
                "idx": int(raw.get("idx") or 0),
                "accountType": "web" if str(raw.get("accountType") or "build").lower() == "web" else "build",
                "autoNSFW": bool(raw.get("autoNSFW")),
                "mintAttempts": max(0, int(raw.get("mintAttempts") or 0)),
                "updatedAt": str(raw.get("updatedAt") or ""),
            }
    with _pending_lock:
        _pending_state_path = resolved
        _pending_jobs.clear()
        _pending_jobs.update(loaded)
    try:
        os.chmod(resolved, 0o600)
    except FileNotFoundError:
        pass
    return len(loaded)


def _persist_pending_job(job: dict[str, Any], *, mint_attempts: int = 0) -> None:
    email = str(job.get("email") or "").strip()
    password = str(job.get("password") or "")
    if not email or not password:
        return
    record = {
        "email": email,
        "password": password,
        "sso": str(job.get("sso") or ""),
        "cookies": job.get("cookies") if isinstance(job.get("cookies"), list) else [],
        "idx": int(job.get("idx") or 0),
        "accountType": "web" if str(job.get("accountType") or "build").lower() == "web" else "build",
        "autoNSFW": bool(job.get("autoNSFW")),
        "mintAttempts": max(0, int(mint_attempts)),
        "updatedAt": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
    }
    with _pending_lock:
        _pending_jobs[email] = record
        _write_pending_jobs_locked()


def _remove_pending_job(email: str) -> None:
    with _pending_lock:
        if _pending_jobs.pop(str(email or "").strip(), None) is not None:
            _write_pending_jobs_locked()


def _pending_job_snapshot() -> list[dict[str, Any]]:
    with _pending_lock:
        return [dict(job) for job in _pending_jobs.values()]


def _inc(key: str, n: int = 1) -> None:
    with _stats_lock:
        _stats[key] = _stats.get(key, 0) + n
        _write_progress_state_locked()


def _percentile(values: list[float], percentile: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    rank = max(0, min(len(ordered) - 1, int((len(ordered) - 1) * percentile + 0.999999)))
    return round(float(ordered[rank]), 3)


def _metrics_payload_locked() -> dict[str, Any]:
    public_records = [{key: value for key, value in record.items() if not key.startswith("_")} for record in _metrics_records]
    succeeded = [record for record in public_records if record.get("status") == "succeeded"]

    def durations(name: str) -> list[float]:
        return [float(record[name]) for record in succeeded if isinstance(record.get(name), (int, float))]

    return {
        "startedAt": _metrics_started_at,
        "finishedAt": _metrics_finished_at or None,
        "summary": {
            "attempts": len(public_records),
            "succeeded": len(succeeded),
            "failed": sum(1 for record in public_records if record.get("status") == "failed"),
            "successRate": round(len(succeeded) / len(public_records), 4) if public_records else 0,
            "turnstileReuseCount": sum(int(record.get("turnstileReuseCount") or 0) for record in public_records),
            "oauthFailed": sum(1 for record in public_records if record.get("oauthOK") is False),
            "importFailed": sum(1 for record in public_records if record.get("importOK") is False),
            "syncFailed": sum(int(record.get("syncFailed") or 0) for record in public_records),
            "registrationSeconds": {
                "p50": _percentile(durations("registrationSeconds"), 0.50),
                "p95": _percentile(durations("registrationSeconds"), 0.95),
            },
            "oauthSeconds": {
                "p50": _percentile(durations("oauthSeconds"), 0.50),
                "p95": _percentile(durations("oauthSeconds"), 0.95),
            },
            "pipelineSeconds": {
                "p50": _percentile(durations("pipelineSeconds"), 0.50),
                "p95": _percentile(durations("pipelineSeconds"), 0.95),
            },
            "peakMemoryMiB": round(_metrics_peak_memory_mib, 2),
            "peakBrowserProcesses": _metrics_peak_browser_processes,
            "batchSeconds": round(max(0.0, time.monotonic() - _metrics_batch_started_monotonic), 3) if _metrics_batch_started_monotonic else 0,
        },
        "attempts": public_records,
    }


def _write_metrics_locked() -> None:
    if not _metrics_state_path:
        return
    path = Path(_metrics_state_path).resolve()
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=".browser-metrics-", suffix=".tmp", dir=str(path.parent))
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            json.dump(_metrics_payload_locked(), handle, ensure_ascii=False, indent=2)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, 0o600)
        os.replace(temporary, path)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


def _start_account_metric(worker_id: int, idx: int, attempt: int) -> dict[str, Any]:
    record = {
        "id": f"{idx}-{attempt}",
        "index": idx,
        "attempt": attempt,
        "worker": worker_id,
        "status": "running",
        "startedAt": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "turnstileReuseCount": 0,
        "_startedMonotonic": time.monotonic(),
        "_profileStartedMonotonic": None,
    }
    with _metrics_lock:
        _metrics_records.append(record)
        _write_metrics_locked()
    return record


def _update_account_metric(record: dict[str, Any], **values: Any) -> None:
    with _metrics_lock:
        record.update(values)
        _write_metrics_locked()


def _metric_log(record: dict[str, Any], worker_id: int, message: str) -> None:
    now = time.monotonic()
    updates: dict[str, Any] = {}
    if "Turnstile 已通过" in message and record.get("turnstileFirstPassSeconds") is None:
        baseline = record.get("_profileStartedMonotonic") or record.get("_startedMonotonic") or now
        updates["turnstileFirstPassSeconds"] = round(now - float(baseline), 3)
    if "Turnstile 二次复用完成" in message:
        updates["turnstileReuseCount"] = int(record.get("turnstileReuseCount") or 0) + 1
    if updates:
        _update_account_metric(record, **updates)
    log(worker_id, message)


def _resource_sampler(stop_event: threading.Event) -> None:
    global _metrics_peak_memory_mib, _metrics_peak_browser_processes
    try:
        import psutil

        process = psutil.Process(os.getpid())
    except Exception:
        return
    while not stop_event.wait(0.5):
        try:
            processes = [process, *process.children(recursive=True)]
            memory = sum(item.memory_info().rss for item in processes if item.is_running()) / (1024 * 1024)
            browser_count = 0
            for item in processes:
                try:
                    name = item.name().lower()
                except Exception:
                    continue
                if any(marker in name for marker in ("chromium", "chrome", "msedge")):
                    browser_count += 1
            with _metrics_lock:
                _metrics_peak_memory_mib = max(_metrics_peak_memory_mib, memory)
                _metrics_peak_browser_processes = max(_metrics_peak_browser_processes, browser_count)
        except Exception:
            continue


# forever 任务索引
_next_idx_lock = threading.Lock()
_next_idx = [1]
_replacement_max_index = [0]

# mint 队列结束哨兵
_MINT_STOP = object()


def resolve_mint_workers(
    *,
    cli_value: int,
    threads: int,
    config: dict,
    inline_mint: bool,
) -> int:
    """Resolve mint worker count.

    Priority: --inline-mint > CLI --mint-workers (>=0) > config cpa_mint_workers > auto.
    auto (-1): match registration threads when CPA export is enabled, else 0.
    0: inline mint on register threads.
    """
    if inline_mint:
        return 0
    if cli_value >= 0:
        return max(0, min(int(cli_value), 10))
    cfg_v = config.get("cpa_mint_workers", -1)
    try:
        cfg_v = int(cfg_v)
    except Exception:
        cfg_v = -1
    if cfg_v >= 0:
        return max(0, min(cfg_v, 10))
    # auto
    if config.get("cpa_export_enabled", True):
        return max(1, min(int(threads), 10))
    return 0


def resolve_mint_queue_max(config: dict, mint_workers: int, cli_value: int | None = None) -> int:
    if cli_value is not None and cli_value >= 0:
        return int(cli_value)
    try:
        v = int(config.get("cpa_mint_queue_max", 0) or 0)
    except Exception:
        v = 0
    if v > 0:
        return v
    # default backpressure: 2 × mint workers (0 if no mint pool)
    return max(0, mint_workers * 2) if mint_workers > 0 else 0


class DummyStop:
    def __call__(self) -> bool:
        return _stop_event.is_set()


def _ensure_browser(worker_id: int, force_recycle: bool = False):
    """Start browser if missing; optional full recycle."""
    if force_recycle:
        try:
            reg.stop_browser()
        except Exception:
            pass
    if reg.TabPool.get_browser() is None:
        reg.start_browser(log_callback=lambda m: log(worker_id, m))


def register_one(
    worker_id: int,
    idx: int,
    total: int,
    accounts_file: str,
    *,
    attempt: int = 1,
    do_mint_inline: bool = False,
    mint_queue: queue.Queue | None = None,
    account_type: str = "build",
    auto_nsfw: bool = False,
) -> dict | None:
    """Run one registration. Enqueue CPA mint (default) instead of blocking.

    Returns dict(email, sso, profile) or None.
    """
    email = ""
    dev_token = ""
    job: dict[str, Any] | None = None
    metric = _start_account_metric(worker_id, idx, attempt)
    metric_log = lambda message: _metric_log(metric, worker_id, message)
    current_config = getattr(reg, "config", {}) or {}
    max_mail_retry = max(1, min(int(current_config.get("mail_retry_count", 3) or 3), 5))
    primary_provider = reg.get_email_provider()
    raw_fallbacks = current_config.get("email_provider_fallbacks", [])
    if isinstance(raw_fallbacks, str):
        raw_fallbacks = [item.strip() for item in raw_fallbacks.split(",") if item.strip()]
    fallbacks = [str(item).strip() for item in raw_fallbacks if str(item).strip()]
    providers = list(dict.fromkeys([primary_provider, *fallbacks]))
    cancel = DummyStop()

    try:
        _ensure_browser(worker_id, force_recycle=False)
    except Exception as exc:
        log(worker_id, f"! 浏览器启动失败: {exc}")
        _update_account_metric(metric, status="failed", failureStage="browser", pipelineSeconds=round(time.monotonic() - metric["_startedMonotonic"], 3))
        return None

    for mail_try in range(1, max_mail_retry + 1):
        email_provider = providers[min(mail_try - 1, len(providers) - 1)]
        try:
            log(worker_id, f"[mail] provider={email_provider} attempt={mail_try}/{max_mail_retry}")
            log(worker_id, f"--- 第 {idx}/{total} 个账号, 邮箱尝试 {mail_try}/{max_mail_retry} ---")
            log(worker_id, "1. 打开注册页")
            reg.open_signup_page(log_callback=lambda m: log(worker_id, m), cancel_callback=cancel)
            log(worker_id, "2. 创建邮箱并提交")
            email, dev_token = reg.fill_email_and_submit(
                log_callback=lambda m: log(worker_id, m),
                cancel_callback=cancel,
                email_provider=email_provider,
            )
            log(worker_id, f"邮箱: {email}")
            log(worker_id, "3. 拉取验证码")
            code = reg.fill_code_and_submit(
                email,
                dev_token,
                log_callback=lambda m: log(worker_id, m),
                cancel_callback=cancel,
                email_provider=email_provider,
            )
            log(worker_id, "验证码获取成功")
            _update_account_metric(metric, emailSeconds=round(time.monotonic() - metric["_startedMonotonic"], 3))
            break
        except Exception as exc:
            msg = str(exc)
            if _stop_event.is_set():
                log(worker_id, "! stop requested during email stage")
                _update_account_metric(metric, status="stopped", failureStage="cancelled", pipelineSeconds=round(time.monotonic() - metric["_startedMonotonic"], 3))
                return None
            retryable = any(
                marker in msg.lower()
                for marker in ("verification", "did not receive", "email_submit_")
            )
            if (retryable or "未收到验证码" in msg or "验证码" in msg) and mail_try < max_mail_retry:
                log(worker_id, f"! 本邮箱未取到验证码，换邮箱重试: {msg}")
                try:
                    reg.restart_browser(log_callback=lambda m: log(worker_id, m))
                except Exception:
                    pass
                reg.sleep_with_cancel(1, cancel)
                continue
            log(worker_id, f"! 邮箱阶段失败: {msg}")
            _update_account_metric(metric, status="failed", failureStage="email", pipelineSeconds=round(time.monotonic() - metric["_startedMonotonic"], 3))
            traceback.print_exc()
            try:
                reg.restart_browser(log_callback=lambda m: log(worker_id, m))
            except Exception:
                pass
            return None

    try:
        log(worker_id, "4. 填写资料")
        _update_account_metric(metric, _profileStartedMonotonic=time.monotonic())
        profile = reg.fill_profile_and_submit(
            log_callback=metric_log, cancel_callback=cancel
        )
        log(worker_id, f"资料已填: {profile.get('given_name')} {profile.get('family_name')}")
        log(worker_id, "5. 等待 sso cookie")
        sso = reg.wait_for_sso_cookie(
            log_callback=metric_log, cancel_callback=cancel
        )
        _update_account_metric(metric, registrationSeconds=round(time.monotonic() - metric["_startedMonotonic"], 3), status="registered")
        password = profile.get("password", "") or ""
        line = f"{email}----{password}----{sso}\n"
        with _accounts_file_lock:
            with open(accounts_file, "a", encoding="utf-8") as f:
                f.write(line)
        log(worker_id, f"+ 注册成功: {email}")
        reg.mark_used(email, password)

        job = {
            "email": email,
            "password": password,
            "sso": sso,
            "profile": profile,
            "idx": idx,
            "cookies": [],
            "accountType": account_type,
            "autoNSFW": bool(auto_nsfw and account_type == "web"),
            "_metric": metric,
        }
        # Persist immediately after SSO/ledger success so a stop or downstream
        # credential failure never causes a replacement account registration.
        _persist_pending_job(job)

        # Capture cookies BEFORE releasing browser (for mint cookie inject)
        page = reg._get_page()
        cookies = []
        try:
            import cpa_export as _cpa_exp

            cookies = _cpa_exp.export_cookies_from_page(page) if page is not None else []
        except Exception:
            cookies = []
        if cookies:
            log(worker_id, f"[*] 导出 cookie {len(cookies)} 条供 mint 注入")
            job["cookies"] = cookies
            if account_type == "build":
                _persist_pending_job(job)

        if page and reg.PERF_FLAGS.get("cookie_snapshot", True):
            try:
                reg.save_cookies_snapshot(page, "success", email)
            except Exception:
                pass
        try:
            reg.add_token_to_grok2api_pools(
                sso, email=email, log_callback=lambda m: log(worker_id, m)
            )
        except Exception as exc:
            log(worker_id, f"[Debug] grok2api: {exc}")

        # Release / recycle register browser BEFORE mint so peak browsers ≈ R+M
        try:
            reg.prepare_browser_for_next_account(log_callback=lambda m: log(worker_id, m))
        except Exception:
            try:
                reg.stop_browser()
            except Exception:
                pass

        if account_type == "web":
            import_started = time.monotonic()
            import_result = _run_web_import_with_retry(f"R{worker_id}", job, getattr(reg, "config", {}) or {})
            import_ok = bool(import_result.get("ok"))
            _update_account_metric(
                metric,
                importSeconds=round(time.monotonic() - import_started, 3),
                importOK=import_ok,
                syncFailed=int(import_result.get("syncFailed") or 0),
                status="succeeded" if import_ok else "failed",
                failureStage=None if import_ok else "import",
                pipelineSeconds=round(time.monotonic() - metric["_startedMonotonic"], 3),
            )
            if not import_result.get("ok"):
                raise RuntimeError(f"Web SSO 导入失败: {import_result}")
        elif do_mint_inline:
            _run_mint_with_retry(f"R{worker_id}", job, getattr(reg, "config", {}) or {})
        elif mint_queue is not None:
            # backpressure: wait while queue is saturated
            qmax = int(getattr(mint_queue, "_reg_qmax", 0) or 0)
            while qmax > 0 and mint_queue.qsize() >= qmax:
                log(worker_id, f"[cpa] mint 队列背压 qsize={mint_queue.qsize()}≥{qmax}，等待...")
                time.sleep(1.0)
            mint_queue.put(job)
            log(worker_id, f"[cpa] enqueued mint for {email} (queue≈{mint_queue.qsize()})")
        else:
            log(worker_id, "[cpa] mint skipped (no queue / inline)")
            _update_account_metric(metric, status="failed", oauthOK=False, failureStage="oauth", pipelineSeconds=round(time.monotonic() - metric["_startedMonotonic"], 3))

        _inc("reg_success")
        return job
    except Exception as exc:
        log(worker_id, f"! 注册失败: {exc}")
        _update_account_metric(metric, status="failed", failureStage=str(metric.get("failureStage") or "registration"), pipelineSeconds=round(time.monotonic() - metric["_startedMonotonic"], 3))
        if job is not None:
            log(worker_id, "[recoverable] SSO 已写入账本，保留同账号 OAuth/import pending 状态")
            _inc("reg_success")
            return job
        if _stop_event.is_set():
            _update_account_metric(metric, status="stopped", failureStage="cancelled")
            return None
        reg.mark_error(email or "", reason=str(exc)[:120])
        traceback.print_exc()
        try:
            reg.restart_browser(log_callback=lambda m: log(worker_id, m))
        except Exception:
            pass
        return None


def _run_web_import_job(worker_id: int | str, job: dict[str, Any], config: dict) -> dict:
    from protocol_register_cli import publish_protocol_credential, write_web_credential

    email = str(job.get("email") or "")
    sso = str(job.get("sso") or "")
    if not email or not sso:
        return {"ok": False, "error": "missing_email_or_sso", "email": email}
    data_dir = Path(os.environ.get("REGISTRATION_DATA_DIR") or os.path.dirname(__file__)).resolve()
    auth_dir = Path(str(config.get("cpa_auth_dir") or (data_dir / "web_auths"))).resolve()
    spool_dir = Path(
        os.environ.get("REGISTRATION_CPA_HOTLOAD_DIR")
        or str(config.get("spool_dir") or (data_dir / "spool" / "incoming"))
    ).resolve()
    source = write_web_credential(auth_dir, email=email, sso=sso, auto_nsfw=bool(job.get("autoNSFW")))
    result = publish_protocol_credential(config, source=source, spool_dir=spool_dir)
    if result.get("ok"):
        log(worker_id, f"Web SSO 已导入并完成首次同步: {email}")
    else:
        log(worker_id, f"Web SSO 导入失败: {result}")
    return result


def _run_web_import_with_retry(
    worker_id: int | str,
    job: dict[str, Any],
    config: dict[str, Any],
    *,
    count_batch_stats: bool = True,
) -> dict[str, Any]:
    email = str(job.get("email") or "").strip()
    attempts = _mint_retry_attempts(config)
    _persist_pending_job(job)
    result: dict[str, Any] = {"ok": False, "error": "web_import_not_attempted"}
    for attempt in range(1, attempts + 1):
        _persist_pending_job(job, mint_attempts=attempt)
        if attempt > 1:
            log(worker_id, f"[web] retrying same registered account import ({attempt}/{attempts})")
        result = _run_web_import_job(worker_id, job, config)
        if result.get("ok"):
            _remove_pending_job(email)
            if count_batch_stats:
                _inc("mint_success")
            return {**result, "import_attempts": attempt}
        if attempt < attempts:
            try:
                delay = max(0.0, min(float(config.get("cpa_mint_retry_delay_sec", 2) or 0), 30.0))
            except (TypeError, ValueError):
                delay = 2.0
            if delay:
                time.sleep(delay)
    if count_batch_stats:
        _inc("mint_fail")
    return {**result, "import_attempts": attempts, "resumable": bool(email)}


def _run_mint_job(
    worker_id: int | str,
    job: dict[str, Any],
    config: dict,
    *,
    count_stats: bool = True,
) -> dict:
    """Standalone CPA mint (own Chromium). Never reuses register browser."""
    email = job.get("email") or ""
    password = job.get("password") or ""
    metric = job.get("_metric") if isinstance(job.get("_metric"), dict) else None
    mint_started = time.monotonic()
    if not email or not password:
        if count_stats:
            _inc("mint_fail")
        if metric is not None:
            _update_account_metric(metric, status="failed", oauthOK=False, failureStage="oauth", pipelineSeconds=round(time.monotonic() - metric["_startedMonotonic"], 3))
        return {"ok": False, "error": "missing email/password", "email": email}
    if not config.get("cpa_export_enabled", True):
        if count_stats:
            _inc("mint_skip")
        log(worker_id, f"[cpa] export disabled, skip {email}")
        if metric is not None:
            _update_account_metric(metric, status="failed", oauthOK=False, failureStage="oauth", pipelineSeconds=round(time.monotonic() - metric["_startedMonotonic"], 3))
        return {"ok": False, "skipped": True, "email": email}
    try:
        import cpa_export

        # page=None always — force standalone path inside export
        result = cpa_export.export_cpa_xai_for_account(
            email,
            password,
            page=None,
            cookies=job.get("cookies"),
            sso=job.get("sso") or "",
            config=config,
            log_callback=lambda m: log(worker_id, m),
        )
        if result.get("ok"):
            log(worker_id, f"+ CPA auth: {result.get('path')}")
            if count_stats:
                _inc("mint_success")
        elif result.get("skipped"):
            if count_stats:
                _inc("mint_skip")
            log(worker_id, f"[cpa] skipped: {result.get('reason')}")
        else:
            if count_stats:
                _inc("mint_fail")
            log(worker_id, f"! CPA auth 未成功: {result.get('error') or result}")
        if metric is not None:
            import_result = result.get("hotload_import") if isinstance(result.get("hotload_import"), dict) else {}
            oauth_ok = bool(result.get("oauth_ok", result.get("ok")))
            import_ok = bool(import_result.get("ok")) if import_result else oauth_ok
            _update_account_metric(
                metric,
                oauthSeconds=float(result.get("oauth_seconds") or round(time.monotonic() - mint_started, 3)),
                importSeconds=float(result.get("import_seconds") or 0),
                oauthOK=oauth_ok,
                importOK=import_ok,
                synced=int(import_result.get("synced") or 0),
                syncFailed=int(import_result.get("syncFailed") or 0),
                status="succeeded" if oauth_ok and import_ok else "failed",
                failureStage=None if oauth_ok and import_ok else "oauth" if not oauth_ok else "import",
                pipelineSeconds=round(time.monotonic() - metric["_startedMonotonic"], 3),
            )
        return result
    except Exception as exc:
        if count_stats:
            _inc("mint_fail")
        log(worker_id, f"! CPA export 异常: {exc}")
        traceback.print_exc()
        if metric is not None:
            _update_account_metric(metric, status="failed", oauthOK=False, failureStage="oauth", oauthSeconds=round(time.monotonic() - mint_started, 3), pipelineSeconds=round(time.monotonic() - metric["_startedMonotonic"], 3))
        return {"ok": False, "error": str(exc), "email": email}


def _mint_retry_attempts(config: dict[str, Any]) -> int:
    try:
        if "cpa_mint_retry_attempts" in config:
            raw = config.get("cpa_mint_retry_attempts")
        elif "cpa_mint_retry_count" in config:
            raw = 1 + int(config.get("cpa_mint_retry_count") or 0)
        else:
            raw = 2
        return max(1, min(int(raw), 5))
    except (TypeError, ValueError):
        return 2


def _run_mint_with_retry(
    worker_id: int | str,
    job: dict[str, Any],
    config: dict[str, Any],
    *,
    count_batch_stats: bool = True,
) -> dict[str, Any]:
    email = str(job.get("email") or "").strip()
    attempts = _mint_retry_attempts(config)
    _persist_pending_job(job)
    result: dict[str, Any] = {"ok": False, "error": "mint_not_attempted"}
    for attempt in range(1, attempts + 1):
        _persist_pending_job(job, mint_attempts=attempt)
        if attempt > 1:
            log(worker_id, f"[cpa] retrying same registered account ({attempt}/{attempts})")
        result = _run_mint_job(worker_id, job, config, count_stats=False)
        if result.get("ok"):
            _remove_pending_job(email)
            if count_batch_stats:
                _inc("mint_success")
            return {**result, "mint_attempts": attempt}
        if result.get("skipped"):
            _remove_pending_job(email)
            if count_batch_stats:
                _inc("mint_skip")
            return {**result, "mint_attempts": attempt}
        if attempt < attempts:
            try:
                delay = max(0.0, min(float(config.get("cpa_mint_retry_delay_sec", 2) or 0), 30.0))
            except (TypeError, ValueError):
                delay = 2.0
            if delay:
                time.sleep(delay)
    if count_batch_stats:
        _inc("mint_fail")
    return {**result, "mint_attempts": attempts, "resumable": bool(email)}


def _resume_pending_jobs(config: dict[str, Any], account_type: str) -> tuple[int, int]:
    succeeded = 0
    failed = 0
    selected = "web" if account_type == "web" else "build"
    jobs = [job for job in _pending_job_snapshot() if str(job.get("accountType") or "build").lower() == selected]
    for number, job in enumerate(jobs, start=1):
        log("RESUME", f"[{selected}] resuming pending credential import {number}")
        if selected == "web":
            result = _run_web_import_with_retry("RESUME", job, config, count_batch_stats=False)
        else:
            result = _run_mint_with_retry("RESUME", job, config, count_batch_stats=False)
        if result.get("ok") or result.get("skipped"):
            succeeded += 1
        else:
            failed += 1
    return succeeded, failed


def _resume_pending_mint_jobs(config: dict[str, Any]) -> tuple[int, int]:
    return _resume_pending_jobs(config, "build")


def registration_exit_code(
    stats: dict[str, int],
    *,
    cpa_export_enabled: bool,
    expected_successes: int | None = None,
    resumable: int = 0,
) -> int:
    registered = stats.get("reg_success", 0)
    required = registered if expected_successes is None else max(0, expected_successes)
    if registered < required or (expected_successes is None and registered <= 0):
        return 1
    if cpa_export_enabled and resumable > 0:
        return 2
    if cpa_export_enabled and stats.get("mint_success", 0) < required:
        return 2
    return 0


def _register_worker(
    worker_id: int,
    task_queue: queue.Queue,
    total: int,
    accounts_file: str,
    mint_queue: queue.Queue | None,
    forever: bool,
    do_mint_inline: bool,
    account_type: str,
    auto_nsfw: bool = False,
):
    while not _stop_event.is_set():
        try:
            idx = task_queue.get_nowait()
        except queue.Empty:
            if not forever:
                break
            with _next_idx_lock:
                nxt = _next_idx[0]
                _next_idx[0] = nxt + 5
            for i in range(nxt, nxt + 5):
                task_queue.put(i)
            continue

        retry = 0
        result = None
        while retry < 2 and not _stop_event.is_set():
            try:
                result = register_one(
                    worker_id,
                    idx,
                    total,
                    accounts_file,
                    attempt=retry + 1,
                    do_mint_inline=do_mint_inline,
                    mint_queue=mint_queue,
                    account_type=account_type,
                    auto_nsfw=auto_nsfw,
                )
                if result:
                    break
                retry += 1
                if retry < 2 and not _stop_event.is_set():
                    log(worker_id, f"[retry] 账号 {idx} 失败，重试 {retry}/1")
                    try:
                        reg.restart_browser(log_callback=lambda m: log(worker_id, m))
                    except Exception:
                        pass
            except Exception:
                retry += 1
                if retry < 2 and not _stop_event.is_set():
                    log(worker_id, f"[retry] 账号 {idx} 异常，重试 {retry}/1")
                    traceback.print_exc()
                    try:
                        reg.restart_browser(log_callback=lambda m: log(worker_id, m))
                    except Exception:
                        pass

        if not result and not _stop_event.is_set():
            _inc("reg_fail")
            if not forever:
                with _next_idx_lock:
                    replacement = _next_idx[0]
                    if _replacement_max_index[0] > 0 and replacement <= _replacement_max_index[0]:
                        _next_idx[0] = replacement + 1
                    else:
                        replacement = 0
                if replacement > 0:
                    task_queue.put(replacement)
                    log(worker_id, f"[replacement] 账号 {idx} 注册耗尽，补充槽位 {replacement}")
                else:
                    log(worker_id, f"[replacement] 账号 {idx} 注册耗尽，已达到批次尝试上限")

    # worker exit: free browser
    try:
        reg.stop_browser()
    except Exception:
        pass
    log(worker_id, "register worker exit")


def _mint_worker(worker_id: str, mint_queue: queue.Queue, config: dict):
    while True:
        job = mint_queue.get()
        try:
            if job is _MINT_STOP:
                break
            if not isinstance(job, dict):
                continue
            if _stop_event.is_set():
                log(worker_id, f"[cpa] stop requested; keeping pending OAuth for {job.get('email') or '(unknown)'}")
                continue
            _run_mint_with_retry(worker_id, job, config)
        finally:
            mint_queue.task_done()
    try:
        from cpa_xai.browser_confirm import shutdown_mint_browsers

        shutdown_mint_browsers()
    except Exception:
        pass
    log(worker_id, "mint worker exit")


def main() -> int:
    parser = argparse.ArgumentParser(description="CLI runner for grok_register_ttk (pipelined).")
    parser.add_argument("--count", type=int, default=1, help="账号总数目标（0=不限；含已有）")
    parser.add_argument(
        "--resume-only",
        action="store_true",
        help="仅恢复已有 pending OAuth/import 任务；不创建新账号",
    )
    parser.add_argument(
        "--extra",
        type=int,
        default=0,
        help="在已有 accounts 基础上再新注册 N 个",
    )
    parser.add_argument("--threads", type=int, default=1, help="注册并发线程数（1-10）")
    parser.add_argument("--account-type", choices=("build", "web"), default="build", help="最终导入 Web SSO 或 Build OAuth 账号")
    parser.add_argument("--auto-nsfw", action="store_true", help="Web credential import enables NSFW")
    parser.add_argument("--config", default=os.environ.get("REGISTRATION_CONFIG_FILE") or os.path.join(os.path.dirname(__file__), "config.json"))
    parser.add_argument("--state-dir", default=os.environ.get("REGISTRATION_DATA_DIR") or os.path.dirname(__file__))
    parser.add_argument("--log-file", default="")
    parser.add_argument("--proxy", default=None)
    parser.add_argument("--preflight", action="store_true")
    parser.add_argument(
        "--mint-workers",
        type=int,
        default=-1,
        help="CPA mint 并发：-1=用 config/auto；0=内联；1-10=固定。覆盖 config.cpa_mint_workers",
    )
    parser.add_argument(
        "--mint-queue-max",
        type=int,
        default=-1,
        help="mint 队列背压上限：-1=用 config/auto(2×workers)；0=不限制",
    )
    parser.add_argument("--accounts-file", default=os.path.join(os.path.dirname(__file__), "accounts_cli.txt"))
    parser.add_argument("--fast", action="store_true", default=True, help="快速模式（默认开）：压缩 sleep、关截图")
    parser.add_argument("--no-fast", action="store_true", help="关闭快速模式")
    parser.add_argument("--no-browser-reuse", action="store_true", help="每号强制 quit 浏览器")
    parser.add_argument("--browser-recycle-every", type=int, default=25, help="复用 N 次后完整回收")
    parser.add_argument("--cookie-snapshot", action="store_true", help="注册成功写 cookie 快照（默认关，fast）")
    parser.add_argument("--inline-mint", action="store_true", help="强制注册线程内联 mint（调试用）")
    args = parser.parse_args()
    _stop_event.clear()
    for signal_name in ("SIGINT", "SIGTERM"):
        value = getattr(signal, signal_name, None)
        if value is not None:
            try:
                signal.signal(value, _request_stop)
            except ValueError:
                # Embedded test runners may invoke main() outside the main thread.
                pass
    global _progress_state_path, _progress_target, _progress_cpa_required, _progress_account_type
    global _pending_state_path
    global _metrics_state_path, _metrics_started_at, _metrics_finished_at, _metrics_batch_started_monotonic
    global _metrics_peak_memory_mib, _metrics_peak_browser_processes

    reg.load_config()
    if args.proxy is not None:
        reg.config["proxy"] = args.proxy
    cfg0 = getattr(reg, "config", {}) or {}
    if args.preflight:
        log_file = os.path.abspath(args.log_file or os.path.join(args.state_dir, "registration.log"))
        return browser_preflight(os.path.abspath(args.config), os.path.abspath(args.state_dir), log_file)
    threads = max(1, min(args.threads, 10))
    fast = bool(args.fast) and not bool(args.no_fast)

    mint_workers = resolve_mint_workers(
        cli_value=args.mint_workers,
        threads=threads,
        config=cfg0,
        inline_mint=bool(args.inline_mint),
    )
    if args.account_type == "web":
        mint_workers = 0
    do_mint_inline = mint_workers == 0
    mint_qmax = resolve_mint_queue_max(
        cfg0,
        mint_workers,
        cli_value=(None if args.mint_queue_max < 0 else args.mint_queue_max),
    )

    # perf knobs
    reg.configure_perf(
        fast=fast,
        sleep_scale=0.15 if fast else 1.0,
        skip_debug_io=fast,
        cookie_snapshot=bool(args.cookie_snapshot) or not fast,
        async_side_effects=True,
        browser_reuse=not args.no_browser_reuse,
        browser_recycle_every=max(1, int(args.browser_recycle_every)),
    )

    # 断点续跑
    done_count = 0
    if os.path.exists(args.accounts_file):
        with open(args.accounts_file) as f:
            done_count = sum(1 for line in f if line.strip())

    if args.resume_only:
        remaining = 0
        args.count = done_count
        print(
            f"[*] 仅恢复 pending {args.account_type} 任务（已有账本 {done_count}，不创建新账号），"
            f"注册线程={threads} mint_workers={mint_workers} mint_queue_max={mint_qmax} fast={fast}",
            flush=True,
        )
    elif args.extra and args.extra > 0:
        target_total = done_count + args.extra
        remaining = args.extra
        print(
            f"[*] 配置加载完成，额外新注册 {args.extra} 个（当前已有 {done_count} → 目标 {target_total}），"
            f"注册线程={threads} mint_workers={mint_workers} mint_queue_max={mint_qmax} fast={fast}",
            flush=True,
        )
        args.count = target_total
    elif args.count == 0:
        remaining = None
        print(
            f"[*] 配置加载完成，不限数量，注册线程={threads} mint_workers={mint_workers} mint_queue_max={mint_qmax} fast={fast}",
            flush=True,
        )
    else:
        remaining = max(0, args.count - done_count)
        print(
            f"[*] 配置加载完成，目标 {args.count} 个账号，注册线程={threads} "
            f"mint_workers={mint_workers} mint_queue_max={mint_qmax} fast={fast}",
            flush=True,
        )
    print(f"[*] accounts_file = {args.accounts_file}", flush=True)
    if done_count > 0:
        print(f"[*] 断点续跑：已完成 {done_count}", flush=True)
    data_dir = os.path.abspath(args.state_dir)
    _progress_state_path = os.path.join(data_dir, "browser_state.json")
    _progress_target = remaining
    _progress_cpa_required = args.account_type == "web" or bool(cfg0.get("cpa_export_enabled", True))
    _progress_account_type = args.account_type
    _pending_state_path = os.path.join(data_dir, "browser_pending_oauth.json")
    try:
        pending_count = _load_pending_jobs(_pending_state_path)
    except Exception as exc:
        print(f"[!] pending OAuth state could not be loaded: {type(exc).__name__}: {exc}", flush=True)
        return 1
    pending_count = _pending_count(args.account_type)
    if pending_count:
        print(f"[*] pending {args.account_type} credential imports to resume: {pending_count}", flush=True)
    with _metrics_lock:
        _metrics_state_path = os.path.join(data_dir, "browser_metrics.json")
        _metrics_started_at = time.strftime("%Y-%m-%dT%H:%M:%S%z")
        _metrics_finished_at = ""
        _metrics_batch_started_monotonic = time.monotonic()
        _metrics_peak_memory_mib = 0.0
        _metrics_peak_browser_processes = 0
        _metrics_records.clear()
        _write_metrics_locked()
    with _stats_lock:
        for key in _stats:
            _stats[key] = 0
        _write_progress_state_locked(status="running")
    log_thread = threading.Thread(target=_log_writer, daemon=True)
    log_thread.start()
    resource_stop = threading.Event()
    resource_thread = threading.Thread(target=_resource_sampler, args=(resource_stop,), daemon=True, name="resource-sampler")
    resource_thread.start()

    if pending_count:
        resumed, resume_failed = _resume_pending_jobs(cfg0, args.account_type)
        print(f"[*] pending credential recovery: succeeded={resumed} failed={resume_failed}", flush=True)
        with _stats_lock:
            _write_progress_state_locked(status="running")

    if remaining is not None and remaining <= 0:
        with _stats_lock:
            s = dict(_stats)
        exit_code = registration_exit_code(
            s,
            cpa_export_enabled=_progress_cpa_required,
            expected_successes=0,
            resumable=_pending_count(args.account_type),
        )
        state_status = "completed" if exit_code == 0 else "partial"
        with _stats_lock:
            _write_progress_state_locked(status=state_status, finished=True)
        _log_queue.put(None)
        log_thread.join(timeout=2)
        resource_stop.set()
        resource_thread.join(timeout=2)
        with _metrics_lock:
            _metrics_finished_at = time.strftime("%Y-%m-%dT%H:%M:%S%z")
            _write_metrics_locked()
        if exit_code == 0:
            print("[*] no new registration is needed and all pending OAuth/import jobs are resolved", flush=True)
        return exit_code

    try:
        reg.TabPool.init(reg.create_browser_options, log_callback=lambda m: log(0, m))
    except Exception as exc:
        print(f"[!] 浏览器初始化失败: {exc}", flush=True)
        with _stats_lock:
            _write_progress_state_locked(status="failed", finished=True)
        resource_stop.set()
        resource_thread.join(timeout=2)
        with _metrics_lock:
            _metrics_finished_at = time.strftime("%Y-%m-%dT%H:%M:%S%z")
            _write_metrics_locked()
        return 1

    task_queue: queue.Queue = queue.Queue()
    mint_queue: queue.Queue | None = queue.Queue() if args.account_type == "build" and not do_mint_inline else None
    if mint_queue is not None:
        mint_queue._reg_qmax = mint_qmax  # type: ignore[attr-defined]
    global _next_idx, _replacement_max_index
    if remaining is not None:
        for i in range(done_count + 1, args.count + 1):
            task_queue.put(i)
        try:
            attempt_multiplier = int(cfg0.get("browser_attempt_multiplier", cfg0.get("protocol_attempt_multiplier", 3)) or 3)
        except (TypeError, ValueError):
            attempt_multiplier = 3
        attempt_multiplier = max(1, min(attempt_multiplier, 10))
        _next_idx[0] = args.count + 1
        _replacement_max_index[0] = done_count + remaining * attempt_multiplier
    else:
        _next_idx[0] = done_count + 1
        _replacement_max_index[0] = 0
        for i in range(done_count + 1, done_count + threads * 5 + 1):
            task_queue.put(i)
        _next_idx[0] = done_count + threads * 5 + 1

    forever = remaining is None
    cfg = getattr(reg, "config", {}) or {}

    # mint workers first (so queue consumers ready)
    mint_threads: list[threading.Thread] = []
    if mint_queue is not None and mint_workers > 0:
        for i in range(1, mint_workers + 1):
            wid = f"M{i}"
            t = threading.Thread(
                target=_mint_worker,
                args=(wid, mint_queue, cfg),
                daemon=True,
                name=f"mint-{i}",
            )
            t.start()
            mint_threads.append(t)

    reg_threads: list[threading.Thread] = []
    for wid in range(1, threads + 1):
        t = threading.Thread(
            target=_register_worker,
            args=(wid, task_queue, args.count, args.accounts_file, mint_queue, forever, do_mint_inline, args.account_type, args.auto_nsfw),
            daemon=True,
            name=f"reg-{wid}",
        )
        t.start()
        reg_threads.append(t)

    try:
        while any(t.is_alive() for t in reg_threads):
            for t in reg_threads:
                t.join(timeout=0.25)
    except KeyboardInterrupt:
        _request_stop()
        print("\n[!] 用户中断", flush=True)
    if _stop_event.is_set():
        print("[!] stop requested; preserving registered accounts for resume", flush=True)
        for t in reg_threads:
            t.join(timeout=5)

    # drain mint queue
    if mint_queue is not None:
        if _stop_event.is_set():
            while True:
                try:
                    queued = mint_queue.get_nowait()
                except queue.Empty:
                    break
                else:
                    if queued is not _MINT_STOP and isinstance(queued, dict):
                        log(0, f"[cpa] preserving pending OAuth for {queued.get('email') or '(unknown)'}")
                    mint_queue.task_done()
        else:
            log(0, f"[cpa] 等待 mint 队列清空（qsize≈{mint_queue.qsize()}）...")
            mint_queue.join()
        for _ in mint_threads:
            mint_queue.put(_MINT_STOP)
        for t in mint_threads:
            t.join(timeout=5 if _stop_event.is_set() else 600)

    try:
        reg.shutdown_browser()
    except Exception:
        pass

    # stop side-effect pool
    try:
        pool = getattr(reg, "_side_effect_pool", None)
        if pool is not None:
            pool.shutdown(wait=False, cancel_futures=True)
    except Exception:
        pass

    _log_queue.put(None)
    log_thread.join(timeout=2)
    resource_stop.set()
    resource_thread.join(timeout=2)

    with _stats_lock:
        s = dict(_stats)
    print(
        f"=== 完成: 注册成功 {s.get('reg_success', 0)}, 注册失败 {s.get('reg_fail', 0)}, "
        f"CPA成功 {s.get('mint_success', 0)}, CPA失败 {s.get('mint_fail', 0)}, "
        f"CPA跳过 {s.get('mint_skip', 0)} ===",
        flush=True,
    )
    exit_code = registration_exit_code(
        s,
        cpa_export_enabled=_progress_cpa_required,
        expected_successes=remaining,
        resumable=_pending_count(args.account_type),
    )
    state_status = "completed" if exit_code == 0 else ("partial" if s.get("reg_success", 0) > 0 else "failed")
    with _stats_lock:
        _write_progress_state_locked(status=state_status, finished=True)
    with _metrics_lock:
        _metrics_finished_at = time.strftime("%Y-%m-%dT%H:%M:%S%z")
        _write_metrics_locked()
    return exit_code


if __name__ == "__main__":
    sys.exit(main())
