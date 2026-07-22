import json
import os
import stat
from pathlib import Path

from protocol_auth.xconsole_client.xai_oauth import (
    save_cliproxyapi_auth_record,
    save_oauth_record,
)


def _assert_private_file(path: Path) -> None:
    assert path.is_file()
    assert not list(path.parent.glob(".*.tmp"))
    if os.name != "nt":
        assert stat.S_IMODE(path.stat().st_mode) == 0o600
        assert stat.S_IMODE(path.parent.stat().st_mode) == 0o700


def test_oauth_record_is_written_atomically_with_private_permissions(tmp_path):
    path = save_oauth_record(
        {"access_token": "synthetic-access", "refresh_token": "synthetic-refresh"},
        userinfo={"email": "account@example.invalid"},
        output_dir=tmp_path / "oauth",
    )

    _assert_private_file(path)
    assert json.loads(path.read_text(encoding="utf-8"))["email"] == "account@example.invalid"


def test_cliproxy_record_is_written_atomically_with_private_permissions(tmp_path):
    path = save_cliproxyapi_auth_record(
        {"access_token": "synthetic-access", "refresh_token": "synthetic-refresh"},
        userinfo={"email": "account@example.invalid"},
        auth_dir=tmp_path / "cliproxy",
    )

    _assert_private_file(path)
    assert json.loads(path.read_text(encoding="utf-8"))["type"] == "xai"
