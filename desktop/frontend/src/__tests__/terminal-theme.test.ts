// Run: tsx src/__tests__/terminal-theme.test.ts

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { JSDOM } from "jsdom";

const dom = new JSDOM("<!doctype html><html><head></head><body><div id='terminal'></div></body></html>");
Object.assign(globalThis, {
  window: dom.window,
  document: dom.window.document,
  MutationObserver: dom.window.MutationObserver,
  getComputedStyle: dom.window.getComputedStyle.bind(dom.window),
});
Object.defineProperty(dom.window, "matchMedia", {
  configurable: true,
  value: () => ({
    matches: false,
    addEventListener() {},
    removeEventListener() {},
  }),
});

const { applyTheme } = await import("../lib/theme");
const {
  applyTerminalThemePreference,
  createTerminalThemeSaveQueue,
  getResolvedTerminalTheme,
  normalizeTerminalThemePreference,
  onTerminalThemePreferenceChange,
  terminalThemeForElement,
} = await import("../lib/terminalTheme");

assert.equal(normalizeTerminalThemePreference("unknown"), "auto");
assert.equal(normalizeTerminalThemePreference(undefined), "auto", "older settings payloads fall back safely");
assert.equal(normalizeTerminalThemePreference("light"), "light");

applyTheme("light", "graphite");
applyTerminalThemePreference("auto");
assert.equal(getResolvedTerminalTheme(), "light", "follow-app resolves the current app theme");
assert.equal(document.documentElement.hasAttribute("data-terminal-theme"), false);

applyTerminalThemePreference("dark");
assert.equal(getResolvedTerminalTheme(), "dark", "explicit terminal theme overrides the app theme");
assert.equal(document.documentElement.getAttribute("data-terminal-theme"), "dark");

let notifications = 0;
const unsubscribe = onTerminalThemePreferenceChange(() => { notifications += 1; });
applyTerminalThemePreference("light");
unsubscribe();
assert.equal(notifications, 1, "open terminals are notified when the preference changes");

const host = document.getElementById("terminal")!;
host.style.setProperty("--terminal-bg", "#fafafa");
host.style.setProperty("--terminal-fg", "#202124");
host.style.setProperty("--terminal-cursor", "#9a4f00");
const xtermTheme = terminalThemeForElement(host);
assert.equal(xtermTheme.background, "#fafafa");
assert.equal(xtermTheme.foreground, "#202124");
assert.equal(xtermTheme.cursor, "#9a4f00");
assert.equal(xtermTheme.black, "#25272a", "light terminal uses a contrast-safe ANSI palette");

function relativeLuminance(hex: string): number {
  assert.match(hex, /^#[0-9a-f]{6}$/i);
  const channels = [1, 3, 5].map((start) => Number.parseInt(hex.slice(start, start + 2), 16) / 255);
  const linear = channels.map((channel) => channel <= 0.04045
    ? channel / 12.92
    : ((channel + 0.055) / 1.055) ** 2.4);
  return 0.2126 * linear[0] + 0.7152 * linear[1] + 0.0722 * linear[2];
}

function contrastRatio(foreground: string, background: string): number {
  const [lighter, darker] = [relativeLuminance(foreground), relativeLuminance(background)]
    .sort((a, b) => b - a);
  return (lighter + 0.05) / (darker + 0.05);
}

const ansiKeys = [
  "black", "red", "green", "yellow", "blue", "magenta", "cyan", "white",
  "brightBlack", "brightRed", "brightGreen", "brightYellow", "brightBlue",
  "brightMagenta", "brightCyan", "brightWhite",
] as const;
for (const key of ansiKeys) {
  const color = xtermTheme[key];
  assert.equal(typeof color, "string");
  assert.ok(
    contrastRatio(color!, xtermTheme.background!) >= 4.5,
    `${key} must remain readable against the light terminal background`,
  );
}

let releaseFirstSave!: () => void;
const firstSaveGate = new Promise<void>((resolve) => { releaseFirstSave = resolve; });
const saveOrder: string[] = [];
const saveTerminalTheme = createTerminalThemeSaveQueue(async (theme) => {
  saveOrder.push(`start:${theme}`);
  if (theme === "light") await firstSaveGate;
  saveOrder.push(`end:${theme}`);
});
const firstSave = saveTerminalTheme("light");
await Promise.resolve();
const secondSave = saveTerminalTheme("dark");
await Promise.resolve();
assert.deepEqual(saveOrder, ["start:light"], "newer intent waits for the active Wails save");
releaseFirstSave();
await Promise.all([firstSave, secondSave]);
assert.deepEqual(saveOrder, ["start:light", "end:light", "start:dark", "end:dark"]);

const recoveryOrder: string[] = [];
const recoverAfterFailure = createTerminalThemeSaveQueue(async (theme) => {
  recoveryOrder.push(theme);
  if (theme === "light") throw new Error("simulated save failure");
});
const failedSave = recoverAfterFailure("light");
const recoveredSave = recoverAfterFailure("dark");
await assert.rejects(failedSave, /simulated save failure/);
await recoveredSave;
assert.deepEqual(recoveryOrder, ["light", "dark"], "a failed save does not poison newer intent");

const testDir = dirname(fileURLToPath(import.meta.url));
const terminalSource = readFileSync(resolve(testDir, "../components/TerminalView.tsx"), "utf8");
const settingsSource = readFileSync(resolve(testDir, "../components/SettingsPanel.tsx"), "utf8");
const stylesSource = readFileSync(resolve(testDir, "../styles.css"), "utf8");
assert.ok(terminalSource.includes("terminal.options.theme = terminalThemeForElement(host)"));
assert.ok(!terminalSource.includes('background: "#111315"'));
assert.ok(settingsSource.includes("createTerminalThemeSaveQueue"));
assert.ok(settingsSource.includes("if (!terminalThemeSavePending.current)"));
assert.ok(settingsSource.includes("onTerminalTheme={setTerminalThemePreference}"));
assert.ok(stylesSource.includes(':root[data-terminal-theme="light"]'));
assert.ok(stylesSource.includes(':root[data-terminal-theme="dark"]'));

console.log("terminal theme tests passed");
