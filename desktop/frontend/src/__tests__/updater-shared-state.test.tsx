// Run: tsx src/__tests__/updater-shared-state.test.tsx

import { JSDOM } from "jsdom";
import React from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { __emitMockUpdater, type AppBindings } from "../lib/bridge";
import { switchUpdaterChannel, UpdaterProvider, useUpdater } from "../lib/useUpdater";

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

function Consumer({ id, checking = false, channel = "" }: { id: string; checking?: boolean; channel?: string }) {
  const updater = useUpdater();
  return (
    <section>
      <output id={`${id}-status`}>{updater.status.kind}</output>
      <output id={`${id}-manual`}>
        {updater.status.kind === "error" && updater.status.manualHint ? "manual" : ""}
      </output>
      <output id={`${id}-received`}>
        {updater.status.kind === "downloading" ? updater.status.received : ""}
      </output>
      {checking && <button id={`${id}-check-update`} type="button" onClick={() => void updater.check(channel)}>Check</button>}
      <button id={`${id}-reset-stable`} type="button" onClick={() => updater.reset("stable")}>Reset stable</button>
      <button id={`${id}-reset-preview`} type="button" onClick={() => updater.reset("preview")}>Reset preview</button>
      {updater.status.kind === "available" && (
        <button id={`${id}-apply`} type="button" onClick={() => updater.apply(updater.status.info)}>Apply</button>
      )}
      {updater.status.kind === "error" && updater.status.info && (
        <button id={`${id}-retry`} type="button" onClick={() => updater.apply(updater.status.info!)}>Retry</button>
      )}
    </section>
  );
}

