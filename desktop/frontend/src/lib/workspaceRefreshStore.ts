import { useEffect, useSyncExternalStore } from "react";
import { app, onEvent } from "./bridge";
import type { WireWorkspaceChanged, WorkspaceRevisions, WorkspaceWatchState } from "./types";

export interface WorkspaceRefreshSnapshot {
  revisions: WorkspaceRevisions;
  changes: WireWorkspaceChanged["changes"];
  allPaths: boolean;
  source: WireWorkspaceChanged["source"];
  watchState: WorkspaceWatchState;
  sequence: number;
}

const zeroRevisions = (): WorkspaceRevisions => ({ content: 0, tree: 0, workingTree: 0, gitMeta: 0, session: 0 });
const EMPTY_SNAPSHOT: WorkspaceRefreshSnapshot = {
  revisions: zeroRevisions(), changes: [], allPaths: false, source: "reconcile", watchState: "unavailable", sequence: 0,
};
const emptySnapshot = (): WorkspaceRefreshSnapshot => EMPTY_SNAPSHOT;

const snapshots = new Map<string, WorkspaceRefreshSnapshot>();
const listeners = new Map<string, Set<() => void>>();
const baseByTab = new Map<string, WorkspaceRefreshSnapshot>();

function key(tabId: string, scopeKey: string): string {
  return `${tabId}\u0000${scopeKey}`;
}

function notify(tabId: string, scopeKey?: string): void {
  const keys = scopeKey ? [key(tabId, scopeKey)] : Array.from(listeners.keys()).filter((candidate) => candidate.startsWith(`${tabId}\u0000`));
  for (const candidate of keys) listeners.get(candidate)?.forEach((listener) => listener());
}

function replace(tabId: string, scopeKey: string, next: WorkspaceRefreshSnapshot): void {
  const k = key(tabId, scopeKey);
  snapshots.set(k, next);
  baseByTab.set(tabId, next);
  notify(tabId, scopeKey);
}

function revisionsOlder(current: WorkspaceRevisions, previous: WorkspaceRevisions): boolean {
  return current.content < previous.content || current.tree < previous.tree || current.workingTree < previous.workingTree || current.gitMeta < previous.gitMeta || current.session < previous.session;
}

function acceptEvent(tabId: string, event: WireWorkspaceChanged): void {
  const previous = baseByTab.get(tabId) ?? emptySnapshot();
  const current = event.revisions;
  if (revisionsOlder(current, previous.revisions)) return;
  const next: WorkspaceRefreshSnapshot = { ...event, sequence: previous.sequence + 1, changes: Array.isArray(event.changes) ? event.changes : [] };
  const scopes = Array.from(listeners.keys()).filter((candidate) => candidate.startsWith(`${tabId}\u0000`));
  baseByTab.set(tabId, next);
  if (scopes.length === 0) {
    notify(tabId);
    return;
  }
  for (const scope of scopes) {
    snapshots.set(scope, next);
  }
  notify(tabId);
}

let stopEvents: (() => void) | null = null;
function ensureEvents(): void {
  if (stopEvents) return;
  stopEvents = onEvent((event) => {
    if (event.kind === "workspace_changed" && event.tabId && event.workspace) {
      acceptEvent(event.tabId, event.workspace);
    }
  });
}

// Kept as an explicit lifecycle hook for tests and hot-reload hosts. Production
// keeps the subscription for the lifetime of the webview.
export function disposeWorkspaceRefreshStore(): void {
  stopEvents?.();
  stopEvents = null;
}

export function useWorkspaceRefresh(tabId: string, scopeKey: string, enabled: boolean): WorkspaceRefreshSnapshot {
  const snapshotKey = key(tabId, scopeKey);
  const subscribe = (listener: () => void) => {
    let set = listeners.get(snapshotKey);
    if (!set) {
      set = new Set();
      listeners.set(snapshotKey, set);
    }
    set.add(listener);
    return () => {
      set?.delete(listener);
      if (set?.size === 0) listeners.delete(snapshotKey);
    };
  };
  const getSnapshot = () => snapshots.get(snapshotKey) ?? baseByTab.get(tabId) ?? emptySnapshot();
  const snapshot = useSyncExternalStore(subscribe, getSnapshot, getSnapshot);

  useEffect(() => {
    if (!enabled) return;
    ensureEvents();
    let live = true;
    app.WorkspaceRevisionForTab(tabId).then((result) => {
      if (!live) return;
      const previous = getSnapshot();
      const revisions = result?.revisions ?? zeroRevisions();
      if (revisionsOlder(revisions, previous.revisions)) return;
      replace(tabId, scopeKey, {
        revisions,
        changes: [],
        allPaths: true,
        source: "reconcile",
        watchState: result?.watchState ?? "unavailable",
        sequence: previous.sequence + 1,
      });
    }).catch(() => undefined);
    return () => { live = false; };
  }, [enabled, scopeKey, tabId]);

  return snapshot;
}

export function markWorkspaceRefresh(tabId: string, scopeKey: string): void {
  const previous = snapshots.get(key(tabId, scopeKey)) ?? emptySnapshot();
  replace(tabId, scopeKey, { ...previous, allPaths: true, source: "reconcile", sequence: previous.sequence + 1 });
}

export async function reconcileWorkspaceRefresh(tabId: string, scopeKey: string): Promise<void> {
  try {
    const result = await app.WorkspaceRevisionForTab(tabId);
    const previous = snapshots.get(key(tabId, scopeKey)) ?? baseByTab.get(tabId) ?? EMPTY_SNAPSHOT;
    const revisions = result?.revisions ?? zeroRevisions();
    const changed = revisionsOlder(revisions, previous.revisions) || revisionsOlder(previous.revisions, revisions);
    if (!changed && result?.watchState === previous.watchState) return;
    replace(tabId, scopeKey, {
      revisions,
      changes: [],
      allPaths: true,
      source: "reconcile",
      watchState: result?.watchState ?? "unavailable",
      sequence: previous.sequence + 1,
    });
  } catch {
    // A transient runtime rebuild must not erase the last good snapshot.
  }
}
