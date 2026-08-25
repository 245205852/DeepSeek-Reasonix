import type { RemoteTabState, TabMeta } from "./types";

type MockRemoteTabChannel = "event" | "state";
const mockListeners = new Map<string, Set<(payload: unknown) => void>>();
const openedListeners = new Set<(meta: TabMeta) => void>();

function runtimeAvailable(): boolean {
  return typeof window !== "undefined" && Boolean(window.runtime);
}

function registerMock(tabId: string, channel: MockRemoteTabChannel, cb: (payload: unknown) => void): () => void {
  const key = `${tabId}:${channel}`;
  const listeners = mockListeners.get(key) ?? new Set<(payload: unknown) => void>();
  listeners.add(cb);
  mockListeners.set(key, listeners);
  return () => {
    listeners.delete(cb);
    if (listeners.size === 0) mockListeners.delete(key);
  };
}

export function onRemoteTabEvent(tabId: string, cb: (frame: unknown) => void): () => void {
  if (runtimeAvailable()) return window.runtime!.EventsOn(`remote-tab:${tabId}:event`, cb);
  return registerMock(tabId, "event", cb);
}

export function onRemoteTabState(tabId: string, cb: (state: RemoteTabState) => void): () => void {
  if (runtimeAvailable()) {
    return window.runtime!.EventsOn(`remote-tab:${tabId}:state`, (payload?: unknown) => cb((payload ?? {}) as RemoteTabState));
  }
  return registerMock(tabId, "state", cb as (payload: unknown) => void);
}

export function onRemoteTabOpened(cb: (meta: TabMeta) => void): () => void {
  if (runtimeAvailable()) {
    return window.runtime!.EventsOn("remote-tab:opened", (payload?: unknown) => cb((payload ?? {}) as TabMeta));
  }
  openedListeners.add(cb);
  return () => openedListeners.delete(cb);
}

export function __emitMockRemoteTab(tabId: string, channel: MockRemoteTabChannel, payload: unknown): void {
  for (const cb of mockListeners.get(`${tabId}:${channel}`) ?? []) cb(payload);
}

export function __emitMockRemoteTabOpened(meta: TabMeta): void {
  for (const cb of openedListeners) cb(meta);
}
