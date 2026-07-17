import { useCallback, useEffect, useMemo, useState } from "react";

import { compareVersions, decodeReleaseManifest, type ReleaseManifest } from "@/shared/version/version-manifest";

const remoteManifestURL = "https://raw.githubusercontent.com/owen891/grok2api/main/frontend/public/release-manifest.json";

export const currentVersion = __APP_VERSION__;

export function useVersionUpdate() {
  const [manifest, setManifest] = useState<ReleaseManifest | null>(null);
  const [checking, setChecking] = useState(false);
  const [checkFailed, setCheckFailed] = useState(false);
  const [open, setOpen] = useState(false);

  const check = useCallback(async () => {
    setChecking(true);
    setCheckFailed(false);
    try {
      setManifest(await loadManifest());
    } catch {
      setCheckFailed(true);
    } finally {
      setChecking(false);
    }
  }, []);

  useEffect(() => {
    let active = true;
    void loadManifest()
      .then((next) => {
        if (!active) return;
        setManifest(next);
        setCheckFailed(false);
        if (compareVersions(next.latest, currentVersion) > 0 && shouldShowReminder(next.latest)) {
          setOpen(true);
        }
      })
      .catch(() => {
        if (active) setCheckFailed(true);
      });
    return () => {
      active = false;
    };
  }, []);

  const latestVersion = manifest?.latest ?? currentVersion;
  const updateAvailable = compareVersions(latestVersion, currentVersion) > 0;

  return useMemo(() => ({
    manifest,
    latestVersion,
    updateAvailable,
    checking,
    checkFailed,
    open,
    setOpen,
    check,
  }), [manifest, latestVersion, updateAvailable, checking, checkFailed, open, check]);
}

function shouldShowReminder(latestVersion: string): boolean {
  const key = `grok2api-version-reminder:${latestVersion}`;
  try {
    if (window.localStorage.getItem(key)) return false;
    window.localStorage.setItem(key, "shown");
  } catch {
    // The reminder still works when storage is blocked.
  }
  return true;
}

async function loadManifest(): Promise<ReleaseManifest> {
  const candidates = [`${remoteManifestURL}?t=${Date.now()}`, "/release-manifest.json"];
  let lastError: unknown;
  for (const url of candidates) {
    try {
      const response = await fetch(url, { cache: "no-store", headers: { Accept: "application/json" } });
      if (!response.ok) throw new Error(`Release manifest HTTP ${response.status}`);
      return decodeReleaseManifest(await response.json());
    } catch (error) {
      lastError = error;
    }
  }
  throw lastError instanceof Error ? lastError : new Error("Release manifest unavailable");
}
