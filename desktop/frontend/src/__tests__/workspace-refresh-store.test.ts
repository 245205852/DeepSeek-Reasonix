import assert from "node:assert/strict";
import {
  acceptWorkspaceRefreshForTests,
  activateWorkspaceRefreshScopeForTests,
  resetWorkspaceRefreshStoreForTests,
  workspaceRefreshSnapshotForTests,
} from "../lib/workspaceRefreshStore";
import { createWorkspaceRefreshScheduler, type WorkspaceRefreshTimer } from "../lib/workspaceRefreshScheduler";
import type { WireWorkspaceChanged, WorkspaceRevisions } from "../lib/types";

const revisions = (content: number): WorkspaceRevisions => ({ content, tree: content, workingTree: content, gitMeta: 0, session: content });
const event = (content: number): WireWorkspaceChanged => ({
  revisions: revisions(content), changes: [], allPaths: true, source: "filesystem", watchState: "active",
});

function workspaceScopesKeepIndependentRevisionBaselines() {
  resetWorkspaceRefreshStoreForTests();
  activateWorkspaceRefreshScopeForTests("tab", "root-a");
  acceptWorkspaceRefreshForTests("tab", event(100));
  assert.equal(workspaceRefreshSnapshotForTests("tab", "root-a").revisions.content, 100);

  activateWorkspaceRefreshScopeForTests("tab", "root-b");
  acceptWorkspaceRefreshForTests("tab", event(1));
  assert.equal(workspaceRefreshSnapshotForTests("tab", "root-b").revisions.content, 1,
    "a new root must not inherit the old root's monotonic baseline");
  assert.equal(workspaceRefreshSnapshotForTests("tab", "root-a").revisions.content, 100);
}

class FakeTimer implements WorkspaceRefreshTimer {
  callbacks: Array<() => void> = [];
  schedule(callback: () => void): unknown { this.callbacks.push(callback); return callback; }
  cancel(handle: unknown): void { this.callbacks = this.callbacks.filter((callback) => callback !== handle); }
  fire(): void { const callback = this.callbacks.shift(); assert.ok(callback); callback(); }
}

async function refreshSchedulerUsesTrailingQuietWindowAndBoundsConcurrency() {
  const timer = new FakeTimer();
  const scheduler = createWorkspaceRefreshScheduler(300, timer);
  const runs: string[] = [];
  let releaseFirst!: () => void;
  const first = new Promise<void>((resolve) => { releaseFirst = resolve; });
  let confirmTrailing!: () => void;
  const trailingStarted = new Promise<void>((resolve) => { confirmTrailing = resolve; });

  scheduler.trigger(() => { runs.push("superseded"); });
  scheduler.trigger(async () => { runs.push("first"); await first; });
  assert.equal(timer.callbacks.length, 1, "retriggering must reset the quiet window");
  timer.fire();
  await Promise.resolve();
  assert.deepEqual(runs, ["first"]);

  scheduler.trigger(() => { runs.push("trailing"); confirmTrailing(); });
  timer.fire();
  await Promise.resolve();
  assert.deepEqual(runs, ["first"], "a refresh must not overlap the in-flight load");
  releaseFirst();
  await trailingStarted;
  assert.deepEqual(runs, ["first", "trailing"]);
}

await workspaceScopesKeepIndependentRevisionBaselines();
await refreshSchedulerUsesTrailingQuietWindowAndBoundsConcurrency();
console.log("ok  workspace refresh scope isolation and scheduling");
