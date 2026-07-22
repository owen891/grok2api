"""Credential-safe values for registration logs and diagnostics."""

from __future__ import annotations

from urllib.parse import urlsplit, urlunsplit


def redact_url(value: str) -> str:
    """Remove userinfo, query parameters, and fragments from a logged URL."""
    try:
        parsed = urlsplit(str(value or ""))
        if not parsed.scheme or not parsed.hostname:
            return parsed.path or "(url)"
        host = parsed.hostname
        if ":" in host and not host.startswith("["):
            host = f"[{host}]"
        port = f":{parsed.port}" if parsed.port is not None else ""
        return urlunsplit((parsed.scheme, host + port, parsed.path, "", ""))
    except (TypeError, ValueError):
        return "(invalid-url)"


__all__ = ["redact_url"]
