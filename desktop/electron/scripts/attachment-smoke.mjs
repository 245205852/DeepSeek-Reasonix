import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, openSync, ftruncateSync, closeSync, statSync, writeFileSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { resolve, join } from "node:path";
import { fileURLToPath } from "node:url";
import { _electron } from "playwright";

const root = resolve(fileURLToPath(new URL("..", import.meta.url)));
const home = mkdtempSync(join(tmpdir(), "reasonix-attachment-native-"));
const workspace = join(home, "workspace");
mkdirSync(workspace);
const nativeFiles = ["large app.apk", "pasted app.apk", "dropped app.apk"].map(name => {
  const path = join(home, name);
  const fd = openSync(path, "w");
  ftruncateSync(fd, 120 * 1024 * 1024);
  closeSync(fd);
  return path;
});
const [apk] = nativeFiles;
const report = { checks: [], errors: [] };
const check = name => { report.checks.push(name); console.log(`PASS ${name}`); };
const invoke = (page, name, args = []) => page.evaluate(({ name, args }) => window.reasonixDesktop.invoke(name, args), { name, args });
async function until(predicate) {
  const deadline = Date.now() + 60000;
  while (Date.now() < deadline) {
    if (await predicate()) return;
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  throw new Error("Attachment smoke condition timed out");
}
const shell = await _electron.launch({ args: [root, `--user-data-dir=${join(home, "electron")}`], env: {
  ...process.env, REASONIX_HOME: home, REASONIX_STATE_HOME: home, REASONIX_CACHE_HOME: join(home, "cache"),
  REASONIX_DEV: "1", REASONIX_DESKTOP_SERVICE: resolve(root, "../build/bin/reasonix-desktop-service"),
}, timeout: 60000 });
try {
  const page = await shell.firstWindow();
  page.on("pageerror", error => report.errors.push(error.message));
  await page.waitForURL("reasonix://app/index.html", { waitUntil: "domcontentloaded", timeout: 60000 });
  await page.waitForFunction(() => Boolean(window.reasonixDesktop) && !document.querySelector(".boot-shell"), null, { timeout: 60000 });
  await until(async () => { try { await invoke(page, "Version"); return true; } catch { return false; } });
  const operationId = "attachment-native-smoke";
  await invoke(page, "BeginManualSessionCreation", [{ operationId, scope: "project", workspaceRoot: workspace }]);
  await until(async () => {
    const creation = await invoke(page, "GetManualSessionCreation", [operationId]);
    if (creation.phase === "failed") throw new Error(creation.error);
    return creation.phase === "ready";
  });
  const creation = await invoke(page, "GetManualSessionCreation", [operationId]);
  report.session = creation.ref;
  const tabs = await invoke(page, "ListTabs");
  const tab = tabs.find(item => item.sessionId === creation.ref.sessionId);
  assert.ok(tab, JSON.stringify(tabs));
  await invoke(page, "SetActiveTab", [tab.id]);
  await page.reload();
  const input = page.locator(".composer-content-file-input");
  await input.waitFor({ state: "attached", timeout: 60000 });
  await page.waitForFunction(() => !document.querySelector(".composer-content-file-input")?.disabled);
  await page.locator(".startup-splash").waitFor({ state: "hidden", timeout: 60000 });
  await page.locator("#composer-input").waitFor({ state: "visible" });
  let uploads = 0;
  page.on("request", request => { if (request.url().includes("/__reasonix_attachment_upload")) uploads++; });
  await input.setInputFiles(apk);
  await until(async () => {
    const state = await invoke(page, "GetSessionComposerState", [creation.ref]);
    return JSON.parse(state.contentJson).workspaceRefs?.some(item => item.path.endsWith("large app.apk"));
  });
  assert.equal(uploads, 0);
  assert.equal(statSync(apk).size, 120 * 1024 * 1024);
  check("120 MiB native picker adds a path reference without an HTTP upload");
  await page.evaluate(() => {
    const source = document.createElement("input");
    source.type = "file";
    source.id = "attachment-smoke-source";
    document.body.append(source);
  });
  for (const [index, kind] of ["paste", "drop"].entries()) {
    await page.locator("#attachment-smoke-source").setInputFiles(nativeFiles[index + 1]);
    await page.evaluate(kind => {
      const transfer = new DataTransfer();
      transfer.items.add(document.querySelector("#attachment-smoke-source").files[0]);
      const event = kind === "paste"
        ? new ClipboardEvent("paste", { clipboardData: transfer, bubbles: true })
        : new DragEvent("drop", { dataTransfer: transfer, bubbles: true });
      document.querySelector("#composer-input").dispatchEvent(event);
    }, kind);
    await until(async () => {
      const state = await invoke(page, "GetSessionComposerState", [creation.ref]);
      return JSON.parse(state.contentJson).workspaceRefs?.length === index + 2;
    });
    assert.equal(uploads, 0);
    check(`120 MiB native ${kind} adds a path reference without an HTTP upload`);
  }
  await page.evaluate(() => {
    const transfer = new DataTransfer();
    const chunk = new Uint8Array(64 * 1024).fill(37);
    transfer.items.add(new File(Array(1280).fill(chunk), "virtual.apk", { type: "application/vnd.android.package-archive" }));
    const input = document.querySelector(".composer-content-file-input");
    input.files = transfer.files;
    input.dispatchEvent(new Event("change", { bubbles: true }));
  });
  await until(async () => {
    const state = await invoke(page, "GetSessionComposerState", [creation.ref]);
    return JSON.parse(state.contentJson).attachments?.some(item => item.displayName === "virtual.apk");
  });
  const saved = JSON.parse((await invoke(page, "GetSessionComposerState", [creation.ref])).contentJson);
  const uploaded = saved.attachments.find(item => item.displayName === "virtual.apk");
  const stored = join(workspace, uploaded.path);
  assert.equal(statSync(stored).size, 80 * 1024 * 1024);
  assert.equal(uploads, 1);
  assert.equal(readFileSync(stored).every(byte => byte === 37), true);
  check("80 MiB pathless File streams through the real Electron protocol and Go service byte-for-byte");
  assert.equal(typeof await invoke(page, "Version"), "string");
  assert.equal(report.errors.length, 0, report.errors.join("\n"));
  assert.equal(await page.locator("[data-crash-overlay], .error-boundary").count(), 0);
  await page.locator("#composer-input").fill("Attachment smoke remains responsive");
  assert.equal(await page.locator("#composer-input").inputValue(), "Attachment smoke remains responsive");
  await page.screenshot({ path: join(home, "attachments.png") });
  check("renderer and service remain responsive with all four large files attached");
} finally {
  await shell.close();
  writeFileSync(join(home, "attachment-smoke-results.json"), JSON.stringify(report, null, 2));
  console.log(`Evidence: ${home}`);
}
