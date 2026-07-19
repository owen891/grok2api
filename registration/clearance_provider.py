from __future__ import annotations

from dataclasses import dataclass
import os
import threading
import time
from typing import Any, Protocol


@dataclass(frozen=True)
class ClearanceResult:
    mode: str
    token: str = ""
    cookies: str = ""
    user_agent: str = ""


class ClearanceProvider(Protocol):
    name: str

    def solve(
        self,
        *,
        website_url: str,
        proxy: str = "",
        website_key: str = "",
    ) -> ClearanceResult: ...


@dataclass
class _EndpointState:
    endpoint: str
    slots: threading.BoundedSemaphore


@dataclass
class _FailureState:
    consecutive: int = 0
    last_failure_at: float = 0.0
    cooldown_until: float = 0.0


def _int_setting(config: dict[str, Any], key: str, default: int, minimum: int, maximum: int) -> int:
    try:
        return max(minimum, min(int(config.get(key, default) or default), maximum))
    except (TypeError, ValueError):
        return default


def _float_setting(config: dict[str, Any], key: str, default: float, minimum: float, maximum: float) -> float:
    try:
        return max(minimum, min(float(config.get(key, default) or default), maximum))
    except (TypeError, ValueError):
        return default


def local_captcha_endpoints(config: dict[str, Any]) -> list[str]:
    values: list[str] = []
    for key in ("clearance_endpoints", "captcha_endpoints", "local_captcha_endpoints"):
        raw = config.get(key)
        if isinstance(raw, list):
            values.extend(str(item or "").strip() for item in raw)
        elif isinstance(raw, str):
            values.extend(part.strip() for part in raw.split(","))
    single = str(
        config.get("clearance_endpoint")
        or config.get("captcha_endpoint")
        or config.get("local_captcha_endpoint")
        or ""
    ).strip()
    if single:
        values.extend(part.strip() for part in single.split(","))
    result: list[str] = []
    for value in values:
        if value and value not in result:
            result.append(value)
    return result


