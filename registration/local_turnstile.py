#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""本地过盾：HTTP 或 docker:// 桥接，不弹浏览器。

captcha_endpoint 示例：
  http://127.0.0.1:5072
  docker://grokcli-2api:5072
"""
from __future__ import annotations

import json
import ipaddress
import os
import shutil
import socket
import subprocess
import time
from typing import Any
from urllib.parse import urlsplit, urlunsplit

import requests

DEFAULT_SITEKEY = "0x4AAAAAAAhrPj9_JwTyl4nM"
SIGNUP_URL = "https://accounts.x.ai/sign-up?redirect=grok-com"
DEFAULT_TASK_TYPE = "TurnstileTaskProxyless"


def _task_payload(website_url: str, website_key: str, task_type: str, proxy: str, client_key: str) -> dict[str, Any]:
    task: dict[str, Any] = {
        "type": task_type or DEFAULT_TASK_TYPE,
        "websiteURL": website_url,
        "websiteKey": website_key,
    }
    if proxy:
        task["proxy"] = proxy
    return {"clientKey": client_key or "local", "task": task}


def _pin_http_endpoint(endpoint: str) -> str:
    """Pin a Compose DNS endpoint to one IP for the whole task lifecycle."""
    parsed = urlsplit(endpoint)
    if parsed.scheme not in {"http", "https"} or not parsed.hostname:
        return endpoint
    try:
        ipaddress.ip_address(parsed.hostname)
        return endpoint
    except ValueError:
        pass
    # Never replace a public HTTPS hostname: its certificate is for the name.
    if parsed.scheme == "https":
        return endpoint
    try:
        port = parsed.port or 80
        addresses = socket.getaddrinfo(parsed.hostname, port, type=socket.SOCK_STREAM)
        hosts = list(dict.fromkeys(item[4][0] for item in addresses if item[4]))
        if not hosts:
            return endpoint
        host = hosts[int(time.monotonic_ns()) % len(hosts)]
        if ":" in host:
            host = f"[{host}]"
        userinfo = ""
        if parsed.username:
            userinfo = parsed.username
            if parsed.password:
                userinfo += f":{parsed.password}"
            userinfo += "@"
        return urlunsplit((parsed.scheme, f"{userinfo}{host}:{port}", parsed.path, parsed.query, parsed.fragment))
    except (OSError, ValueError):
        return endpoint


def _dockerize_loopback_proxy(proxy: str) -> str:
    """Map a host loopback proxy to Docker Desktop's host gateway."""
    value = (proxy or "").strip()
    if not value:
        return ""
    parsed = urlsplit(value)
    if parsed.hostname not in {"127.0.0.1", "localhost", "::1"}:
        return value
    userinfo = ""
    if parsed.username:
        userinfo = parsed.username
        if parsed.password:
            userinfo += f":{parsed.password}"
        userinfo += "@"
    port = f":{parsed.port}" if parsed.port else ""
    return urlunsplit((parsed.scheme, f"{userinfo}host.docker.internal{port}", parsed.path, parsed.query, parsed.fragment))


def _extract_token(result: dict[str, Any]) -> str:
    solution = result.get("solution") if isinstance(result.get("solution"), dict) else {}
    token = (
        (solution or {}).get("token")
        or (solution or {}).get("gRecaptchaResponse")
        or result.get("token")
    )
    return str(token or "")


