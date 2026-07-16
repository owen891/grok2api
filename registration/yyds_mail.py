#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""轻量 YYDS 邮箱助手：协议注册用，不依赖 DrissionPage。"""
from __future__ import annotations

import re
import secrets
import string
import time
from typing import Any, Optional

import requests
import urllib3
from urllib3.exceptions import InsecureRequestWarning

urllib3.disable_warnings(InsecureRequestWarning)

YYDS_API_BASE = "https://maliapi.215.im/v1"
config: dict[str, Any] = {}


def _headers(api_key: Optional[str] = None, jwt: Optional[str] = None, json_body: bool = False) -> dict[str, str]:
    key = api_key or config.get("yyds_api_key") or ""
    token = jwt or config.get("yyds_jwt") or ""
    headers: dict[str, str] = {}
    if json_body:
        headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = f"Bearer {token}"
    elif key:
        headers["X-API-Key"] = str(key)
    return headers


def yyds_get_domains(api_key=None, jwt=None):
    resp = requests.get(f"{YYDS_API_BASE}/domains", headers=_headers(api_key, jwt), timeout=20, verify=False)
    resp.raise_for_status()
    data = resp.json()
    return data.get("data", []) if data.get("success") else []


def yyds_create_account(address=None, domain=None, api_key=None, jwt=None):
    payload: dict[str, Any] = {}
    if address:
        payload["address"] = address
    if domain:
        payload["domain"] = domain
    else:
        payload["autoDomainStrategy"] = "prefer_owned"
    resp = requests.post(
        f"{YYDS_API_BASE}/accounts",
        json=payload,
        headers=_headers(api_key, jwt, json_body=True),
        timeout=20,
        verify=False,
    )
    resp.raise_for_status()
    data = resp.json()
    if data.get("success"):
        return data.get("data", {})
    raise RuntimeError(f"YYDS 创建邮箱失败: {data}")


def yyds_get_token(address, api_key=None, jwt=None):
    resp = requests.post(
        f"{YYDS_API_BASE}/token",
        json={"address": address},
        headers=_headers(api_key, jwt, json_body=True),
        timeout=20,
        verify=False,
    )
    resp.raise_for_status()
    data = resp.json()
    if data.get("success"):
        return data.get("data", {}).get("token")
    raise RuntimeError(f"YYDS 获取 token 失败: {data}")


def yyds_get_messages(address, token=None, api_key=None, jwt=None):
    headers = _headers(api_key, jwt or token)
    if token and not headers.get("Authorization"):
        headers["Authorization"] = f"Bearer {token}"
    resp = requests.get(
        f"{YYDS_API_BASE}/messages",
        params={"address": address},
        headers=headers,
        timeout=20,
        verify=False,
    )
    resp.raise_for_status()
    data = resp.json()
    if data.get("success"):
        return data.get("data", {}).get("messages", [])
    return []


def yyds_get_message_detail(message_id, token=None, api_key=None, jwt=None):
    headers = _headers(api_key, jwt or token)
    if token and not headers.get("Authorization"):
        headers["Authorization"] = f"Bearer {token}"
    resp = requests.get(
        f"{YYDS_API_BASE}/messages/{message_id}",
        headers=headers,
        timeout=20,
        verify=False,
    )
    resp.raise_for_status()
    data = resp.json()
    if data.get("success"):
        return data.get("data", {})
    raise RuntimeError(f"YYDS 获取邮件详情失败: {data}")


def yyds_generate_username(length: int = 10) -> str:
    chars = string.ascii_lowercase + string.digits
    return "".join(secrets.choice(chars) for _ in range(length))


def yyds_pick_domain(api_key=None, jwt=None) -> str:
    domains = yyds_get_domains(api_key=api_key, jwt=jwt)
    if not domains:
        raise RuntimeError("YYDS 没有返回任何可用域名")
    private = [d for d in domains if d.get("isVerified") and not d.get("isPublic")]
    pool = private or [d for d in domains if d.get("isVerified")] or domains
    item = pool[0]
    if isinstance(item, str):
        return item
    return str(item.get("domain") or item.get("name") or "")


def yyds_get_email_and_token(api_key=None, jwt=None):
    key = api_key or config.get("yyds_api_key")
    token = jwt or config.get("yyds_jwt")
    if not token and not key:
        raise RuntimeError("YYDS API Key 或 JWT 未配置")
    domain = yyds_pick_domain(api_key=key, jwt=token)
    username = yyds_generate_username(10)
    result = yyds_create_account(address=username, domain=domain, api_key=key, jwt=token)
    address = result.get("address") or f"{username}@{domain}"
    temp_token = result.get("token") or yyds_get_token(address, api_key=key, jwt=token)
    if not temp_token:
        raise RuntimeError("获取 YYDS token 失败")
    return address, temp_token


def extract_verification_code(text: str, subject: str = "") -> Optional[str]:
    """提取 xAI 邮箱验证码。

    xAI 常见格式：
      - ABC-DEF（3+3 带横杠）
      - 6 位字母数字（必须含字母）
    明确拒绝纯数字（如 333333），避免从 HTML/CSS 误抓。
    """
    subject = subject or ""
    text = text or ""
    blob = subject + "\n" + text
    if not blob.strip():
        return None

    # 1) subject: "LSQ-OPU xAI"
    m = re.search(r"(?i)^\s*([A-Z0-9]{3}-[A-Z0-9]{3})\s+xAI", subject)
    if m:
        return m.group(1).upper()

    # 2) body/subject ABC-DEF
    m = re.search(r"(?<![A-Z0-9])([A-Z0-9]{3}-[A-Z0-9]{3})(?![A-Z0-9])", blob, flags=re.I)
    if m:
        return m.group(1).upper()

    # 3) keyword-anchored alphanumeric 4-8（拒绝纯数字与常见英文词）
    deny = {
        "CODE",
        "CODES",
        "VERIFY",
        "VERIF",
        "OTP",
        "XAI",
        "EMAIL",
        "LOGIN",
        "TOKEN",
        "HTTPS",
        "HTTP",
        "WWW",
    }
    m = re.search(
        r"(?i)(?:your\s+code\s+is|code\s+is|verification\s+code|verify\s+code|otp|验证码|code)"
        r"\s*[:=\-]?\s*([A-Z0-9]{3}-[A-Z0-9]{3}|[A-Z0-9]{4,8})",
        blob,
    )
    if m:
        code = m.group(1).upper()
        if not code.isdigit() and code not in deny and not code.startswith("HTTP"):
            return code

    # 4) "XXXXXX is your code"
    m = re.search(
        r"(?i)\b([A-Z0-9]{3}-[A-Z0-9]{3}|[A-Z0-9]{6})\b\s+is\s+your\s+code",
        blob,
    )
    if m:
        code = m.group(1).upper()
        if not code.isdigit() and code not in deny:
            return code

    # 5) standalone 6-char alnum containing letters
    for m in re.finditer(r"(?i)\b([A-Z0-9]{6})\b", blob):
        code = m.group(1).upper()
        if code.isdigit() or code in deny:
            continue
        if code.lower() in {"ffffff", "000000", "abcdef"}:
            continue
        if any(ch.isalpha() for ch in code):
            return code

    return None

