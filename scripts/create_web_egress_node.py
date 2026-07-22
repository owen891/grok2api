#!/usr/bin/env python3
"""Create a grok_web egress node for Cloudflare-bypass image generation.

Fill PROXY_URL / CF_COOKIES / USER_AGENT below, set
GROK2API_ADMIN_PASSWORD or enter it when prompted, then run the script.
"""

from __future__ import annotations

import getpass
import json
import os
import urllib.request

BASE = "http://127.0.0.1:8002"
ADMIN_USER = "admin"

# ---- fill these ----
PROXY_URL = "http://user:pass@host:port"  # stable residential/proxy URL
USER_AGENT = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
CF_COOKIES = "cf_clearance=PASTE_HERE; __cf_bm=OPTIONAL"
NODE_NAME = "web-cf-1"
# --------------------

_PUBLIC_NODE_FIELDS = (
    "id",
    "name",
    "scope",
    "enabled",
    "proxyConfigured",
    "cookieConfigured",
)


def login() -> str:
    password = os.environ.get("GROK2API_ADMIN_PASSWORD", "") or getpass.getpass("Grok2API admin password: ")
    if not password:
        raise RuntimeError("GROK2API admin password is required")
    req = urllib.request.Request(
        f"{BASE}/api/admin/v1/auth/login",
        data=json.dumps({"username": ADMIN_USER, "password": password}).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=15) as resp:
        body = json.loads(resp.read().decode())
    return body["data"]["tokens"]["accessToken"]


def create_node(token: str) -> dict:
    payload = {
        "name": NODE_NAME,
        "scope": "grok_web",
        "enabled": True,
        "proxyURL": PROXY_URL,
        "userAgent": USER_AGENT,
        "cloudflareCookies": CF_COOKIES,
    }
    req = urllib.request.Request(
        f"{BASE}/api/admin/v1/egress-nodes",
        data=json.dumps(payload).encode(),
        headers={
            "Authorization": f"Bearer {token}",
            "Content-Type": "application/json",
        },
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=15) as resp:
        return json.loads(resp.read().decode())


def list_nodes(token: str) -> dict:
    req = urllib.request.Request(
        f"{BASE}/api/admin/v1/egress-nodes",
        headers={"Authorization": f"Bearer {token}"},
    )
    with urllib.request.urlopen(req, timeout=15) as resp:
        return json.loads(resp.read().decode())


def public_node_summary(value: object) -> dict:
    if not isinstance(value, dict):
        return {}
    return {field: value[field] for field in _PUBLIC_NODE_FIELDS if field in value}


def public_create_summary(response: object) -> dict:
    if not isinstance(response, dict):
        return {}
    return public_node_summary(response.get("data", response))


def public_list_summary(response: object) -> list[dict]:
    if not isinstance(response, dict):
        return []
    data = response.get("data", response)
    items = data.get("items", []) if isinstance(data, dict) else []
    return [summary for value in items if (summary := public_node_summary(value))]


def main() -> None:
    if "PASTE_HERE" in CF_COOKIES or "user:pass@host" in PROXY_URL:
        raise SystemExit("Please edit PROXY_URL / CF_COOKIES / USER_AGENT in this script first.")
    token = login()
    created = create_node(token)
    print("created:", json.dumps(public_create_summary(created), ensure_ascii=False, indent=2))
    print("nodes:", json.dumps(public_list_summary(list_nodes(token)), ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
