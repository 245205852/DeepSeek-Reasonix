#!/usr/bin/env node

import { spawnSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const metricUserColumnDefinitions = Object.freeze({
  arch: "TEXT NOT NULL DEFAULT ''",
  os_build: "INTEGER NOT NULL DEFAULT 0",
  os_revision: "INTEGER NOT NULL DEFAULT 0",
  channel: "TEXT NOT NULL DEFAULT ''",
  distro_id: "TEXT NOT NULL DEFAULT ''",
  distro_version: "TEXT NOT NULL DEFAULT ''",
  kernel_version: "TEXT NOT NULL DEFAULT ''",
  session_type: "TEXT NOT NULL DEFAULT ''",
  runtime_engine: "TEXT NOT NULL DEFAULT ''",
  runtime_version: "TEXT NOT NULL DEFAULT ''",
  gpu_mode: "TEXT NOT NULL DEFAULT ''",
  event_count: "INTEGER NOT NULL DEFAULT 0",
});

const columnSets = {
  reports: ["webview2", "web_runtime"],
  pings: [
    "os_build", "os_revision", "channel", "distro_id", "distro_version",
    "kernel_version", "session_type", "runtime_engine", "runtime_version", "gpu_mode",
  ],
  cli_pings: [
    "os_build", "os_revision", "channel", "distro_id", "distro_version",
    "kernel_version", "session_type", "runtime_engine", "runtime_version", "gpu_mode",
  ],
  metric_users: Object.keys(metricUserColumnDefinitions),
  cli_metric_users: Object.keys(metricUserColumnDefinitions),
};

const requiredObjects = {
  table: ["report_daily", "report_installations", "report_event_dimensions", "diagnostics_meta"],
  index: [
    "report_installations_fingerprint_date",
    "report_event_dimensions_fingerprint_date",
    "pings_diagnostics_window",
  ],
};

export const diagnosticsV2SchemaEntries = Object.freeze([
  ...Object.entries(columnSets).flatMap(([table, columns]) =>
    columns.map((column) => `column:${table}.${column}`)),
  ...Object.entries(requiredObjects).flatMap(([kind, names]) =>
    names.map((name) => `${kind}:${name}`)),
]);

export const diagnosticsV2ReconciliationEntries = Object.freeze(
  ["metric_users", "cli_metric_users"].flatMap((table) =>
    columnSets[table].map((column) => `column:${table}.${column}`)),
);

const reconciliationEntrySet = new Set(diagnosticsV2ReconciliationEntries);

const tableNames = Object.keys(columnSets).map((name) => `'${name}'`).join(", ");
const objectNames = Object.values(requiredObjects).flat().map((name) => `'${name}'`).join(", ");

export const diagnosticsV2SchemaQuery = `
SELECT 'column' AS kind, m.name || '.' || p.name AS name
FROM sqlite_master AS m, pragma_table_info(m.name) AS p
WHERE m.type = 'table' AND m.name IN (${tableNames})
UNION ALL
SELECT type AS kind, name
FROM sqlite_master
WHERE type IN ('table', 'index') AND name IN (${objectNames})
ORDER BY kind, name;
`.trim();

export function parseWranglerRows(output) {
  const payload = JSON.parse(output);
  const rows = [];
  let sawResults = false;
  const visit = (value) => {
    if (Array.isArray(value)) {
      for (const item of value) visit(item);
      return;
    }
    if (!value || typeof value !== "object") return;
    if (Array.isArray(value.results)) {
      sawResults = true;
      rows.push(...value.results);
    }
    if (Array.isArray(value.result)) visit(value.result);
  };
  visit(payload);
  if (!sawResults) throw new Error("Wrangler returned no D1 result set");
  return rows;
}

export function classifyDiagnosticsV2Schema(rows) {
  const present = new Set(rows.map((row) => `${String(row.kind)}:${String(row.name)}`));
  const missing = diagnosticsV2SchemaEntries.filter((entry) => !present.has(entry));
  if (missing.length === 0) return { state: "complete", missing };
  if (missing.length === diagnosticsV2SchemaEntries.length) return { state: "absent", missing };
  return { state: "partial", missing };
}

export function isKnownDiagnosticsV2Reconciliation(state) {
  return state.state === "partial"
    && state.missing.length > 0
    && state.missing.every((entry) => reconciliationEntrySet.has(entry));
}

export function parseDiagnosticsV2ReconciliationSQL(sql) {
  const statements = new Map();
  for (const rawStatement of sql.split(";")) {
    const statement = rawStatement.trim();
    if (!statement) continue;
    const match = statement.match(
      /^ALTER TABLE (metric_users|cli_metric_users) ADD COLUMN ([a-z_]+) (.+)$/,
    );
    if (!match) throw new Error(`Unexpected diagnostics v2 reconciliation statement: ${statement}`);
    const entry = `column:${match[1]}.${match[2]}`;
    const definition = metricUserColumnDefinitions[match[2]];
    const expected = definition ? `ALTER TABLE ${match[1]} ADD COLUMN ${match[2]} ${definition}` : "";
    if (!reconciliationEntrySet.has(entry) || statements.has(entry) || statement !== expected) {
      throw new Error(`Unexpected diagnostics v2 reconciliation entry: ${entry}`);
    }
    statements.set(entry, `${statement};`);
  }
  const missingDefinitions = diagnosticsV2ReconciliationEntries.filter((entry) => !statements.has(entry));
  if (missingDefinitions.length > 0) {
    throw new Error(`Diagnostics v2 reconciliation SQL is incomplete: ${missingDefinitions.join(", ")}`);
  }
  return statements;
}

function runWrangler(projectDir, args, captureOutput = false) {
  const executable = process.platform === "win32" ? "wrangler.cmd" : "wrangler";
  const wrangler = path.join(projectDir, "node_modules", ".bin", executable);
  const result = spawnSync(wrangler, args, {
    cwd: projectDir,
    encoding: "utf8",
    env: process.env,
    stdio: captureOutput ? ["ignore", "pipe", "inherit"] : "inherit",
  });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`wrangler exited with status ${result.status}`);
  return result.stdout ?? "";
}

