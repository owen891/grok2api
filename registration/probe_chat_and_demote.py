#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""探测 v3 导出账号的 chat 可用性，并对 403 号执行 disable。

用法：
  python probe_chat_and_demote.py --base http://127.0.0.1:8002 \
    --user admin --password '...' [--dry-run] [--limit 200]
"""
from __future__ import annotations

import argparse
import json
import time
import urllib.error
import urllib.request
from collections import Counter
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Any


def api(base: str, method: str, path: str, token: str | None = None, body: dict | None = None) -> Any:
    data = None if body is None else json.dumps(body).encode()
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(base.rstrip("/") + path, data=data, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=60) as resp:
        return json.loads(resp.read().decode("utf-8", "replace"))


def login(base: str, user: str, password: str) -> str:
    d = api(base, "POST", "/api/admin/v1/auth/login", body={"username": user, "password": password})
    return d["data"]["tokens"]["accessToken"]


def probe_token(token: str) -> dict[str, Any]:
    headers = {
        "Authorization": f"Bearer {token}",
        "Content-Type": "application/json",
        "User-Agent": "grok-shell/0.2.99 (windows; x86_64)",
        "x-grok-client-version": "0.2.99",
        "x-grok-client-identifier": "grok-shell",
    }
    payload = json.dumps(
        {"model": "grok-4.5", "stream": False, "messages": [{"role": "user", "content": "ping"}]}
    ).encode()
    req = urllib.request.Request(
        "https://cli-chat-proxy.grok.com/v1/chat/completions",
        data=payload,
        headers=headers,
        method="POST",
    )
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            body = resp.read().decode("utf-8", "replace")
            return {"ok": True, "status": resp.status, "ms": int((time.time() - t0) * 1000), "preview": body[:80]}
    except urllib.error.HTTPError as e:
        body = e.read().decode("utf-8", "replace") if hasattr(e, "read") else ""
        code = "permission_denied" if e.code in (401, 403) else f"http_{e.code}"
        if e.code == 429:
            code = "rate_limited"
        return {
            "ok": False,
            "status": e.code,
            "code": code,
            "ms": int((time.time() - t0) * 1000),
            "preview": body[:120],
        }
    except Exception as e:
        return {"ok": False, "code": "network", "error": str(e)[:160]}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:8002")
    ap.add_argument("--user", default="admin")
    ap.add_argument("--password", required=True)
    ap.add_argument("--limit", type=int, default=200)
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--workers", type=int, default=8)
    args = ap.parse_args()

    token = login(args.base, args.user, args.password)
    exported = api(args.base, "GET", "/api/admin/v1/accounts/export", token=token)
    accounts = exported.get("accounts") if isinstance(exported, dict) else []
    if not isinstance(accounts, list):
        raise SystemExit(f"unexpected export shape: {type(exported)}")

    # list for ids mapping by email
    listed = api(args.base, "GET", f"/api/admin/v1/accounts?page=1&pageSize={max(args.limit, 100)}", token=token)
    items = (listed.get("data") or {}).get("items") or []
    # paginate
    total = (listed.get("data") or {}).get("total") or len(items)
    page = 2
    while len(items) < min(total, args.limit):
        more = api(args.base, "GET", f"/api/admin/v1/accounts?page={page}&pageSize=100", token=token)
        batch = (more.get("data") or {}).get("items") or []
        if not batch:
            break
        items.extend(batch)
        page += 1
        if page > 50:
            break
    by_email = {str(i.get("email") or "").lower(): i for i in items}

    targets = []
    for a in accounts[: args.limit]:
        if not isinstance(a, dict):
            continue
        access = a.get("access_token") or ""
        email = str(a.get("email") or "")
        if not access or not email:
            continue
        meta = by_email.get(email.lower()) or {}
        targets.append(
            {
                "email": email,
                "access_token": access,
                "id": meta.get("id") or a.get("id"),
                "enabled": meta.get("enabled"),
                "authStatus": meta.get("authStatus"),
            }
        )

    print(f"probe_targets={len(targets)}")
    results = []
    with ThreadPoolExecutor(max_workers=max(1, args.workers)) as ex:
        futs = {ex.submit(probe_token, t["access_token"]): t for t in targets}
        for fut in as_completed(futs):
            t = futs[fut]
            r = fut.result()
            results.append({**t, **r})

    usable = [r for r in results if r.get("ok")]
    denied = [r for r in results if not r.get("ok") and r.get("code") == "permission_denied"]
    other = [r for r in results if not r.get("ok") and r.get("code") != "permission_denied"]
    print("usable", len(usable), "permission_denied", len(denied), "other", len(other))
    print("other_codes", Counter(r.get("code") for r in other))

    demoted = []
    for r in denied:
        acc_id = r.get("id")
        if not acc_id:
            continue
        if args.dry_run:
            demoted.append({"id": acc_id, "email": r.get("email"), "dry_run": True})
            continue
        try:
            api(args.base, "PATCH", f"/api/admin/v1/accounts/{acc_id}", token=token, body={"enabled": False})
            demoted.append({"id": acc_id, "email": r.get("email"), "disabled": True})
        except Exception as e:
            demoted.append({"id": acc_id, "email": r.get("email"), "error": str(e)[:160]})

    report = {
        "usable_count": len(usable),
        "usable": [{"id": r.get("id"), "email": r.get("email"), "ms": r.get("ms")} for r in usable],
        "denied_count": len(denied),
        "demoted": demoted,
        "other_count": len(other),
    }
    try:
        from pathlib import Path
        out = Path("chat_probe_demote_report.json")
        out.write_text(json.dumps(report, ensure_ascii=False, indent=2), encoding="utf-8")
        print("report", out.resolve())
    except Exception as e:
        print("report_write_failed", e)
    print("demoted", len([d for d in demoted if d.get("disabled") or d.get("dry_run")]))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
