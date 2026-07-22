"""Approve xAI device-code in Chromium (DrissionPage).

Paths resolve relative to the grok_reg project root (parent of cpa_xai).

Proven flow (2026-07-10, free account):
  1. Open verification_uri_complete (user_code prefilled)
  2. Click 继续 on device page
  3. Cookie / 隐私偏好 banner: 全部允许 BEFORE OAuth 允许 (modal blocks consent)
  4. 使用邮箱登录 → fill email → 下一步
  5. Wait cf-turnstile-response → fill password → REAL click 登录
  6. May land /account redirect or device page → 继续
  7. Consent page /oauth2/device/consent → REAL click exact 允许
     (by_js click causes Invalid action / empty form action)
  8. /oauth2/device/done "设备已授权" + token poll SUCCESS

Hard rules:
  - Token poll is source of truth
  - Button match is EXACT text only (允许 ≠ 全部允许)
  - Cookie modal must be dismissed before consent Allow
  - Consent Allow MUST be a real click, not by_js
  - Prefer headed browser + register turnstilePatch
"""

from __future__ import annotations

import os
import re
import sys
import tempfile
import threading
import time
from pathlib import Path
from typing import Any, Callable

from log_safety import redact_url

LogFn = Callable[[str], None]


def _noop_log(_: str) -> None:
    return None


class BrowserConfirmError(RuntimeError):
    pass


def _sleep(sec: float) -> None:
    time.sleep(sec)



def _project_root() -> Path:
    return Path(__file__).resolve().parents[1]


def _debug_shot_dir() -> Path:
    d = _project_root() / "screenshots"
    d.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(d, 0o700)
    return d


def _safe_tag(s: str) -> str:
    s = (s or "na").strip()
    out = []
    for ch in s:
        if ch.isalnum() or ch in ("@", ".", "-", "_"):
            out.append(ch)
        else:
            out.append("_")
    return "".join(out)[:80] or "na"


def _write_private_text(path: Path, value: str) -> None:
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}-", suffix=".tmp", dir=path.parent)
    temporary = Path(temporary_name)
    try:
        os.chmod(temporary, 0o600)
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            descriptor = -1
            handle.write(value)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
        os.chmod(path, 0o600)
    finally:
        if descriptor >= 0:
            os.close(descriptor)
        temporary.unlink(missing_ok=True)


def _save_debug_shot(
    page: Any,
    *,
    tag: str,
    email: str = "",
    log: LogFn | None = None,
) -> str | None:
    """Save page screenshot for failed Turnstile/auth; never raise."""
    log = log or _noop_log
    try:
        ts = time.strftime("%Y%m%d-%H%M%S")
        name = f"{ts}_{_safe_tag(email)}_{_safe_tag(tag)}.png"
        path = _debug_shot_dir() / name
        # DrissionPage: page.get_screenshot(path=...) or .screenshot
        saved = None
        for kwargs in (
            {"path": str(path), "full_page": True},
            {"path": str(path)},
            {"name": str(path)},
        ):
            try:
                if hasattr(page, "get_screenshot"):
                    page.get_screenshot(**kwargs)
                    saved = path
                    break
            except TypeError:
                continue
            except Exception:
                continue
        if saved is None and hasattr(page, "get_screenshot"):
            try:
                page.get_screenshot(path=str(path))
                saved = path
            except Exception:
                pass
        if saved is None:
            # last resort: capture via CDP-ish run_js not available; try tab screenshot attr
            try:
                data = page.run_js(
                    "return document.documentElement && document.documentElement.outerHTML ? 'html-ok' : 'no'"
                )
                log(f"截图接口不可用，已保存页面诊断信息：{data}")
            except Exception:
                pass
            log(f"截图失败，阶段={tag}")
            return None
        os.chmod(saved, 0o600)
        # also dump short text/url alongside
        try:
            meta = path.with_suffix(".txt")
            url = redact_url(_page_url(page))
            vis = _norm(_visible_text(page))[:800]
            _write_private_text(meta, f"url={url}\nemail={email}\ntag={tag}\nvisible={vis}\n")
        except Exception:
            pass
        log(f"调试截图已保存：{saved}")
        return str(saved)
    except Exception as e:  # noqa: BLE001
        log(f"截图异常：{e}")
        return None


def _is_turnstile_challenge(text: str) -> bool:
    t = text or ""
    tl = t.lower()
    needles = (
        "确认您是真人",
        "确认你是真人",
        "verify you are human",
        "confirm you are human",
        "just a moment",
        "checking your browser",
        "cf-turnstile",
        "进行人机验证",
        "人机验证",
    )
    return any(n in t or n in tl for n in needles)

