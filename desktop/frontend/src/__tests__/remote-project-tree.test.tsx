// Run: tsx src/__tests__/remote-project-tree.test.tsx
// Source-contract test: the remote project group's tree behavior — session
// rows, the + action, the remote context menu, the connection badge, and
// the local-action guards — is wired exactly once and in the remote shape.

import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

let passed = 0;
let failed = 0;
function ok(value: boolean, label: string) {
  if (value) {
    process.stdout.write(`  PASS  ${label}\n`);
    passed += 1;
  } else {
    process.stdout.write(`  FAIL  ${label}\n`);
    failed += 1;
  }
}

console.log("\nRemote project tree wiring");
const here = dirname(fileURLToPath(import.meta.url));
const source = readFileSync(resolve(here, "../components/ProjectTree.tsx"), "utf8");
const remoteSource = readFileSync(resolve(here, "../components/ProjectTreeRemoteGroups.tsx"), "utf8");

ok(
  /<div[\s\S]*?key=\{`remote-session:/.test(remoteSource) && !/sessionName: row\.name/.test(remoteSource),
  "session rows remain non-interactive until the remote-tab surface lands",
);
ok(
  /rows\.map\(\(row\) =>/.test(remoteSource),
  "remote group children render from the fetched session list",
);
ok(
  /app\.RemoteProjectSessions\(hostId, workspace\)/.test(remoteSource),
  "sessions are fetched through the bridge",
);
ok(
  /state === "connected" \|\| state === "degraded"/.test(remoteSource),
  "session fetch waits for a connected host",
);
ok(
  /key: "remote-open-window"[\s\S]*?key: "remote-stop-server"[\s\S]*?key: "remote-unpin"/.test(remoteSource) && !/key: "remote-new-session"/.test(remoteSource),
  "the partial-stage menu exposes only actions that have a visible surface",
);
ok(
  /items=\{node\.remote \? remoteProjectMenuItems :/.test(source),
  "remote groups swap out the local project menu",
);
ok(
  /app\.ConnectRemoteHost\(ref\.hostId\)[\s\S]*?waitForRemoteConnection\(ref\.hostId\)[\s\S]*?app\.OpenRemoteWorkspace\(ref\.hostId, ref\.workspace\)/.test(remoteSource) && /openRemoteWindow\(node\.remote\)/.test(source),
  "remote project reconnects before opening the supported remote window surface",
);
ok(
  /app\.RemoveRemoteProject\(ref\.hostId, ref\.workspace\)/.test(remoteSource) && /void refresh\(\);/.test(remoteSource),
  "unpin removes the registration and refreshes the tree",
);
ok(
  /project-tree__remote-badge--\$\{remoteStatuses\[node\.remote\.hostId\]\?\.state/.test(source),
  "the group row badge reflects the live host status",
);
ok(
  /sessionLoads\.current\.has\(key\)/.test(remoteSource) && /eligibleSessionKeys\.current\.has\(key\)/.test(remoteSource) && /filter\(\(\[key\]\) => eligible\.has\(key\)\)/.test(remoteSource),
  "session fetches deduplicate in flight and discard disconnected or stale group results",
);

process.stdout.write(`\n${passed} passed, ${failed} failed\n`);
if (failed > 0) process.exit(1);
