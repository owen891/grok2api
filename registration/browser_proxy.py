"""Chromium proxy configuration with optional HTTP proxy authentication."""

from __future__ import annotations

import atexit
import hashlib
import json
import os
import shutil
import tempfile
import threading
from pathlib import Path
from typing import Any
from urllib.parse import unquote, urlparse


_AUTH_SCHEMES = frozenset({"http", "https"})
_extension_lock = threading.Lock()
_extension_directories: dict[str, Path] = {}


def _parsed_proxy(raw_proxy: str):
    value = (raw_proxy or "").strip()
    if not value:
        return None
    parsed = urlparse(value if "://" in value else f"http://{value}")
    if not parsed.hostname:
        raise ValueError("proxy host is missing")
    return parsed


def chromium_proxy_server(raw_proxy: str) -> str:
    parsed = _parsed_proxy(raw_proxy)
    if parsed is None:
        return ""
    scheme = (parsed.scheme or "http").lower()
    host = parsed.hostname or ""
    if ":" in host and not host.startswith("["):
        host = f"[{host}]"
    port = parsed.port or (443 if scheme == "https" else 1080 if scheme.startswith("socks") else 80)
    return f"{scheme}://{host}:{port}"


def proxy_auth_supported(raw_proxy: str) -> tuple[bool, str]:
    parsed = _parsed_proxy(raw_proxy)
    if parsed is None or parsed.username is None:
        return True, "no proxy authentication"
    scheme = (parsed.scheme or "http").lower()
    if scheme not in _AUTH_SCHEMES:
        return False, "authenticated SOCKS proxies require a local unauthenticated relay"
    return True, "Chromium proxy authentication extension"


def _proxy_auth_extension(raw_proxy: str) -> Path | None:
    parsed = _parsed_proxy(raw_proxy)
    if parsed is None or parsed.username is None:
        return None
    supported, detail = proxy_auth_supported(raw_proxy)
    if not supported:
        raise ValueError(detail)

    with _extension_lock:
        cache_key = hashlib.sha256(raw_proxy.encode("utf-8")).hexdigest()
        cached = _extension_directories.get(cache_key)
        if cached and cached.is_dir():
            return cached

        directory = Path(tempfile.mkdtemp(prefix=f"grok2api-proxy-auth-{os.getpid()}-"))
        os.chmod(directory, 0o700)
        manifest = {
            "manifest_version": 3,
            "name": "Grok2API Proxy Authentication",
            "version": "1.0.0",
            "permissions": ["webRequest", "webRequestAuthProvider"],
            "host_permissions": ["<all_urls>"],
            "background": {"service_worker": "service_worker.js"},
        }
        credentials = {
            "username": unquote(parsed.username or ""),
            "password": unquote(parsed.password or ""),
        }
        worker = (
            f"const credentials = {json.dumps(credentials, ensure_ascii=True)};\n"
            "chrome.webRequest.onAuthRequired.addListener(\n"
            "  (details, callback) => callback(details.isProxy ? {authCredentials: credentials} : {}),\n"
            "  {urls: ['<all_urls>']},\n"
            "  ['asyncBlocking']\n"
            ");\n"
        )
        manifest_path = directory / "manifest.json"
        worker_path = directory / "service_worker.js"
        manifest_path.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
        worker_path.write_text(worker, encoding="utf-8")
        os.chmod(manifest_path, 0o600)
        os.chmod(worker_path, 0o600)
        _extension_directories[cache_key] = directory
        return directory


def configure_chromium_proxy(options: Any, raw_proxy: str) -> str:
    server = chromium_proxy_server(raw_proxy)
    if not server:
        return ""
    options.set_argument(f"--proxy-server={server}")
    extension = _proxy_auth_extension(raw_proxy)
    if extension is not None:
        options.add_extension(str(extension))
    return server


def cleanup_proxy_auth_extensions() -> None:
    with _extension_lock:
        directories = list(_extension_directories.values())
        _extension_directories.clear()
    for directory in directories:
        shutil.rmtree(directory, ignore_errors=True)


atexit.register(cleanup_proxy_auth_extensions)


__all__ = [
    "chromium_proxy_server",
    "cleanup_proxy_auth_extensions",
    "configure_chromium_proxy",
    "proxy_auth_supported",
]
