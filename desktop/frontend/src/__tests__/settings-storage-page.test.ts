// Run: tsx src/__tests__/settings-storage-page.test.ts

import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const testDir = dirname(fileURLToPath(import.meta.url));
const settings = readFileSync(resolve(testDir, "../components/SettingsPanel.tsx"), "utf8");
const styles = readFileSync(resolve(testDir, "../styles.css"), "utf8");
const types = readFileSync(resolve(testDir, "../lib/types.ts"), "utf8");
const locales = ["zh.ts", "zh-TW.ts", "en.ts"].map((name) => readFileSync(resolve(testDir, `../locales/${name}`), "utf8"));

let passed = 0;
let failed = 0;

function ok(condition: boolean, label: string) {
  if (condition) {
    process.stdout.write(`  PASS  ${label}\n`);
    passed += 1;
  } else {
    process.stdout.write(`  FAIL  ${label}\n`);
    failed += 1;
  }
}

console.log("\nsettings storage page contract");

const tabs = settings.match(/const SETTINGS_TABS: SettingsTab\[\] = \[([^\]]+)\]/)?.[1] ?? "";
ok(/"storage",\s*"updates"\s*$/.test(tabs), "Storage is immediately before Updates in the settings navigation");
ok(types.includes('| "storage" | "updates";'), "SettingsTab exposes the storage route before updates");
ok(settings.includes('tab === "storage"') && settings.includes("<StorageSettingsSection s={s} />"), "Storage renders the same page on every platform");

const generalStart = settings.indexOf("function GeneralSection(");
const generalEnd = settings.indexOf("function ModelsSection(", generalStart);
const generalSection = settings.slice(generalStart, generalEnd);
const storageStart = settings.indexOf("function StorageSettingsSection(");
const storageEnd = settings.indexOf("function GeneralSection(", storageStart);
const storageSection = settings.slice(storageStart, storageEnd);
ok(!generalSection.includes("StorageSettingsSection"), "General no longer embeds storage settings");
ok(storageSection.includes('className="mem-input"') && !storageSection.includes('className="text-input"'), "Storage paths reuse the shared settings input recipe");
ok((storageSection.match(/readOnly/g) ?? []).length === 2, "Default workspace and repeated storage inputs are explicitly read-only");
ok(!storageSection.includes("PickStorageFolder") && !storageSection.includes("MigrateStorage") && !storageSection.includes("SetDefaultWorkspace") && !storageSection.includes("RestartApplication"), "Storage page exposes no path mutation or restart actions");
ok(!storageSection.includes("<button") && !styles.includes("settings-path-control__migrate"), "Storage page contains no hidden migration controls or migration-only styling");
ok(!storageSection.includes("storageReadOnlyHint") && locales.every((locale) => !locale.includes("settings.storageReadOnlyHint")), "Storage page omits the redundant read-only notice");
ok(locales.every((locale) => !/Windows.*(?:迁移|移動|migrat)/i.test(locale.match(/"settings\.pageDesc\.storage":\s*"[^"]*"/)?.[0] ?? "")), "Storage page descriptions do not advertise Windows migration");

console.log(`\n${passed} passed, ${failed} failed, ${passed + failed} total`);
if (failed > 0) process.exit(1);