class DockerTokenProvider:
    name = "docker"

    def __init__(self, config: dict[str, Any]):
        self.config = dict(config)
        self._endpoint_index = 0
        self._condition = threading.Condition(threading.Lock())
        self._timeout = _float_setting(
            self.config,
            "clearance_timeout",
            _float_setting(self.config, "turnstile_timeout", 180.0, 1.0, 900.0),
            1.0,
            900.0,
        )
        environment_value = (
            os.environ.get("REGISTRATION_CAPTCHA_CONCURRENCY")
            or os.environ.get("LOCAL_CAPTCHA_CONCURRENCY")
            or ""
        ).strip()
        if environment_value:
            try:
                self._endpoint_concurrency = max(1, min(int(environment_value), 8))
            except ValueError:
                self._endpoint_concurrency = _int_setting(
                    self.config,
                    "captcha_endpoint_concurrency",
                    1,
                    1,
                    8,
                )
        else:
            self._endpoint_concurrency = _int_setting(
                self.config,
                "captcha_endpoint_concurrency",
                1,
                1,
                8,
            )
        self._failure_threshold = _int_setting(
            self.config, "captcha_failure_threshold", 2, 1, 10
        )
        self._failure_cooldown = _float_setting(
            self.config, "captcha_failure_cooldown", 20.0, 1.0, 300.0
        )
        self._failure_window = _float_setting(
            self.config, "captcha_failure_window", 60.0, 1.0, 900.0
        )
        self._states = [
            _EndpointState(endpoint, threading.BoundedSemaphore(self._endpoint_concurrency))
            for endpoint in local_captcha_endpoints(self.config)
        ]
        self._failures: dict[tuple[str, str], _FailureState] = {}

    @property
    def concurrency(self) -> int:
        return max(1, len(self._states) * self._endpoint_concurrency)

    def _next_states(self) -> list[_EndpointState]:
        with self._condition:
            if not self._states:
                return []
            start = self._endpoint_index % len(self._states)
            self._endpoint_index = (start + 1) % len(self._states)
            return self._states[start:] + self._states[:start]

    @staticmethod
    def _route_key(endpoint: str, proxy: str) -> tuple[str, str]:
        return endpoint, (proxy or "<direct>").strip()

    def _is_cooling_locked(self, state: _EndpointState, proxy: str, now: float) -> bool:
        failure = self._failures.get(self._route_key(state.endpoint, proxy))
        return bool(failure and failure.cooldown_until > now)

    def _acquire_state(
        self,
        states: list[_EndpointState],
        attempted: set[str],
        proxy: str,
        deadline: float,
    ) -> _EndpointState | None:
        with self._condition:
            while True:
                candidates = [state for state in states if state.endpoint not in attempted]
                if not candidates:
                    return None
                now = time.monotonic()
                remaining = deadline - now
                if remaining <= 0:
                    raise TimeoutError("等待本地过盾容量超时")
                healthy = [
                    state for state in candidates if not self._is_cooling_locked(state, proxy, now)
                ]
                if not healthy:
                    retry_at = min(
                        self._failures[self._route_key(state.endpoint, proxy)].cooldown_until
                        for state in candidates
                    )
                    raise RuntimeError(
                        f"本地过盾已熔断，{max(0.1, retry_at - now):.1f}s 后重试"
                    )
                for state in healthy:
                    if state.slots.acquire(blocking=False):
                        return state
                self._condition.wait(timeout=min(0.5, remaining))

    def _release_state(self, state: _EndpointState) -> None:
        with self._condition:
            state.slots.release()
            self._condition.notify_all()

    def _record_success(self, state: _EndpointState, proxy: str) -> None:
        with self._condition:
            self._failures.pop(self._route_key(state.endpoint, proxy), None)

    def _record_failure(self, state: _EndpointState, proxy: str) -> None:
        now = time.monotonic()
        key = self._route_key(state.endpoint, proxy)
        with self._condition:
            failure = self._failures.setdefault(key, _FailureState())
            if now - failure.last_failure_at > self._failure_window:
                failure.consecutive = 0
            failure.consecutive += 1
            failure.last_failure_at = now
            if failure.consecutive >= self._failure_threshold:
                failure.cooldown_until = now + self._failure_cooldown

    def solve(
        self,
        *,
        website_url: str,
        proxy: str = "",
        website_key: str = "",
    ) -> ClearanceResult:
        from local_turnstile import solve_turnstile_local
        from xconsole_client import config as auth_config

        states = self._next_states()
        if not states:
            raise RuntimeError("本地过盾未配置 captcha_endpoint；可配置逗号分隔的多个 solver endpoint")
        timeout = self._timeout
        deadline = time.monotonic() + timeout
        attempted: set[str] = set()
        failures: list[str] = []
        while len(attempted) < len(states):
            state = self._acquire_state(states, attempted, proxy, deadline)
            if state is None:
                break
            attempted.add(state.endpoint)
            try:
                remaining = max(0.1, deadline - time.monotonic())
                token = solve_turnstile_local(
                    website_url=website_url,
                    website_key=website_key
                    or str(getattr(auth_config, "TURNSTILE_SITEKEY", "0x4AAAAAAAhrPj9_JwTyl4nM")),
                    timeout=remaining,
                    headless=True,
                    proxy=proxy,
                    endpoint=state.endpoint,
                    client_key=str(self.config.get("captcha_client_key") or self.config.get("local_captcha_client_key") or "local"),
                    task_type=str(self.config.get("captcha_task_type") or "TurnstileTaskProxyless"),
                )
                self._record_success(state, proxy)
                return ClearanceResult(mode="token", token=token)
            except Exception as exc:
                self._record_failure(state, proxy)
                failures.append(f"{state.endpoint}: {exc}")
            finally:
                self._release_state(state)
        raise RuntimeError("所有本地过盾 endpoint 均失败: " + "; ".join(failures))


class YesCaptchaTokenProvider:
    name = "yescaptcha"

    def __init__(self, api_key: str):
        self.api_key = api_key

    def solve(
        self,
        *,
        website_url: str,
        proxy: str = "",
        website_key: str = "",
    ) -> ClearanceResult:
        del proxy
        from xconsole_client import YesCaptchaSolver, config as auth_config

        token = YesCaptchaSolver(self.api_key).solve_turnstile(
            website_url=website_url,
            website_key=website_key or auth_config.TURNSTILE_SITEKEY,
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