def create_standalone_page(
    *,
    proxy: str | None = None,
    headless: bool = False,
    log: LogFn | None = None,
) -> tuple[Any, Any]:
    log = log or _noop_log
    try:
        from DrissionPage import Chromium, ChromiumOptions
    except ImportError as e:
        raise BrowserConfirmError(
            "DrissionPage not installed; run inside grok_reg uv env or pip install DrissionPage"
        ) from e

    from .proxyutil import proxy_log_label, resolve_proxy

    proxy = resolve_proxy(proxy)
    opts = None
    configured_by_factory = False
    # Project root = parent of this package (./cpa_xai → ../)
    _pkg_root = Path(__file__).resolve().parents[1]
    try:
        reg_file = _pkg_root / "grok_register_ttk.py"
        if reg_file.is_file():
            reg_dir = str(_pkg_root)
            if reg_dir not in sys.path:
                sys.path.insert(0, reg_dir)
            try:
                from grok_register_ttk import create_browser_options  # type: ignore

                opts = create_browser_options(proxy_override=proxy)
                configured_by_factory = True
                log("已复用注册浏览器配置（含 Turnstile 扩展）")
            except Exception as e:  # noqa: BLE001
                log(f"注册浏览器配置不可用：{e}")
                opts = None
    except Exception as e:  # noqa: BLE001
        log(f"检测注册浏览器配置失败：{e}")
        opts = None

    if opts is None:
        opts = ChromiumOptions()
        opts.auto_port()
        opts.set_timeouts(base=2)
        for flag in (
            "--disable-gpu",
            "--no-sandbox",
            "--disable-dev-shm-usage",
            "--mute-audio",
            "--no-first-run",
            "--disable-background-networking",
            "--window-size=1280,900",
        ):
            opts.set_argument(flag)
        ext = str(_pkg_root / "turnstilePatch")
        if os.path.isdir(ext):
            try:
                opts.add_extension(ext)
                log(f"已加载浏览器扩展：{ext}")
            except Exception as e:  # noqa: BLE001
                log(f"加载浏览器扩展失败：{e}")

    from browser_runtime import apply_browser_runtime

    apply_browser_runtime(opts, default_headless=headless, log=log)

    from browser_proxy import configure_chromium_proxy

    chrome_proxy = configure_chromium_proxy(opts, proxy) if not configured_by_factory else ""
    if chrome_proxy:
        log(f"浏览器代理：{proxy_log_label(proxy)}（Chromium {chrome_proxy}）")
    elif proxy and configured_by_factory:
        log(f"浏览器代理：{proxy_log_label(proxy)}（已由注册浏览器配置加载）")
    else:
        log("浏览器代理：直连")

    browser = Chromium(opts)
    try:
        from browser_runtime import hide_browser_windows

        hide_browser_windows(getattr(browser, "process_id", None))
    except Exception:
        pass
    page = browser.latest_tab
    log("独立 Chromium 已启动")
    return browser, page


def close_standalone(browser: Any) -> None:
    try:
        browser.quit()
    except Exception:
        pass


# ── mint browser reuse (per-thread) ──
_mint_tls = threading.local()


def _mint_tls_get() -> dict[str, Any]:
    d = getattr(_mint_tls, "state", None)
    if d is None:
        d = {"browser": None, "page": None, "served": 0, "proxy": None, "headless": None}
        _mint_tls.state = d
    return d


def clear_page_session(page: Any, browser: Any | None = None, log: LogFn | None = None) -> None:
    """Blank page + wipe storage/cookies for reuse between mint jobs."""
    log = log or _noop_log
    try:
        if page is not None:
            try:
                page.get("about:blank")
            except Exception:
                pass
            for js in (
                "try{localStorage.clear()}catch(e){}",
                "try{sessionStorage.clear()}catch(e){}",
            ):
                try:
                    page.run_js(js)
                except Exception:
                    pass
        for target in (page, browser):
            if target is None:
                continue
            try:
                target.set.cookies.clear()  # type: ignore[attr-defined]
                log("CPA 浏览器会话已清理")
                break
            except Exception:
                try:
                    # older API
                    cks = target.cookies()
                    if isinstance(cks, list):
                        for c in cks:
                            try:
                                target.set.cookies.remove(c)  # type: ignore[attr-defined]
                            except Exception:
                                pass
                except Exception:
                    pass
    except Exception as e:
        log(f"清理浏览器会话失败：{e}")


def normalize_cookies(cookies: Any) -> list[dict[str, Any]]:
    """Normalize DrissionPage / browser cookie list to settable dicts.

    Also clones SSO-like cookies onto accounts.x.ai / auth.x.ai domains so
    device-auth can skip secondary login when possible.
    """
    out: list[dict[str, Any]] = []
    if not cookies:
        return out
    if isinstance(cookies, dict):
        for k, v in cookies.items():
            if k and v is not None:
                out.append({"name": str(k), "value": str(v), "domain": ".x.ai", "path": "/"})
        cookies = out
        out = []
    if not isinstance(cookies, (list, tuple)):
        return out
    for c in cookies:
        if not isinstance(c, dict):
            continue
        name = c.get("name") or c.get("Name")
        value = c.get("value") or c.get("Value")
        if not name or value is None:
            continue
        domain = str(c.get("domain") or c.get("Domain") or ".x.ai")
        path = str(c.get("path") or c.get("Path") or "/")
        item = {
            "name": str(name),
            "value": str(value),
            "domain": domain,
            "path": path,
        }
        for src, dst in (
            ("expiry", "expiry"),
            ("expires", "expiry"),
            ("secure", "secure"),
            ("httpOnly", "httpOnly"),
            ("sameSite", "sameSite"),
        ):
            if src in c and c[src] is not None:
                item[dst] = c[src]
        out.append(item)

    # Expand SSO cookies to xAI account hosts (register browser is often on grok.com)
    sso_names = {"sso", "sso-rw", "cf_clearance", "sso_jwt", "__cf_bm"}
    extras: list[dict[str, Any]] = []
    seen = {(i["name"], i["domain"], i["path"]) for i in out}
    for item in list(out):
        n = item["name"]
        if n not in sso_names and not n.startswith("sso"):
            continue
        for dom in (".x.ai", "accounts.x.ai", ".accounts.x.ai", "auth.x.ai", ".auth.x.ai"):
            key = (n, dom, item["path"])
            if key in seen:
                continue
            clone = dict(item)
            clone["domain"] = dom
            extras.append(clone)
            seen.add(key)
    out.extend(extras)
    return out


