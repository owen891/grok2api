"""Small file-only publisher used by the protocol registration worker."""

from __future__ import annotations

import json
import os
import shutil
import tempfile
import time
from pathlib import Path
from typing import Any


def stage_hotload_file(source: Path, incoming_dir: Path) -> Path:
    incoming_dir.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{source.stem}-", suffix=".tmp", dir=incoming_dir)
    os.close(descriptor)
    temporary = Path(temporary_name)
    destination = incoming_dir / source.name
    try:
        shutil.copyfile(source, temporary)
        os.chmod(temporary, 0o600)
        os.replace(temporary, destination)
    finally:
        temporary.unlink(missing_ok=True)
    return destination


def await_hotload_result(
    incoming_dir: Path,
    credential_stem: str,
    *,
    submitted_at: float,
    timeout: float,
    poll_interval: float = 0.5,
) -> dict[str, Any]:
    deadline = time.monotonic() + timeout
    spool_root = incoming_dir.parent
    while time.monotonic() < deadline:
        candidates: list[tuple[float, str, Path]] = []
        for bucket in ("processed", "failed"):
            directory = spool_root / bucket
            if not directory.is_dir():
                continue
            for path in directory.glob(f"{credential_stem}*.result.json"):
                try:
                    modified = path.stat().st_mtime
                except OSError:
                    continue
                if modified + 1 < submitted_at:
                    continue
                candidates.append((modified, bucket, path))
        for _, bucket, path in sorted(candidates, reverse=True):
            try:
                payload = json.loads(path.read_text(encoding="utf-8"))
            except (OSError, ValueError):
                continue
            if not isinstance(payload, dict):
                continue
            status = str(payload.get("status") or "")
            sync_failed = int(payload.get("syncFailed") or 0)
            return {
                "ok": bucket == "processed" and status == "processed" and sync_failed == 0,
                "bucket": bucket,
                "status": status,
                "created": int(payload.get("created") or 0),
                "updated": int(payload.get("updated") or 0),
                "synced": int(payload.get("synced") or 0),
                "syncFailed": sync_failed,
                "syncErrors": payload.get("syncErrors") if isinstance(payload.get("syncErrors"), list) else [],
                "processedAt": str(payload.get("processedAt") or ""),
            }
        time.sleep(max(0.05, poll_interval))
    return {"ok": False, "status": "timeout"}
