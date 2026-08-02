#!/usr/bin/env node

import { readFileSync } from "node:fs";
import { releaseForVersion, validateCatalog } from "./release-notes.mjs";

const [catalogPath, version] = process.argv.slice(2);
if (!catalogPath || !/^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/.test(version || "")) {
  throw new Error("usage: validate-stable-release-record.mjs CATALOG MAJOR.MINOR.PATCH");
}

const catalog = validateCatalog(JSON.parse(readFileSync(catalogPath, "utf8")));
const release = releaseForVersion(catalog, version);
if (release.version !== version || release.channel !== "stable" || release.status !== "reviewed") {
  throw new Error(`v${version} must have one reviewed Stable release record`);
}

console.log(`reviewed Stable release record verified: v${version}`);