def inject_cookies(page: Any, cookies: Any, log: LogFn | None = None) -> int:
    """Inject cookies into page/browser. Returns count attempted."""
    log = log or _noop_log
    items = normalize_cookies(cookies)
    if not items or page is None:
        return 0
    for url in (
        "https://accounts.x.ai/",
        "https://auth.x.ai/",
        "https://grok.com/",
    ):
        try:
            page.get(url)
            _sleep(0.4)
        except Exception:
            continue

    n = 0
    for target_name, target in (("page", page), ("browser", getattr(page, "browser", None))):
        if target is None:
            continue
        try:
            target.set.cookies(items)  # type: ignore[attr-defined]
            n = len(items)
            log(f"批量注入 Cookie 成功：目标={target_name}，数量={n}")
            break
        except Exception as e:
            log(f"批量注入 Cookie 失败：目标={target_name}，错误={e}")

    if n == 0:
        for item in items:
            ok = False
            for target in (page, getattr(page, "browser", None)):
                if target is None:
                    continue
                try:
                    target.set.cookies(item)  # type: ignore[attr-defined]
                    ok = True
                    break
                except Exception:
                    continue
            if ok:
                n += 1
        log(f"逐条注入 Cookie：{n}/{len(items)}")

    # JS document.cookie for non-httpOnly SSO cookies (best effort)
    try:
        js_items = [
            c
            for c in items
            if (not c.get("httpOnly")) and c.get("name") in {"sso", "sso-rw", "cf_clearance"}
        ]
        if js_items:
            page.get("https://accounts.x.ai/")
            for c in js_items:
                name = str(c["name"])
                val = str(c["value"])
                # avoid quote breakage
                if "'" in name or "'" in val:
                    continue
                page.run_js(
                    "document.cookie='"
                    + name
                    + "="
                    + val
                    + "; path=/; domain=.x.ai; Secure; SameSite=None'"
                )
            log(f"Cookie 脚本回退已执行：{len(js_items)} 条")
    except Exception as e:
        log(f"Cookie 脚本回退失败：{e}")

    return n


def acquire_mint_browser(

    *,
    proxy: str | None = None,
    headless: bool = False,
    reuse: bool = True,
    recycle_every: int = 15,
    log: LogFn | None = None,
) -> tuple[Any, Any, bool]:
    """Return (browser, page, owned). owned=True means caller must close if not reusing.

    When reuse=True, browser is kept in thread-local and cleared between jobs.
    """
    log = log or _noop_log
    st = _mint_tls_get()
    if reuse and st.get("browser") is not None:
        # recycle if proxy/headless changed or served enough
        need_recycle = (
            st.get("proxy") != (proxy or None)
            or st.get("headless") != headless
            or (recycle_every > 0 and int(st.get("served") or 0) >= recycle_every)
        )
        if not need_recycle:
            page = st.get("page")
            browser = st.get("browser")
            clear_page_session(page, browser, log=log)
            log(f"复用 CPA 浏览器，已处理 {st.get('served')} 个账号")
            return browser, page, False
        log("CPA 浏览器达到回收条件，正在重建")
        try:
            close_standalone(st.get("browser"))
        except Exception:
            pass
        st["browser"] = None
        st["page"] = None
        st["served"] = 0

    browser, page = create_standalone_page(proxy=proxy, headless=headless, log=log)
    if reuse:
        st["browser"] = browser
        st["page"] = page
        st["proxy"] = proxy or None
        st["headless"] = headless
        st["served"] = 0
        return browser, page, False
    return browser, page, True


def release_mint_browser(
    *,
    owned: bool,
    success: bool = True,
    force_quit: bool = False,
    log: LogFn | None = None,
) -> None:
    log = log or _noop_log
    st = _mint_tls_get()
    if force_quit or owned:
        browser = st.get("browser") if not owned else None
        # if owned, caller passes via closing create path — handle both
        if owned:
            # owned browser not in tls
            return
        if browser is not None:
            close_standalone(browser)
        st["browser"] = None
        st["page"] = None
        st["served"] = 0
        log("CPA 浏览器已关闭")
        return
    if success:
        st["served"] = int(st.get("served") or 0) + 1
    else:
        # fail: drop browser to avoid dirty state
        if st.get("browser") is not None:
            close_standalone(st.get("browser"))
            st["browser"] = None
            st["page"] = None
            st["served"] = 0
            log("CPA 失败后已丢弃当前浏览器会话")


