export const releaseEntryTypes = ["feature", "fix", "security", "ops", "improvement"] as const;

export type ReleaseEntryType = (typeof releaseEntryTypes)[number];

export type ReleaseEntry = {
  type: ReleaseEntryType;
  zh: string;
  en: string;
};

export type ReleaseRecord = {
  version: string;
  date: string;
  entries: ReleaseEntry[];
};

export type ReleaseManifest = {
  latest: string;
  repositoryURL: string;
  releases: ReleaseRecord[];
};

export function compareVersions(left: string, right: string): number {
  const leftParts = numericVersion(left);
  const rightParts = numericVersion(right);
  for (let index = 0; index < 3; index += 1) {
    const difference = leftParts[index] - rightParts[index];
    if (difference !== 0) return difference;
  }
  return 0;
}

export function decodeReleaseManifest(value: unknown): ReleaseManifest {
  if (!isRecord(value) || !isVersion(value.latest) || !isHTTPURL(value.repositoryURL) || !Array.isArray(value.releases)) {
    throw new Error("Invalid release manifest");
  }
  const releases = value.releases.slice(0, 50).map((release) => {
    if (!isRecord(release) || !isVersion(release.version) || !isDate(release.date) || !Array.isArray(release.entries)) {
      throw new Error("Invalid release record");
    }
    const entries = release.entries.slice(0, 50).map((entry) => {
      if (!isRecord(entry) || !releaseEntryTypes.includes(entry.type as ReleaseEntryType) || !isText(entry.zh) || !isText(entry.en)) {
        throw new Error("Invalid release entry");
      }
      return { type: entry.type as ReleaseEntryType, zh: entry.zh, en: entry.en };
    });
    return { version: release.version, date: release.date, entries };
  });
  if (!releases.some((release) => release.version === value.latest)) {
    throw new Error("Latest release is missing from history");
  }
  return { latest: value.latest, repositoryURL: value.repositoryURL, releases };
}

function numericVersion(value: string): [number, number, number] {
  const match = /^v?(\d+)\.(\d+)\.(\d+)/i.exec(value.trim());
  if (!match) return [0, 0, 0];
  return [Number(match[1]), Number(match[2]), Number(match[3])];
}

function isVersion(value: unknown): value is string {
  return typeof value === "string" && /^v?\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$/.test(value);
}

function isDate(value: unknown): value is string {
  return typeof value === "string" && /^\d{4}-\d{2}-\d{2}$/.test(value);
}

function isText(value: unknown): value is string {
  return typeof value === "string" && value.trim().length > 0 && value.length <= 1000;
}

function isHTTPURL(value: unknown): value is string {
  if (typeof value !== "string") return false;
  try {
    const parsed = new URL(value);
    return parsed.protocol === "https:" || parsed.protocol === "http:";
  } catch {
    return false;
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