function inspectRemoteSchema(projectDir, database) {
  const output = runWrangler(projectDir, [
    "d1", "execute", database, "--remote", "--json", "--command", diagnosticsV2SchemaQuery,
  ], true);
  return classifyDiagnosticsV2Schema(parseWranglerRows(output));
}

function main() {
  const projectDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
  const database = process.env.DIAGNOSTICS_D1_DATABASE || "reasonix-crash";
  const before = inspectRemoteSchema(projectDir, database);
  if (before.state === "complete") {
    console.log("Diagnostics v2 D1 schema is already complete; migration skipped.");
    return;
  }
  if (before.state === "partial") {
    if (!isKnownDiagnosticsV2Reconciliation(before)) {
      throw new Error(
        `Diagnostics v2 D1 schema has an unknown partial state; refusing reconciliation. Missing: ${before.missing.join(", ")}`,
      );
    }
    const reconciliationSQL = readFileSync(
      path.join(projectDir, "migrate-diagnostics-v2-reconcile.sql"),
      "utf8",
    );
    const reconciliationStatements = parseDiagnosticsV2ReconciliationSQL(reconciliationSQL);
    console.log("Recording the current D1 Time Travel bookmark before known-state reconciliation.");
    runWrangler(projectDir, ["d1", "time-travel", "info", database]);
    console.log(`Reconciling ${before.missing.length} known diagnostics v2 columns in ${database}.`);
    for (const entry of before.missing) {
      const statement = reconciliationStatements.get(entry);
      if (!statement) throw new Error(`Missing diagnostics v2 reconciliation statement: ${entry}`);
      try {
        runWrangler(projectDir, [
          "d1", "execute", database, "--remote", "--yes", "--command",
          statement,
        ]);
      } catch (error) {
        const current = inspectRemoteSchema(projectDir, database);
        if (current.missing.includes(entry)) throw error;
        console.log(`Verified ${entry} after Wrangler returned an error; continuing reconciliation.`);
      }
    }
    const after = inspectRemoteSchema(projectDir, database);
    if (after.state !== "complete") {
      throw new Error(`Diagnostics v2 D1 reconciliation verification failed. Missing: ${after.missing.join(", ")}`);
    }
    console.log("Diagnostics v2 D1 known-state reconciliation and verification completed.");
    return;
  }

  console.log("Recording the current D1 Time Travel bookmark before migration.");
  runWrangler(projectDir, ["d1", "time-travel", "info", database]);
  console.log(`Applying diagnostics v2 migration to ${database}.`);
  runWrangler(projectDir, [
    "d1", "execute", database, "--remote", "--yes", "--file", "migrate-diagnostics-v2.sql",
  ]);
  const after = inspectRemoteSchema(projectDir, database);
  if (after.state !== "complete") {
    throw new Error(`Diagnostics v2 D1 schema verification failed. Missing: ${after.missing.join(", ")}`);
  }
  console.log("Diagnostics v2 D1 schema migration and verification completed.");
}

const invokedPath = process.argv[1] ? path.resolve(process.argv[1]) : "";
if (invokedPath === fileURLToPath(import.meta.url)) {
  try {
    main();
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error));
    process.exitCode = 1;
  }
}
