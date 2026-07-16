"""注册机浏览器运行环境配置。"""

from __future__ import annotations

import os
import re
import shutil
from pathlib import Path
from typing import Any, Callable


VALID_BROWSER_MODES = frozenset({"xvfb", "headless", "headed", "background"})
DEFAULT_WINDOW_SIZE = "1280,900"
_WINDOW_SIZE_PATTERN = re.compile(r"^[1-9]\d{2,4},[1-9]\d{2,4}$")
_LINUX_BROWSER_CANDIDATES = (
    "/usr/bin/chromium",
    "/usr/bin/chromium-browser",
    "/usr/bin/google-chrome",
    "/usr/bin/google-chrome-stable",
)


def configured_browser_mode() -> str | None:
    value = os.getenv("REGISTRATION_BROWSER_MODE", "").strip().lower()
    return value or None


def browser_mode(default: str = "headed") -> str:
    mode = configured_browser_mode() or default
    if mode not in VALID_BROWSER_MODES:
        choices = ", ".join(sorted(VALID_BROWSER_MODES))
        raise ValueError(f"REGISTRATION_BROWSER_MODE must be one of: {choices}")
    return mode


def browser_window_size() -> str:
    value = os.getenv("REGISTRATION_BROWSER_WINDOW", "").strip()
    return value if _WINDOW_SIZE_PATTERN.fullmatch(value) else DEFAULT_WINDOW_SIZE


def browser_path() -> str | None:
    configured = os.getenv("REGISTRATION_BROWSER_PATH", "").strip()
    if configured:
        return str(Path(configured).expanduser().resolve())
    executable = next(
        (
            value
            for name in ("chromium", "chromium-browser", "google-chrome", "chrome", "msedge")
            if (value := shutil.which(name))
        ),
        None,
    )
    if executable:
        return str(Path(executable).resolve())
    candidates = list(_LINUX_BROWSER_CANDIDATES)
    for base in (os.getenv("PROGRAMFILES"), os.getenv("PROGRAMFILES(X86)"), os.getenv("LOCALAPPDATA")):
        if base:
            candidates.extend(
                (
                    str(Path(base) / "Google" / "Chrome" / "Application" / "chrome.exe"),
                    str(Path(base) / "Microsoft" / "Edge" / "Application" / "msedge.exe"),
                )
            )
    return next((candidate for candidate in candidates if Path(candidate).is_file()), None)


def browser_headless(default: bool = False) -> bool:
    mode = configured_browser_mode()
    if mode == "headless":
        return True
    if mode == "headed":
        return False
    if mode == "background":
        return False
    return default


def apply_browser_runtime(
    options: Any,
    *,
    default_headless: bool = False,
    log: Callable[[str], None] | None = None,
) -> str:
    mode = configured_browser_mode()
    effective_mode = browser_mode("headless" if default_headless else "headed")
    headless = browser_headless(default_headless)

    try:
        options.headless(headless)
    except Exception:
        if headless:
            options.set_argument("--headless=new")

    options.set_argument(f"--window-size={browser_window_size()}")
    if effective_mode == "background":
        for flag in (
            "--start-minimized",
            "--window-position=-32000,-32000",
            "--disable-background-timer-throttling",
            "--disable-backgrounding-occluded-windows",
            "--disable-renderer-backgrounding",
        ):
            options.set_argument(flag)
    executable = browser_path()
    if executable:
        options.set_browser_path(executable)

    if log:
        source = "environment" if mode else "config"
        display = os.getenv("DISPLAY", "")
        log(
            f"browser mode={effective_mode} headless={headless} source={source} "
            f"path={executable or 'auto'} DISPLAY={display!r}"
        )
    return effective_mode


__all__ = [
    "DEFAULT_WINDOW_SIZE",
    "VALID_BROWSER_MODES",
    "apply_browser_runtime",
    "browser_headless",
    "browser_mode",
    "browser_path",
    "browser_window_size",
    "configured_browser_mode",
]