def _http_solve(base: str, payload: dict[str, Any], timeout: float) -> str:
    create_resp = requests.post(f"{base}/createTask", json=payload, timeout=45)
    if create_resp.status_code >= 400:
        raise RuntimeError(f"本地过盾 createTask HTTP {create_resp.status_code}: {create_resp.text[:300]}")
    create_data = create_resp.json()
    if create_data.get("errorId", 0) not in (0, "0", None):
        raise RuntimeError(f"本地过盾 createTask 失败: {create_data}")
    task_id = create_data.get("taskId") or create_data.get("task_id")
    if not task_id:
        token = _extract_token(create_data)
        if len(token) > 20:
            return token
        raise RuntimeError(f"本地过盾 createTask 未返回 taskId: {create_data}")

    solve_timeout = max(30.0, float(timeout or 120))
    deadline = time.monotonic() + solve_timeout
    while time.monotonic() < deadline:
        remaining = max(1.0, deadline - time.monotonic())
        result_resp = requests.post(
            f"{base}/getTaskResult",
            json={"clientKey": payload.get("clientKey") or "local", "taskId": task_id},
            timeout=min(15.0, remaining),
        )
        if result_resp.status_code >= 400:
            raise RuntimeError(f"本地过盾 getTaskResult HTTP {result_resp.status_code}: {result_resp.text[:300]}")
        result = result_resp.json()
        if result.get("errorId", 0) not in (0, "0", None):
            raise RuntimeError(f"本地过盾任务失败: {result}")
        error_code = str(result.get("errorCode") or "").strip()
        status = str(result.get("status") or "").lower()
        if error_code or status in {"failed", "error"}:
            raise RuntimeError(f"本地过盾任务失败: {result}")
        token = _extract_token(result)
        if len(token) > 20:
            return token
        if status in {"ready", "success", "completed"}:
            raise RuntimeError(f"本地过盾 status={status} 但无 token: {result}")
        time.sleep(min(2.0, max(0.1, deadline - time.monotonic())))
    diagnostic = _http_health(base)
    detail = f"，health={diagnostic}" if diagnostic else ""
    raise TimeoutError(f"本地过盾超时（{solve_timeout:.1f}s），taskId={task_id}{detail}")


def _http_health(base: str) -> str:
    """Best-effort solver health data for timeout errors; never masks the cause."""
    try:
        response = requests.get(f"{base.rstrip('/')}/health", timeout=5)
        if response.status_code >= 400:
            return f"HTTP {response.status_code}"
        data = response.json()
        if isinstance(data, dict):
            fields = {
                key: data[key]
                for key in ("ok", "pool_ready", "queue", "in_flight", "owned", "idle_sec")
                if key in data
            }
            return json.dumps(fields, ensure_ascii=False, separators=(",", ":"))
        return str(data)
    except Exception as exc:
        return f"unavailable: {exc}"


def _http_solve_with_retries(base: str, payload: dict[str, Any], timeout: float) -> str:
    """Retry a serialized solver task when the browser reports a transient timeout/failure."""
    raw_retries = os.environ.get("LOCAL_CAPTCHA_RETRIES", "1").strip()
    try:
        retries = max(0, min(int(raw_retries), 3))
    except ValueError:
        retries = 1
    failures: list[str] = []
    last_error: Exception | None = None
    for attempt in range(retries + 1):
        try:
            return _http_solve(base, payload, timeout)
        except Exception as exc:
            last_error = exc
            failures.append(f"attempt {attempt + 1}/{retries + 1}: {exc}")
            if attempt < retries:
                time.sleep(1.0)
    if retries == 0 and last_error is not None:
        raise last_error
    raise RuntimeError("; ".join(failures)) from None


def _docker_exec_json_raw(container: str, path: str, body: dict[str, Any], port: int) -> dict[str, Any]:
    script = (
        "import json,urllib.request;"
        f"req=urllib.request.Request('http://127.0.0.1:{port}{path}', data=json.dumps({json.dumps(body)}).encode(), headers={{'Content-Type':'application/json'}});"
        "print(urllib.request.urlopen(req, timeout=60).read().decode())"
    )
    proc = subprocess.run(
        ["docker", "exec", container, "python", "-c", script],
        capture_output=True,
        text=True,
        timeout=90,
    )
    if proc.returncode != 0:
        raise RuntimeError(f"docker exec captcha 失败: {proc.stderr or proc.stdout}")
    out = (proc.stdout or "").strip()
    return json.loads(out)


def _docker_cli_available() -> bool:
    return bool(shutil.which("docker"))


