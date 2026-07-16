#!/usr/bin/env python3
"""Create a grok_web egress node for Cloudflare-bypass image generation.

Fill PROXY_URL / CF_COOKIES / USER_AGENT below, then run:
  python scripts/create_web_egress_node.py
"""

from __future__ import annotations

import json
import urllib.request

BASE = "http://127.0.0.1:8002"
ADMIN_USER = "admin"
ADMIN_PASSWORD = "local-v3-preview-password-20260714"

# ---- fill these ----
PROXY_URL = "http://user:pass@host:port"  # stable residential/proxy URL
USER_AGENT = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/138.0.0.0 Safari/537.36"
CF_COOKIES = "cf_clearance=PASTE_HERE; __cf_bm=OPTIONAL"
NODE_NAME = "web-cf-1"
# --------------------


def login() -> str:
    req = urllib.request.Request(
        f"{BASE}/api/admin/v1/auth/login",
        data=json.dumps({"username": ADMIN_USER, "password": ADMIN_PASSWORD}).encode(),
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


def main() -> None:
    if "PASTE_HERE" in CF_COOKIES or "user:pass@host" in PROXY_URL:
        raise SystemExit("Please edit PROXY_URL / CF_COOKIES / USER_AGENT in this script first.")
    token = login()
    created = create_node(token)
    print("created:", json.dumps(created, ensure_ascii=False, indent=2))
    print("nodes:", json.dumps(list_nodes(token), ensure_ascii=False, indent=2)[:2000])


if __name__ == "__main__":
    main()
