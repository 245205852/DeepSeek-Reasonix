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
const appSource = readFileSync(resolve(here, "../App.tsx"), "utf8");
const modeActionsSource = readFileSync(resolve(here, "../lib/useComposerModeActions.ts"), "utf8");

ok(
  /remoteSession: \{ hostId: node\.remote!\.hostId, workspace: node\.remote!\.workspace, name: row\.name \}/.test(remoteSource) &&
    /openRemoteSessionNode\(remote, openRemoteProject\)/.test(source) &&
    /void open\(remote, remote\.name \? \{ sessionName: remote\.name \} : \{ focus: true \}\)/.test(remoteSource),
  "session rows open the matching in-app remote session",
);
ok(
  /rows\.map\(\(row\): ProjectNode =>/.test(remoteSource) && /mergeRemoteSessionsIntoTree\(tree, remoteSessions, t\)/.test(source),
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
  /key: "remote-new-session"[\s\S]*?key: "remote-open-window"[\s\S]*?key: "remote-stop-server"[\s\S]*?key: "remote-unpin"/.test(remoteSource),
  "the remote menu exposes new-session, browser, stop, and unpin actions",
);
ok(
  /items=\{node\.remote \? remoteProjectMenuItems :/.test(source),
  "remote groups swap out the local project menu",
);
ok(
  /app\.OpenRemoteProjectTab\(ref\.hostId, ref\.workspace,[\s\S]*?newSession: true/.test(remoteSource) &&
    /app\.ConnectRemoteHost\(ref\.hostId\)[\s\S]*?waitForRemoteConnection\(ref\.hostId\)[\s\S]*?app\.OpenRemoteWorkspace\(ref\.hostId, ref\.workspace\)/.test(remoteSource),
  "in-app tabs use the remote-session bridge while the browser surface reconnects first",
);
ok(
  /app\.RemoveRemoteProject\(ref\.hostId, ref\.workspace\)/.test(remoteSource) && /void refresh\(\);/.test(remoteSource),
  "unpin removes the registration and refreshes the tree",
);
ok(
  /project-tree__remote-badge--\$\{remoteServeBadgeState\(remoteServers\[node\.remote\.hostId\]\?\.\[node\.remote\.workspace\]\)\}/.test(source),
  "the group row badge reflects the workspace-specific serve state",
);
ok(
  /sessionLoads\.current\.has\(key\)/.test(remoteSource) &&
    /eligibleSessionKeys\.current\.has\(key\)/.test(remoteSource) &&
    /filter\(\(\[key\]\) => connected\.has\(key\)\)/.test(remoteSource),
  "session fetches deduplicate in flight and discard disconnected or stale group results",
);
ok(
  /useComposerModeActions\(\{[\s\S]*?remote: remoteSurfaceActive/.test(appSource) &&
    /if \(remote && activeTabId\)[\s\S]*?SetRemoteTabPlanMode\(activeTabId,[\s\S]*?SetRemoteTabToolApprovalMode\(activeTabId,/.test(modeActionsSource),
  "remote composer mode changes route through the remote plan and approval endpoints",
);
ok(
  /tab\.id === tabId && tab\.remote[\s\S]*?SetRemoteTabGoal\(tabId, trimmed\)/.test(appSource) &&
    /onSend=\{remoteSurfaceActive \? remoteComposerSend : handleSend\}/.test(appSource),
  "remote goal activation and goal-draft submission stay on the remote controller",
);

process.stdout.write(`\n${passed} passed, ${failed} failed\n`);
if (failed > 0) process.exit(1);
