#!/usr/bin/env python3
"""Patch the pinned solver so each task can carry its selected proxy."""

from __future__ import annotations

import sys
from pathlib import Path


def replace_once(text: str, old: str, new: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"expected one occurrence, found {count}: {old[:80]!r}")
    return text.replace(old, new, 1)


def replace_last(text: str, old: str, new: str) -> str:
    index = text.rfind(old)
    if index < 0:
        raise SystemExit(f"expected an occurrence: {old[:80]!r}")
    return text[:index] + new + text[index + len(old) :]


path = Path(sys.argv[1])
text = path.read_text(encoding="utf-8")
text = replace_once(
    text,
    "from patchright.async_api import async_playwright",
    "try:\n    from patchright.async_api import async_playwright\nexcept ImportError:\n    async_playwright = None",
)
text = replace_once(
    text,
    "        if self.browser_type in ['chromium', 'chrome', 'msedge']:\n            playwright = await async_playwright().start()\n            self._playwright = playwright",
    "        if self.browser_type in ['chromium', 'chrome', 'msedge']:\n            if async_playwright is None:\n                raise RuntimeError(\n                    'Patchright is unavailable; use a full solver image for Chromium.'\n                )\n            playwright = await async_playwright().start()\n            self._playwright = playwright",
)
text = replace_once(
    text,
    '        elif self.browser_type == "camoufox":\n            camoufox = AsyncCamoufox(headless=self.headless)\n            self._camoufox = camoufox',
    '        elif self.browser_type == "camoufox":\n            camoufox_options = {"headless": self.headless}\n            if os.getenv("TURNSTILE_SOLVER_VARIANT", "").strip().lower() == "camoufox-linux":\n                camoufox_options["os"] = "linux"\n            camoufox = AsyncCamoufox(**camoufox_options)\n            self._camoufox = camoufox',
)
text = replace_once(
    text,
    "async def _solve_turnstile(self, task_id: str, url: str, sitekey: str, action: Optional[str] = None, cdata: Optional[str] = None):",
    "async def _solve_turnstile(self, task_id: str, url: str, sitekey: str, action: Optional[str] = None, cdata: Optional[str] = None, proxy_override: Optional[str] = None):",
)
text = replace_once(text, "        proxy = None\n        context = None", "        proxy = (proxy_override or \"\").strip() or None\n        context = None")
text = replace_once(text, "            if self.proxy_support:\n                proxy_file_path", "            if self.proxy_support or proxy:\n                proxy_file_path")
text = replace_once(text, "                    proxy = random.choice(proxies) if proxies else None", "                    if not proxy:\n                        proxy = random.choice(proxies) if proxies else None")
text = replace_once(text, "self._build_context_options(browser_config or {}, proxy if self.proxy_support else None)", "self._build_context_options(browser_config or {}, proxy)")
text = replace_once(
    text,
    "        cdata: Optional[str] = None,\n    ):\n        \"\"\"",
    "        cdata: Optional[str] = None,\n        proxy: Optional[str] = None,\n    ):\n        \"\"\"",
)
text = replace_once(
    text,
    "                    cdata=cdata,\n                )",
    "                    cdata=cdata,\n                    proxy_override=proxy,\n                )",
)
text = replace_once(
    text,
    "        cdata = task.get(\"cdata\") or task.get(\"data\")\n\n        # CapSolver",
    "        cdata = task.get(\"cdata\") or task.get(\"data\")\n        proxy = task.get(\"proxy\") or None\n\n        # CapSolver",
)
text = replace_last(
    text,
    "task_id, err = await self._enqueue_turnstile(url, sitekey, action, cdata)\n        if err:",
    "task_id, err = await self._enqueue_turnstile(url, sitekey, action, cdata, proxy)\n        if err:",
)
path.write_text(text, encoding="utf-8", newline="\n")
