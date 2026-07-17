import assert from "node:assert/strict";

import { compareVersions, decodeReleaseManifest } from "../src/shared/version/version-manifest.ts";

assert.equal(compareVersions("v3.0.2", "v3.0.1"), 1);
assert.equal(compareVersions("3.0.1", "v3.0.1"), 0);
assert.equal(compareVersions("v2.9.9", "v3.0.0"), -1);

const manifest = decodeReleaseManifest({
  latest: "v3.0.2",
  repositoryURL: "https://github.com/owen891/grok2api",
  releases: [{
    version: "v3.0.2",
    date: "2026-07-17",
    entries: [{ type: "feature", zh: "版本提醒", en: "Version reminder" }],
  }],
});
assert.equal(manifest.releases[0].entries[0].type, "feature");
assert.throws(() => decodeReleaseManifest({ latest: "v3.0.2", repositoryURL: "javascript:alert(1)", releases: [] }));

console.log("version update tests passed");