def shutdown_mint_browsers() -> None:
    st = getattr(_mint_tls, "state", None)
    if not st:
        return
    if st.get("browser") is not None:
        close_standalone(st.get("browser"))
    st["browser"] = None
    st["page"] = None
    st["served"] = 0


def _page_url(page: Any) -> str:
    try:
        return page.url or ""
    except Exception:
        return ""


def _visible_text(page: Any) -> str:
    try:
        t = page.run_js(
            "return (document.body && (document.body.innerText || document.body.textContent)) || '';"
        )
        if isinstance(t, str) and t.strip():
            return t
    except Exception:
        pass
    try:
        raw = getattr(page, "raw_text", None)
        if callable(raw):
            t = raw()
            if isinstance(t, str) and t.strip():
                return t
        if isinstance(raw, str) and raw.strip():
            return raw
    except Exception:
        pass
    return ""


def _norm(s: str) -> str:
    return re.sub(r"\s+", " ", (s or "").strip())


def _find_button_exact(page: Any, label: str) -> Any | None:
    try:
        for el in page.eles("tag:button") or []:
            try:
                if _norm(el.text or "") == label:
                    return el
            except Exception:
                continue
    except Exception:
        pass
    try:
        return page.ele(f"xpath://button[normalize-space(.)='{label}']", timeout=0.3)
    except Exception:
        return None


def _cookie_banner_visible(text: str) -> bool:
    """Strong signals only — avoid false-positive on 隐私政策 / ToS links."""
    t = text or ""
    tl = t.lower()
    strong = (
        "隐私偏好",
        "全部允许",
        "全部拒绝",
        "privacy preference",
        "privacy preferences",
        "manage cookies",
        "we use cookies",
        "我们使用 cookie",
        "接受所有 cookie",
        "accept all cookies",
        "cookie preferences",
    )
    return any(n in t or n in tl for n in strong)


def _dismiss_cookie_banner(page: Any, log: LogFn) -> bool:
    """Dismiss xAI/OneTrust-style cookie/privacy modal so consent Allow is clickable.

    Prefer 全部允许 (Accept all). Never click bare 允许 here — that is OAuth consent.
    Returns True if a dismiss action was attempted/succeeded.
    """
    text = _visible_text(page)
    if not _cookie_banner_visible(text):
        return False

    # Exact labels only — 允许 alone is OAuth, not cookie
    labels = [
        "全部允许",
        "接受所有",
        "接受全部",
        "Accept all",
        "Accept All",
        "Allow all",
        "Allow All",
        "I agree",
        "Agree",
    ]
    hit = _click_exact(page, labels, log, real=False)
    if hit:
        log(f"已关闭 Cookie 提示：{hit!r}")
        _sleep(0.8)
        return True

    # JS: click highest z-index / dialog button matching accept-all text
    try:
        ok = page.run_js(
            """
const want = new Set([
  '全部允许','接受所有','接受全部','Accept all','Accept All','Allow all','Allow All','I agree','Agree'
]);
const btns = Array.from(document.querySelectorAll('button, [role="button"], a'));
const match = btns.find((b) => want.has(String(b.innerText || b.textContent || '').trim()));
if (match) { match.click(); return String(match.innerText || '').trim(); }
// close icon on privacy dialog
const close = document.querySelector(
  '[aria-label="Close"], [aria-label="关闭"], button[class*="close"], [data-testid*="close"]'
);
if (close) { close.click(); return 'close'; }
return '';
            """
        )
        if ok:
            log(f"已通过脚本关闭 Cookie 提示：{ok!r}")
            _sleep(0.8)
            return True
    except Exception as e:
        log(f"脚本关闭 Cookie 提示失败：{e}")

    # last resort: 全部拒绝 also clears the overlay
    hit = _click_exact(page, ["全部拒绝", "Reject all", "Reject All", "Decline"], log, real=False)
    if hit:
        log(f"已拒绝 Cookie 提示：{hit!r}")
        _sleep(0.8)
        return True
    log("检测到 Cookie 提示，但关闭失败")
    return False


def _click_exact(
    page: Any,
    labels: list[str],
    log: LogFn,
    *,
    real: bool = False,
) -> str | None:
    """Click button by EXACT visible text. real=True uses physical click (needed for consent)."""
    for label in labels:
        el = _find_button_exact(page, label)
        if not el:
            continue
        try:
            if real:
                try:
                    el.scroll.to_see()
                except Exception:
                    pass
                el.click()
                log(f"已点击按钮：{label!r}")
            else:
                el.click(by_js=True)
                log(f"已通过脚本点击按钮：{label!r}")
            return label
        except Exception as e:
            log(f"点击按钮 {label!r} 失败：{e}")
            if real:
                try:
                    el.click(by_js=True)
                    log(f"已通过回退脚本点击按钮：{label!r}")
                    return label
                except Exception as e2:
                    log(f"回退脚本点击 {label!r} 失败：{e2}")
    return None


