#!/usr/bin/env node

import { execFileSync } from "node:child_process";

const version = process.argv[2];
const expectedSha = process.env.EXPECTED_SHA || "";
if (!/^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/.test(version || "")) {
  throw new Error("usage: finalize-npm-official-release.mjs MAJOR.MINOR.PATCH");
}
if (expectedSha && !/^[0-9a-f]{40}$/.test(expectedSha)) {
  throw new Error("EXPECTED_SHA must be a full lowercase commit SHA");
}

const packages = [
  "reasonix",
  "@reasonix/cli-darwin-arm64", "@reasonix/cli-darwin-x64",
  "@reasonix/cli-linux-arm64", "@reasonix/cli-linux-x64",
  "@reasonix/cli-win32-arm64", "@reasonix/cli-win32-x64",
];

function metadata(name) {
  const output = execFileSync(
    "npm",
    ["view", `${name}@${version}`, "name", "version", "gitHead", "reasonixCandidateSha", "--json"],
    { encoding: "utf8" },
  );
  return JSON.parse(output);
}

for (const name of packages) {
  const actual = metadata(name);
  if (actual.name !== name || actual.version !== version) {
    throw new Error(`${name}@${version} is unavailable or resolved to another package`);
  }
  if (expectedSha && actual.gitHead !== expectedSha) {
    throw new Error(`${name}@${version} gitHead ${actual.gitHead || "<missing>"} does not match ${expectedSha}`);
  }
  if (expectedSha && actual.reasonixCandidateSha !== expectedSha) {
    throw new Error(`${name}@${version} reasonixCandidateSha ${actual.reasonixCandidateSha || "<missing>"} does not match ${expectedSha}`);
  }
}

for (const name of packages) {
  for (const tag of ["latest", "canary", "next"]) {
    execFileSync("npm", ["dist-tag", "add", `${name}@${version}`, tag], { stdio: "inherit" });
  }
  const tags = JSON.parse(execFileSync("npm", ["view", name, "dist-tags", "--json"], { encoding: "utf8" }));
  for (const tag of ["latest", "canary", "next"]) {
    if (tags[tag] !== version) throw new Error(`${name} ${tag} is ${tags[tag] || "<missing>"}; want ${version}`);
  }
  if (tags["official-staging"] === version) {
    execFileSync("npm", ["dist-tag", "rm", name, "official-staging"], { stdio: "inherit" });
  }
}

