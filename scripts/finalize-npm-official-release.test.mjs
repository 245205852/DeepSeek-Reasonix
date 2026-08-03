import assert from "node:assert/strict";
import { chmodSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const candidate = "c46e3af1c2732fe2b3dedb0bd47eb39a629357d2";

function run(expectedSha = candidate, publishedSha = candidate, staleReads = 0) {
  const directory = mkdtempSync(join(tmpdir(), "reasonix-npm-alias-test-"));
  const npm = join(directory, "npm");
  const state = join(directory, "state");
  writeFileSync(npm, `#!/usr/bin/env node
const fs = require("node:fs");
const args = process.argv.slice(2);
if (args[0] === "view" && args[1].includes("@1.19.2") && args.at(-1) === "--json") {
  const name = args[1].slice(0, -"@1.19.2".length);
  console.log(JSON.stringify({ name, version: "1.19.2", gitHead: ${JSON.stringify(publishedSha)}, reasonixCandidateSha: ${JSON.stringify(publishedSha)} }));
} else if (args[0] === "view" && args[2] === "dist-tags") {
  const statePath = process.env.NPM_FAKE_STATE;
  const reads = fs.existsSync(statePath) ? Number(fs.readFileSync(statePath, "utf8")) : 0;
  fs.writeFileSync(statePath, String(reads + 1));
  const stale = reads < Number(process.env.NPM_FAKE_STALE_READS || 0);
  console.log(JSON.stringify(stale
    ? { latest: "1.19.2", canary: "1.19.2-canary.1", next: "1.19.0-rc.3" }
    : { latest: "1.19.2", canary: "1.19.2", next: "1.19.2" }));
} else if (args[0] === "dist-tag" && args[1] === "add") {
  process.exit(0);
} else {
  console.error("unexpected npm arguments", JSON.stringify(args));
  process.exit(2);
}
`);
  chmodSync(npm, 0o755);
  return spawnSync(process.execPath, ["scripts/finalize-npm-official-release.mjs", "1.19.2"], {
    cwd: new URL("..", import.meta.url),
    encoding: "utf8",
    env: {
      ...process.env,
      EXPECTED_SHA: expectedSha,
      NPM_FAKE_STATE: state,
      NPM_FAKE_STALE_READS: String(staleReads),
      NPM_TAG_VERIFY_DELAY_MS: "1",
      PATH: `${directory}:${process.env.PATH}`,
    },
  });
}

test("aligns all aliases after exact package provenance validation", () => {
  const result = run();
  assert.equal(result.status, 0, result.stderr);
});

test("waits for npm alias propagation before continuing", () => {
  const result = run(candidate, candidate, 2);
  assert.equal(result.status, 0, result.stderr);
});

test("fails closed before alias mutation when provenance differs", () => {
  const result = run(candidate, "a".repeat(40));
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /does not match/);
});