def _wait_turnstile(
    page: Any,
    log: LogFn,
    timeout: float = 45.0,
    *,
    email: str = "",
    raise_on_timeout: bool = False,
) -> bool:
    """Wait/click Cloudflare Turnstile on the mint browser page.

    On timeout: optionally screenshot + raise BrowserConfirmError so backfill
    skips this account instead of spinning until --timeout.
    """
    deadline = time.time() + timeout
    clicked = False
    while time.time() < deadline:
        try:
            el = page.ele("css:input[name='cf-turnstile-response']", timeout=0.3)
            if el is not None:
                v = (el.attr("value") or "").strip()
                if len(v) > 20:
                    log(f"Turnstile 验证已就绪，令牌长度={len(v)}")
                    return True
        except Exception:
            pass

        # Mimic register-machine: shadow-root checkbox click
        try:
            challenge_input = page.ele("@name=cf-turnstile-response", timeout=0.2)
            if challenge_input is not None:
                wrapper = challenge_input.parent()
                iframe = None
                try:
                    iframe = wrapper.shadow_root.ele("tag:iframe")
                except Exception:
                    iframe = None
                if iframe is not None:
                    try:
                        iframe.run_js(
                            """
window.dtp = 1;
function getRandomInt(min, max) { return Math.floor(Math.random() * (max - min + 1)) + min; }
let sx = getRandomInt(800, 1200);
let sy = getRandomInt(400, 700);
Object.defineProperty(MouseEvent.prototype, 'screenX', { value: sx });
Object.defineProperty(MouseEvent.prototype, 'screenY', { value: sy });
                            """
                        )
                    except Exception:
                        pass
                    try:
                        body_sr = iframe.ele("tag:body").shadow_root
                        btn = body_sr.ele("tag:input")
                        if btn is not None:
                            btn.click()
                            if not clicked:
                                log("已点击 Turnstile 验证框")
                                clicked = True
                    except Exception:
                        pass
        except Exception:
            pass

        if not clicked:
            try:
                page.run_js(
                    """
const nodes = Array.from(document.querySelectorAll('div,span,iframe')).filter((n) => {
  const txt = (n.className || '') + ' ' + (n.id || '') + ' ' + (n.getAttribute?.('src') || '');
  return String(txt).toLowerCase().includes('turnstile');
});
if (nodes.length && typeof nodes[0].click === 'function') nodes[0].click();
                    """
                )
                clicked = True
                log("已通过脚本触发 Turnstile 验证")
            except Exception:
                pass
        _sleep(0.9)
    log("Turnstile 验证等待超时")
    shot = _save_debug_shot(page, tag="turnstile-timeout", email=email, log=log)
    if raise_on_timeout:
        msg = "turnstile timeout"
        if shot:
            msg = f"{msg} shot={shot}"
        raise BrowserConfirmError(f"auth failed: {msg}")
    return False



def _fill(page: Any, selector: str, value: str, log: LogFn, label: str = "") -> bool:
    """Fill an input by CSS selector. Returns True on success."""
    label = label or selector
    value = value or ""
    try:
        el = page.ele(selector, timeout=1.5)
        if el is None:
            log(f"填写 {label} 失败：未找到输入框（{selector}）")
            return False
        try:
            el.clear()
        except Exception:
            pass
        try:
            el.input(value)
        except Exception:
            # fallback JS set
            page.run_js(
                """
                const sel = arguments[0], v = arguments[1];
                const el = document.querySelector(sel);
                if (!el) return false;
                el.focus();
                el.value = v;
                el.dispatchEvent(new Event('input', {bubbles:true}));
                el.dispatchEvent(new Event('change', {bubbles:true}));
                return true;
                """,
                selector,
                value,
            )
        log(f"已填写 {label}")
        return True
    except TypeError:
        # run_js may not accept args
        try:
            el = page.ele(selector, timeout=1.5)
            if el is None:
                return False
            try:
                el.clear()
            except Exception:
                pass
            el.input(value)
            log(f"已填写 {label}")
            return True
        except Exception as e:
            log(f"填写 {label} 失败：{e}")
            return False
    except Exception as e:
        log(f"填写 {label} 失败：{e}")
        return False


def _fill_input(page: Any, selector: str, value: str, label: str, log: LogFn) -> bool:
    """Compat wrapper: (page, selector, value, label, log)."""
    return _fill(page, selector, value, log, label)



