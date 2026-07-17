#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""本地过盾：HTTP 或 docker:// 桥接，不弹浏览器。

captcha_endpoint 示例：
  http://127.0.0.1:5072
  docker://grokcli-2api:5072
"""
from __future__ import annotations

import json
import os
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

    deadline = time.time() + max(30.0, float(timeout or 120))
    while time.time() < deadline:
        result_resp = requests.post(
            f"{base}/getTaskResult",
            json={"clientKey": payload.get("clientKey") or "local", "taskId": task_id},
            timeout=45,
        )
        if result_resp.status_code >= 400:
            raise RuntimeError(f"本地过盾 getTaskResult HTTP {result_resp.status_code}: {result_resp.text[:300]}")
        result = result_resp.json()
        if result.get("errorId", 0) not in (0, "0", None):
            raise RuntimeError(f"本地过盾任务失败: {result}")
        token = _extract_token(result)
        if len(token) > 20:
            return token
        status = str(result.get("status") or "").lower()
        if status in {"ready", "success", "completed"}:
            raise RuntimeError(f"本地过盾 status={status} 但无 token: {result}")
        time.sleep(2.0)
    raise TimeoutError(f"本地过盾超时（{timeout}s），taskId={task_id}")


def _docker_exec_json(container: str, path: str, body: dict[str, Any], port: int) -> dict[str, Any]:
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


def _docker_solve(endpoint: str, payload: dict[str, Any], timeout: float) -> str:
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

    deadline = time.time() + max(30.0, float(timeout or 180))
    while time.time() < deadline:
        result = _docker_exec_json(
            container,
            "/getTaskResult",
            {"clientKey": payload.get("clientKey") or "local", "taskId": task_id},
            port,
        )
        if result.get("errorId", 0) not in (0, "0", None):
            raise RuntimeError(f"docker 本地过盾任务失败: {result}")
        token = _extract_token(result)
        if len(token) > 20:
            return token
        status = str(result.get("status") or "").lower()
        if status in {"ready", "success", "completed"}:
            raise RuntimeError(f"docker 本地过盾 status={status} 但无 token: {result}")
        time.sleep(2.5)
    raise TimeoutError(f"docker 本地过盾超时（{timeout}s），taskId={task_id}")


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

    try:
        return _http_solve(endpoint.rstrip("/"), payload, timeout)
    except Exception as primary:
        if "5072" in endpoint:
            try:
                return _docker_solve("docker://grokcli-2api:5072", payload, timeout)
            except Exception as secondary:
                raise RuntimeError(f"HTTP 本地过盾失败: {primary}; docker 回退失败: {secondary}") from secondary
        raise