def _docker_container_state(container: str) -> tuple[str, str, str]:
    """Return Docker status, exit code, and OOM flag with actionable errors."""
    proc = subprocess.run(
        ["docker", "inspect", "--format", "{{.State.Status}}|{{.State.ExitCode}}|{{.State.OOMKilled}}", container],
        capture_output=True,
        text=True,
        timeout=15,
    )
    if proc.returncode != 0:
        detail = (proc.stderr or proc.stdout or "container not found").strip()
        raise RuntimeError(f"captcha container {container!r} inspect failed: {detail}")
    fields = (proc.stdout or "").strip().split("|", 2)
    if len(fields) != 3:
        raise RuntimeError(f"captcha container {container!r} returned invalid state: {proc.stdout!r}")
    return fields[0], fields[1], fields[2]


def _ensure_docker_container_running(container: str, wait_timeout: float = 30.0) -> None:
    """Start a stopped solver container once and wait until Docker reports running."""
    status, exit_code, oom_killed = _docker_container_state(container)
    if status == "running":
        return
    autostart = os.environ.get("LOCAL_CAPTCHA_DOCKER_AUTOSTART", "1").strip().lower()
    if autostart in {"0", "false", "no", "off"}:
        raise RuntimeError(
            f"captcha container {container!r} is {status} (exit={exit_code}, oom={oom_killed}); "
            "set LOCAL_CAPTCHA_DOCKER_AUTOSTART=1 or start it with docker start"
        )
    start = subprocess.run(["docker", "start", container], capture_output=True, text=True, timeout=30)
    if start.returncode != 0:
        detail = (start.stderr or start.stdout or "docker start failed").strip()
        raise RuntimeError(
            f"captcha container {container!r} start failed (previous state={status}, "
            f"exit={exit_code}, oom={oom_killed}): {detail}"
        )
    deadline = time.time() + max(1.0, min(float(wait_timeout or 30.0), 45.0))
    last_state = status
    while time.time() < deadline:
        time.sleep(0.5)
        state, code, oom = _docker_container_state(container)
        last_state = state
        if state == "running":
            return
        if state in {"exited", "dead"}:
            raise RuntimeError(
                f"captcha container {container!r} stopped while starting: state={state}, exit={code}, oom={oom}"
            )
    raise TimeoutError(f"captcha container {container!r} did not become running (last state={last_state})")


def _docker_exec_json(container: str, path: str, body: dict[str, Any], port: int) -> dict[str, Any]:
    """Execute a solver request, recovering once if Docker reports a stopped container."""
    for attempt in range(2):
        _ensure_docker_container_running(container)
        try:
            return _docker_exec_json_raw(container, path, body, port)
        except RuntimeError as exc:
            detail = str(exc)
            if attempt == 0 and "not running" in detail.lower():
                _ensure_docker_container_running(container)
                continue
            try:
                status, exit_code, oom_killed = _docker_container_state(container)
                detail = f"{detail}; state={status}, exit={exit_code}, oom={oom_killed}"
            except Exception as state_exc:
                detail = f"{detail}; state lookup failed: {state_exc}"
            raise RuntimeError(detail) from exc
    raise RuntimeError("docker exec captcha failed after retry")