def _detect_auth_error(text: str, url: str = "") -> str | None:
    """Return a short error if page shows non-retryable auth / block failure."""
    t = text or ""
    tl = t.lower()
    u = (url or "").lower()
    needles = [
        ("错误的邮箱地址或密码", "错误的邮箱地址或密码"),
        ("incorrect email or password", "incorrect email or password"),
        ("wrong email or password", "wrong email or password"),
        ("invalid email or password", "invalid email or password"),
        ("邮箱地址或密码不正确", "邮箱地址或密码不正确"),
        ("密码错误", "密码错误"),
        ("账号不存在", "账号不存在"),
        ("account not found", "account not found"),
        ("too many attempts", "too many login attempts"),
        ("尝试次数过多", "登录尝试次数过多"),
        ("登录尝试次数过多", "登录尝试次数过多"),
        # Cloudflare / WAF hard blocks — never worth waiting for timeout
        ("sorry, you have been blocked", "cloudflare blocked"),
        ("you are unable to access", "cloudflare blocked"),
        ("why have i been blocked", "cloudflare blocked"),
        ("attention required! | cloudflare", "cloudflare challenge/block"),
        ("access denied", "access denied"),
        ("请求被拒绝", "access denied"),
        ("访问被拒绝", "access denied"),
        ("has been blocked", "blocked by waf"),
        ("cf-error-details", "cloudflare error"),
        ("error 1020", "cloudflare error 1020"),
        ("error 1015", "cloudflare rate limited"),
    ]
    for needle, msg in needles:
        if needle.lower() in tl or needle in t:
            return msg
    # set-cookie hop that landed on a block page (url alone is not enough)
    if "auth.grok.com/set-cookie" in u and (
        "blocked" in tl or "unable to access" in tl or "cloudflare" in tl
    ):
        return "cloudflare blocked on set-cookie"
    return None


