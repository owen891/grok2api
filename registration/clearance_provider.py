from __future__ import annotations

from dataclasses import dataclass
import threading
from typing import Any, Protocol


@dataclass(frozen=True)
class ClearanceResult:
    mode: str
    token: str = ""
    cookies: str = ""
    user_agent: str = ""


class ClearanceProvider(Protocol):
    name: str

    def solve(self, *, website_url: str, proxy: str = "") -> ClearanceResult: ...


class DockerTokenProvider:
    name = "docker"

    def __init__(self, config: dict[str, Any]):
        self.config = config
        self._endpoint_index = 0
        self._endpoint_lock = threading.Lock()

    def _endpoints(self) -> list[str]:
        values: list[str] = []
        for key in ("clearance_endpoints", "captcha_endpoints", "local_captcha_endpoints"):
            raw = self.config.get(key)
            if isinstance(raw, list):
                values.extend(str(item or "").strip() for item in raw)
            elif isinstance(raw, str):
                values.extend(part.strip() for part in raw.split(","))
        single = str(
            self.config.get("clearance_endpoint")
            or self.config.get("captcha_endpoint")
            or self.config.get("local_captcha_endpoint")
            or ""
        ).strip()
        if single:
            values.extend(part.strip() for part in single.split(","))
        result: list[str] = []
        for value in values:
            if value and value not in result:
                result.append(value)
        return result

    def _next_endpoint(self, endpoints: list[str]) -> list[str]:
        with self._endpoint_lock:
            start = self._endpoint_index % len(endpoints)
            self._endpoint_index = (start + 1) % len(endpoints)
        return endpoints[start:] + endpoints[:start]

    def solve(self, *, website_url: str, proxy: str = "") -> ClearanceResult:
        from local_turnstile import solve_turnstile_local
        from xconsole_client import config as auth_config

        endpoints = self._endpoints()
        if not endpoints:
            raise RuntimeError("本地过盾未配置 captcha_endpoint；可配置逗号分隔的多个 solver endpoint")
        failures: list[str] = []
        for endpoint in self._next_endpoint(endpoints):
            try:
                token = solve_turnstile_local(
                    website_url=website_url,
                    website_key=str(getattr(auth_config, "TURNSTILE_SITEKEY", "0x4AAAAAAAhrPj9_JwTyl4nM")),
                    timeout=float(self.config.get("clearance_timeout") or self.config.get("turnstile_timeout") or 180),
                    headless=True,
                    proxy=proxy,
                    endpoint=endpoint,
                    client_key=str(self.config.get("captcha_client_key") or self.config.get("local_captcha_client_key") or "local"),
                    task_type=str(self.config.get("captcha_task_type") or "TurnstileTaskProxyless"),
                )
                return ClearanceResult(mode="token", token=token)
            except Exception as exc:
                failures.append(f"{endpoint}: {exc}")
        raise RuntimeError("所有本地过盾 endpoint 均失败: " + "; ".join(failures))


class YesCaptchaTokenProvider:
    name = "yescaptcha"

    def __init__(self, api_key: str):
        self.api_key = api_key

    def solve(self, *, website_url: str, proxy: str = "") -> ClearanceResult:
        del proxy
        from xconsole_client import YesCaptchaSolver, config as auth_config

        token = YesCaptchaSolver(self.api_key).solve_turnstile(
            website_url=website_url,
            website_key=auth_config.TURNSTILE_SITEKEY,
            premium=True,
        )
        return ClearanceResult(mode="token", token=token)


def create_clearance_provider(config: dict[str, Any], *, yescaptcha_key: str = "") -> ClearanceProvider:
    mode = str(config.get("clearance_provider") or config.get("captcha_solver") or "docker").strip().lower()
    if mode in {"yescaptcha", "yes", "remote"}:
        if not yescaptcha_key:
            raise RuntimeError("clearance_provider=yescaptcha but no API key is configured")
        return YesCaptchaTokenProvider(yescaptcha_key)
    return DockerTokenProvider(config)