def _docker_solve(endpoint: str, payload: dict[str, Any], timeout: float) -> str:
    if not _docker_cli_available():
        raise RuntimeError(
            "Docker 回退不可用：服务器未找到 docker 命令；请启动 HTTP solver 并使用 http(s)://host:5072，"
            "或安装 Docker 后配置 docker:// 容器端点"
        )
    raw = endpoint[len("docker://") :]
    if ":" in raw:
        container, port_s = raw.rsplit(":", 1)
        port = int(port_s)
    else:
        container, port = raw, 5072

    task = payload.get("task")
    if isinstance(task, dict) and task.get("proxy"):
        payload = {**payload, "task": {**task, "proxy": _dockerize_loopback_proxy(str(task["proxy"]))}}

    create_data = _docker_exec_json(container, "/createTask", payload, port)
    if create_data.get("errorId", 0) not in (0, "0", None):
        raise RuntimeError(f"docker 本地过盾 createTask 失败: {create_data}")
    task_id = create_data.get("taskId") or create_data.get("task_id")
    if not task_id:
        token = _extract_token(create_data)
        if len(token) > 20:
            return token
        raise RuntimeError(f"docker 本地过盾无 taskId: {create_data}")

    solve_timeout = max(30.0, float(timeout or 180))
    deadline = time.monotonic() + solve_timeout
    while time.monotonic() < deadline:
        result = _docker_exec_json(
            container,
            "/getTaskResult",
            {"clientKey": payload.get("clientKey") or "local", "taskId": task_id},
            port,
        )
        if result.get("errorId", 0) not in (0, "0", None):
            raise RuntimeError(f"docker 本地过盾任务失败: {result}")
        error_code = str(result.get("errorCode") or "").strip()
        status = str(result.get("status") or "").lower()
        if error_code or status in {"failed", "error"}:
            raise RuntimeError(f"docker 本地过盾任务失败: {result}")
        token = _extract_token(result)
        if len(token) > 20:
            return token
        if status in {"ready", "success", "completed"}:
            raise RuntimeError(f"docker 本地过盾 status={status} 但无 token: {result}")
        time.sleep(min(2.5, max(0.1, deadline - time.monotonic())))
    raise TimeoutError(f"docker 本地过盾超时（{solve_timeout:.1f}s），taskId={task_id}")


def solve_turnstile_local(
    *,
    website_url: str = SIGNUP_URL,
    website_key: str = DEFAULT_SITEKEY,
    timeout: float = 180.0,
    headless: bool = True,
    proxy: str = "",
    endpoint: str = "",
    client_key: str = "",
    task_type: str = DEFAULT_TASK_TYPE,
) -> str:
    del headless
    endpoint = (
        endpoint
        or os.environ.get("LOCAL_CAPTCHA_ENDPOINT")
        or os.environ.get("YESCAPTCHA_ENDPOINT")
        or ""
    ).strip()
    if not endpoint:
        raise RuntimeError(
            "本地过盾未配置 captcha_endpoint。可用 http://127.0.0.1:5072 或 docker://grokcli-2api:5072"
        )
    client_key = (client_key or os.environ.get("LOCAL_CAPTCHA_CLIENT_KEY") or "local").strip()
    if client_key.startswith("AC-"):
        client_key = "local"
    payload = _task_payload(website_url, website_key, task_type, proxy, client_key)

    if endpoint.startswith("docker://"):
        return _docker_solve(endpoint, payload, timeout)

    pinned_endpoint = _pin_http_endpoint(endpoint.rstrip("/"))
    try:
        return _http_solve_with_retries(pinned_endpoint, payload, timeout)
    except Exception as primary:
        # An HTTP endpoint may be a remote/container service. Only try the
        # legacy local-container fallback when Docker is actually available;
        # otherwise preserve the solver error and actionable environment hint.
        parsed = urlsplit(endpoint)
        fallback_enabled = os.environ.get("LOCAL_CAPTCHA_DOCKER_FALLBACK", "1").strip().lower() not in {
            "0", "false", "no", "off"
        }
        if parsed.port == 5072 and parsed.hostname in {"127.0.0.1", "localhost", "::1"} and fallback_enabled:
            if not _docker_cli_available():
                raise RuntimeError(
                    f"HTTP 本地过盾失败: {primary}; 未执行 Docker 回退：服务器未找到 docker 命令。"
                    "请检查 HTTP solver 的 /health，或配置可用的 docker:// 容器端点"
                ) from primary
            try:
                return _docker_solve("docker://grokcli-2api:5072", payload, timeout)
            except Exception as secondary:
                raise RuntimeError(f"HTTP 本地过盾失败: {primary}; docker 回退失败: {secondary}") from secondary
        raise
