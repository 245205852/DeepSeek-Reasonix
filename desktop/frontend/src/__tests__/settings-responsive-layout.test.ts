// Run: tsx src/__tests__/settings-responsive-layout.test.ts

import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const testDir = dirname(fileURLToPath(import.meta.url));
const styles = readFileSync(resolve(testDir, "../styles.css"), "utf8");

let passed = 0;
let failed = 0;

function eq(actual: unknown, expected: unknown, label: string) {
  if (actual === expected) {
    process.stdout.write(`  PASS  ${label}\n`);
    passed += 1;
  } else {
    process.stdout.write(`  FAIL  ${label}: expected ${JSON.stringify(expected)}, got ${JSON.stringify(actual)}\n`);
    failed += 1;
  }
}

function ruleBlock(source: string, selector: string): string {
  const start = source.indexOf(`${selector} {`);
  if (start < 0) return "";
  const bodyStart = source.indexOf("{", start) + 1;
  const end = source.indexOf("}", bodyStart);
  return source.slice(bodyStart, end);
}

function declaration(block: string, property: string): string | undefined {
  const match = block.match(new RegExp(`(?:^|;)\\s*${property}\\s*:\\s*([^;]+)`));
  return match?.[1].trim();
}

console.log("\nsettings responsive layout contract");

const settingsModal = ruleBlock(styles, ".settings-modal");
eq(declaration(settingsModal, "width"), "min(1380px, calc(100vw - clamp(32px, calc(96px - (900px - 100vw) * 0.457), 96px)))", "wide settings modal keeps centered desktop margins");
eq(declaration(settingsModal, "height"), "min(960px, calc(100vh - 80px))", "wide settings modal keeps the intended desktop height without becoming full screen");
const settingsCenter = ruleBlock(styles, ".settings-center");
eq(declaration(settingsCenter, "grid-template-columns"), "clamp(220px, 20.5vw, 304px) minmax(0, 1fr)", "settings navigation remains readable without consuming the content pane");

const generalSectionBoundary = ruleBlock(styles, ".settings-page--general > .settings-section:not(:last-child) .settings-section__body");
eq(
  declaration(generalSectionBoundary, "border-bottom"),
  "1px solid var(--border-soft)",
  "general settings end intermediate sections with a subtle divider",
);

const wideTail = ruleBlock(styles, ".memory-tabs-row__tail");
eq(declaration(wideTail, "display"), "contents", "wide memory controls preserve the original row flex items");
eq(declaration(wideTail, "flex"), undefined, "wide memory wrapper does not compete in flex sizing");

const settingsResponsiveStart = styles.indexOf("/* ── responsive: compact content on narrow screens");
const narrowStart = styles.indexOf("@media (max-width: 900px)", settingsResponsiveStart);
const narrowEnd = styles.indexOf("@media (max-width: 760px)", narrowStart);
const narrowSettings = styles.slice(narrowStart, narrowEnd);
const narrowTail = ruleBlock(narrowSettings, ".memory-tabs-row__tail");

eq(declaration(narrowTail, "display"), "flex", "narrow memory controls form a flex group");
eq(declaration(narrowTail, "flex"), "0 1 100%", "narrow memory controls stay on their own row");
eq(declaration(narrowTail, "min-width"), "0", "narrow memory controls may shrink without overflowing");
eq(declaration(narrowTail, "gap"), "12px", "narrow memory controls retain their compact spacing");

const narrowModalOverrideSelector = ".settings-modal.management-modal,\n  :root[data-theme-style] .settings-modal.management-modal";
const narrowModalOverride = ruleBlock(styles, narrowModalOverrideSelector);
eq(declaration(narrowModalOverride, "width"), "100vw", "minimum-width settings modal spans the viewport");
eq(declaration(narrowModalOverride, "height"), "100vh", "minimum-width settings modal uses the full viewport height");
eq(declaration(narrowModalOverride, "max-height"), "100vh", "minimum-width settings modal clears the shared modal height cap");
eq(declaration(narrowModalOverride, "border-radius"), "0", "minimum-width settings modal removes floating-panel corners");
eq(
  styles.indexOf(narrowModalOverrideSelector) > styles.indexOf(":root[data-theme-style] .management-modal,"),
  true,
  "minimum-width settings modal override follows the shared theme shell",
);

console.log(`\n${passed} passed, ${failed} failed, ${passed + failed} total`);
if (failed > 0) process.exit(1);