def approve_device_code(
    page: Any,
    *,
    verification_uri_complete: str,
    email: str,
    password: str,
    user_code: str = "",
    timeout_sec: float = 240.0,
    stop_event: threading.Event | None = None,
    log: LogFn | None = None,
) -> None:
    log = log or _noop_log
    if page is None:
        raise BrowserConfirmError("page is None")
    email = (email or "").strip()
    password = password or ""
    if not email or not password:
        raise BrowserConfirmError("email/password required")

    if not user_code and "user_code=" in (verification_uri_complete or ""):
        try:
            user_code = verification_uri_complete.split("user_code=", 1)[1].split("&", 1)[0]
        except Exception:
            user_code = ""

    log(f"正在打开设备授权页：{redact_url(verification_uri_complete)}")
    try:
        page.get(verification_uri_complete, timeout=60)
    except TypeError:
        page.get(verification_uri_complete)
    _sleep(2.0)

    deadline = time.time() + timeout_sec
    phase = "device"
    login_attempts = 0
    last_url = ""

    while time.time() < deadline:
        if stop_event is not None and stop_event.is_set():
            log("令牌已获取，结束浏览器授权流程")
            return

        url = _page_url(page)
        text = _visible_text(page)
        if url != last_url:
            log(f"页面已切换：{redact_url(url)[:180]}")
            last_url = url

        # Non-retryable auth / CF block — skip account immediately (no timeout wait)
        auth_err = _detect_auth_error(text, url)
        if auth_err:
            shot = None
            if "block" in auth_err or "cloudflare" in auth_err or "access denied" in auth_err:
                shot = _save_debug_shot(page, tag="cf-block", email=email, log=log)
            msg = auth_err
            if shot:
                msg = f"{auth_err} shot={shot}"
            log(f"授权错误，跳过当前账号：{msg}")
            raise BrowserConfirmError(f"auth failed: {msg}")

        # Done page
        if "device/done" in url or "设备已授权" in text or "device authorized" in text.lower():
            log("设备授权已完成，正在等待令牌")
            _sleep(1.5)
            continue

        if "Invalid action" in text:
            log("授权动作已失效，重新打开设备授权页")
            page.get(verification_uri_complete)
            _sleep(2.0)
            phase = "device"
            continue

        # Cookie / privacy modal first (blocks OAuth 允许 on consent page)
        if _cookie_banner_visible(text):
            if _dismiss_cookie_banner(page, log):
                _sleep(0.6)
                continue
            # Modal still up: never click OAuth 允许 under the overlay
            if "隐私偏好" in text or "全部允许" in text:
                if "/consent" in url or "授权 Grok Build" in text or "Authorize Grok Build" in text:
                    log("Cookie 提示遮挡授权按钮，正在重新关闭")
                    _sleep(0.8)
                    continue

        # Consent page — REAL click exact 允许 (never 全部允许)
        if "/consent" in url or "授权 Grok Build" in text or "Authorize Grok Build" in text:
            phase = "consent"
            # double-check banner cleared this frame
            if _cookie_banner_visible(_visible_text(page)):
                _dismiss_cookie_banner(page, log)
                _sleep(0.6)
                continue
            # Prefer real click; React needs it to set form action=allow
            if _click_exact(page, ["允许", "Allow", "Authorize", "Approve"], log, real=True):
                _sleep(2.5)
                # if cookie reappeared after click, loop will dismiss next iter
                continue
            # last resort: set action and submit only the OAuth form (not cookie form)
            try:
                page.run_js(
                    """
                    const forms = Array.from(document.querySelectorAll('form'));
                    const f = forms.find((x) => {
                      const t = (x.innerText || '');
                      return t.includes('Grok Build') || t.includes('允许') || t.includes('Allow');
                    }) || document.querySelector('form');
                    if(!f) return;
                    // skip cookie preference forms
                    const ft = (f.innerText || '');
                    if (ft.includes('隐私偏好') || ft.includes('全部允许') || /cookie/i.test(ft)) return;
                    let a=f.querySelector('input[name=action]');
                    if(!a){a=document.createElement('input');a.type='hidden';a.name='action';f.appendChild(a);}
                    a.value='allow';
                    const btn=[...f.querySelectorAll('button')].find(b=>{
                      const t=(b.innerText||'').trim();
                      return t==='允许'||t==='Allow'||t==='Authorize'||t==='Approve';
                    });
                    if(btn) btn.click(); else f.submit();
                    """
                )
                log("已通过脚本回退提交授权表单")
                _sleep(2.5)
            except Exception as e:
                log(f"授权表单回退提交失败：{e}")
            continue

        # Device code entry
        if page.ele("css:input[name='user_code']", timeout=0.3) and "consent" not in url:
            phase = "device"
            if user_code:
                try:
                    uc = page.ele("css:input[name='user_code']")
                    cur = (uc.value or "") if uc else ""
                    if user_code.replace("-", "") not in cur.replace("-", ""):
                        uc.clear()
                        uc.input(user_code)
                        log("已填写设备验证码")
                except Exception:
                    pass
            if _click_exact(page, ["继续", "Continue"], log, real=False):
                _sleep(2.0)
                continue
            try:
                el = page.ele("css:button[type='submit']", timeout=0.5)
                if el:
                    el.click(by_js=True)
                    log("已提交设备验证码")
                    _sleep(2.0)
                    continue
            except Exception:
                pass

        # Account redirect
        if "正在重定向" in text or ("/account" in url and "sign-in" not in url):
            if _click_exact(page, ["继续", "Continue"], log, real=False):
                _sleep(2.0)
                continue

        # Cookie banner fallback (non-consent pages)
        if _cookie_banner_visible(text):
            _dismiss_cookie_banner(page, log)
            _sleep(0.4)

        # Sign-in chooser
        if "使用邮箱登录" in text or "Continue with email" in text:
            if _click_exact(page, ["使用邮箱登录", "Continue with email", "Sign in with email"], log, real=False):
                _sleep(1.5)
                phase = "email"
                continue

        # Email only step
        if page.ele("css:input[type='email']", timeout=0.3) and not page.ele(
            "css:input[type='password']", timeout=0.2
        ):
            phase = "email"
            _fill(page, "css:input[type='email']", email, log, "email")
            if _click_exact(page, ["下一步", "Next", "Continue", "继续"], log, real=False):
                _sleep(1.8)
                continue

        # Password login
        if page.ele("css:input[type='password']", timeout=0.3):
            phase = "password"
            if login_attempts >= 3:
                # Already tried enough — check page text once more then skip
                auth_err = _detect_auth_error(text, url) or "login failed after retries (still on password page)"
                log(f"登录授权错误，跳过当前账号：{auth_err}")
                raise BrowserConfirmError(f"auth failed: {auth_err}")
            login_attempts += 1
            log(f"正在尝试登录，第 {login_attempts} 次")
            _fill(page, "css:input[type='email']", email, log, "email")
            # Turnstile hard gate: timeout → screenshot + skip account (no batch hang)
            _wait_turnstile(
                page,
                log,
                25,
                email=email,
                raise_on_timeout=True,
            )
            _fill(page, "css:input[type='password']", password, log, "password")
            _wait_turnstile(
                page,
                log,
                12,
                email=email,
                raise_on_timeout=False,
            )
            # REAL click login helps form submit
            if not _click_exact(page, ["登录", "Sign in", "Log in"], log, real=True):
                try:
                    el = page.ele("css:button[type='submit']", timeout=0.5) or page.ele(
                        "css:button[data-testid='sign-in-submit']", timeout=0.5
                    )
                    if el:
                        el.click()
                        log("已点击登录按钮")
                except Exception as e:
                    log(f"提交登录失败：{e}")
            # wait navigation / surface error banner
            for _ in range(20):
                if stop_event is not None and stop_event.is_set():
                    return
                _sleep(0.5)
                post = _visible_text(page)
                auth_err = _detect_auth_error(post, _page_url(page))
                if auth_err:
                    log(f"登录后检测到授权错误，跳过当前账号：{auth_err}")
                    raise BrowserConfirmError(f"auth failed: {auth_err}")
                if not page.ele("css:input[type='password']", timeout=0.2):
                    break
                if "sign-in" not in _page_url(page):
                    break
            # still on password page?
            post = _visible_text(page)
            auth_err = _detect_auth_error(post, _page_url(page))
            if auth_err:
                log(f"登录后检测到授权错误，跳过当前账号：{auth_err}")
                raise BrowserConfirmError(f"auth failed: {auth_err}")
            if page.ele("css:input[type='password']", timeout=0.2) and (
                _is_turnstile_challenge(post) or login_attempts >= 2
            ):
                shot = _save_debug_shot(
                    page,
                    tag="login-stuck-turnstile",
                    email=email,
                    log=log,
                )
                msg = "turnstile/login stuck after submit"
                if shot:
                    msg = f"{msg} shot={shot}"
                log(f"登录授权错误，跳过当前账号：{msg}")
                raise BrowserConfirmError(f"auth failed: {msg}")
            continue

        _sleep(1.0)

    if stop_event is not None and stop_event.is_set():
        log("浏览器授权流程已结束")
        return
    shot = _save_debug_shot(page, tag=f"timeout-phase-{phase}", email=email, log=log)
    msg = f"browser confirm timeout phase={phase} login_attempts={login_attempts}"
    if shot:
        msg = f"{msg} shot={shot}"
    log(msg)
    # Hard-skip so mint/backfill do not hang waiting on a dead CF challenge
    if phase in ("password", "email") or _is_turnstile_challenge(_visible_text(page)):
        raise BrowserConfirmError(f"auth failed: {msg}")
    raise BrowserConfirmError(msg)


