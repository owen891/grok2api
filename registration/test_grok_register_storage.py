import json
import os
import stat
from types import SimpleNamespace

import grok_register_ttk as registration


def test_private_registration_files_are_atomic_and_owner_only(tmp_path):
    text_path = tmp_path / "runtime" / "accounts.txt"
    config_path = tmp_path / "runtime" / "config.json"

    registration._append_private_text(text_path, "synthetic-account\n")
    registration._write_private_json(config_path, {"api_key": "synthetic"})

    assert text_path.read_text(encoding="utf-8") == "synthetic-account\n"
    assert json.loads(config_path.read_text(encoding="utf-8")) == {"api_key": "synthetic"}
    assert not list(config_path.parent.glob(".registration-*.tmp"))
    if os.name != "nt":
        assert stat.S_IMODE(text_path.stat().st_mode) == 0o600
        assert stat.S_IMODE(config_path.stat().st_mode) == 0o600
        assert stat.S_IMODE(config_path.parent.stat().st_mode) == 0o700


def test_registration_debug_url_does_not_expose_query_tokens(capsys):
    page = SimpleNamespace(
        run_js=lambda _script: {
            "url": "https://accounts.x.ai/set-cookie?q=secret-jwt#secret-fragment",
            "btns": [],
            "inputs": [],
        }
    )

    registration.dump_state(page, "oauth")

    rendered = capsys.readouterr().out
    assert "https://accounts.x.ai/set-cookie" in rendered
    assert "secret" not in rendered


def test_cookie_snapshot_is_private_and_atomic(tmp_path, monkeypatch):
    cookie_dir = tmp_path / "cookies" / "grok"
    monkeypatch.setattr(registration, "_COOKIE_DIR", str(cookie_dir))
    monkeypatch.setattr(registration, "_get_browser", lambda: SimpleNamespace(cookies=lambda: [{"name": "sso", "value": "synthetic"}]))
    monkeypatch.setitem(registration.PERF_FLAGS, "cookie_snapshot", True)

    registration.save_cookies_snapshot(SimpleNamespace(url="https://grok.com"), tag="success", email="user@example.test")

    snapshots = list(cookie_dir.glob("full_*_success.json"))
    assert len(snapshots) == 1
    assert json.loads(snapshots[0].read_text(encoding="utf-8"))["cookies"][0]["value"] == "synthetic"
    assert not list(cookie_dir.glob(".registration-*.tmp"))
    if os.name != "nt":
        assert stat.S_IMODE(snapshots[0].stat().st_mode) == 0o600
        assert stat.S_IMODE(cookie_dir.stat().st_mode) == 0o700


def test_local_token_pool_is_private_and_atomic(tmp_path, monkeypatch):
    token_path = tmp_path / "pool" / "token.json"
    monkeypatch.setattr(registration, "resolve_grok2api_local_token_file", lambda: str(token_path))
    monkeypatch.setitem(registration.config, "grok2api_pool_name", "grok")

    assert registration.add_token_to_grok2api_local_pool("sso=synthetic-token", "user@example.test")

    data = json.loads(token_path.read_text(encoding="utf-8"))
    assert data["grok"] == [{"token": "synthetic-token", "tags": ["auto-register"], "note": "user@example.test"}]
    assert not list(token_path.parent.glob(".registration-*.tmp"))
    if os.name != "nt":
        assert stat.S_IMODE(token_path.stat().st_mode) == 0o600
        assert stat.S_IMODE(token_path.parent.stat().st_mode) == 0o700