const dom = new JSDOM("<!doctype html><html><body><div id=\"root\"></div></body></html>", {
  pretendToBeVisual: true,
  url: "http://localhost/",
});
(globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
globalThis.window = dom.window as unknown as Window & typeof globalThis;
globalThis.document = dom.window.document;
globalThis.Node = dom.window.Node;
globalThis.Element = dom.window.Element;
globalThis.HTMLElement = dom.window.HTMLElement;
globalThis.Event = dom.window.Event;
globalThis.MouseEvent = dom.window.MouseEvent;

const root = createRoot(document.getElementById("root")!);
await act(async () => {
  root.render(
    <UpdaterProvider>
      <Consumer id="banner" checking />
      <Consumer id="settings" checking channel="preview" />
    </UpdaterProvider>,
  );
});

ok(document.getElementById("banner-status")?.textContent === "idle", "banner starts idle");
ok(document.getElementById("settings-status")?.textContent === "idle", "settings starts idle");

await act(async () => {
  (document.getElementById("banner-check-update") as HTMLButtonElement).click();
  await new Promise((resolve) => setTimeout(resolve, 0));
});

ok(document.getElementById("banner-status")?.textContent === "upToDate", "banner receives the check result");
ok(document.getElementById("settings-status")?.textContent === "upToDate", "settings receives the same check result");

const debInfo = {
  available: true,
  current: "v1.0.0",
  latest: "v1.1.0",
  notes: "",
  channel: "stable",
  canSelfUpdate: true,
  manualOnly: false,
  installMode: "deb",
  requiresElevation: true,
  downloaded: false,
  downloadUrl: "https://example.invalid/download",
  assetSize: 42,
};
const applyAttempts: Array<{
  requestId: string;
  version: string;
  channel: string;
  resolve: () => void;
  reject: (err: Error) => void;
}> = [];
const checkedChannels: string[] = [];
window.go = {
  main: {
    App: {
      async CheckUpdate(channel: string) {
        checkedChannels.push(channel);
        return { ...debInfo, channel: channel === "preview" ? "preview" : "stable" };
      },
      ApplyUpdateRequest(channel: string, expectedVersion: string, requestId: string) {
        return new Promise<void>((resolve, reject) => applyAttempts.push({
          requestId,
          version: expectedVersion,
          channel,
          resolve,
          reject,
        }));
      },
    } as AppBindings,
  },
};

await act(async () => {
  (document.getElementById("settings-check-update") as HTMLButtonElement).click();
  await new Promise((resolve) => setTimeout(resolve, 0));
});
ok(checkedChannels[0] === "preview", "settings check forwards the selected Preview channel");
ok(document.getElementById("banner-status")?.textContent === "available", "deb update becomes available");
ok(document.getElementById("settings-status")?.textContent === "available", "deb availability is shared");

await act(async () => {
  (document.getElementById("banner-apply") as HTMLButtonElement).click();
});
ok(document.getElementById("banner-status")?.textContent === "authorizing", "deb apply starts authorizing");
ok(document.getElementById("settings-status")?.textContent === "authorizing", "authorizing state is shared");

await act(async () => {
  __emitMockUpdater({
    requestId: applyAttempts[0].requestId,
    version: applyAttempts[0].version,
    channel: "preview",
    phase: "downloading",
    received: 10,
    total: 42,
  });
});
ok(document.getElementById("banner-status")?.textContent === "downloading", "download phase is shared");
ok(document.getElementById("banner-received")?.textContent === "10", "download progress is shared");

await act(async () => {
  __emitMockUpdater({
    requestId: applyAttempts[0].requestId,
    version: applyAttempts[0].version,
    channel: "preview",
    phase: "verifying",
    received: 42,
    total: 42,
  });
});
ok(document.getElementById("banner-status")?.textContent === "verifying", "verify phase is shared");

await act(async () => {
  __emitMockUpdater({
    requestId: applyAttempts[0].requestId,
    version: applyAttempts[0].version,
    channel: "preview",
    phase: "installing",
    received: 42,
    total: 42,
  });
});
ok(document.getElementById("banner-status")?.textContent === "installing", "install phase advances");

await act(async () => {
  applyAttempts[0]?.reject(new Error("update: manual update required: system update helper is unavailable"));
  await new Promise((resolve) => setTimeout(resolve, 0));
});
ok(document.getElementById("banner-status")?.textContent === "error", "manual reclassification leaves busy state");
ok(document.getElementById("banner-manual")?.textContent === "manual", "manual reclassification offers download fallback");
ok(document.getElementById("settings-status")?.textContent === "error", "manual fallback error is shared");
ok(!!document.getElementById("banner-retry"), "error state exposes retry");

// Retry releases the mutex and can start a new apply immediately.
await act(async () => {
  (document.getElementById("banner-retry") as HTMLButtonElement).click();
});
ok(document.getElementById("banner-status")?.textContent === "authorizing", "retry re-enters applying");
await act(async () => {
  applyAttempts[1]?.resolve();
  __emitMockUpdater({
    requestId: applyAttempts[1].requestId,
    version: applyAttempts[1].version,
    channel: "preview",
    phase: "relaunching",
    received: 0,
    total: 0,
  });
  await new Promise((resolve) => setTimeout(resolve, 0));
});
ok(document.getElementById("banner-status")?.textContent === "relaunching", "relaunching phase is shared");

let resolveSavedCheck!: (value: typeof debInfo) => void;
let resolvePreviewCheck!: (value: typeof debInfo) => void;
window.go.main.App.CheckUpdate = (selected: string) =>
  new Promise<typeof debInfo>((resolve) => {
    if (selected === "preview") resolvePreviewCheck = resolve;
    else resolveSavedCheck = resolve;
  });

await act(async () => {
  (document.getElementById("banner-check-update") as HTMLButtonElement).click();
  (document.getElementById("settings-check-update") as HTMLButtonElement).click();
});
await act(async () => {
  resolvePreviewCheck({ ...debInfo, channel: "preview", latest: "v1.2.0" });
  await new Promise((resolve) => setTimeout(resolve, 0));
});
ok(document.getElementById("banner-status")?.textContent === "available", "newest channel check publishes its result");
await act(async () => {
  resolveSavedCheck({ ...debInfo, available: false, channel: "stable", latest: "v1.0.0" });
  await new Promise((resolve) => setTimeout(resolve, 0));
});
ok(document.getElementById("banner-status")?.textContent === "available", "stale channel check cannot overwrite the newest result");

let oldApplyRequestId = "";
let oldApplyVersion = "";
let resolveOldApply!: () => void;
window.go.main.App.ApplyUpdateRequest = (_channel: string, expectedVersion: string, requestId: string) =>
  new Promise<void>((resolve) => {
    oldApplyRequestId = requestId;
    oldApplyVersion = expectedVersion;
    resolveOldApply = resolve;
  });
await act(async () => {
  (document.getElementById("banner-apply") as HTMLButtonElement).click();
});
ok(document.getElementById("banner-status")?.textContent === "authorizing", "Preview apply starts");
await act(async () => {
  __emitMockUpdater({
    requestId: oldApplyRequestId,
    version: `${oldApplyVersion}-wrong`,
    channel: "preview",
    phase: "installing",
    received: 42,
    total: 42,
  });
});
ok(
  document.getElementById("banner-status")?.textContent === "authorizing",
  "same-request wrong-version progress cannot advance the active apply",
);
await act(async () => {
  __emitMockUpdater({
    requestId: oldApplyRequestId,
    version: oldApplyVersion,
    channel: "stable",
    phase: "downloading",
    received: 41,
    total: 42,
  });
});
ok(document.getElementById("banner-received")?.textContent === "", "wrong-channel progress cannot update the active Preview apply");
await act(async () => {
  (document.getElementById("banner-reset-preview") as HTMLButtonElement).click();
  __emitMockUpdater({
    requestId: oldApplyRequestId,
    version: oldApplyVersion,
    channel: "preview",
    phase: "error",
    received: 0,
    total: 0,
    err: "old apply failed",
  });
  resolveOldApply();
  await new Promise((resolve) => setTimeout(resolve, 0));
});
ok(document.getElementById("banner-status")?.textContent === "idle", "same-channel superseded progress and Promise completion stay ignored");

window.go.main.App.CheckUpdate = async (selected: string) => ({
  ...debInfo,
  channel: selected === "preview" ? "preview" : "stable",
  latest: selected === "preview" ? "v1.2.0" : "v1.1.0",
});
window.go.main.App.ApplyUpdateRequest = async () => {
  throw new Error("should not reach when version mismatches from progress only");
};

await act(async () => {
  (document.getElementById("settings-check-update") as HTMLButtonElement).click();
  await new Promise((resolve) => setTimeout(resolve, 0));
});
ok(document.getElementById("banner-status")?.textContent === "available", "Preview update is available again");

window.go.main.App.ApplyUpdateRequest = async (_channel: string, _expectedVersion: string, requestId: string) => {
  // Hang until cancelled by reset so we can prove supersession.
  return new Promise<void>((_resolve, reject) => {
    applyAttempts.push({
      requestId,
      version: _expectedVersion,
      channel: _channel,
      resolve: () => {},
      reject,
    });
  });
};
await act(async () => {
  (document.getElementById("banner-apply") as HTMLButtonElement).click();
});
const staleApply = applyAttempts[applyAttempts.length - 1];
ok(document.getElementById("banner-status")?.textContent === "authorizing", "Preview apply starts");
await act(async () => {
  (document.getElementById("banner-reset-stable") as HTMLButtonElement).click();
  __emitMockUpdater({
    requestId: staleApply.requestId,
    version: staleApply.version,
    channel: "preview",
    phase: "error",
    received: 0,
    total: 0,
    err: "old install failed",
  });
  staleApply.reject(new Error("old Preview apply failed"));
  await new Promise((resolve) => setTimeout(resolve, 0));
});
ok(document.getElementById("banner-status")?.textContent === "idle", "superseded apply progress and rejection stay ignored");

window.go.main.App.CheckUpdate = async () => ({ ...debInfo, channel: "stable" });
await act(async () => {
  (document.getElementById("settings-check-update") as HTMLButtonElement).click();
  await new Promise((resolve) => setTimeout(resolve, 0));
});
ok(document.getElementById("banner-status")?.textContent === "error", "wrong-channel check response leaves the checking state");

window.go.main.App.CheckUpdate = async () => ({ ...debInfo, channel: "preview", latest: "v1.3.0" });
await act(async () => {
  (document.getElementById("settings-check-update") as HTMLButtonElement).click();
  await new Promise((resolve) => setTimeout(resolve, 0));
});
ok(document.getElementById("banner-status")?.textContent === "available", "check can retry after a wrong-channel response");

// switchUpdaterChannel helper still works.
let saved: string | null = null;
await act(async () => {
  await switchUpdaterChannel(
    "stable",
    () => {},
    async (ch) => {
      saved = ch;
      return true;
    },
    async () => {},
  );
});
ok(saved === "stable", "switchUpdaterChannel saves the selected channel");

delete window.go;

process.stdout.write(`\n${passed} passed, ${failed} failed\n`);
if (failed > 0) process.exit(1);