def mint_with_browser(
    *,
    email: str,
    password: str,
    page: Any | None = None,
    proxy: str | None = None,
    headless: bool = False,
    browser_timeout_sec: float = 240.0,
    poll_log: LogFn | None = None,
    cancel: Callable[[], bool] | None = None,
    force_standalone: bool = True,
    cookies: Any | None = None,
    reuse_browser: bool = True,
    recycle_every: int = 15,
) -> dict[str, Any]:
    """Request device code, approve in browser, poll tokens.

    force_standalone=True (default): do not reuse the *register* tab.
    Mint workers may still reuse their *own* Chromium via reuse_browser.
    cookies: optional register-browser cookie list to skip re-login.
    """
    from .oauth_device import OAuthDeviceError, poll_device_token, request_device_code
    from .proxyutil import proxy_log_label, resolve_proxy, set_runtime_proxy

    log = poll_log or _noop_log
    own_browser = None
    owned = False
    work_page = None if force_standalone else page
    resolved = resolve_proxy(proxy)
    set_runtime_proxy(resolved or None)
    success = False
    try:
        last_err: BaseException | None = None
        sess = None
        for attempt in range(1, 4):
            try:
                sess = request_device_code(proxy=resolved or None)
                last_err = None
                break
            except BaseException as e:  # noqa: BLE001
                last_err = e
                log(f"请求设备验证码失败（{attempt}/3）：{e}")
                _sleep(1.5 * attempt)
        if sess is None:
            raise last_err or RuntimeError("request_device_code failed")
        log(
            f"设备验证码={sess.user_code}，有效期={sess.expires_in} 秒，"
            f"代理={proxy_log_label(resolved) or '直连'}"
        )

        if work_page is None:
            own_browser, work_page, owned = acquire_mint_browser(
                proxy=resolved or None,
                headless=headless,
                reuse=reuse_browser,
                recycle_every=recycle_every,
                log=log,
            )
            if owned:
                # non-reuse path: track for finally close
                pass

        # Cookie inject before opening device URL (skip secondary login when possible)
        if cookies:
            n = inject_cookies(work_page, cookies, log=log)
            log(f"已注入 {n} 条 Cookie")
            try:
                work_page.get("https://accounts.x.ai/")
                _sleep(1.0)
                url = _page_url(work_page)
                log(f"注入登录状态后的页面：{redact_url(url)[:120]}")
            except Exception as e:
                log(f"检查注入后的登录状态失败：{e}")

        stop_event = threading.Event()
        token_box: dict[str, Any] = {}
        err_box: dict[str, BaseException] = {}

        def _poll() -> None:
            try:
                time.sleep(2)
                tr = poll_device_token(
                    sess.device_code,
                    interval=max(sess.interval, 5),
                    expires_in=min(sess.expires_in, int(browser_timeout_sec) + 60),
                    log=log,
                    cancel=cancel,
                    proxy=resolved or None,
                )
                token_box["token"] = tr
                stop_event.set()
                log("令牌获取成功，正在结束授权流程")
            except BaseException as e:  # noqa: BLE001
                err_box["err"] = e
                stop_event.set()

        t = threading.Thread(target=_poll, name="oauth-poll", daemon=True)
        t.start()
        try:
            approve_device_code(
                work_page,
                verification_uri_complete=sess.verification_uri_complete,
                email=email,
                password=password,
                user_code=sess.user_code,
                timeout_sec=browser_timeout_sec,
                stop_event=stop_event,
                log=log,
            )
        except BrowserConfirmError as e:
            msg = str(e)
            # Non-retryable auth failures: abort mint immediately (backfill will skip)
            low = msg.lower()
            hard = (
                "auth failed" in low
                or "turnstile" in low
                or "cloudflare" in low
                or "blocked" in low
                or "access denied" in low
                or "错误的邮箱" in msg
                or "password" in low
                or "browser confirm timeout" in low
            )
            if hard:
                log(f"浏览器授权已中止：{e}")
                stop_event.set()
                raise
            log(f"浏览器授权警告：{e}")

        t.join(timeout=max(browser_timeout_sec, 60) + 30)
        if "token" in token_box:
            tr = token_box["token"]
            success = True
            return {
                "access_token": tr.access_token,
                "refresh_token": tr.refresh_token,
                "id_token": tr.id_token,
                "token_type": tr.token_type,
                "expires_in": tr.expires_in,
                "user_code": sess.user_code,
            }
        if "err" in err_box:
            raise err_box["err"]
        raise OAuthDeviceError("token poll thread ended without result")
    finally:
        if own_browser is not None:
            if owned:
                close_standalone(own_browser)
            else:
                release_mint_browser(owned=False, success=success, log=log)
