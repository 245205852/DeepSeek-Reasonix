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
  getResolvedTerminalTheme,
  normalizeTerminalThemePreference,
  onTerminalThemePreferenceChange,
  terminalThemeForElement,
} = await import("../lib/terminalTheme");

assert.equal(normalizeTerminalThemePreference("unknown"), "auto");
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

const testDir = dirname(fileURLToPath(import.meta.url));
const terminalSource = readFileSync(resolve(testDir, "../components/TerminalView.tsx"), "utf8");
const stylesSource = readFileSync(resolve(testDir, "../styles.css"), "utf8");
assert.ok(terminalSource.includes("terminal.options.theme = terminalThemeForElement(host)"));
assert.ok(!terminalSource.includes('background: "#111315"'));
assert.ok(stylesSource.includes(':root[data-terminal-theme="light"]'));
assert.ok(stylesSource.includes(':root[data-terminal-theme="dark"]'));

console.log("terminal theme tests passed");
