import { useCallback, useEffect, useSyncExternalStore } from "react";
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
const activeScopeByTab = new Map<string, string>();

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
  notify(tabId, scopeKey);
}

function revisionsOlder(current: WorkspaceRevisions, previous: WorkspaceRevisions): boolean {
  return current.content < previous.content || current.tree < previous.tree || current.workingTree < previous.workingTree || current.gitMeta < previous.gitMeta || current.session < previous.session;
}

function acceptEvent(tabId: string, event: WireWorkspaceChanged): void {
  const scopeKey = activeScopeByTab.get(tabId);
  if (!scopeKey) return;
  const snapshotKey = key(tabId, scopeKey);
  if (!listeners.has(snapshotKey)) return;
  const previous = snapshots.get(snapshotKey) ?? emptySnapshot();
  const current = event.revisions;
  if (revisionsOlder(current, previous.revisions)) return;
  const next: WorkspaceRefreshSnapshot = { ...event, sequence: previous.sequence + 1, changes: Array.isArray(event.changes) ? event.changes : [] };
  snapshots.set(snapshotKey, next);
  notify(tabId, scopeKey);
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

async function workspaceRevisionForTab(tabId: string) {
  const binding = app.WorkspaceRevisionForTab;
  if (typeof binding !== "function") return undefined;
  return binding(tabId);
}

// Kept as an explicit lifecycle hook for tests and hot-reload hosts. Production
// keeps the subscription for the lifetime of the webview.
export function disposeWorkspaceRefreshStore(): void {
  stopEvents?.();
  stopEvents = null;
  snapshots.clear();
  listeners.clear();
  activeScopeByTab.clear();
}

export function useWorkspaceRefresh(tabId: string, scopeKey: string, enabled: boolean): WorkspaceRefreshSnapshot {
  const snapshotKey = key(tabId, scopeKey);
  const subscribe = useCallback((listener: () => void) => {
    let set = listeners.get(snapshotKey);
    if (!set) {
      set = new Set();
      listeners.set(snapshotKey, set);
    }
    set.add(listener);
    return () => {
      set?.delete(listener);
      if (set?.size === 0) {
        listeners.delete(snapshotKey);
        snapshots.delete(snapshotKey);
        if (activeScopeByTab.get(tabId) === scopeKey) activeScopeByTab.delete(tabId);
      }
    };
  }, [snapshotKey]);
  const getSnapshot = useCallback(() => snapshots.get(snapshotKey) ?? emptySnapshot(), [snapshotKey]);
  const snapshot = useSyncExternalStore(subscribe, getSnapshot, getSnapshot);

  useEffect(() => {
    if (!enabled) return;
    activeScopeByTab.set(tabId, scopeKey);
    ensureEvents();
    let live = true;
    workspaceRevisionForTab(tabId).then((result) => {
      if (!live || !result) return;
      const previous = getSnapshot();
      const revisions = result.revisions ?? zeroRevisions();
      if (revisionsOlder(revisions, previous.revisions)) return;
      replace(tabId, scopeKey, {
        revisions,
        changes: [],
        allPaths: true,
        source: "reconcile",
        watchState: result.watchState ?? "unavailable",
        sequence: previous.sequence + 1,
      });
    }).catch(() => undefined);
    return () => {
      live = false;
      if (activeScopeByTab.get(tabId) === scopeKey) activeScopeByTab.delete(tabId);
    };
  }, [enabled, scopeKey, tabId]);

  return snapshot;
}

export function markWorkspaceRefresh(tabId: string, scopeKey: string): void {
  const previous = snapshots.get(key(tabId, scopeKey)) ?? emptySnapshot();
  replace(tabId, scopeKey, { ...previous, allPaths: true, source: "reconcile", sequence: previous.sequence + 1 });
}

export async function reconcileWorkspaceRefresh(tabId: string, scopeKey: string): Promise<void> {
  try {
    activeScopeByTab.set(tabId, scopeKey);
    const result = await workspaceRevisionForTab(tabId);
    if (!result) return;
    if (activeScopeByTab.get(tabId) !== scopeKey) return;
    const snapshotKey = key(tabId, scopeKey);
    if (!listeners.has(snapshotKey)) return;
    const previous = snapshots.get(snapshotKey) ?? EMPTY_SNAPSHOT;
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

// Deterministic seams for the store's scope and monotonicity contracts.
export function resetWorkspaceRefreshStoreForTests(): void {
  disposeWorkspaceRefreshStore();
}

export function activateWorkspaceRefreshScopeForTests(tabId: string, scopeKey: string): void {
  activeScopeByTab.set(tabId, scopeKey);
  listeners.set(key(tabId, scopeKey), new Set());
}

export function acceptWorkspaceRefreshForTests(tabId: string, event: WireWorkspaceChanged): void {
  acceptEvent(tabId, event);
}

export function workspaceRefreshSnapshotForTests(tabId: string, scopeKey: string): WorkspaceRefreshSnapshot {
  return snapshots.get(key(tabId, scopeKey)) ?? emptySnapshot();
}
