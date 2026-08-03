/**
 * Workbench Target projection helpers for Local vs Remote adapters.
 * Desktop always starts Local; Remote is opt-in via reconnect / Connect.
 *
 * After an unexpected SSH drop, kind stays "ssh" with state reconnecting or
 * reconnect_failed: the last Remote snapshot stays visible and mutations are
 * disabled until recovery or an explicit switch to Local.
 */

import { app } from "./bridge";

export type WorkbenchTargetKind = "local" | "ssh";

export type WorkbenchTargetState =
  | "connecting"
  | "connected"
  | "reconnecting"
  | "reconnect_failed"
  | "disconnected"
  | "background_activity"
  | string;

export type WorkbenchActiveTarget = {
  state?: WorkbenchTargetState;
  kind: WorkbenchTargetKind;
  hostId?: string;
  workspace?: string;
  identityGen?: number;
  requestSeq?: number;
  attempt?: number;
  retryable?: boolean;
  error?: string;
  reconnect?: WorkbenchRemoteHint;
};

export type WorkbenchRemoteHint = {
  hostId?: string;
  workspace?: string;
  label?: string;
};

export type ProviderTrustPrompt = {
  hostId: string;
  host: string;
  keyType: string;
  fingerprint: string;
  workspace: string;
  providerRefs: string[];
  warning: string;
};

/** True while the backend is preparing a candidate target, auto-recovering, or
 * stuck in reconnect_failed. Mutations stay disabled; the last Remote
 * transcript remains visible. */
export function workbenchTargetTransitioning(target: WorkbenchActiveTarget): boolean {
  return (
    target.state === "connecting" ||
    target.state === "reconnecting" ||
    target.state === "reconnect_failed"
  );
}

/** True when the status bar should offer a manual reconnect retry. */
export function workbenchRemoteRetryable(target: WorkbenchActiveTarget): boolean {
  return target.kind === "ssh" && target.state === "reconnect_failed" && target.retryable === true;
}

/** Resolve the workspace used by one-click Remote connections. */
export function resolveRemoteWorkspace(lastWorkspace?: string, defaultWorkspace?: string): string {
  return lastWorkspace?.trim() || defaultWorkspace?.trim() || "";
}

export async function preferredRemoteWorkspace(hostId: string, defaultWorkspace?: string): Promise<string> {
  try {
    const lastWorkspace = await app.RemoteLastWorkspace(hostId);
    return resolveRemoteWorkspace(lastWorkspace, defaultWorkspace);
  } catch {
    return resolveRemoteWorkspace(undefined, defaultWorkspace);
  }
}

export async function fetchActiveTarget(): Promise<WorkbenchActiveTarget> {
  return app.WorkbenchActiveTarget();
}

export async function fetchLastRemoteHint(): Promise<WorkbenchRemoteHint | null> {
  const hint = await app.WorkbenchLastRemoteHint();
  if (!hint?.hostId) return null;
  return hint;
}

export async function switchToLocal(): Promise<WorkbenchActiveTarget> {
  return app.WorkbenchSwitchLocal();
}

export async function connectRemote(hostId: string, workspace: string): Promise<void> {
  await app.WorkbenchConnectRemote(hostId, workspace);
}

export async function disconnectRemote(): Promise<void> {
  await app.WorkbenchDisconnectRemote();
}

export async function remoteRequest(method: string, params: unknown = {}): Promise<unknown> {
  const raw = await app.WorkbenchRemoteRequest(method, JSON.stringify(params ?? {}));
  try {
    return JSON.parse(raw);
  } catch {
    return raw;
  }
}

export async function resolveProviderTrust(accept: boolean): Promise<void> {
  await app.WorkbenchResolveProviderTrust(accept);
}

export async function pendingProviderTrust(): Promise<ProviderTrustPrompt | null> {
  const p = await app.WorkbenchPendingProviderTrust();
  return p?.hostId ? p : null;
}
