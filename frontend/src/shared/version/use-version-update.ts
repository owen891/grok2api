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
      // Manual checks must bypass the manifest bundled into an older image.
      setManifest(await loadManifest(true));
    } catch {
      setCheckFailed(true);
    } finally {
      setChecking(false);
    }
  }, []);

  useEffect(() => {
    let active = true;
    void loadManifest()
      .then((bundled) => {
        if (active) {
          setManifest(bundled);
          setCheckFailed(false);
        }
        return loadManifest(true);
      })
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

async function loadManifest(preferRemote = false): Promise<ReleaseManifest> {
  // Startup should render immediately from the image; an explicit check must
  // query the repository first so an older image cannot hide a newer release.
  const remote = `${remoteManifestURL}?t=${Date.now()}`;
  const candidates = preferRemote ? [remote, "/release-manifest.json"] : ["/release-manifest.json", remote];
  let lastError: unknown;
  for (const url of candidates) {
    try {
      const response = await fetch(url, { cache: "no-store", headers: { Accept: "application/json" } });
      if (!response.ok) throw new Error(`Release manifest HTTP ${response.status}`);
      const manifest = decodeReleaseManifest(await response.json());
      if (compareVersions(manifest.latest, currentVersion) < 0) {
        throw new Error(`Release manifest ${manifest.latest} is older than ${currentVersion}`);
      }
      return manifest;
    } catch (error) {
      lastError = error;
    }
  }
  throw lastError instanceof Error ? lastError : new Error("Release manifest unavailable");
}
